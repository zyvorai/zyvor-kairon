// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// importPreflightEnv holds seams so tests need no guestkit binary.
type importPreflightEnv struct {
	lookPath  func(string) (string, error)
	runDoctor func(ctx context.Context, bin, image string) ([]byte, error)
	readFile  func(path string) ([]byte, error)
}

func defaultImportPreflightEnv() importPreflightEnv {
	return importPreflightEnv{
		lookPath: exec.LookPath,
		// GuestKit's doctor flag is `-o json` (or `--output json`), with the
		// image as a positional argument. `kaironctl preflight --image-doctor`
		// still invokes `guestkit doctor --json IMAGE`; that spelling is not
		// what current GuestKit accepts.
		runDoctor: func(ctx context.Context, bin, image string) ([]byte, error) {
			return exec.CommandContext(ctx, bin, "doctor", image, "--target", "kvm", "-o", "json").Output()
		},
		readFile: os.ReadFile,
	}
}

// importPreflightOpts is the parsed `import preflight` command line.
type importPreflightOpts struct {
	source   string
	output   string
	fromJSON string
	firmware string
	strict   bool
}

const importPreflightUsage = "usage: kaironctl import preflight SOURCE [-o table|json|yaml] [--strict] [--firmware bios|efi] [--from-json PATH]"

func parseImportPreflightArgs(args []string) (importPreflightOpts, error) {
	var opts importPreflightOpts
	fs := flag.NewFlagSet("import preflight", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.output, "output", "table", "output format: table|json|yaml")
	fs.StringVar(&opts.output, "o", "table", "output format: table|json|yaml")
	fs.BoolVar(&opts.strict, "strict", false, "exit non-zero on warnings as well as failures")
	fs.StringVar(&opts.firmware, "firmware", "", "expected firmware (bios or efi); compared with OVF firmware when present")
	fs.StringVar(&opts.fromJSON, "from-json", "", "read a saved guestkit doctor (or inspect) JSON instead of running guestkit; PATH may be - for stdin")
	// Match `import ova SOURCE [flags]`: the source may come before the
	// flags. Go's flag package otherwise stops at the first positional.
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		opts.source = args[0]
		flagArgs = args[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, fmt.Errorf("%s\n%w", importPreflightUsage, err)
	}
	rest := fs.Args()
	switch {
	case opts.source == "" && len(rest) == 0:
		if opts.fromJSON == "" {
			return opts, fmt.Errorf("%s", importPreflightUsage)
		}
	case opts.source == "" && len(rest) == 1:
		opts.source = rest[0]
	case opts.source != "" && len(rest) == 0:
		// already set
	default:
		return opts, fmt.Errorf("%s", importPreflightUsage)
	}
	switch opts.output {
	case "", "table", "json", "yaml":
		if opts.output == "" {
			opts.output = "table"
		}
	default:
		return opts, fmt.Errorf("unknown output %q (want table, json or yaml)", opts.output)
	}
	fw, err := normalizeFirmware(opts.firmware)
	if err != nil {
		return opts, err
	}
	opts.firmware = fw
	return opts, nil
}

func importPreflightArgs(cmd interface {
	Context() context.Context
	InOrStdin() io.Reader
	OutOrStdout() io.Writer
}, args []string) error {
	opts, err := parseImportPreflightArgs(args)
	if err != nil {
		return err
	}
	rep, err := runImportPreflight(cmd.Context(), opts, cmd.InOrStdin(), defaultImportPreflightEnv())
	if err != nil {
		return err
	}
	if err := writeImportPreflight(cmd.OutOrStdout(), rep, opts.output); err != nil {
		return err
	}
	if rep.Summary.Fail > 0 || (opts.strict && rep.Summary.Warn > 0) {
		return &ExitCodeError{Code: 1}
	}
	return nil
}

const (
	pfPass = "PASS"
	pfWarn = "WARN"
	pfFail = "FAIL"
)

// importPreflightCheck is one line of the pre-import report.
type importPreflightCheck struct {
	Status  string `json:"status"`
	Check   string `json:"check"`
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
	ID      string `json:"id,omitempty"`
}

type importPreflightSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// importPreflightReport is the JSON/YAML document of `import preflight`.
type importPreflightReport struct {
	Source  string                 `json:"source,omitempty"`
	Score   *float64               `json:"score,omitempty"`
	Target  string                 `json:"target,omitempty"`
	Checks  []importPreflightCheck `json:"checks"`
	Summary importPreflightSummary `json:"summary"`
}

func (r *importPreflightReport) add(status, check, message, source, id string) {
	r.Checks = append(r.Checks, importPreflightCheck{
		Status:  status,
		Check:   check,
		Message: message,
		Source:  source,
		ID:      id,
	})
	switch status {
	case pfFail:
		r.Summary.Fail++
	case pfWarn:
		r.Summary.Warn++
	default:
		r.Summary.Pass++
	}
}

func writeImportPreflight(w io.Writer, rep *importPreflightReport, output string) error {
	if rep.Checks == nil {
		rep.Checks = []importPreflightCheck{}
	}
	switch output {
	case "json", "yaml":
		return writeStructured(w, rep, output)
	}
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STATUS\tCHECK\tMESSAGE")
	for _, c := range rep.Checks {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Status, c.Check, c.Message)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "\n%d fail, %d warn, %d pass\n", rep.Summary.Fail, rep.Summary.Warn, rep.Summary.Pass)
	return nil
}

// runImportPreflight is the offline entry point: format and OVF checks, then
// a read-only GuestKit doctor (or a saved JSON). It never contacts a cluster
// and never converts or repairs a disk.
func runImportPreflight(ctx context.Context, opts importPreflightOpts, stdin io.Reader, env importPreflightEnv) (*importPreflightReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if env.lookPath == nil {
		env.lookPath = exec.LookPath
	}
	if env.readFile == nil {
		env.readFile = os.ReadFile
	}
	rep := &importPreflightReport{Source: opts.source, Checks: []importPreflightCheck{}}

	var windows bool
	if opts.source != "" {
		windows = addSourceChecks(rep, opts)
	}

	if opts.fromJSON != "" {
		raw, err := readImportPreflightJSON(opts.fromJSON, stdin, env)
		if err != nil {
			return nil, err
		}
		if err := applyGuestkitJSON(rep, raw, windows); err != nil {
			return nil, err
		}
		return rep, nil
	}

	if opts.source == "" {
		return nil, fmt.Errorf("%s", importPreflightUsage)
	}
	if shouldRunGuestkitDoctor(opts.source) {
		if err := runGuestkitDoctor(ctx, rep, opts.source, windows, env); err != nil {
			return nil, err
		}
	} else {
		rep.add(pfWarn, "guestkit", "guestkit doctor was not run: an OVA/OVF is an archive. Run import preflight on a disk from the package (vmdk, qcow2, raw, vhd, vhdx) for virtio, fstab and cloud-init checks.", "format", "")
	}
	return rep, nil
}

// addSourceChecks records format and (for OVA/OVF) firmware/OS findings.
// It returns whether the OVF looks like a Windows guest.
func addSourceChecks(rep *importPreflightReport, opts importPreflightOpts) bool {
	format, known := sourceFormat(opts.source)
	switch {
	case format != "" && !known:
		rep.add(pfFail, "format", fmt.Sprintf("%s is not a supported Kairon import format (want qcow2, raw, ova, vmdk, vhd or vhdx); FluxVM will refuse conversion", format), "format", "")
	case format != "" && known:
		rep.add(pfPass, "format", fmt.Sprintf("%s is a supported Kairon import format", format), "format", "")
	default:
		rep.add(pfWarn, "format", "could not tell the disk format from the source name; set spec.image.source.format explicitly (qcow2, raw, ova, vmdk, vhd or vhdx)", "format", "")
	}

	info, err := readSourceOVF(opts.source)
	if err != nil {
		if isOVAName(opts.source) {
			rep.add(pfFail, "ovf", err.Error(), "ovf", "")
		}
		return false
	}
	if info == nil {
		return false
	}
	windows := looksLikeWindowsOS(info.OS)
	if info.Firmware != "" {
		addFirmwareChecks(rep, info.Firmware, opts.firmware)
	} else if opts.firmware != "" {
		rep.add(pfWarn, "firmware", "the OVF does not declare firmware; cannot compare with --firmware "+opts.firmware, "ovf", "")
	}
	if windows {
		rep.add(pfWarn, "windows-virtio", "OVF OS "+quoteOr(info.OS, "windows")+" looks like Windows; first boot on KVM needs virtio-win drivers (FluxVM injects them when virtio_win_dir is set, otherwise install them before export). Run import preflight on the disk for GuestKit virtio findings.", "ovf", "")
	}
	if info.Disks > 1 {
		rep.add(pfWarn, "disks", fmt.Sprintf("the OVA has %d disks; confirm data disks are attached as import-diskN after conversion", info.Disks), "ovf", "")
	}
	return windows
}

func addFirmwareChecks(rep *importPreflightReport, sourceFW, wantFW string) {
	src, _ := normalizeFirmware(sourceFW)
	if src == "" {
		return
	}
	if wantFW != "" && src != wantFW {
		rep.add(pfFail, "firmware", fmt.Sprintf("BIOS vs UEFI mismatch: source firmware is %s, --firmware is %s", src, wantFW), "ovf", "")
		return
	}
	if src == "efi" {
		rep.add(pfWarn, "firmware", "source VM boots UEFI; schedule it on a node whose FluxVM sets qemu_ovmf_code", "ovf", "")
		return
	}
	rep.add(pfPass, "firmware", "source VM boots BIOS", "ovf", "")
}

func readSourceOVF(source string) (*ovfInfo, error) {
	if !isOVAName(source) {
		return nil, nil
	}
	if strings.HasSuffix(strings.ToLower(sourceName(source)), ".ovf") && !isHTTPSource(source) {
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read OVF: %w", err)
		}
		info, err := parseOVF(data)
		if err != nil {
			return nil, err
		}
		return &info, nil
	}
	rc, err := openImportSource(context.Background(), source)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	ovf, _, _, err := scanOVA(rc)
	if err != nil {
		return nil, err
	}
	infos, _, err := parseOVFPackage(ovf)
	if err != nil {
		info, err := parseOVF(ovf)
		if err != nil {
			return nil, err
		}
		return &info, nil
	}
	if len(infos) == 0 {
		return nil, errors.New("OVA has no VirtualSystem")
	}
	// Multi-VM: report on the first system; disk count and firmware of
	// later VMs can differ, so callers should preflight each disk too.
	info := infos[0]
	return &info, nil
}

func shouldRunGuestkitDoctor(source string) bool {
	if isOVAName(source) {
		return false
	}
	return !isHTTPSource(source)
}

func runGuestkitDoctor(ctx context.Context, rep *importPreflightReport, source string, windows bool, env importPreflightEnv) error {
	if st, err := os.Stat(source); err != nil || st.IsDir() {
		rep.add(pfFail, "guestkit", fmt.Sprintf("%s is not a local file (guestkit doctor needs a path on this machine)", source), "guestkit", "")
		return nil
	}
	bin, err := env.lookPath("guestkit")
	if err != nil {
		rep.add(pfFail, "guestkit", "guestkit not found on PATH; install it from https://github.com/zyvorai/zyvor-guestkit (or pass --from-json with a saved doctor report)", "guestkit", "")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := env.runDoctor(ctx, bin, source)
	if err != nil {
		rep.add(pfFail, "guestkit", "guestkit doctor failed: "+err.Error(), "guestkit", "")
		return nil
	}
	return applyGuestkitJSON(rep, out, windows)
}

func readImportPreflightJSON(path string, stdin io.Reader, env importPreflightEnv) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(stdin, 16<<20))
	}
	return env.readFile(path)
}

// applyGuestkitJSON consumes a guestkit doctor JSON document, a UI-style
// findings report, a BootInspectSummary, an inspect inventory, or a wrapper
// object with "doctor" and/or "inspect" keys. Only fields those documents
// actually expose are turned into checks; nothing is inferred past that.
func applyGuestkitJSON(rep *importPreflightReport, raw []byte, windows bool) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("--from-json is empty")
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("guestkit JSON is not an object: %w", err)
	}
	if d, ok := top["doctor"]; ok {
		b, _ := json.Marshal(d)
		if err := applyGuestkitJSON(rep, b, windows); err != nil {
			return err
		}
		if ins, ok := top["inspect"]; ok {
			ib, _ := json.Marshal(ins)
			applyInspectJSON(rep, ib, &windows)
		}
		return nil
	}
	if src, _ := top["source"].(string); src == "guestkit.inspect" || hasInspectShape(top) {
		applyInspectJSON(rep, raw, &windows)
		return nil
	}

	if boot, ok := top["bootability"].(map[string]any); ok {
		applyBootability(rep, top, boot, windows)
		return nil
	}
	if _, ok := top["findings"]; ok {
		applyFindings(rep, top, windows)
		return nil
	}
	if _, ok := top["fstab_valid"]; ok {
		applyBootInspectSummary(rep, top, windows)
		return nil
	}
	return errors.New("guestkit JSON has no bootability, findings, or inspect payload")
}

func applyBootability(rep *importPreflightReport, top, boot map[string]any, windows bool) {
	if t, _ := strField(top, "target"); t != "" {
		rep.Target = t
	}
	if t, _ := strField(boot, "target"); t != "" {
		rep.Target = t
	}
	if score, ok := numberField(boot, "score"); ok {
		rep.Score = &score
		rep.add(pfPass, "guestkit", fmt.Sprintf("guestkit boot score %s on %s", fmtScore(score), orDash(rep.Target)), "guestkit", "")
	} else if score, ok := numberField(top, "score"); ok {
		rep.Score = &score
		rep.add(pfPass, "guestkit", fmt.Sprintf("guestkit boot score %s", fmtScore(score)), "guestkit", "")
	}

	seen := map[string]bool{}
	addFinding := func(status, id, title, message, remediation string) {
		key := id + "\x00" + title + "\x00" + message
		if seen[key] {
			return
		}
		seen[key] = true
		check := classifyGuestkitIssue(id, title, message, windows)
		msg := strings.TrimSpace(title)
		if message != "" && !strings.EqualFold(message, title) {
			if msg != "" {
				msg += ": " + message
			} else {
				msg = message
			}
		}
		if remediation != "" {
			msg = strings.TrimSpace(msg + " (" + remediation + ")")
		}
		if msg == "" {
			msg = id
		}
		rep.add(status, check, msg, "guestkit", id)
	}

	for _, f := range objectList(boot, "blockers") {
		addFinding(pfFail, strDefault(f, "check_id", strDefault(f, "id", "")), strDefault(f, "title", ""), strDefault(f, "message", strDefault(f, "detail", "")), strDefault(f, "remediation", strDefault(f, "fix", "")))
	}
	for _, f := range objectList(boot, "warnings") {
		addFinding(pfWarn, strDefault(f, "check_id", strDefault(f, "id", "")), strDefault(f, "title", ""), strDefault(f, "message", strDefault(f, "detail", "")), strDefault(f, "remediation", strDefault(f, "fix", "")))
	}
	for _, c := range objectList(boot, "checks") {
		id := strDefault(c, "id", strDefault(c, "check_id", ""))
		name := strDefault(c, "name", strDefault(c, "title", ""))
		msg := strDefault(c, "message", "")
		passed, hasPassed := boolField(c, "passed")
		sev := strings.ToLower(strDefault(c, "severity", ""))
		if hasPassed && passed && !keepPassedCheck(id) {
			continue
		}
		status := pfWarn
		if hasPassed && passed {
			// GuestKit marks "Cloud-init not configured" as a passed info
			// check; call the absence out as a warning so it is visible
			// before conversion.
			if isCloudInitAbsence(id, name, msg) {
				status = pfWarn
			} else {
				status = pfPass
			}
		} else {
			status = severityStatus(sev, !hasPassed || !passed)
		}
		key := id + "\x00" + name + "\x00" + msg
		if seen[key] {
			continue
		}
		addFinding(status, id, name, msg, "")
	}
}

func applyFindings(rep *importPreflightReport, top map[string]any, windows bool) {
	if t, _ := strField(top, "target"); t != "" {
		rep.Target = t
	}
	if score, ok := numberField(top, "score"); ok {
		rep.Score = &score
		rep.add(pfPass, "guestkit", fmt.Sprintf("guestkit boot score %s on %s", fmtScore(score), orDash(rep.Target)), "guestkit", "")
	}
	for _, f := range objectList(top, "findings") {
		sev := strings.ToLower(strDefault(f, "severity", ""))
		title := strDefault(f, "title", "")
		detail := strDefault(f, "detail", strDefault(f, "message", ""))
		fix := strDefault(f, "fix", strDefault(f, "remediation", ""))
		id := strDefault(f, "check_id", strDefault(f, "id", ""))
		status := severityStatus(sev, false)
		if status == pfPass && (sev == "low" || sev == "info" || sev == "note" || sev == "") {
			// UI findings with low/info stay PASS; medium/high already mapped.
			if sev == "" {
				status = pfWarn
			}
		}
		check := classifyGuestkitIssue(id, title, detail, windows)
		msg := title
		if detail != "" && !strings.EqualFold(detail, title) {
			if msg != "" {
				msg += ": " + detail
			} else {
				msg = detail
			}
		}
		if fix != "" {
			msg = strings.TrimSpace(msg + " (" + fix + ")")
		}
		rep.add(status, check, msg, "guestkit", id)
	}
}

func applyBootInspectSummary(rep *importPreflightReport, top map[string]any, _ bool) {
	if osr, _ := strField(top, "os_release"); osr != "" {
		rep.add(pfPass, "os", "GuestKit reports "+osr, "guestkit", "")
	}
	if v, ok := boolField(top, "fstab_valid"); ok {
		if v {
			rep.add(pfPass, "fstab", "GuestKit reports fstab is valid", "guestkit", "")
		} else {
			rep.add(pfFail, "fstab", "GuestKit reports fstab is not valid", "guestkit", "")
		}
	}
	if v, ok := boolField(top, "cloud_init_present"); ok {
		if v {
			rep.add(pfPass, "cloud-init", "GuestKit reports cloud-init is present", "guestkit", "")
		} else {
			rep.add(pfWarn, "cloud-init", "GuestKit reports cloud-init is not present; first-boot configuration (SSH keys, hostname) may not apply unless you attach a seed or use spec.cloudInit", "guestkit", "")
		}
	}
	if msg, _ := strField(top, "message"); msg != "" {
		rep.add(pfPass, "guestkit", msg, "guestkit", "")
	}
}

func hasInspectShape(top map[string]any) bool {
	_, os := top["operating_system"]
	_, storage := top["storage"]
	_, pkgs := top["packages"]
	return os || storage || pkgs
}

func applyInspectJSON(rep *importPreflightReport, raw []byte, windows *bool) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return
	}
	if osMap, ok := top["operating_system"].(map[string]any); ok {
		distro, _ := strField(osMap, "distro")
		product, _ := strField(osMap, "product_name")
		if looksLikeWindowsOS(distro + " " + product) {
			*windows = true
		}
	}
	// Mount device names (not UUID= / by-uuid) are the fstab-style issue
	// FluxVM repair rewrites for virtio. Only emitted when inspect JSON
	// actually lists mount devices.
	for _, dev := range inspectMountDevices(top) {
		if isUnstableDiskName(dev) {
			rep.add(pfWarn, "fstab", fmt.Sprintf("inspect lists mount device %s; device names like /dev/sdX change to /dev/vdX on virtio (import repair rewrites fstab when repair is enabled)", dev), "inspect", "")
		}
	}
	if mention, ok := inspectMentionsGuestAgent(top); ok {
		if mention {
			rep.add(pfPass, "guest-agent", "inspect lists a qemu guest agent package or service", "inspect", "")
		}
		// A truncated packages.sample is not proof of absence; do not fail.
	}
}

func inspectMountDevices(top map[string]any) []string {
	var out []string
	collect := func(m map[string]any) {
		for _, key := range []string{"device", "mount_device", "source"} {
			if s, ok := strField(m, key); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	if storage, ok := top["storage"].(map[string]any); ok {
		for _, item := range objectList(storage, "mounts") {
			collect(item)
		}
		for _, item := range objectList(storage, "fstab") {
			collect(item)
		}
	}
	return out
}

func inspectMentionsGuestAgent(top map[string]any) (present bool, ok bool) {
	names := []string{}
	if pkgs, ok := top["packages"].(map[string]any); ok {
		for _, item := range objectList(pkgs, "sample") {
			if n, _ := strField(item, "name"); n != "" {
				names = append(names, n)
			}
		}
		if list, ok := pkgs["list"].([]any); ok {
			for _, it := range list {
				if m, ok := it.(map[string]any); ok {
					if n, _ := strField(m, "name"); n != "" {
						names = append(names, n)
					}
				} else if s, ok := it.(string); ok {
					names = append(names, s)
				}
			}
		}
	}
	if svc, ok := top["services"].(map[string]any); ok {
		if sample, ok := svc["sample"].([]any); ok {
			for _, it := range sample {
				if s, ok := it.(string); ok {
					names = append(names, s)
				}
			}
		}
	}
	for _, n := range names {
		ln := strings.ToLower(n)
		if strings.Contains(ln, "qemu-guest-agent") || strings.Contains(ln, "qemu_guest_agent") || ln == "qemu-ga.service" {
			return true, true
		}
	}
	return false, false
}

func keepPassedCheck(id string) bool {
	switch strings.ToUpper(id) {
	case "BOOT-001", "BOOT-005", "BOOT-006", "BOOT-008", "BOOT-012":
		return true
	}
	return false
}

func isCloudInitAbsence(id, name, msg string) bool {
	if strings.EqualFold(id, "BOOT-008") && strings.Contains(strings.ToLower(msg), "not configured") {
		return true
	}
	blob := strings.ToLower(name + " " + msg)
	return strings.Contains(blob, "cloud-init") && (strings.Contains(blob, "absent") || strings.Contains(blob, "not present") || strings.Contains(blob, "not configured"))
}

func severityStatus(sev string, failed bool) string {
	switch strings.ToLower(sev) {
	case "blocker", "critical", "high", "error", "fail", "failed":
		return pfFail
	case "warning", "warn", "medium", "review":
		return pfWarn
	case "info", "low", "note", "pass":
		if failed {
			return pfWarn
		}
		return pfPass
	}
	if failed {
		return pfFail
	}
	return pfWarn
}

// classifyGuestkitIssue maps a GuestKit check or finding onto a stable check
// name used in the report. Unknown checks keep the id, or "guestkit".
func classifyGuestkitIssue(id, title, message string, windows bool) string {
	idU := strings.ToUpper(strings.TrimSpace(id))
	blob := strings.ToLower(id + " " + title + " " + message)
	switch idU {
	case "BOOT-001":
		return "fstab"
	case "BOOT-005", "BOOT-012":
		return "firmware"
	case "BOOT-006":
		if windows || strings.Contains(blob, "windows") || strings.Contains(blob, "virtio-win") {
			return "windows-virtio"
		}
		return "virtio"
	case "BOOT-008":
		return "cloud-init"
	}
	switch {
	case strings.Contains(blob, "virtio-win") || (windows && strings.Contains(blob, "virtio")):
		return "windows-virtio"
	case strings.Contains(blob, "virtio") || strings.Contains(blob, "initramfs"):
		return "virtio"
	case strings.Contains(blob, "fstab") || strings.Contains(blob, "uuid"):
		return "fstab"
	case strings.Contains(blob, "cloud-init") || strings.Contains(blob, "nocloud") || strings.Contains(blob, "cloud_init"):
		return "cloud-init"
	case strings.Contains(blob, "guest agent") || strings.Contains(blob, "guest-agent") || strings.Contains(blob, "qemu-guest-agent"):
		return "guest-agent"
	case strings.Contains(blob, "firmware") || strings.Contains(blob, "uefi") || strings.Contains(blob, "secure boot") || (strings.Contains(blob, "efi") && !strings.Contains(blob, "verification")):
		return "firmware"
	case strings.Contains(blob, "unsupported") && strings.Contains(blob, "format"):
		return "format"
	}
	if id != "" {
		return strings.ToLower(id)
	}
	return "guestkit"
}

func normalizeFirmware(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "bios":
		return "bios", nil
	case "efi", "uefi":
		return "efi", nil
	}
	return "", fmt.Errorf("unknown --firmware %q (want bios or efi)", s)
}

func looksLikeWindowsOS(os string) bool {
	s := strings.ToLower(strings.TrimSpace(os))
	if s == "" {
		return false
	}
	// VMware osType values start with "windows" or "win"; avoid matching
	// "darwin", which contains the letters "win".
	return strings.Contains(s, "windows") || strings.HasPrefix(s, "win")
}

func sourceFormat(source string) (format string, known bool) {
	name := strings.ToLower(sourceName(source))
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	if strings.HasSuffix(name, ".qcow2") {
		ext = "qcow2"
	}
	switch ext {
	case "qcow2", "raw", "ova", "vmdk", "vhd", "vhdx":
		return ext, true
	case "qcow":
		return "qcow2", true
	case "ovf":
		return "ova", true
	case "img":
		return "", false
	case "vdi", "qed", "vpc", "iso":
		return ext, false
	case "":
		return "", false
	default:
		return ext, false
	}
}

func sourceName(source string) string {
	if isHTTPSource(source) {
		if u, err := url.Parse(source); err == nil && u.Path != "" {
			return u.Path
		}
	}
	return source
}

func isHTTPSource(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

func isOVAName(source string) bool {
	name := strings.ToLower(sourceName(source))
	return strings.HasSuffix(name, ".ova") || strings.HasSuffix(name, ".ovf")
}

func isUnstableDiskName(dev string) bool {
	d := strings.ToLower(strings.TrimSpace(dev))
	if d == "" || strings.HasPrefix(d, "uuid=") || strings.Contains(d, "/disk/by-uuid/") || strings.HasPrefix(d, "label=") || strings.HasPrefix(d, "partuuid=") {
		return false
	}
	for _, p := range []string{"/dev/sd", "/dev/hd", "/dev/xvd"} {
		if strings.HasPrefix(d, p) {
			return true
		}
	}
	return false
}

func quoteOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	default:
		return fmt.Sprint(t), true
	}
}

func strDefault(m map[string]any, key, fallback string) string {
	if s, ok := strField(m, key); ok && s != "" {
		return s
	}
	return fallback
}

func numberField(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func boolField(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func objectList(m map[string]any, key string) []map[string]any {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if obj, ok := it.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}
