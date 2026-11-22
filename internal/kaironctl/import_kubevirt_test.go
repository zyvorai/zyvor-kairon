// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

const kvWebVM = `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: web-1
  namespace: prod
  labels:
    app: web
    tier: frontend
  uid: 0a1b2c3d
  creationTimestamp: "2026-01-01T00:00:00Z"
spec:
  runStrategy: Always
  template:
    metadata:
      labels:
        kubevirt.io/domain: web-1
    spec:
      domain:
        cpu:
          cores: 2
          sockets: 2
          threads: 1
        memory:
          guest: 8Gi
        resources:
          requests:
            memory: 8Gi
        firmware:
          bootloader:
            efi:
              secureBoot: true
        devices:
          tpm: {}
          disks:
            - name: rootdisk
              bootOrder: 1
              disk:
                bus: virtio
            - name: data
              disk:
                bus: virtio
            - name: cloudinitdisk
              disk:
                bus: virtio
      volumes:
        - name: rootdisk
          dataVolume:
            name: web-1-root
        - name: data
          persistentVolumeClaim:
            claimName: web-1-data
        - name: cloudinitdisk
          cloudInitNoCloud:
            userData: |
              #cloud-config
              hostname: web-1
              ssh_authorized_keys:
                - ssh-ed25519 AAAAC3Nza example
              packages: [nginx]
              runcmd:
                - systemctl enable --now nginx
              write_files:
                - path: /etc/motd
                  content: hello
                  permissions: "0644"
`

const kvDBVM = `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: db-1
spec:
  running: false
  template:
    spec:
      domain:
        cpu:
          cores: 4
        resources:
          requests:
            memory: 16Gi
        devices:
          interfaces:
            - name: default
              masquerade: {}
            - name: storage
              bridge: {}
          hostDevices:
            - name: gpu0
              deviceName: nvidia.com/A10
          disks:
            - name: rootdisk
              disk: {}
      networks:
        - name: default
          pod: {}
        - name: storage
          multus:
            networkName: storage-net
      volumes:
        - name: rootdisk
          containerDisk:
            image: quay.io/containerdisks/ubuntu:24.04
  instancetype:
    name: u1.large
`

func TestConvertKubevirtVM(t *testing.T) {
	docs := func(src string) map[string]any {
		d, err := kubevirtDocs(strings.NewReader(src))
		if err != nil || len(d) != 1 {
			t.Fatalf("docs: %v %d", err, len(d))
		}
		return d[0]
	}
	tests := []struct {
		name  string
		src   string
		ns    string
		check func(t *testing.T, m model.Machine)
		warns []string // substrings, each must be present; len(warns)==0 means none expected
	}{
		{
			name: "full web VM maps cleanly",
			src:  kvWebVM,
			check: func(t *testing.T, m model.Machine) {
				s := m.Spec
				if m.Metadata.Name != "web-1" || m.Metadata.Namespace != "prod" || m.Metadata.Labels["app"] != "web" || m.Metadata.Labels["kairon.zyvor.dev/imported-from"] != "kubevirt" {
					t.Errorf("metadata %+v", m.Metadata)
				}
				if s.Resources.CPU != "4" || s.Resources.Memory != "8Gi" || s.PowerState != "Running" {
					t.Errorf("resources %+v power %q", s.Resources, s.PowerState)
				}
				if !s.Security.SecureBoot || !s.Security.TPM {
					t.Errorf("security %+v", s.Security)
				}
				if len(s.Volumes) != 1 || s.Volumes[0].ClaimName != "web-1-root" {
					t.Errorf("volumes %+v", s.Volumes)
				}
				if len(s.Disks) != 1 || s.Disks[0].Name != "data" || s.Disks[0].ClaimName != "web-1-data" {
					t.Errorf("disks %+v", s.Disks)
				}
				ci := s.CloudInit
				if ci.Hostname != "web-1" || len(ci.SSHAuthorizedKeys) != 1 || len(ci.Packages) != 1 || len(ci.RunCmd) != 1 ||
					len(ci.WriteFiles) != 1 || ci.WriteFiles[0].Path != "/etc/motd" || ci.WriteFiles[0].Permissions != "0644" {
					t.Errorf("cloudInit %+v", ci)
				}
			},
			// The template's pod labels are the only thing without a Machine field.
			warns: []string{"spec.template.metadata not converted"},
		},
		{
			name: "namespace override",
			src:  kvWebVM,
			ns:   "other",
			check: func(t *testing.T, m model.Machine) {
				if m.Metadata.Namespace != "other" {
					t.Errorf("namespace %q", m.Metadata.Namespace)
				}
			},
			warns: []string{"spec.template.metadata"},
		},
		{
			name: "unmapped fields are reported",
			src:  kvDBVM,
			check: func(t *testing.T, m model.Machine) {
				s := m.Spec
				if m.Metadata.Namespace != "default" || s.PowerState != "Stopped" || s.Resources.CPU != "4" || s.Resources.Memory != "16Gi" {
					t.Errorf("spec %+v ns %q", s, m.Metadata.Namespace)
				}
				if s.Image.Source == nil || s.Image.Source.OCI != "quay.io/containerdisks/ubuntu:24.04" {
					t.Errorf("image %+v", s.Image)
				}
			},
			warns: []string{
				"warning: db-1: spec.template.spec.domain.devices.interfaces not converted",
				"warning: db-1: spec.template.spec.domain.devices.hostDevices not converted",
				"warning: db-1: spec.template.spec.networks not converted",
				"warning: db-1: spec.instancetype not converted",
				"containerDisk.image not converted: the image has no @sha256 digest",
			},
		},
		{
			name: "containerDisk digest is kept",
			src: `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: cd}
spec:
  template:
    spec:
      domain:
        cpu: {cores: 1}
        memory: {guest: 1Gi}
      volumes:
        - name: root
          containerDisk:
            image: registry.example.com/os/fedora@sha256:abcd
`,
			check: func(t *testing.T, m model.Machine) {
				if m.Spec.Image.Digest != "sha256:abcd" || m.Spec.Image.Source.OCI == "" {
					t.Errorf("image %+v", m.Spec.Image)
				}
			},
		},
		{
			name: "second containerDisk, emptyDisk, script userData and network data",
			src: `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: odd}
spec:
  runStrategy: Manual
  template:
    spec:
      domain:
        cpu: {cores: 1}
        memory: {guest: 1Gi}
        devices:
          disks:
            - {name: root, bootOrder: 1, disk: {}}
            - {name: extra, disk: {}}
            - {name: tmp, disk: {}, cache: none}
            - {name: iso, cdrom: {}}
      volumes:
        - name: root
          persistentVolumeClaim: {claimName: odd-root, readOnly: true}
        - name: extra
          containerDisk: {image: r.example.com/x@sha256:1}
        - name: tmp
          emptyDisk: {capacity: 1Gi}
        - name: iso
          persistentVolumeClaim: {claimName: install-iso}
        - name: ci
          cloudInitNoCloud:
            userData: "#!/bin/sh\necho hi\n"
            networkData: "version: 2"
`,
			check: func(t *testing.T, m model.Machine) {
				if len(m.Spec.Volumes) != 1 || len(m.Spec.Disks) != 0 || m.Spec.PowerState != "" {
					t.Errorf("spec %+v", m.Spec)
				}
			},
			warns: []string{
				"spec.runStrategy not converted",
				"volumes[0].persistentVolumeClaim.readOnly not converted",
				"volumes[1].containerDisk not converted",
				"volumes[2].emptyDisk not converted: this volume type",
				"disks[2].cache not converted",
				"disks[3] not converted: cdrom",
				"networkData not converted",
				"userData not converted: not a #cloud-config",
			},
		},
		{
			name: "cloud-config keys and secrets",
			src: `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: ci}
spec:
  template:
    spec:
      domain:
        cpu: {cores: 1}
        memory: {guest: 1Gi}
      volumes:
        - name: root
          persistentVolumeClaim: {claimName: c}
        - name: ci
          cloudInitNoCloud:
            userDataSecretRef: {name: my-secret}
        - name: ci2
          cloudInitNoCloud:
            userDataBase64: ` + "I2Nsb3VkLWNvbmZpZwp1c2VyczogW3tuYW1lOiBib2J9XQpydW5jbWQ6IFtbZWNobywgaGldXQo=" + `
`,
			check: func(t *testing.T, m model.Machine) {
				if len(m.Spec.CloudInit.RunCmd) != 0 {
					t.Errorf("runCmd %v", m.Spec.CloudInit.RunCmd)
				}
			},
			warns: []string{
				"userDataSecretRef not converted: the Secret is not read",
				`cloud-config key "users" has no Machine`,
				"userData:runcmd[0] not converted",
			},
		},
		{
			name: "efi without secureBoot and no resources",
			src: `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: e}
spec:
  template:
    spec:
      domain:
        firmware: {bootloader: {efi: {persistent: true}}}
      volumes:
        - {name: r, persistentVolumeClaim: {claimName: p}}
`,
			check: func(t *testing.T, m model.Machine) {
				if m.Spec.Security.SecureBoot {
					t.Error("secureBoot must stay false")
				}
			},
			warns: []string{"efi not converted: secureBoot is not set", "efi.persistent not converted", "no CPU count", "no memory size"},
		},
		{
			name: "memory and cpu requests differing from guest are reported",
			src: `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: r}
spec:
  template:
    spec:
      domain:
        cpu: {cores: 2}
        memory: {guest: 4Gi}
        resources:
          requests: {memory: 5Gi, cpu: "1"}
          limits: {memory: 4Gi, cpu: "2"}
      volumes:
        - {name: r, persistentVolumeClaim: {claimName: p}}
`,
			warns: []string{"resources.requests.memory not converted: differs", "resources.requests.cpu not converted: differs"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, warns, err := convertKubevirtVM(docs(tt.src), tt.ns)
			if err != nil {
				t.Fatal(err)
			}
			if tt.check != nil {
				tt.check(t, m)
			}
			all := strings.Join(warns, "\n")
			for _, w := range tt.warns {
				if !strings.Contains(all, w) {
					t.Errorf("missing warning %q in:\n%s", w, all)
				}
			}
			if len(tt.warns) == 0 && len(warns) != 0 {
				t.Errorf("unexpected warnings:\n%s", all)
			}
			for _, w := range warns {
				if !strings.HasPrefix(w, "warning: "+m.Metadata.Name+": ") || !strings.Contains(w, " not converted: ") {
					t.Errorf("malformed warning %q", w)
				}
			}
		})
	}
}

func TestConvertKubevirtVMNoName(t *testing.T) {
	if _, _, err := convertKubevirtVM(map[string]any{"kind": "VirtualMachine"}, ""); err == nil {
		t.Fatal("want error")
	}
}

func writeKV(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vm.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportKubevirtFiles(t *testing.T) {
	multi := kvWebVM + "---\n" + kvDBVM + "---\napiVersion: v1\nkind: Service\nmetadata: {name: svc}\n"
	tests := []struct {
		name       string
		input      string
		stdin      bool
		format     string
		strict     bool
		ns         string
		wantErr    string
		wantOut    []string
		wantErrOut []string
		check      func(t *testing.T, out string)
	}{
		{
			name: "multi-doc yaml", input: multi, format: "yaml",
			wantOut:    []string{"kind: Machine", "name: web-1", "name: db-1", "\n---\n"},
			wantErrOut: []string{"warning: db-1:", "Service svc skipped"},
		},
		{
			name: "stdin", input: kvWebVM, stdin: true, format: "yaml", wantOut: []string{"name: web-1"},
		},
		{
			name: "json single", input: kvWebVM, format: "json",
			check: func(t *testing.T, out string) {
				var m model.Machine
				if err := json.Unmarshal([]byte(out), &m); err != nil || m.Metadata.Name != "web-1" {
					t.Fatalf("%v %q", err, out)
				}
			},
		},
		{
			name: "json multi is a List", input: multi, format: "json",
			check: func(t *testing.T, out string) {
				var l struct {
					Kind  string          `json:"kind"`
					Items []model.Machine `json:"items"`
				}
				if err := json.Unmarshal([]byte(out), &l); err != nil || l.Kind != "List" || len(l.Items) != 2 {
					t.Fatalf("%v %s", err, out)
				}
			},
		},
		{
			name: "json input is accepted", format: "yaml",
			input:   `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"j"},"spec":{"template":{"spec":{"domain":{"cpu":{"cores":1},"memory":{"guest":"1Gi"}},"volumes":[{"name":"r","persistentVolumeClaim":{"claimName":"p"}}]}}}}`,
			wantOut: []string{"name: j"},
		},
		{
			name: "namespace override", input: kvDBVM, format: "yaml", ns: "lab", wantOut: []string{"namespace: lab"},
		},
		{
			name: "strict fails on warnings but still prints", input: kvDBVM, format: "yaml", strict: true,
			wantErr: "--strict", wantOut: []string{"name: db-1"}, wantErrOut: []string{"warning: db-1:"},
		},
		{
			name: "strict passes without warnings", input: strings.Replace(kvWebVM, "    metadata:\n      labels:\n        kubevirt.io/domain: web-1\n", "", 1),
			format: "yaml", strict: true, wantOut: []string{"name: web-1"},
		},
		{
			name: "bad format", input: kvWebVM, format: "xml", wantErr: "--output must be",
		},
		{
			name: "no VM", input: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n", format: "yaml", wantErr: "no KubeVirt VirtualMachine",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var stdin strings.Reader
			files := []string{}
			if tt.stdin {
				stdin = *strings.NewReader(tt.input)
				files = append(files, "-")
			} else {
				files = append(files, writeKV(t, tt.input))
			}
			err := importKubevirt(files, &stdin, &out, &errOut, tt.ns, tt.format, tt.strict)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err %v, want %q", err, tt.wantErr)
			}
			for _, w := range tt.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout missing %q:\n%s", w, out.String())
				}
			}
			for _, w := range tt.wantErrOut {
				if !strings.Contains(errOut.String(), w) {
					t.Errorf("stderr missing %q:\n%s", w, errOut.String())
				}
			}
			if tt.check != nil {
				tt.check(t, out.String())
			}
			if strings.Contains(out.String(), "warning:") {
				t.Errorf("warnings leaked to stdout:\n%s", out.String())
			}
		})
	}
}

func TestImportKubevirtNoFiles(t *testing.T) {
	if err := importKubevirt(nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, "", "yaml", false); err == nil {
		t.Fatal("want usage error")
	}
}

// The command must run with no kubeconfig and no cluster: it is offline.
func TestImportKubevirtCommandIsOffline(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("KAIRON_KUBE_URL", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	p := writeKV(t, kvDBVM)
	if code := Run([]string{"import", "kubevirt", p, "--strict"}, "test"); code == 0 {
		t.Fatal("--strict with unmapped fields must exit non-zero")
	}
	if code := Run([]string{"import", "kubevirt", p}, "test"); code != 0 {
		t.Fatalf("exit %d, want 0 without a cluster", code)
	}
}
