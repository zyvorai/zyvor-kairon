// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// This file implements `kaironctl export kubevirt FILE...`: an offline
// converter from Kairon Machine manifests to KubeVirt kubevirt.io/v1
// VirtualMachine manifests, so leaving Kairon never means rewriting every
// VM by hand. It reads files (or stdin) and writes to stdout/stderr only; it
// never builds a Kubernetes client, so it cannot reach a cluster.

const kveUsage = "usage: kaironctl export kubevirt FILE... [--namespace NS] [-o yaml|json] [--strict]   (FILE - reads stdin)"

// kveHandled lists the normalized manifest paths ("spec.volumes[].name":
// array indexes become "[]") that the converter reads. Anything else that
// carries a value is reported, never dropped silently.
var kveHandled = map[string]bool{
	"apiVersion": true, "kind": true,
	"metadata.name": true, "metadata.namespace": true, "metadata.labels": true,
	// Server-populated bookkeeping and status: nothing to carry over.
	"metadata.uid": true, "metadata.resourceVersion": true, "metadata.generation": true,
	"metadata.creationTimestamp": true, "metadata.managedFields": true, "metadata.selfLink": true,
	"metadata.deletionTimestamp": true, "metadata.finalizers": true,
	"status":             true,
	"spec.powerState":    true,
	"spec.resources.cpu": true, "spec.resources.memory": true,
	"spec.security.secureBoot": true, "spec.security.tpm": true,
	"spec.image.source.oci": true,
	"spec.volumes[].name":   true, "spec.volumes[].claimName": true,
	"spec.disks[].name": true, "spec.disks[].claimName": true,
	"spec.cloudInit.hostname": true, "spec.cloudInit.user": true,
	"spec.cloudInit.sshAuthorizedKeys": true, "spec.cloudInit.packages": true,
	"spec.cloudInit.runCmd": true, "spec.cloudInit.writeFiles": true,
	"spec.cloudInit.writeFiles[].path": true, "spec.cloudInit.writeFiles[].content": true,
	"spec.cloudInit.writeFiles[].permissions": true,
}

// kveContainers are the paths the walker descends into: every proper prefix
// of a handled path, plus the list/map roots of handled leaves.
var kveContainers = func() map[string]bool {
	c := map[string]bool{}
	for p := range kveHandled {
		parts := strings.Split(p, ".")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], ".")
			c[prefix] = true
			c[strings.TrimSuffix(prefix, "[]")] = true
		}
	}
	return c
}()

// kveReasons explains why a path is not converted. The first entry whose
// substring occurs in the normalized path wins.
var kveReasons = []struct{ match, reason string }{
	{"spec.resources.maxCpu", "CPU hotplug bounds have no KubeVirt equivalent here; set domain.cpu.maxSockets by hand"},
	{"spec.resources.maxMemory", "memory hotplug bounds have no KubeVirt equivalent here; set domain.memory.maxGuest by hand"},
	{"spec.resources.elasticMemory", "elastic memory (balloon) has no KubeVirt equivalent here; configure the memory balloon by hand"},
	{"spec.resources.limits", "cgroup limits have no KubeVirt equivalent here"},
	{"spec.resources.hugepages", "set domain.memory.hugepages by hand with the page size you want"},
	{"spec.resources.cpuPinning", "set domain.cpu.dedicatedCpuPlacement by hand"},
	{"spec.resources.cpuSet", "CPU sets have no KubeVirt equivalent; use dedicatedCpuPlacement by hand"},
	{"spec.resources.numaNode", "NUMA node pinning has no direct KubeVirt equivalent; see domain.cpu.numa"},
	{"spec.resources", "this resource option has no KubeVirt equivalent"},
	{"spec.accelerators", "accelerator profiles expand to PCI devices; configure KubeVirt hostDevices or gpus by hand"},
	{"spec.devices", "PCI passthrough devices are not converted; configure KubeVirt hostDevices or gpus by hand"},
	{"spec.deviceClaims", "device claims are Kairon-specific; configure KubeVirt hostDevices by hand"},
	{"spec.apple", "Mac fabric (Apple silicon) has no KubeVirt equivalent"},
	{"spec.network", "the Kairon network fabric (forwards, security groups) has no KubeVirt mapping; define spec.template.spec.networks and interfaces by hand"},
	{"spec.serviceFabric", "the service fabric has no KubeVirt equivalent"},
	{"spec.placement", "scheduling constraints are not converted; set nodeSelector, affinity or tolerations by hand"},
	{"spec.nodeName", "node pinning is not converted; set nodeSelector by hand"},
	{"spec.topology", "NUMA topology placement is not converted"},
	{"spec.tenant", "the tenant fence is a Kairon feature with no KubeVirt equivalent"},
	{"spec.instanceType", "a MachineInstanceType is not resolved offline; KubeVirt would need its own instancetype"},
	{"spec.preference", "a MachinePreference is not resolved offline; KubeVirt would need its own preference"},
	{"spec.image.digest", "KubeVirt has no separate image digest; pin the containerDisk reference with @sha256: yourself"},
	{"spec.image.source", "only an OCI image source becomes a containerDisk; download and import other sources into a PVC yourself"},
	{"spec.image", "this boot image option has no KubeVirt equivalent; import the disk into a PVC yourself"},
	{"spec.cdroms", "CD-ROM media is not converted; add a cdrom disk backed by a PVC or DataVolume by hand"},
	{"spec.disks[].io", "disk bus, cache and throttle options are not converted"},
	{"spec.volumes[].guestPath", "mounting a volume at a guest path is not converted; mount the disk inside the guest"},
	{"spec.volumes[].atlas", "Atlas-provisioned volumes are not converted; create the PVC yourself and reference it"},
	{"spec.volumes[]", "this volume option has no KubeVirt equivalent"},
	{"spec.cloudInit", "this cloud-init option has no mapping"},
	{"spec.runtime", "the Kairon runtime backend and kernel have no KubeVirt equivalent"},
	{"spec.guestAgent", "guest agent settings are not converted; KubeVirt uses the qemu-guest-agent channel automatically"},
	{"spec.sandbox", "FluxVM sandboxes have no KubeVirt equivalent"},
	{"spec.lifecycle", "lifecycle bounds (TTL, idle suspend) have no KubeVirt equivalent"},
	{"spec.ttlSeconds", "TTL has no KubeVirt equivalent"},
	{"spec.priority", "pending-queue priority has no KubeVirt equivalent; see PriorityClass"},
	{"metadata.annotations", "Kairon annotations are not carried over; copy any you need by hand"},
}

const kveDefaultReason = "no equivalent in the KubeVirt VirtualMachine this command writes"

func kveReason(norm string) string {
	for _, r := range kveReasons {
		if strings.Contains(norm, r.match) {
			return r.reason
		}
	}
	return kveDefaultReason
}

// kveConverter holds the warnings of one Machine.
type kveConverter struct {
	machine string
	warn    []string
}

func (c *kveConverter) warnf(path, format string, args ...any) {
	c.warn = append(c.warn, fmt.Sprintf("warning: %s: %s not converted: %s", c.machine, path, fmt.Sprintf(format, args...)))
}

var kveIndexRE = regexp.MustCompile(`\[[0-9]+\]`)

// kveZero reports a value that carries no information (unset, false, empty).
func kveZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case float64:
		return t == 0
	case int64:
		return t == 0
	case int:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// walk warns for every populated subtree of v the converter does not read.
func (c *kveConverter) walk(v any, path string) {
	norm := kveIndexRE.ReplaceAllString(path, "[]")
	if path != "" {
		if kveHandled[norm] || kveZero(v) {
			return
		}
		if !kveContainers[norm] {
			c.warnf(path, "%s", kveReason(norm))
			return
		}
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := k
			if path != "" {
				child = path + "." + k
			}
			c.walk(t[k], child)
		}
	case []any:
		for i, e := range t {
			c.walk(e, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func kveMap(v any, keys ...string) map[string]any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	m, _ := v.(map[string]any)
	return m
}

func kveStr(v any, keys ...string) string {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}

func kveBool(v any, keys ...string) bool {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		v = m[k]
	}
	b, _ := v.(bool)
	return b
}

func kveList(v any, keys ...string) []any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	l, _ := v.([]any)
	return l
}

// kveStrings reads a list of strings, warning for non-string entries.
func (c *kveConverter) strings(path string, v []any) []string {
	var out []string
	for i, e := range v {
		s, ok := e.(string)
		if !ok {
			c.warnf(fmt.Sprintf("%s[%d]", path, i), "expected a string")
			continue
		}
		out = append(out, s)
	}
	return out
}

// convertMachineToKubevirt converts one Machine document into a KubeVirt
// VirtualMachine plus the warnings for everything it could not express.
func convertMachineToKubevirt(doc map[string]any, nsOverride string) (map[string]any, []string, error) {
	name := kveStr(doc, "metadata", "name")
	if name == "" {
		return nil, nil, errors.New("machine has no metadata.name")
	}
	c := &kveConverter{machine: name}

	meta := map[string]any{"name": name}
	ns := nsOverride
	if ns == "" {
		ns = kveStr(doc, "metadata", "namespace")
	}
	if ns != "" {
		meta["namespace"] = ns
	}
	if labels := kveMap(doc, "metadata", "labels"); len(labels) > 0 {
		meta["labels"] = labels
	}

	domain := map[string]any{}
	devices := map[string]any{}
	var disks, volumes []any
	used := map[string]bool{}

	vm := map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "metadata": meta}
	spec := map[string]any{}
	vm["spec"] = spec

	// Run strategy. An unset powerState is Running in Kairon.
	switch ps := kveStr(doc, "spec", "powerState"); ps {
	case "", "Running":
		spec["runStrategy"] = "Always"
	case "Stopped":
		spec["runStrategy"] = "Halted"
	default:
		c.warnf("spec.powerState", "power state %q has no KubeVirt runStrategy; the VirtualMachine uses Always", ps)
		spec["runStrategy"] = "Always"
	}

	// CPU and memory.
	if cpu := kveStr(doc, "spec", "resources", "cpu"); cpu != "" {
		if n, err := strconv.Atoi(cpu); err == nil && n > 0 {
			domain["cpu"] = map[string]any{"cores": n}
		} else {
			// A fractional quantity (500m) is a request, not a topology.
			domain["resources"] = map[string]any{"requests": map[string]any{"cpu": cpu}}
		}
	}
	if mem := kveStr(doc, "spec", "resources", "memory"); mem != "" {
		domain["memory"] = map[string]any{"guest": mem}
	}

	// Firmware and TPM.
	if kveBool(doc, "spec", "security", "secureBoot") {
		domain["firmware"] = map[string]any{"bootloader": map[string]any{"efi": map[string]any{"secureBoot": true}}}
		domain["features"] = map[string]any{"smm": map[string]any{"enabled": true}} // required for secureBoot
	}
	if kveBool(doc, "spec", "security", "tpm") {
		devices["tpm"] = map[string]any{}
	}

	addPVC := func(name, claim string, boot bool) {
		used[name] = true
		d := map[string]any{"name": name, "disk": map[string]any{}}
		if boot {
			d["bootOrder"] = 1
		}
		disks = append(disks, d)
		volumes = append(volumes, map[string]any{"name": name, "persistentVolumeClaim": map[string]any{"claimName": claim}})
	}

	// Boot volume and extra volumes (PVC references).
	vols := kveList(doc, "spec", "volumes")
	for i, e := range vols {
		vn, claim := kveStr(e, "name"), kveStr(e, "claimName")
		if vn == "" || claim == "" {
			c.warnf(fmt.Sprintf("spec.volumes[%d]", i), "needs both name and claimName to become a PVC volume")
			continue
		}
		addPVC(vn, claim, i == 0)
	}

	// Boot image: an OCI reference becomes a containerDisk.
	if oci := kveStr(doc, "spec", "image", "source", "oci"); oci != "" {
		if len(vols) > 0 {
			c.warnf("spec.image.source.oci", "the boot volume spec.volumes[0] takes precedence; the OCI image is not attached")
		} else {
			bn := "rootdisk"
			for used[bn] {
				bn += "-boot"
			}
			used[bn] = true
			disks = append([]any{map[string]any{"name": bn, "disk": map[string]any{}, "bootOrder": 1}}, disks...)
			volumes = append([]any{map[string]any{"name": bn, "containerDisk": map[string]any{"image": oci}}}, volumes...)
		}
	}
	if len(vols) == 0 && kveStr(doc, "spec", "image", "source", "oci") == "" {
		c.warnf("spec.image", "no boot disk: the Machine has neither spec.volumes nor an OCI spec.image.source, so the VirtualMachine has none")
	}

	// Extra disks (PVC-backed).
	for i, e := range kveList(doc, "spec", "disks") {
		dn, claim := kveStr(e, "name"), kveStr(e, "claimName")
		if dn == "" || claim == "" {
			c.warnf(fmt.Sprintf("spec.disks[%d]", i), "needs both name and claimName to become a PVC disk")
			continue
		}
		if used[dn] {
			c.warnf(fmt.Sprintf("spec.disks[%d].name", i), "name %q is already used by another volume; the disk is skipped", dn)
			continue
		}
		addPVC(dn, claim, false)
	}

	// Cloud-init.
	if ud := c.cloudConfig(doc); ud != "" {
		used["cloudinit"] = true
		disks = append(disks, map[string]any{"name": "cloudinit", "disk": map[string]any{}})
		volumes = append(volumes, map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"userData": ud}})
	}

	if len(disks) > 0 {
		devices["disks"] = disks
	}
	if len(devices) > 0 {
		domain["devices"] = devices
	}
	tmplSpec := map[string]any{"domain": domain}
	if len(volumes) > 0 {
		tmplSpec["volumes"] = volumes
	}
	spec["template"] = map[string]any{"spec": tmplSpec}

	c.walk(doc, "")
	return vm, c.warn, nil
}

// cloudConfig renders spec.cloudInit as #cloud-config user data, or "".
func (c *kveConverter) cloudConfig(doc map[string]any) string {
	ci := kveMap(doc, "spec", "cloudInit")
	cfg := map[string]any{}
	if h := kveStr(ci, "hostname"); h != "" {
		cfg["hostname"] = h
	}
	if u := kveStr(ci, "user"); u != "" {
		cfg["user"] = u
	}
	if keys := c.strings("spec.cloudInit.sshAuthorizedKeys", kveList(ci, "sshAuthorizedKeys")); len(keys) > 0 {
		cfg["ssh_authorized_keys"] = keys
	}
	if pk := c.strings("spec.cloudInit.packages", kveList(ci, "packages")); len(pk) > 0 {
		cfg["packages"] = pk
	}
	if rc := c.strings("spec.cloudInit.runCmd", kveList(ci, "runCmd")); len(rc) > 0 {
		cfg["runcmd"] = rc
	}
	wf := kveList(ci, "writeFiles")
	files := make([]any, 0, len(wf))
	for _, e := range wf {
		f := map[string]any{"path": kveStr(e, "path"), "content": kveStr(e, "content")}
		if p := kveStr(e, "permissions"); p != "" {
			f["permissions"] = p
		}
		files = append(files, f)
	}
	if len(files) > 0 {
		cfg["write_files"] = files
	}
	if len(cfg) == 0 {
		return ""
	}
	raw, err := sigsyaml.Marshal(cfg)
	if err != nil {
		c.warnf("spec.cloudInit", "cannot render user data: %v", err)
		return ""
	}
	return "#cloud-config\n" + string(raw)
}

// kveDocs decodes every YAML or JSON document in r.
func kveDocs(r io.Reader) ([]map[string]any, error) {
	dec := yaml.NewYAMLOrJSONDecoder(r, 4096)
	var docs []map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, err
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
}

// exportKubevirt converts the sources and writes the VirtualMachines to out
// and the warnings to errOut. With strict, any warning makes it return an
// error after the manifests are written.
func exportKubevirt(files []string, stdin io.Reader, out, errOut io.Writer, ns, format string, strict bool) error {
	if len(files) == 0 {
		return errors.New(kveUsage)
	}
	if format != "yaml" && format != "json" {
		return errors.New("--output must be yaml or json")
	}
	var vms []map[string]any
	warnings := 0
	emit := func(lines ...string) {
		for _, l := range lines {
			_, _ = fmt.Fprintln(errOut, l)
		}
		warnings += len(lines)
	}
	for _, f := range files {
		var r io.Reader
		if f == "-" {
			r = stdin
		} else {
			data, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			r = bytes.NewReader(data)
		}
		docs, err := kveDocs(r)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		var machines []map[string]any
		for _, d := range docs {
			if items, ok := d["items"].([]any); ok && kveStr(d, "kind") == "List" {
				for _, it := range items {
					if im, ok := it.(map[string]any); ok {
						machines = append(machines, im)
					}
				}
				continue
			}
			machines = append(machines, d)
		}
		for i, d := range machines {
			kind, api := kveStr(d, "kind"), kveStr(d, "apiVersion")
			if kind != "Machine" || !strings.HasPrefix(api, "kairon.zyvor.dev/") {
				label := kveStr(d, "metadata", "name")
				if label == "" {
					label = fmt.Sprintf("document %d", i+1)
				}
				emit(fmt.Sprintf("warning: %s: %s/%s %s skipped: only a kairon.zyvor.dev Machine is converted", f, api, kind, label))
				continue
			}
			vm, warns, err := convertMachineToKubevirt(d, ns)
			if err != nil {
				return fmt.Errorf("%s: document %d: %w", f, i+1, err)
			}
			emit(warns...)
			vms = append(vms, vm)
		}
	}
	if len(vms) == 0 {
		return errors.New("no Kairon Machine found in the input")
	}
	if format == "json" && len(vms) > 1 {
		items := make([]any, len(vms))
		for i, v := range vms {
			items[i] = v
		}
		if err := printStructured(out, map[string]any{"apiVersion": "v1", "kind": "List", "items": items}, "json"); err != nil {
			return err
		}
	} else {
		for i, v := range vms {
			if format == "yaml" && i > 0 {
				_, _ = fmt.Fprintln(out, "---")
			}
			if err := printStructured(out, v, format); err != nil {
				return err
			}
		}
	}
	if strict && warnings > 0 {
		return fmt.Errorf("%d warning(s) with --strict: the input has fields a KubeVirt VirtualMachine cannot express here", warnings)
	}
	return nil
}

// newExportKubevirtCmd is the offline `export kubevirt`. It parses its own
// arguments (like `import kubevirt`) and builds no cluster client.
func newExportKubevirtCmd(_ *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "kubevirt FILE...",
		Short: "Convert Machine manifests into KubeVirt VirtualMachines (offline)",
		Long: "Convert Kairon Machine manifests (YAML or JSON, multi-document, - for stdin) into kubevirt.io/v1\n" +
			"VirtualMachine manifests on stdout. Offline: reads the files you name and never contacts a cluster.\n" +
			"Fields KubeVirt cannot express are reported on stderr as 'warning: MACHINE: PATH not converted: REASON';\n" +
			"--strict exits non-zero if any warning was printed.\n\n" + kveUsage,
		Example:            "  $ kaironctl export kubevirt machine.yaml -o yaml > vm.yaml\n  $ kaironctl export kubevirt a.yaml b.yaml --namespace prod --strict",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if wantsHelp(args) {
				return cmd.Help()
			}
			return exportKubevirtArgs(cmd, args)
		},
	}
}

func exportKubevirtArgs(cmd *cobra.Command, args []string) error {
	ns, format, strict := "", "yaml", false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case a == "--strict":
			strict = true
		case a == "-n" || a == "--namespace":
			ns, err = next()
		case strings.HasPrefix(a, "--namespace="):
			ns = strings.TrimPrefix(a, "--namespace=")
		case a == "-o" || a == "--output":
			format, err = next()
		case strings.HasPrefix(a, "--output="):
			format = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-o="):
			format = strings.TrimPrefix(a, "-o=")
		case a == "-" || !strings.HasPrefix(a, "-"):
			files = append(files, a)
		default:
			err = fmt.Errorf("unknown flag %s\n%s", a, kveUsage)
		}
		if err != nil {
			return err
		}
	}
	return exportKubevirt(files, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), ns, format, strict)
}
