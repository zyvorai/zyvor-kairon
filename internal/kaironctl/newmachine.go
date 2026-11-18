// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/model"
)

// `kaironctl new machine NAME --preset P` prints a commented starter Machine
// manifest. It is complementary to `create`: it never builds a cluster client
// and never talks to a cluster, it only writes YAML (or JSON) to stdout so you
// can edit it, keep it in git and `kubectl apply` it. The manifest is built
// from the real model.Machine type, so it can only contain fields that exist,
// and it is checked with the same pure model validators the admission webhook
// runs on a Machine create before it is printed.

const (
	newMachineDefaultPreset = "medium"
	maxKeyFileBytes         = 16 << 10
)

// machinePreset is a built-in starting point. Presets are not tuned for any
// workload; they only pick plausible sizes and the fields that kind of guest
// usually needs.
type machinePreset struct {
	Name        string
	Description string
	CPU, Memory string
	DiskSize    string // "" leaves the image's own size
	apply       func(s *model.MachineSpec)
}

const (
	placeholderLinuxImage   = "/var/lib/fluxvm/images/ubuntu-24.04.qcow2"
	placeholderWindowsImage = "/var/lib/fluxvm/images/windows-server-2022-cloudbase-init.qcow2"
	placeholderSandboxImage = "/var/lib/fluxvm/images/sandbox.qcow2"
)

func linuxBase(s *model.MachineSpec) { s.Runtime.Backend = "qemu" }

var machinePresets = []machinePreset{
	{Name: "small", Description: "general-purpose Linux guest, 1 vCPU", CPU: "1", Memory: "2Gi", DiskSize: "20Gi", apply: linuxBase},
	{Name: "medium", Description: "general-purpose Linux guest, 2 vCPU", CPU: "2", Memory: "4Gi", DiskSize: "40Gi", apply: linuxBase},
	{Name: "large", Description: "general-purpose Linux guest, 8 vCPU", CPU: "8", Memory: "16Gi", DiskSize: "100Gi", apply: linuxBase},
	{Name: "gpu", Description: "Linux guest asking for an accelerator through a MachineAcceleratorProfile", CPU: "8", Memory: "32Gi", DiskSize: "100Gi", apply: func(s *model.MachineSpec) {
		linuxBase(s)
		s.Accelerators = []model.MachineAccelerator{{Name: "gpu0", Profile: "gpu-profile"}}
	}},
	{Name: "windows", Description: "Windows Server/10 guest with a pre-built image (add --secure-boot --tpm for Windows 11)", CPU: "4", Memory: "8Gi", DiskSize: "80Gi", apply: func(s *model.MachineSpec) {
		linuxBase(s)
		s.GuestAgent.Enabled = true
	}},
	{Name: "sandbox", Description: "short-lived FluxVM sandbox that expires after 15 minutes", CPU: "1", Memory: "512Mi", apply: func(s *model.MachineSpec) {
		s.TTLSeconds = 900
		s.GuestAgent.Console = true
		s.Sandbox = &model.SandboxSpec{}
	}},
	{Name: "database", Description: "Linux guest for a database: more memory, a large disk and the guest agent", CPU: "4", Memory: "16Gi", DiskSize: "200Gi", apply: func(s *model.MachineSpec) {
		linuxBase(s)
		s.GuestAgent.Enabled = true
	}},
}

func findMachinePreset(name string) (machinePreset, bool) {
	for _, p := range machinePresets {
		if p.Name == name {
			return p, true
		}
	}
	return machinePreset{}, false
}

func machinePresetNames() []string {
	names := make([]string, len(machinePresets))
	for i, p := range machinePresets {
		names[i] = p.Name
	}
	return names
}

// newMachineOptions are the inputs of one scaffold.
type newMachineOptions struct {
	Name, Namespace, Preset      string
	CPU, Memory, DiskSize, Image string
	SSHKeyFile, CloudInitFile    string
	SecureBoot, TPM              bool
	InstanceType, Preference     string
	ClusterInstanceType          bool
	ClusterPreference            bool
	// Warnf receives non-fatal notes (cloud-config keys a Machine cannot
	// carry). Nil discards them.
	Warnf func(string)
}

// imageSpecFromRef turns --image REF into a spec.image. REF is an absolute
// host path, "catalog:NAME" (FluxVM image catalog) or "image:NAME" (a
// cluster MachineImage).
func imageSpecFromRef(ref string) (model.ImageSpec, error) {
	switch {
	case strings.HasPrefix(ref, "/"):
		return model.ImageSpec{Path: ref}, nil
	case strings.HasPrefix(ref, "catalog:") && len(ref) > len("catalog:"):
		return model.ImageSpec{CatalogName: strings.TrimPrefix(ref, "catalog:")}, nil
	case strings.HasPrefix(ref, "image:") && len(ref) > len("image:"):
		n := strings.TrimPrefix(ref, "image:")
		if validDNSLabel(n) != "" && !regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`).MatchString(n) {
			return model.ImageSpec{}, fmt.Errorf("--image %q: MachineImage name must be a DNS name", ref)
		}
		return model.ImageSpec{ImageRef: n}, nil
	}
	return model.ImageSpec{}, fmt.Errorf("--image %q: want an absolute node path (/var/lib/fluxvm/images/x.qcow2), catalog:NAME or image:NAME (a MachineImage)", ref)
}

var scaffoldKeyTypes = []string{"ssh-ed25519", "ssh-rsa", "ssh-dss", "ecdsa-sha2-", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-"}

// readPublicKeyFile reads an authorized_keys-style PUBLIC key file. A file
// that looks like a private key is refused without echoing its content.
func readPublicKeyFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--ssh-key-file: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("--ssh-key-file: %w", err)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, fmt.Errorf("--ssh-key-file %s: larger than %d bytes, not a public key file", path, maxKeyFileBytes)
	}
	if bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("-----BEGIN")) {
		return nil, fmt.Errorf("--ssh-key-file %s looks like a private key; pass the .pub file", path)
	}
	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ok := false
		for _, t := range scaffoldKeyTypes {
			if strings.HasPrefix(line, t) {
				ok = true
				break
			}
		}
		if !ok || len(strings.Fields(line)) < 2 {
			return nil, fmt.Errorf("--ssh-key-file %s: not an SSH public key line (want \"ssh-ed25519 AAAA... comment\")", path)
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("--ssh-key-file %s: no public key found", path)
	}
	return keys, nil
}

// buildNewMachine assembles and validates the Machine for the options.
func buildNewMachine(o newMachineOptions) (model.Machine, error) {
	if validDNSLabel(o.Name) != "" {
		return model.Machine{}, fmt.Errorf("invalid Machine name %q: use lowercase letters, digits and '-', start and end with a letter or digit, at most 63 characters", o.Name)
	}
	ns := o.Namespace
	if ns == "" {
		ns = "default"
	}
	if validDNSLabel(ns) != "" {
		return model.Machine{}, fmt.Errorf("invalid namespace %q", ns)
	}
	if o.Preset == "" {
		o.Preset = newMachineDefaultPreset
	}
	p, ok := findMachinePreset(o.Preset)
	if !ok {
		return model.Machine{}, fmt.Errorf("unknown preset %q (want one of %s)", o.Preset, strings.Join(machinePresetNames(), ", "))
	}

	s := model.MachineSpec{PowerState: "Running"}
	p.apply(&s)
	s.Resources = model.ResourceSpec{CPU: p.CPU, Memory: p.Memory}
	s.Image = model.ImageSpec{Path: placeholderLinuxImage, DiskSize: p.DiskSize}
	switch p.Name {
	case "windows":
		s.Image.Path = placeholderWindowsImage
	case "sandbox":
		s.Image.Path = placeholderSandboxImage
	}
	if p.Name != "windows" {
		s.CloudInit.Hostname = o.Name
	}

	if o.CPU != "" {
		s.Resources.CPU = o.CPU
	}
	if o.Memory != "" {
		s.Resources.Memory = o.Memory
	}
	if o.DiskSize != "" {
		s.Image.DiskSize = o.DiskSize
	}
	if o.Image != "" {
		img, err := imageSpecFromRef(o.Image)
		if err != nil {
			return model.Machine{}, err
		}
		img.DiskSize = s.Image.DiskSize
		s.Image = img
	}
	if o.SecureBoot || o.TPM {
		if p.Name == "sandbox" {
			return model.Machine{}, errors.New("--secure-boot and --tpm do not apply to the sandbox preset (FluxVM sandboxes use their own hypervisor)")
		}
		s.Security = model.SecuritySpec{SecureBoot: o.SecureBoot, TPM: o.TPM}
	}
	if o.InstanceType != "" {
		s.InstanceTypeName = o.InstanceType
		if o.ClusterInstanceType {
			s.InstanceTypeKind = model.KindMachineClusterInstanceType
		}
		// The instance type supplies cpu and memory unless given here.
		if o.CPU == "" {
			s.Resources.CPU = ""
		}
		if o.Memory == "" {
			s.Resources.Memory = ""
		}
	} else if o.ClusterInstanceType {
		return model.Machine{}, errors.New("--cluster-instance-type needs --instance-type")
	}
	if o.Preference != "" {
		s.PreferenceName = o.Preference
		if o.ClusterPreference {
			s.PreferenceKind = model.KindMachineClusterPreference
		}
	} else if o.ClusterPreference {
		return model.Machine{}, errors.New("--cluster-preference needs --preference")
	}

	if o.SSHKeyFile != "" {
		keys, err := readPublicKeyFile(o.SSHKeyFile)
		if err != nil {
			return model.Machine{}, err
		}
		s.CloudInit.SSHAuthorizedKeys = keys
	}
	m := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: o.Name, Namespace: ns},
		Spec:     s,
	}
	if o.CloudInitFile != "" {
		raw, err := os.ReadFile(o.CloudInitFile)
		if err != nil {
			return model.Machine{}, fmt.Errorf("--cloud-init-file: %w", err)
		}
		if len(raw) > 1<<20 {
			return model.Machine{}, fmt.Errorf("--cloud-init-file %s: larger than 1 MiB", o.CloudInitFile)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(raw)), "#cloud-config") {
			return model.Machine{}, fmt.Errorf("--cloud-init-file %s: not a #cloud-config document (Machine cloudInit cannot carry scripts)", o.CloudInitFile)
		}
		// Same conversion `import kubevirt` uses: cloud-config keys a
		// Machine can carry become spec.cloudInit fields, the rest warn.
		before := m.Spec.CloudInit
		c := &kvConverter{vm: o.CloudInitFile}
		c.cloudInit("cloud-init-file", map[string]any{"userData": string(raw)}, &m)
		// Keep the key file's keys when the cloud-config adds its own.
		m.Spec.CloudInit.SSHAuthorizedKeys = appendUnique(before.SSHAuthorizedKeys, m.Spec.CloudInit.SSHAuthorizedKeys)
		if o.Warnf != nil {
			for _, w := range c.warn {
				o.Warnf(w)
			}
		}
	}
	if err := validateNewMachine(m); err != nil {
		return model.Machine{}, err
	}
	return m, nil
}

func appendUnique(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := map[string]bool{}
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

// validateNewMachine applies the pure checks the admission webhook runs on a
// Machine create (internal/controller/webhook.go validateMachineCreate), minus
// the ones that need a cluster (instance-type lookups, quotas, attestation).
func validateNewMachine(m model.Machine) error {
	s := m.Spec
	if err := validateScaffoldQuantities(s.Resources); err != nil {
		return err
	}
	if s.Image.DiskSize != "" {
		if gib, err := model.ParseDiskSizeGiB(s.Image.DiskSize); err != nil || gib == 0 {
			return fmt.Errorf("spec.image.diskSize %q: want a positive size such as 40Gi", s.Image.DiskSize)
		}
	}
	checks := []func() error{
		func() error { return model.ValidateImageSource(s.Image) },
		func() error { return model.ValidateDisks(s.Disks) },
		func() error { return model.ValidateRootImage(s.Image) },
		func() error { return model.ValidateCdroms(s.Cdroms) },
		func() error { return model.ValidateTopology(s) },
		func() error { return model.ValidatePlacement(s) },
		func() error { return model.ValidateSandboxOCI(s) },
		func() error { return model.ValidateAccelerators(s, m.Metadata.Annotations) },
		func() error { return model.ValidateElasticMemory(s) },
		func() error { return model.ValidateExtraInterfaces(s.Network) },
		func() error { return model.ValidateDataplaneMode(s.Network) },
		func() error { return model.ValidateEgress(s.Network) },
		func() error { return model.ValidateCiliumAttach(s.Network) },
		func() error { return model.ValidateMachineVirtualNetwork(s.Network) },
		func() error { return model.ValidateLifecycle(s.Lifecycle) },
		func() error { return model.ValidateInstanceTypeRef(s) },
	}
	for _, c := range checks {
		if err := c(); err != nil {
			return err
		}
	}
	// The CRD documents these as refused at creation; its schema cannot
	// express the backend coupling, so check it here.
	if s.Security.SecureBoot && s.Runtime.Backend != "qemu" {
		return errors.New("spec.security.secureBoot needs the qemu backend")
	}
	if s.Security.TPM && s.Runtime.Backend != "qemu" && s.Runtime.Backend != "cloud-hypervisor" {
		return errors.New("spec.security.tpm needs the qemu or cloud-hypervisor backend")
	}
	return nil
}

// validateScaffoldQuantities mirrors the controller's validateResourceQuantities.
// With an instance type, resources may be empty (the controller fills them).
func validateScaffoldQuantities(r model.ResourceSpec) error {
	if r.CPU != "" {
		if _, err := model.ParseVCPUs(r.CPU); err != nil {
			return fmt.Errorf("spec.resources.cpu: %v", err)
		}
	}
	if r.Memory != "" {
		if _, err := model.ParseMemoryMiB(r.Memory); err != nil {
			return fmt.Errorf("spec.resources.memory: %v", err)
		}
	}
	return nil
}

// ---- ordered tree, comments and rendering ----

type onode struct {
	keys   []string
	vals   []*onode // object values, parallel to keys
	items  []*onode // array elements
	scalar any      // string, float64 or bool
	kind   byte     // 'o' object, 'a' array, 's' scalar
}

func decodeOrdered(dec *json.Decoder) (*onode, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := t.(type) {
	case json.Delim:
		if v == '{' {
			n := &onode{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				child, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, kt.(string))
				n.vals = append(n.vals, child)
			}
			_, err := dec.Token()
			return n, err
		}
		n := &onode{kind: 'a'}
		for dec.More() {
			child, err := decodeOrdered(dec)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, child)
		}
		_, err := dec.Token()
		return n, err
	default:
		return &onode{kind: 's', scalar: v}, nil
	}
}

// prune drops empty values, except an object at a keepEmpty path
// (spec.sandbox: {} is how a Machine opts into the sandbox track).
func (n *onode) prune(path string) bool {
	switch n.kind {
	case 's':
		switch v := n.scalar.(type) {
		case nil:
			return false
		case string:
			return v != "" && v != "0001-01-01T00:00:00Z"
		}
		return true
	case 'o':
		keys, vals := n.keys[:0:0], n.vals[:0:0]
		for i, k := range n.keys {
			cp := k
			if path != "" {
				cp = path + "." + k
			}
			if n.vals[i].prune(cp) {
				keys, vals = append(keys, k), append(vals, n.vals[i])
			}
		}
		n.keys, n.vals = keys, vals
		return len(n.keys) > 0 || path == "spec.sandbox"
	default:
		items := n.items[:0:0]
		for _, it := range n.items {
			if it.prune(path + "[]") {
				items = append(items, it)
			}
		}
		n.items = items
		return len(n.items) > 0
	}
}

func (n *onode) toAny() any {
	switch n.kind {
	case 'o':
		m := make(map[string]any, len(n.keys))
		for i, k := range n.keys {
			m[k] = n.vals[i].toAny()
		}
		return m
	case 'a':
		out := make([]any, len(n.items))
		for i, it := range n.items {
			out[i] = it.toAny()
		}
		return out
	}
	return n.scalar
}

// machineTree returns the pruned, ordered tree of m (without status).
func machineTree(m model.Machine) (*onode, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	root, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	for i, k := range root.keys {
		if k == "status" {
			root.keys = append(root.keys[:i], root.keys[i+1:]...)
			root.vals = append(root.vals[:i], root.vals[i+1:]...)
			break
		}
	}
	root.prune("")
	return root, nil
}

// machineFieldComments explains every field a preset can emit. Keys are
// dotted paths with [] for list elements. A test requires an entry for every
// emitted key.
var machineFieldComments = map[string]string{
	"apiVersion":                              "Kairon API group and version.",
	"kind":                                    "A virtual machine.",
	"metadata":                                "Object identity.",
	"spec":                                    "Desired state.",
	"metadata.name":                           "Machine name (DNS label, at most 63 characters).",
	"metadata.namespace":                      "Namespace the Machine lives in; quotas and budgets apply per namespace.",
	"spec.image":                              "Boot disk.",
	"spec.image.path":                         "EDIT: boot disk path on the node that runs the Machine, or use catalogName / imageRef.",
	"spec.image.catalogName":                  "Name of a FluxVM image-catalog entry on the node.",
	"spec.image.imageRef":                     "A cluster MachineImage (kind disk), copied in once before boot.",
	"spec.image.diskSize":                     "Grow the root disk to this size at creation (never shrinks).",
	"spec.resources":                          "Guest size.",
	"spec.resources.cpu":                      "vCPUs, e.g. \"2\" (quoted: a string).",
	"spec.resources.memory":                   "Guest memory, e.g. 4Gi.",
	"spec.instanceTypeName":                   "MachineInstanceType that supplies cpu and memory once, at creation.",
	"spec.instanceTypeKind":                   "MachineClusterInstanceType: the instance type is cluster-scoped.",
	"spec.preferenceName":                     "MachinePreference that fills still-unset guest-OS defaults once.",
	"spec.preferenceKind":                     "MachineClusterPreference: the preference is cluster-scoped.",
	"spec.runtime":                            "Hypervisor backend.",
	"spec.runtime.backend":                    "qemu supports secure boot, TPM and device passthrough.",
	"spec.security":                           "Firmware security; both need the qemu backend (TPM also cloud-hypervisor).",
	"spec.security.secureBoot":                "UEFI Secure Boot (qemu only).",
	"spec.security.tpm":                       "Emulated TPM 2.0.",
	"spec.cloudInit":                          "First-boot configuration through cloud-init (cloudbase-init on Windows).",
	"spec.cloudInit.hostname":                 "Guest hostname.",
	"spec.cloudInit.user":                     "Guest user to configure.",
	"spec.cloudInit.sshAuthorizedKeys":        "Public keys allowed to log in.",
	"spec.cloudInit.packages":                 "Packages installed at first boot.",
	"spec.cloudInit.runCmd":                   "Shell commands run once at first boot.",
	"spec.cloudInit.writeFiles":               "Files written before first boot.",
	"spec.cloudInit.writeFiles[].path":        "Destination path in the guest.",
	"spec.cloudInit.writeFiles[].content":     "File content.",
	"spec.cloudInit.writeFiles[].permissions": "Octal mode, e.g. \"0644\".",
	"spec.guestAgent":                         "Guest agent channels.",
	"spec.guestAgent.enabled":                 "qemu-guest-agent: the image must run it; reports the guest IP and enables exec.",
	"spec.guestAgent.console":                 "FluxVM's vsock agent: enables Kairon's guest file, exec and console features.",
	"spec.accelerators":                       "GPUs or other accelerators, by profile.",
	"spec.accelerators[].name":                "Name for the devices this entry expands to (DNS label).",
	"spec.accelerators[].profile":             "EDIT: name of a cluster MachineAcceleratorProfile (kubectl get machineacceleratorprofiles).",
	"spec.sandbox":                            "Run on FluxVM's lightweight sandbox track instead of a full VM ({} = defaults).",
	"spec.ttlSeconds":                         "Delete the Machine this many seconds after creation.",
	"spec.powerState":                         "Running, Stopped, Paused, Halted or Hibernated.",
}

func commentFor(path string) string {
	if c, ok := machineFieldComments[path]; ok {
		return c
	}
	return ""
}

var yamlSafeRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_./-]*$`)

var yamlReserved = map[string]bool{"true": true, "false": true, "null": true, "yes": true, "no": true, "on": true, "off": true, "y": true, "n": true}

func yamlScalar(v any) string {
	switch t := v.(type) {
	case string:
		if yamlSafeRE.MatchString(t) && !yamlReserved[strings.ToLower(t)] {
			return t
		}
		b, _ := json.Marshal(t)
		return string(b)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return "null"
}

// writeYAMLObject writes the keys of n at column col. With dash, the first
// key shares the line of the list dash two columns to the left.
func writeYAMLObject(b *strings.Builder, n *onode, path string, col int, dash bool) {
	for i, k := range n.keys {
		v := n.vals[i]
		p := k
		if path != "" {
			p = path + "." + k
		}
		lead, cind := strings.Repeat(" ", col), col
		if dash && i == 0 {
			lead, cind = strings.Repeat(" ", col-2)+"- ", col-2
		}
		if c := commentFor(p); c != "" {
			b.WriteString(strings.Repeat(" ", cind) + "# " + c + "\n")
		}
		switch {
		case v.kind == 's':
			b.WriteString(lead + k + ": " + yamlScalar(v.scalar) + "\n")
		case v.kind == 'o' && len(v.keys) == 0:
			b.WriteString(lead + k + ": {}\n")
		case v.kind == 'o':
			b.WriteString(lead + k + ":\n")
			writeYAMLObject(b, v, p, col+2, false)
		default:
			b.WriteString(lead + k + ":\n")
			for _, it := range v.items {
				if it.kind == 's' {
					b.WriteString(strings.Repeat(" ", col+2) + "- " + yamlScalar(it.scalar) + "\n")
				} else {
					writeYAMLObject(b, it, p+"[]", col+4, true)
				}
			}
		}
	}
}

// renderMachineYAML renders m as commented YAML.
func renderMachineYAML(m model.Machine, preset string, p machinePreset) (string, error) {
	root, err := machineTree(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Generated by: kaironctl new machine " + m.Metadata.Name + " --preset " + preset + "\n")
	b.WriteString("# A starting point, not tuned for any workload: review every value, then\n")
	b.WriteString("#   kubectl apply -f THIS_FILE   (or: kaironctl create ... for flag-driven creation)\n")
	b.WriteString("# Field reference: kaironctl explain machine.spec\n")
	if preset != "" {
		b.WriteString("# Preset " + preset + ": " + p.Description + ".\n")
	}
	writeYAMLObject(&b, root, "", 0, false)
	return b.String(), nil
}

// renderMachineJSON renders m as indented JSON.
func renderMachineJSON(m model.Machine) (string, error) {
	root, err := machineTree(m)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(root.toAny(), "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

func writePresetList(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PRESET\tCPU\tMEMORY\tDISK\tDESCRIPTION")
	for _, p := range machinePresets {
		disk := p.DiskSize
		if disk == "" {
			disk = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.CPU, p.Memory, disk, p.Description)
	}
	_ = tw.Flush()
}

func newNewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Print starter manifests from built-in presets (offline)",
		Long: `Print a valid, commented starter manifest without contacting a cluster.

Unlike ` + "`create`" + `, which sends an object to the cluster, ` + "`new`" + ` only writes
YAML (or JSON) to stdout: redirect it to a file, edit it, keep it in git,
and apply it yourself.`,
	}
	cmd.AddCommand(newNewMachineCmd())
	return cmd
}

func newNewMachineCmd() *cobra.Command {
	var o newMachineOptions
	var output string
	var list bool
	cmd := &cobra.Command{
		Use:   "machine NAME [flags]",
		Short: "Print a commented starter Machine manifest from a preset (offline)",
		Long: `Print a starter Machine manifest built from a preset, with a short comment
on every field. Nothing is sent to a cluster and no kubeconfig is read.

The manifest is built from the same Machine type the controller uses and is
checked with the admission webhook's own validators before it is printed.
Presets are starting points with plausible sizes, not tuned for any workload;
the image path in particular is a placeholder you must edit.

--image REF takes an absolute node path, catalog:NAME (FluxVM image catalog)
or image:NAME (a MachineImage). --ssh-key-file reads a PUBLIC key file only.
--cloud-init-file reads a #cloud-config file and keeps the keys a Machine's
spec.cloudInit can carry (hostname, ssh_authorized_keys, packages, runcmd,
write_files); anything else is reported on stderr.`,
		Example: `  $ kaironctl new machine --list-presets
  $ kaironctl new machine web-1 --preset small > web-1.yaml
  $ kaironctl new machine db-1 --preset database --disk-size 500Gi --ssh-key-file ~/.ssh/id_ed25519.pub
  $ kaironctl new machine win11 --preset windows --secure-boot --tpm -n vms -o json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if list {
				return cobra.MaximumNArgs(0)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if list {
				writePresetList(out)
				return nil
			}
			switch output {
			case "", "yaml", "json":
			default:
				return fmt.Errorf("unknown output format %q (want yaml or json)", output)
			}
			o.Name = args[0]
			o.Namespace, _ = cmd.Flags().GetString("namespace")
			errOut := cmd.ErrOrStderr()
			o.Warnf = func(s string) { _, _ = fmt.Fprintln(errOut, s) }
			m, err := buildNewMachine(o)
			if err != nil {
				return err
			}
			var text string
			if output == "json" {
				text, err = renderMachineJSON(m)
			} else {
				preset := o.Preset
				if preset == "" {
					preset = newMachineDefaultPreset
				}
				p, _ := findMachinePreset(preset)
				text, err = renderMachineYAML(m, preset, p)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(out, text)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Preset, "preset", "", "starting point: "+strings.Join(machinePresetNames(), "|")+" (default "+newMachineDefaultPreset+")")
	f.BoolVar(&list, "list-presets", false, "list the built-in presets and exit")
	f.StringVar(&o.CPU, "cpu", "", "vCPUs, overriding the preset (e.g. 4)")
	f.StringVar(&o.Memory, "memory", "", "memory, overriding the preset (e.g. 8Gi)")
	f.StringVar(&o.DiskSize, "disk-size", "", "root disk size, overriding the preset (e.g. 60Gi)")
	f.StringVar(&o.Image, "image", "", "boot image: absolute node path, catalog:NAME or image:NAME (a MachineImage)")
	f.StringVar(&o.SSHKeyFile, "ssh-key-file", "", "PUBLIC key file to authorize in the guest (private keys are refused)")
	f.StringVar(&o.CloudInitFile, "cloud-init-file", "", "#cloud-config file to convert into spec.cloudInit")
	f.BoolVar(&o.SecureBoot, "secure-boot", false, "enable UEFI Secure Boot (qemu backend)")
	f.BoolVar(&o.TPM, "tpm", false, "attach an emulated TPM 2.0")
	f.StringVar(&o.InstanceType, "instance-type", "", "MachineInstanceType to take cpu/memory from (unless --cpu/--memory are given)")
	f.BoolVar(&o.ClusterInstanceType, "cluster-instance-type", false, "--instance-type names a cluster-scoped MachineClusterInstanceType")
	f.StringVar(&o.Preference, "preference", "", "MachinePreference supplying guest-OS defaults")
	f.BoolVar(&o.ClusterPreference, "cluster-preference", false, "--preference names a cluster-scoped MachineClusterPreference")
	f.StringVarP(&output, "output", "o", "yaml", "output format: yaml|json")
	return cmd
}
