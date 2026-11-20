// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/model"
)

const kveWebMachine = `apiVersion: kairon.zyvor.dev/v1alpha1
kind: Machine
metadata:
  name: web01
  namespace: shop
  labels:
    app: web
    tier: frontend
  uid: 1234
  resourceVersion: "77"
spec:
  powerState: Running
  resources:
    cpu: "4"
    memory: 8Gi
  security:
    secureBoot: true
    tpm: true
  image:
    source:
      oci: registry.example.com/images/ubuntu:24.04
  disks:
  - name: data
    claimName: web01-data
  cloudInit:
    hostname: web01
    user: ops
    sshAuthorizedKeys:
    - ssh-ed25519 AAAAC3Nza-example ops@example.com
    packages: [nginx]
    runCmd:
    - systemctl enable --now nginx
    writeFiles:
    - path: /etc/motd
      content: hello
      permissions: "0644"
status:
  phase: Running
`

const kveDBMachine = `apiVersion: kairon.zyvor.dev/v1alpha1
kind: Machine
metadata:
  name: db01
spec:
  powerState: Stopped
  resources: {cpu: "2", memory: 16Gi}
  volumes:
  - name: root
    claimName: db01-root
`

func runKVE(t *testing.T, files map[string]string, stdin string, args ...string) (out, errOut string, err error) {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, len(files)+len(args))
	for name, body := range files {
		p := filepath.Join(dir, name)
		if werr := os.WriteFile(p, []byte(body), 0o600); werr != nil {
			t.Fatal(werr)
		}
		paths = append(paths, p)
	}
	var o, e bytes.Buffer
	cmd := newExportKubevirtCmd(&Options{})
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	err = cmd.RunE(cmd, append(paths, args...))
	return o.String(), e.String(), err
}

func kveDecode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := sigsyaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode: %v\n%s", err, s)
	}
	return m
}

func TestExportKubevirtMapped(t *testing.T) {
	out, errOut, err := runKVE(t, map[string]string{"web.yaml": kveWebMachine}, "")
	if err != nil {
		t.Fatal(err)
	}
	if errOut != "" {
		t.Fatalf("unexpected warnings:\n%s", errOut)
	}
	vm := kveDecode(t, out)
	checks := map[string]string{
		kveStr(vm, "apiVersion"):                "kubevirt.io/v1",
		kveStr(vm, "kind"):                      "VirtualMachine",
		kveStr(vm, "metadata", "name"):          "web01",
		kveStr(vm, "metadata", "namespace"):     "shop",
		kveStr(vm, "metadata", "labels", "app"): "web",
		kveStr(vm, "spec", "runStrategy"):       "Always",
		kveStr(kveMap(vm, "spec", "template", "spec", "domain", "memory"), "guest"): "8Gi",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("got %q want %q\n%s", got, want, out)
		}
	}
	domain := kveMap(vm, "spec", "template", "spec", "domain")
	if cores, _ := kveMap(domain, "cpu")["cores"].(float64); cores != 4 {
		t.Errorf("cores %v", cores)
	}
	if !kveBool(domain, "firmware", "bootloader", "efi", "secureBoot") || !kveBool(domain, "features", "smm", "enabled") {
		t.Errorf("secure boot not set:\n%s", out)
	}
	if kveMap(domain, "devices", "tpm") == nil {
		t.Errorf("tpm missing:\n%s", out)
	}
	vols := kveList(vm, "spec", "template", "spec", "volumes")
	if len(vols) != 3 {
		t.Fatalf("want containerDisk, data PVC, cloudinit; got %d:\n%s", len(vols), out)
	}
	if kveStr(vols[0], "containerDisk", "image") != "registry.example.com/images/ubuntu:24.04" {
		t.Errorf("containerDisk: %v", vols[0])
	}
	if kveStr(vols[1], "persistentVolumeClaim", "claimName") != "web01-data" {
		t.Errorf("data pvc: %v", vols[1])
	}
	ud := kveStr(vols[2], "cloudInitNoCloud", "userData")
	for _, want := range []string{"#cloud-config\n", "hostname: web01", "ssh_authorized_keys:", "- nginx", "runcmd:", "write_files:", "path: /etc/motd"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data missing %q:\n%s", want, ud)
		}
	}
	if disks := kveList(domain, "devices", "disks"); len(disks) != 3 {
		t.Errorf("disks %d", len(disks))
	}
}

func TestExportKubevirtTable(t *testing.T) {
	tests := []struct {
		name     string
		machine  string
		args     []string
		contains []string
		warns    []string
		noWarn   bool
	}{
		{
			name:     "stopped boot pvc",
			machine:  kveDBMachine,
			contains: []string{"runStrategy: Halted", "claimName: db01-root", "bootOrder: 1", "guest: 16Gi"},
			noWarn:   true,
		},
		{
			name:     "namespace flag overrides",
			machine:  kveWebMachine,
			args:     []string{"--namespace", "prod"},
			contains: []string{"namespace: prod"},
			warns:    nil,
			noWarn:   true,
		},
		{
			name: "unmapped fields warn",
			machine: kveDBMachine + `  mystery: 1
`,
			contains: []string{"kind: VirtualMachine"},
			warns:    []string{"warning: db01: spec.mystery not converted: "},
		},
		{
			name: "hotplug accelerators mac fabric",
			machine: `apiVersion: kairon.zyvor.dev/v1alpha1
kind: Machine
metadata: {name: gpu01, annotations: {a: b}}
spec:
  resources: {cpu: "8", memory: 32Gi, maxCpu: "16", maxMemory: 64Gi, hugepages: true}
  volumes: [{name: root, claimName: gpu01-root}]
  accelerators: [{name: a100}]
  apple: {rosetta: true}
  tenant: acme
  network: {forwards: [{hostPort: 2222, guestPort: 22}]}
  runtime: {backend: qemu}
  image: {digest: "sha256:abc"}
  placement: {nodeSelector: {zone: a}}
`,
			warns: []string{
				"warning: gpu01: metadata.annotations not converted:",
				"warning: gpu01: spec.resources.maxCpu not converted: CPU hotplug",
				"warning: gpu01: spec.resources.maxMemory not converted: memory hotplug",
				"warning: gpu01: spec.resources.hugepages not converted:",
				"warning: gpu01: spec.accelerators not converted: accelerator",
				"warning: gpu01: spec.apple not converted: Mac fabric",
				"warning: gpu01: spec.tenant not converted: the tenant fence",
				"warning: gpu01: spec.network not converted: the Kairon network fabric",
				"warning: gpu01: spec.runtime not converted:",
				"warning: gpu01: spec.image.digest not converted:",
				"warning: gpu01: spec.placement not converted:",
			},
		},
		{
			name: "paused has no run strategy",
			machine: `apiVersion: kairon.zyvor.dev/v1alpha1
kind: Machine
metadata: {name: p1}
spec:
  powerState: Paused
  resources: {cpu: "1", memory: 1Gi}
  volumes: [{name: root, claimName: p1-root, guestPath: /mnt, readOnly: true}]
`,
			warns: []string{
				`warning: p1: spec.powerState not converted: power state "Paused"`,
				"warning: p1: spec.volumes[0].guestPath not converted:",
				"warning: p1: spec.volumes[0].readOnly not converted:",
			},
		},
		{
			name: "no boot disk and non-oci source",
			machine: `apiVersion: kairon.zyvor.dev/v1alpha1
kind: Machine
metadata: {name: h1}
spec:
  resources: {cpu: 500m, memory: 1Gi}
  image: {source: {httpURL: "https://example.com/a.qcow2"}}
`,
			contains: []string{"cpu: 500m"},
			warns: []string{
				"warning: h1: spec.image not converted: no boot disk",
				"warning: h1: spec.image.source.httpURL not converted: only an OCI image source",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, err := runKVE(t, map[string]string{"m.yaml": tc.machine}, "", tc.args...)
			if err != nil {
				t.Fatalf("err %v", err)
			}
			for _, w := range tc.contains {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, w := range tc.warns {
				if !strings.Contains(errOut, w) {
					t.Errorf("stderr missing %q:\n%s", w, errOut)
				}
			}
			if tc.noWarn && errOut != "" {
				t.Errorf("unexpected warnings:\n%s", errOut)
			}
		})
	}
}

func TestExportKubevirtMultiDocAndList(t *testing.T) {
	in := kveWebMachine + "---\n" + kveDBMachine + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: cm}\n"
	out, errOut, err := runKVE(t, map[string]string{"m.yaml": in}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "kind: VirtualMachine") != 2 || strings.Count(out, "\n---\n") != 1 {
		t.Errorf("want two documents:\n%s", out)
	}
	if !strings.Contains(errOut, "v1/ConfigMap cm skipped") {
		t.Errorf("stderr: %s", errOut)
	}
	jout, _, err := runKVE(t, map[string]string{"m.yaml": kveWebMachine + "---\n" + kveDBMachine}, "", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	list := kveDecode(t, jout)
	if kveStr(list, "kind") != "List" || len(kveList(list, "items")) != 2 {
		t.Errorf("json list:\n%s", jout)
	}
}

func TestExportKubevirtStdinAndErrors(t *testing.T) {
	out, _, err := runKVE(t, nil, kveDBMachine, "-")
	if err != nil || !strings.Contains(out, "name: db01") {
		t.Fatalf("stdin: %v\n%s", err, out)
	}
	if _, _, err := runKVE(t, nil, ""); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("no files: %v", err)
	}
	if _, _, err := runKVE(t, map[string]string{"m.yaml": kveDBMachine}, "", "-o", "xml"); err == nil {
		t.Error("bad format accepted")
	}
	if _, _, err := runKVE(t, map[string]string{"m.yaml": kveDBMachine}, "", "--bogus"); err == nil {
		t.Error("unknown flag accepted")
	}
	if _, _, err := runKVE(t, map[string]string{"m.yaml": "apiVersion: v1\nkind: Pod\n"}, ""); err == nil {
		t.Error("no machine should be an error")
	}
}

func TestExportKubevirtStrict(t *testing.T) {
	// Clean input: strict passes.
	if _, _, err := runKVE(t, map[string]string{"m.yaml": kveDBMachine}, "", "--strict"); err != nil {
		t.Fatalf("clean strict: %v", err)
	}
	// Unmapped field: manifest and warning are still printed, then it fails.
	dirty := kveDBMachine + "  tenant: acme\n"
	out, errOut, err := runKVE(t, map[string]string{"m.yaml": dirty}, "", "--strict")
	if err == nil || !strings.Contains(err.Error(), "--strict") {
		t.Fatalf("strict err %v", err)
	}
	if !strings.Contains(out, "kind: VirtualMachine") || !strings.Contains(errOut, "spec.tenant not converted") {
		t.Errorf("out=%q err=%q", out, errOut)
	}
}

func TestExportKubevirtNeverContactsCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	if _, _, err := runKVE(t, map[string]string{"m.yaml": kveWebMachine}, ""); err != nil {
		t.Fatal(err)
	}
}

// TestExportKubevirtRoundTrip converts hand-written Machine fixtures to a
// VirtualMachine and back through `import kubevirt`, and checks the fields
// both directions can express survive. Fixtures are this file's own.
func TestExportKubevirtRoundTrip(t *testing.T) {
	for name, src := range map[string]string{"web": kveWebMachine, "db": kveDBMachine} {
		t.Run(name, func(t *testing.T) {
			var orig model.Machine
			if err := sigsyaml.Unmarshal([]byte(src), &orig); err != nil {
				t.Fatal(err)
			}
			vmYAML, _, err := runKVE(t, map[string]string{"m.yaml": src}, "")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			vmFile := filepath.Join(dir, "vm.yaml")
			if err := os.WriteFile(vmFile, []byte(vmYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			var back bytes.Buffer
			if err := importKubevirt([]string{vmFile}, nil, &back, &bytes.Buffer{}, "", "yaml", false); err != nil {
				t.Fatal(err)
			}
			var got model.Machine
			if err := sigsyaml.Unmarshal(back.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Metadata.Name != orig.Metadata.Name {
				t.Errorf("name %q", got.Metadata.Name)
			}
			if got.Spec.Resources.CPU != orig.Spec.Resources.CPU || got.Spec.Resources.Memory != orig.Spec.Resources.Memory {
				t.Errorf("resources %+v want %+v", got.Spec.Resources, orig.Spec.Resources)
			}
			if got.Spec.Security != orig.Spec.Security {
				t.Errorf("security %+v", got.Spec.Security)
			}
			if got.Spec.PowerState != orig.Spec.PowerState && (orig.Spec.PowerState != "Running" || got.Spec.PowerState != "Running") {
				t.Errorf("powerState %q want %q", got.Spec.PowerState, orig.Spec.PowerState)
			}
			if orig.Spec.Image.Source != nil && (got.Spec.Image.Source == nil || got.Spec.Image.Source.OCI != orig.Spec.Image.Source.OCI) {
				t.Errorf("image %+v", got.Spec.Image.Source)
			}
			if len(got.Spec.Disks) != len(orig.Spec.Disks) || (len(got.Spec.Disks) > 0 && got.Spec.Disks[0].ClaimName != orig.Spec.Disks[0].ClaimName) {
				t.Errorf("disks %+v", got.Spec.Disks)
			}
			if len(got.Spec.Volumes) != len(orig.Spec.Volumes) || (len(got.Spec.Volumes) > 0 && got.Spec.Volumes[0].ClaimName != orig.Spec.Volumes[0].ClaimName) {
				t.Errorf("volumes %+v", got.Spec.Volumes)
			}
			if got.Spec.CloudInit.Hostname != orig.Spec.CloudInit.Hostname || len(got.Spec.CloudInit.RunCmd) != len(orig.Spec.CloudInit.RunCmd) {
				t.Errorf("cloudInit %+v", got.Spec.CloudInit)
			}
		})
	}
}
