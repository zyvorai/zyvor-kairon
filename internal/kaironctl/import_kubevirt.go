// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"bytes"
	"encoding/base64"
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

	"github.com/zyvorai/kairon/internal/model"
)

// This file implements `kaironctl import kubevirt FILE...`: an offline
// converter from KubeVirt VirtualMachine manifests to Kairon Machine
// manifests. It reads files (or stdin) and writes to stdout/stderr only; it
// never builds a Kubernetes client, so it cannot reach a cluster.

const kubevirtImportUsage = "usage: kaironctl import kubevirt FILE... [--namespace NS] [-o yaml|json] [--strict]   (FILE - reads stdin)"

// kvPathSet matches normalized manifest paths ("spec.template.spec.volumes[]"):
// array indexes are replaced by "[]".
type kvPathSet map[string]bool

func newKVPathSet(paths ...string) kvPathSet {
	s := kvPathSet{}
	for _, p := range paths {
		s[p] = true
	}
	return s
}

const (
	kvDomain  = "spec.template.spec.domain"
	kvVolumes = "spec.template.spec.volumes[]"
)

// kvHandled lists the paths the converter reads. A handled path that is not
// also a container is consumed whole; everything else is reported.
var kvHandled = newKVPathSet(
	"apiVersion", "kind",
	"metadata.name", "metadata.namespace", "metadata.labels",
	// Server-populated bookkeeping: nothing to carry over.
	"metadata.uid", "metadata.resourceVersion", "metadata.generation", "metadata.creationTimestamp",
	"metadata.managedFields", "metadata.selfLink", "metadata.deletionTimestamp", "metadata.finalizers",
	"status",
	"spec.runStrategy", "spec.running",
	kvDomain+".cpu.cores", kvDomain+".cpu.sockets", kvDomain+".cpu.threads",
	kvDomain+".memory.guest",
	kvDomain+".resources.requests.cpu", kvDomain+".resources.requests.memory",
	kvDomain+".resources.limits.cpu", kvDomain+".resources.limits.memory",
	kvDomain+".firmware.bootloader.efi.secureBoot",
	kvDomain+".devices.disks[].name", kvDomain+".devices.disks[].disk", kvDomain+".devices.disks[].bootOrder",
	kvVolumes+".name",
	kvVolumes+".persistentVolumeClaim.claimName",
	kvVolumes+".dataVolume.name",
	kvVolumes+".containerDisk.image",
	kvVolumes+".cloudInitNoCloud.userData", kvVolumes+".cloudInitNoCloud.userDataBase64",
	kvVolumes+".cloudInitNoCloud.networkData", kvVolumes+".cloudInitNoCloud.networkDataBase64",
	kvVolumes+".cloudInitNoCloud.userDataSecretRef", kvVolumes+".cloudInitNoCloud.networkDataSecretRef",
)

// kvContainers are paths the walker descends into: every proper prefix of a
// handled path, plus paths that are both a switch and a container (tpm, efi),
// whose own children are not understood.
var kvContainers = func() kvPathSet {
	c := kvPathSet{kvDomain + ".devices.tpm": true, kvDomain + ".firmware.bootloader.efi": true}
	for p := range kvHandled {
		parts := strings.Split(p, ".")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], ".")
			c[prefix] = true
			c[strings.TrimSuffix(prefix, "[]")] = true // the list itself
		}
	}
	return c
}()

// kvReasons explains why a path is not converted. The first entry whose
// substring occurs in the normalized path wins.
var kvReasons = []struct{ match, reason string }{
	{"dataVolumeTemplates", "a Machine only references an existing PVC by name; create the PVC (for example from the DataVolume) before applying"},
	{".networks", "pod, Multus and other networks have no Machine equivalent; set spec.network by hand"},
	{"devices.interfaces", "network interfaces (bridge, masquerade, SR-IOV, ...) have no Machine equivalent; set spec.network by hand"},
	{"hostDevices", "host device passthrough has no automatic mapping; use spec.devices or spec.accelerators by hand"},
	{"devices.gpus", "GPU passthrough has no automatic mapping; use spec.devices or spec.accelerators by hand"},
	{".instancetype", "a KubeVirt instancetype is a different resource; Machine uses its own MachineInstanceType (spec.instanceTypeName)"},
	{".preference", "a KubeVirt preference is a different resource; Machine uses its own MachinePreference (spec.preferenceName)"},
	{"nodeSelector", "scheduling constraints are dropped; see spec.placement"},
	{".affinity", "scheduling constraints are dropped; see spec.placement"},
	{".tolerations", "tolerations are dropped; see spec.placement"},
	{"devices.disks[]", "this disk option has no Machine equivalent"},
	{"spec.template.spec.volumes[]", "this volume type or option has no Machine equivalent"},
	{"devices.tpm", "only the presence of a TPM is converted"},
	{"bootloader.efi", "only secureBoot is converted"},
}

const kvDefaultReason = "no equivalent field in the Machine spec"

func kvReason(norm string) string {
	for _, r := range kvReasons {
		if strings.Contains(norm, r.match) {
			return r.reason
		}
	}
	return kvDefaultReason
}

// kvConverter holds the warnings of one VirtualMachine.
type kvConverter struct {
	vm   string
	warn []string
}

func (c *kvConverter) warnf(path, format string, args ...any) {
	c.warn = append(c.warn, fmt.Sprintf("warning: %s: %s not converted: %s", c.vm, path, fmt.Sprintf(format, args...)))
}

var kvIndexRE = regexp.MustCompile(`\[[0-9]+\]`)

// walkUnmapped warns for every subtree of v that the converter does not read.
func (c *kvConverter) walkUnmapped(v any, path string) {
	norm := kvIndexRE.ReplaceAllString(path, "[]")
	if path != "" && !kvContainers[norm] {
		if !kvHandled[norm] {
			c.warnf(path, "%s", kvReason(norm))
		}
		return
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
			c.walkUnmapped(t[k], child)
		}
	case []any:
		for i, e := range t {
			c.walkUnmapped(e, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

// Small accessors over decoded YAML/JSON; a wrong type reads as absent.
func kvMap(v any, keys ...string) map[string]any {
	m, _ := kvGet(v, keys...).(map[string]any)
	return m
}

func kvGet(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func kvString(v any, keys ...string) string {
	switch t := kvGet(v, keys...).(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func kvInt(v any, keys ...string) (int64, bool) {
	f, ok := kvGet(v, keys...).(float64)
	return int64(f), ok
}

// convertKubevirtVM converts one VirtualMachine document.
func convertKubevirtVM(doc map[string]any, nsOverride string) (model.Machine, []string, error) {
	name := kvString(doc, "metadata", "name")
	if name == "" {
		return model.Machine{}, nil, errors.New("VirtualMachine has no metadata.name")
	}
	c := &kvConverter{vm: name}
	ns := nsOverride
	if ns == "" {
		ns = kvString(doc, "metadata", "namespace")
	}
	if ns == "" {
		ns = "default"
	}
	m := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"kairon.zyvor.dev/imported-from": "kubevirt"}},
	}
	for k, v := range kvMap(doc, "metadata", "labels") {
		if s, ok := v.(string); ok {
			m.Metadata.Labels[k] = s
		}
	}

	c.runStrategy(doc, &m)
	c.resources(doc, &m)
	c.firmware(doc, &m)
	c.storage(doc, &m)
	c.walkUnmapped(doc, "")
	return m, c.warn, nil
}

func (c *kvConverter) runStrategy(doc map[string]any, m *model.Machine) {
	rs := kvString(doc, "spec", "runStrategy")
	if rs == "" {
		if r, ok := kvGet(doc, "spec", "running").(bool); ok {
			rs = map[bool]string{true: "Always", false: "Halted"}[r]
		}
	}
	switch rs {
	case "Always":
		m.Spec.PowerState = "Running"
	case "Halted":
		m.Spec.PowerState = "Stopped"
	case "":
	default:
		c.warnf("spec.runStrategy", "runStrategy %q has no Machine equivalent; the Machine uses the default power state", rs)
	}
}

func (c *kvConverter) resources(doc map[string]any, m *model.Machine) {
	domain := kvMap(doc, "spec", "template", "spec", "domain")
	cpu := kvMap(domain, "cpu")
	if cpu != nil {
		total := int64(1)
		for _, k := range []string{"cores", "sockets", "threads"} {
			if n, ok := kvInt(cpu, k); ok && n > 0 {
				total *= n
			}
		}
		m.Spec.Resources.CPU = strconv.FormatInt(total, 10)
	}
	for _, kind := range []string{"requests", "limits"} {
		q := kvString(domain, "resources", kind, "cpu")
		switch {
		case q == "":
		case m.Spec.Resources.CPU == "":
			m.Spec.Resources.CPU = q
		case q != m.Spec.Resources.CPU:
			c.warnf(kvDomain+".resources."+kind+".cpu", "differs from the converted cpu %s; Machine has a single CPU count", m.Spec.Resources.CPU)
		}
	}
	m.Spec.Resources.Memory = kvString(domain, "memory", "guest")
	for _, kind := range []string{"requests", "limits"} {
		q := kvString(domain, "resources", kind, "memory")
		switch {
		case q == "":
		case m.Spec.Resources.Memory == "":
			m.Spec.Resources.Memory = q
		case q != m.Spec.Resources.Memory:
			c.warnf(kvDomain+".resources."+kind+".memory", "differs from the converted memory %s; Machine has a single memory size", m.Spec.Resources.Memory)
		}
	}
	if m.Spec.Resources.CPU == "" {
		c.warnf(kvDomain+".cpu", "no CPU count in the VM; spec.resources.cpu is left empty and must be set")
	}
	if m.Spec.Resources.Memory == "" {
		c.warnf(kvDomain+".memory", "no memory size in the VM; spec.resources.memory is left empty and must be set")
	}
}

func (c *kvConverter) firmware(doc map[string]any, m *model.Machine) {
	devices := kvMap(doc, "spec", "template", "spec", "domain", "devices")
	if _, ok := devices["tpm"]; ok {
		m.Spec.Security.TPM = true
	}
	efi, ok := kvGet(doc, "spec", "template", "spec", "domain", "firmware", "bootloader", "efi").(map[string]any)
	if !ok {
		return
	}
	if sb, ok := efi["secureBoot"].(bool); ok {
		m.Spec.Security.SecureBoot = sb
	} else {
		c.warnf(kvDomain+".firmware.bootloader.efi", "secureBoot is not set in the VM, so spec.security.secureBoot is left false; KubeVirt's default may enable it")
	}
}

func (c *kvConverter) storage(doc map[string]any, m *model.Machine) {
	specPath := "spec.template.spec"
	tspec := kvMap(doc, "spec", "template", "spec")
	disks, _ := kvGet(tspec, "domain", "devices", "disks").([]any)
	vols, _ := kvGet(tspec, "volumes").([]any)

	// The boot volume is the lowest bootOrder, else the first disk, else the
	// first volume that is not cloud-init.
	diskOf := map[string]map[string]any{}
	bootName, bestOrder := "", int64(0)
	for i, d := range disks {
		dm, _ := d.(map[string]any)
		n := kvString(dm, "name")
		diskOf[n] = dm
		if dm["cdrom"] != nil || dm["lun"] != nil {
			c.warnf(fmt.Sprintf("%s.domain.devices.disks[%d]", specPath, i), "cdrom and lun disks are skipped; use spec.cdroms for install media")
			continue
		}
		if o, ok := kvInt(dm, "bootOrder"); ok && (bootName == "" || o < bestOrder) {
			bootName, bestOrder = n, o
		}
	}
	isCloudInit := func(v any) bool {
		return kvMap(v, "cloudInitNoCloud") != nil || kvMap(v, "cloudInitConfigDrive") != nil
	}
	if bootName == "" {
		for _, d := range disks {
			dm, _ := d.(map[string]any)
			if dm["cdrom"] == nil && dm["lun"] == nil {
				bootName = kvString(dm, "name")
				break
			}
		}
	}
	if bootName == "" {
		for _, v := range vols {
			if !isCloudInit(v) {
				bootName = kvString(v, "name")
				break
			}
		}
	}

	booted := false
	for i, v := range vols {
		path := fmt.Sprintf("%s.volumes[%d]", specPath, i)
		vname := kvString(v, "name")
		if d, ok := diskOf[vname]; ok && (d["cdrom"] != nil || d["lun"] != nil) {
			continue // already reported with the disk
		}
		if nc := kvMap(v, "cloudInitNoCloud"); nc != nil {
			c.cloudInit(path+".cloudInitNoCloud", nc, m)
			continue
		}
		claim := kvString(v, "persistentVolumeClaim", "claimName")
		if claim == "" {
			claim = kvString(v, "dataVolume", "name")
		}
		oci := kvString(v, "containerDisk", "image")
		isBoot := vname == bootName
		switch {
		case claim != "" && isBoot:
			m.Spec.Volumes = append(m.Spec.Volumes, model.MachineVolume{Name: vname, ClaimName: claim})
			booted = true
		case claim != "":
			m.Spec.Disks = append(m.Spec.Disks, model.MachineDisk{Name: vname, ClaimName: claim})
		case oci != "" && isBoot:
			m.Spec.Image.Source = &model.ImageSource{OCI: oci}
			if _, digest, ok := strings.Cut(oci, "@"); ok {
				m.Spec.Image.Digest = digest
			} else {
				c.warnf(path+".containerDisk.image", "the image has no @sha256 digest; set spec.image.digest by hand, which kairon-node needs to pull it")
			}
			booted = true
		case oci != "":
			c.warnf(path+".containerDisk", "only the boot disk can be a containerDisk; Machine cannot attach a second image as a data disk")
		}
		// Other volume types (emptyDisk, configMap, secret, hostDisk, ...)
		// are reported by walkUnmapped.
	}
	if !booted {
		c.warnf(specPath+".volumes", "no boot disk could be converted (need a PVC, DataVolume or containerDisk); spec.image or spec.volumes[0] must be set by hand")
	}
}

// cloudInit converts a cloudInitNoCloud volume.
func (c *kvConverter) cloudInit(path string, nc map[string]any, m *model.Machine) {
	userData := kvString(nc, "userData")
	if b64 := kvString(nc, "userDataBase64"); b64 != "" && userData == "" {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			c.warnf(path+".userDataBase64", "not valid base64: %v", err)
		} else {
			userData = string(raw)
		}
	}
	if nc["userDataSecretRef"] != nil {
		c.warnf(path+".userDataSecretRef", "the Secret is not read (this command is offline); copy its cloud-config into spec.cloudInit by hand")
	}
	if nc["networkData"] != nil || nc["networkDataBase64"] != nil || nc["networkDataSecretRef"] != nil {
		c.warnf(path+".networkData", "Machine cloudInit has no network-config; configure the guest network by hand")
	}
	if strings.TrimSpace(userData) == "" {
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(userData), "#cloud-config") {
		c.warnf(path+".userData", "not a #cloud-config document (script or other format); Machine cloudInit cannot carry it")
		return
	}
	var cfg map[string]any
	if err := sigsyaml.Unmarshal([]byte(userData), &cfg); err != nil {
		c.warnf(path+".userData", "cloud-config is not valid YAML: %v", err)
		return
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ci := &m.Spec.CloudInit
	for _, k := range keys {
		v := cfg[k]
		p := path + ".userData:" + k
		switch k {
		case "hostname":
			ci.Hostname, _ = v.(string)
		case "ssh_authorized_keys":
			ci.SSHAuthorizedKeys = kvStrings(c, p, v)
		case "packages":
			ci.Packages = kvStrings(c, p, v)
		case "runcmd":
			ci.RunCmd = kvStrings(c, p, v)
		case "write_files":
			c.writeFiles(p, v, ci)
		default:
			c.warnf(p, "cloud-config key %q has no Machine cloudInit equivalent", k)
		}
	}
}

// kvStrings returns the string entries of a list; other entries (such as
// runcmd's list-form commands) are reported and skipped.
func kvStrings(c *kvConverter, path string, v any) []string {
	list, _ := v.([]any)
	var out []string
	for i, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		} else {
			c.warnf(fmt.Sprintf("%s[%d]", path, i), "only plain string entries are converted")
		}
	}
	return out
}

func (c *kvConverter) writeFiles(path string, v any, ci *model.CloudInitSpec) {
	list, _ := v.([]any)
	for i, e := range list {
		p := fmt.Sprintf("%s[%d]", path, i)
		f, _ := e.(map[string]any)
		file := model.CloudInitFile{Path: kvString(f, "path"), Content: kvString(f, "content"), Permissions: kvString(f, "permissions")}
		if file.Path == "" {
			c.warnf(p, "write_files entry has no path")
			continue
		}
		skip := false
		for k := range f {
			if k != "path" && k != "content" && k != "permissions" {
				c.warnf(p+"."+k, "write_files option has no Machine equivalent; the file is skipped so it is not written wrongly")
				skip = true
			}
		}
		if !skip {
			ci.WriteFiles = append(ci.WriteFiles, file)
		}
	}
}

// kubevirtDocs reads every YAML/JSON document from r.
func kubevirtDocs(r io.Reader) ([]map[string]any, error) {
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

// importKubevirt converts the sources and writes the Machines to out and the
// warnings to errOut. With strict, any warning makes it return an error after
// the manifests are written.
func importKubevirt(files []string, stdin io.Reader, out, errOut io.Writer, ns, format string, strict bool) error {
	if len(files) == 0 {
		return errors.New(kubevirtImportUsage)
	}
	if format != "yaml" && format != "json" {
		return errors.New("--output must be yaml or json")
	}
	var machines []model.Machine
	warnings := 0
	emit := func(lines []string) {
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
		docs, err := kubevirtDocs(r)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		var vms []map[string]any
		for _, d := range docs {
			if items, ok := d["items"].([]any); ok && kvString(d, "kind") == "List" {
				for _, it := range items {
					if im, ok := it.(map[string]any); ok {
						vms = append(vms, im)
					}
				}
				continue
			}
			vms = append(vms, d)
		}
		for i, d := range vms {
			kind, api := kvString(d, "kind"), kvString(d, "apiVersion")
			if kind != "VirtualMachine" || !strings.HasPrefix(api, "kubevirt.io/") {
				label := kvString(d, "metadata", "name")
				if label == "" {
					label = fmt.Sprintf("document %d", i+1)
				}
				emit([]string{fmt.Sprintf("warning: %s: %s %s skipped: only kubevirt.io VirtualMachine is converted", f, api+"/"+kind, label)})
				continue
			}
			m, warns, err := convertKubevirtVM(d, ns)
			if err != nil {
				return fmt.Errorf("%s: document %d: %w", f, i+1, err)
			}
			emit(warns)
			machines = append(machines, m)
		}
	}
	if len(machines) == 0 {
		return errors.New("no KubeVirt VirtualMachine found in the input")
	}
	if format == "json" && len(machines) > 1 {
		items := make([]any, len(machines))
		for i, m := range machines {
			items[i] = m
		}
		if err := printManifest(out, map[string]any{"apiVersion": "v1", "kind": "List", "items": items}, "json"); err != nil {
			return err
		}
	} else {
		for i, m := range machines {
			if format == "yaml" && i > 0 {
				_, _ = fmt.Fprintln(out, "---")
			}
			if err := printManifest(out, m, format); err != nil {
				return err
			}
		}
	}
	if strict && warnings > 0 {
		return fmt.Errorf("%d warning(s) with --strict: the input has fields a Machine cannot express", warnings)
	}
	return nil
}

// kubevirtConvertArgs holds what `import kubevirt` and `export kubevirt` both parse from their command line.
type kubevirtConvertArgs struct {
	files  []string
	ns     string
	format string
	strict bool
}

// parseKubevirtConvertArgs parses the shared flags (-n/--namespace, -o/--output, --strict) and file operands. usage is
// appended to the "unknown flag" error.
func parseKubevirtConvertArgs(args []string, usage string) (kubevirtConvertArgs, error) {
	p := kubevirtConvertArgs{format: "yaml"}
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
			p.strict = true
		case a == "-n" || a == "--namespace":
			p.ns, err = next()
		case strings.HasPrefix(a, "--namespace="):
			p.ns = strings.TrimPrefix(a, "--namespace=")
		case a == "-o" || a == "--output":
			p.format, err = next()
		case strings.HasPrefix(a, "--output="):
			p.format = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-o="):
			p.format = strings.TrimPrefix(a, "-o=")
		case a == "-" || !strings.HasPrefix(a, "-"):
			p.files = append(p.files, a)
		default:
			err = fmt.Errorf("unknown flag %s\n%s", a, usage)
		}
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

// importKubevirtArgs parses the arguments after `import kubevirt`.
func importKubevirtArgs(cmd *cobra.Command, args []string) error {
	p, err := parseKubevirtConvertArgs(args, kubevirtImportUsage)
	if err != nil {
		return err
	}
	return importKubevirt(p.files, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), p.ns, p.format, p.strict)
}
