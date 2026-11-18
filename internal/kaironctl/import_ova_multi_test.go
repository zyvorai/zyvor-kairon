// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"flag"
	"strconv"
	"strings"
	"testing"
)

const ovfHead = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
  xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
  xmlns:vmw="http://www.vmware.com/schema/ovf">
  <References><File ovf:href="a.vmdk" ovf:id="file1"/><File ovf:href="b.vmdk" ovf:id="file2"/><File ovf:href="c.vmdk" ovf:id="file3"/></References>
  <DiskSection><Disk ovf:diskId="vmdisk1" ovf:fileRef="file1"/><Disk ovf:diskId="vmdisk2" ovf:fileRef="file2"/><Disk ovf:diskId="vmdisk3" ovf:fileRef="file3"/></DiskSection>
`

// vsys renders one VirtualSystem: name, vCPUs, MiB, NIC count and disk refs.
func vsys(id, name string, cpu, mib, nics int, disks []string, extra string) string {
	var b strings.Builder
	b.WriteString(`<VirtualSystem ovf:id="` + id + `">`)
	if name != "" {
		b.WriteString(`<Name>` + name + `</Name>`)
	}
	b.WriteString(`<OperatingSystemSection ovf:id="94" vmw:osType="ubuntu64Guest"/><VirtualHardwareSection>`)
	b.WriteString(`<Item><rasd:ResourceType>3</rasd:ResourceType><rasd:VirtualQuantity>` + strconv.Itoa(cpu) + `</rasd:VirtualQuantity></Item>`)
	b.WriteString(`<Item><rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits><rasd:ResourceType>4</rasd:ResourceType><rasd:VirtualQuantity>` + strconv.Itoa(mib) + `</rasd:VirtualQuantity></Item>`)
	for i := 0; i < nics; i++ {
		b.WriteString(`<Item><rasd:ResourceType>10</rasd:ResourceType></Item>`)
	}
	for _, d := range disks {
		b.WriteString(`<Item><rasd:HostResource>ovf:/disk/` + d + `</rasd:HostResource><rasd:ResourceType>17</rasd:ResourceType></Item>`)
	}
	b.WriteString(`</VirtualHardwareSection>` + extra + `</VirtualSystem>`)
	return b.String()
}

func ovfDoc(body string) []byte { return []byte(ovfHead + body + `</Envelope>`) }

func TestParseOVFPackage(t *testing.T) {
	tests := []struct {
		name      string
		ovf       []byte
		want      []ovfInfo
		wantWarns []string // substrings, each must appear in some warning
		noWarns   []string // substrings that must not appear
	}{
		{
			name: "single VM keeps DiskSection count",
			ovf:  ovfDoc(vsys("web", "Web01", 4, 8192, 2, []string{"vmdisk1"}, "")),
			want: []ovfInfo{{Name: "Web01", VCPUs: 4, MemoryMiB: 8192, OS: "ubuntu64Guest", NICs: 2, Disks: 3}},
		},
		{
			name: "two VirtualSystems under Envelope",
			ovf: ovfDoc(vsys("web", "web01", 2, 4096, 1, []string{"vmdisk1"}, "") +
				vsys("db", "db01", 8, 32768, 2, []string{"vmdisk2"}, "")),
			want: []ovfInfo{
				{Name: "web01", VCPUs: 2, MemoryMiB: 4096, OS: "ubuntu64Guest", NICs: 1, Disks: 1},
				{Name: "db01", VCPUs: 8, MemoryMiB: 32768, OS: "ubuntu64Guest", NICs: 2, Disks: 1},
			},
			wantWarns: []string{"all 2 Machines reference the same OVA"},
			noWarns:   []string{"shared", "startup", "vApp"},
		},
		{
			name: "three VMs in a VirtualSystemCollection with startup order and vApp properties",
			ovf: ovfDoc(`<VirtualSystemCollection ovf:id="shop"><Info>vApp</Info><Name>Shop vApp</Name>
<StartupSection><Info>start</Info><Item ovf:id="db01" ovf:order="0" ovf:startDelay="30"/><Item ovf:id="app01" ovf:order="1"/><Item ovf:id="web01" ovf:order="2"/></StartupSection>
<ProductSection><Info>p</Info><Property ovf:key="admin.password" ovf:type="password"/><Property ovf:key="domain" ovf:type="string"/></ProductSection>` +
				vsys("db01", "db01", 4, 16384, 1, []string{"vmdisk1"}, "") +
				vsys("app01", "app01", 2, 4096, 1, []string{"vmdisk2"}, `<ProductSection><Info>x</Info><Property ovf:key="app.token"/></ProductSection>`) +
				vsys("web01", "web01", 1, 2048, 1, []string{"vmdisk3"}, "") +
				`</VirtualSystemCollection>`),
			want: []ovfInfo{
				{Name: "db01", VCPUs: 4, MemoryMiB: 16384, OS: "ubuntu64Guest", NICs: 1, Disks: 1},
				{Name: "app01", VCPUs: 2, MemoryMiB: 4096, OS: "ubuntu64Guest", NICs: 1, Disks: 1},
				{Name: "web01", VCPUs: 1, MemoryMiB: 2048, OS: "ubuntu64Guest", NICs: 1, Disks: 1},
			},
			wantWarns: []string{
				`collection "Shop vApp": startup ordering db01(order=0), app01(order=1), web01(order=2) not converted`,
				`collection "Shop vApp": vApp properties admin.password, domain not converted`,
				`VM "app01": vApp properties app.token not converted`,
			},
		},
		{
			name: "nested collection",
			ovf: ovfDoc(`<VirtualSystemCollection ovf:id="outer"><VirtualSystemCollection ovf:id="inner">` +
				vsys("a", "a", 1, 1024, 1, nil, "") + `</VirtualSystemCollection>` + vsys("b", "b", 1, 1024, 1, nil, "") + `</VirtualSystemCollection>`),
			want: []ovfInfo{
				{Name: "b", VCPUs: 1, MemoryMiB: 1024, OS: "ubuntu64Guest", NICs: 1},
				{Name: "a", VCPUs: 1, MemoryMiB: 1024, OS: "ubuntu64Guest", NICs: 1},
			},
		},
		{
			name: "shared disk and unnamed VM",
			ovf: ovfDoc(vsys("n1", "node1", 2, 2048, 1, []string{"vmdisk1", "vmdisk3"}, "") +
				vsys("n2-id", "", 2, 2048, 1, []string{"vmdisk2", "vmdisk3"}, "")),
			want: []ovfInfo{
				{Name: "node1", VCPUs: 2, MemoryMiB: 2048, OS: "ubuntu64Guest", NICs: 1, Disks: 2},
				{Name: "n2-id", VCPUs: 2, MemoryMiB: 2048, OS: "ubuntu64Guest", NICs: 1, Disks: 2},
			},
			wantWarns: []string{
				"disk ovf:/disk/vmdisk3 is shared by VMs node1, n2-id",
				`VM "node1": has 2 disks; only the boot disk is attached`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, warns, err := parseOVFPackage(tc.ovf)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d VMs %+v, want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("VM %d: got %+v\nwant %+v", i, got[i], tc.want[i])
				}
			}
			all := strings.Join(warns, "\n")
			for _, w := range tc.wantWarns {
				if !strings.Contains(all, w) {
					t.Errorf("missing warning %q in:\n%s", w, all)
				}
			}
			for _, w := range tc.noWarns {
				if strings.Contains(all, w) {
					t.Errorf("unexpected %q in:\n%s", w, all)
				}
			}
		})
	}
}

func TestParseOVFPackageErrors(t *testing.T) {
	if _, _, err := parseOVFPackage([]byte(`<Envelope><VirtualSystemCollection/></Envelope>`)); err == nil {
		t.Fatal("collection without VirtualSystem must fail")
	}
	if _, _, err := parseOVFPackage([]byte(`<nope`)); err == nil {
		t.Fatal("bad XML must fail")
	}
}

func TestUniqueMachineNames(t *testing.T) {
	long := strings.Repeat("a", 70)
	got, notes := uniqueMachineNames([]string{"Web 01", "web-01", "WEB_01", "", "db.prod", "!!!", long, long})
	want := []string{"web-01", "web-01-2", "web-01-3", "vm-4", "db-prod", "vm-6", strings.Repeat("a", 63), strings.Repeat("a", 61) + "-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], `"Web 01" becomes Machine "web-01"`) {
		t.Fatalf("notes %v", notes)
	}
}

func TestBuildImportMachinesYAML(t *testing.T) {
	infos, _, err := parseOVFPackage(ovfDoc(vsys("a", "Web 01", 2, 4096, 1, nil, "") + vsys("b", "web-01", 8, 16384, 1, nil, "")))
	if err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	specFn, _ := machineSpecFromFlags(fs)
	if err := fs.Parse([]string{"--cpu", "6"}); err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	ms, _ := buildImportMachines(infos, multiImportOpts{url: "https://files.example.com/shop.ova", namespace: "lab", sum: "sha256:abc", set: set, specFn: specFn})
	if len(ms) != 2 || ms[0].Metadata.Name != "web-01" || ms[1].Metadata.Name != "web-01-2" {
		t.Fatalf("machines %+v", ms)
	}
	if ms[0].Spec.Resources.CPU != "6" || ms[1].Spec.Resources.CPU != "6" {
		t.Fatalf("--cpu must win: %q %q", ms[0].Spec.Resources.CPU, ms[1].Spec.Resources.CPU)
	}
	if ms[0].Spec.Resources.Memory != "4096Mi" || ms[1].Spec.Resources.Memory != "16384Mi" {
		t.Fatalf("memory %q %q", ms[0].Spec.Resources.Memory, ms[1].Spec.Resources.Memory)
	}
	if src := ms[1].Spec.Image.Source; src == nil || src.Format != "ova" || !src.Repair || src.HTTPURL != "https://files.example.com/shop.ova" || ms[1].Spec.Image.Digest != "sha256:abc" {
		t.Fatalf("image %+v", ms[1].Spec.Image)
	}
	var buf bytes.Buffer
	if err := writeMachinesYAML(&buf, ms); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "\n---\n") != 1 || strings.Count(out, "kind: Machine") != 2 || !strings.Contains(out, "namespace: lab") || !strings.Contains(out, "imported-from: ova") {
		t.Fatalf("yaml stream:\n%s", out)
	}
}
