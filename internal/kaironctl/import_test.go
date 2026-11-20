// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const testOVF = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
  xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
  xmlns:vmw="http://www.vmware.com/schema/ovf">
  <References><File ovf:href="d1.vmdk" ovf:id="file1"/><File ovf:href="d2.vmdk" ovf:id="file2"/></References>
  <DiskSection><Disk ovf:diskId="vmdisk1" ovf:fileRef="file1"/><Disk ovf:diskId="vmdisk2" ovf:fileRef="file2"/></DiskSection>
  <VirtualSystem ovf:id="web01-id">
    <Name>Web01</Name>
    <OperatingSystemSection ovf:id="94" vmw:osType="ubuntu64Guest"/>
    <VirtualHardwareSection>
      <Item><rasd:ResourceType>3</rasd:ResourceType><rasd:VirtualQuantity>4</rasd:VirtualQuantity></Item>
      <Item><rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits><rasd:ResourceType>4</rasd:ResourceType><rasd:VirtualQuantity>8192</rasd:VirtualQuantity></Item>
      <Item><rasd:ResourceType>10</rasd:ResourceType></Item>
      <Item><rasd:ResourceType>10</rasd:ResourceType></Item>
      <vmw:Config ovf:required="false" vmw:key="firmware" vmw:value="EFI"/>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

func TestParseOVF(t *testing.T) {
	info, err := parseOVF([]byte(testOVF))
	if err != nil {
		t.Fatal(err)
	}
	want := ovfInfo{Name: "Web01", VCPUs: 4, MemoryMiB: 8192, Firmware: "efi", OS: "ubuntu64Guest", NICs: 2, Disks: 2}
	if info != want {
		t.Fatalf("got %+v\nwant %+v", info, want)
	}
	gib := strings.Replace(testOVF, "byte * 2^20", "byte * 2^30", 1)
	gib = strings.Replace(gib, ">8192<", ">16<", 1)
	if info, _ := parseOVF([]byte(gib)); info.MemoryMiB != 16<<10 {
		t.Fatalf("GiB units: %d MiB", info.MemoryMiB)
	}
	multi := `<Envelope><VirtualSystemCollection><VirtualSystem/></VirtualSystemCollection></Envelope>`
	if _, err := parseOVF([]byte(multi)); err == nil || !strings.Contains(err.Error(), "multi-VM") {
		t.Fatalf("multi-VM OVF: %v", err)
	}
}

func TestScanOVAHashesWholeFile(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"web01.ovf", testOVF}, {"d1.vmdk", strings.Repeat("x", 4096)}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)

	ovf, digest, size, err := scanOVA(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(ovf) != testOVF || digest != "sha256:"+hex.EncodeToString(sum[:]) || size != int64(len(raw)) {
		t.Fatalf("digest %s size %d ovf %d bytes", digest, size, len(ovf))
	}
	if _, _, _, err := scanOVA(bytes.NewReader(make([]byte, 1024))); err == nil {
		t.Fatal("empty tar must fail (no .ovf)")
	}
}
