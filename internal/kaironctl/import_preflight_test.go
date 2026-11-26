// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const doctorJSON = `{
  "target": "kvm",
  "evidence_schema": "1",
  "bootability": {
    "score": 78,
    "confidence": 0.9,
    "target": "kvm",
    "summary": "review virtio before first boot",
    "blockers": [],
    "warnings": [
      {
        "check_id": "BOOT-006",
        "title": "Virtio driver readiness",
        "message": "VMware tools detected but no virtio modules — driver injection may be required",
        "remediation": "enable repair or inject virtio into the initramfs"
      }
    ],
    "checks": [
      {"id": "BOOT-001", "name": "Fstab UUID consistency", "passed": true, "severity": "Info", "message": "All fstab UUID references match partition table", "weight": 15},
      {"id": "BOOT-005", "name": "EFI boot support", "passed": true, "severity": "Info", "message": "Legacy BIOS boot mode (no EFI partition detected)", "weight": 5},
      {"id": "BOOT-006", "name": "Virtio driver readiness", "passed": false, "severity": "Warning", "message": "VMware tools detected but no virtio modules — driver injection may be required", "weight": 15},
      {"id": "BOOT-008", "name": "Cloud-init conflicts", "passed": true, "severity": "Info", "message": "Cloud-init not configured", "weight": 8},
      {"id": "BOOT-011", "name": "Systemd static analysis", "passed": true, "severity": "Info", "message": "No systemd static issues in 40 units", "weight": 8}
    ]
  }
}`

func TestParseImportPreflightArgs(t *testing.T) {
	opts, err := parseImportPreflightArgs([]string{"disk.qcow2", "-o", "json", "--strict", "--firmware", "uefi"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.source != "disk.qcow2" || opts.output != "json" || !opts.strict || opts.firmware != "efi" {
		t.Fatalf("got %+v", opts)
	}
	if _, err := parseImportPreflightArgs(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("empty args: %v", err)
	}
	if _, err := parseImportPreflightArgs([]string{"--from-json", "r.json"}); err != nil {
		t.Fatalf("--from-json without source should be ok: %v", err)
	}
	if _, err := parseImportPreflightArgs([]string{"--firmware", "openfirmware", "d.qcow2"}); err == nil {
		t.Fatal("bad firmware must fail")
	}
	if _, err := parseImportPreflightArgs([]string{"-o", "xml", "d.qcow2"}); err == nil {
		t.Fatal("bad output must fail")
	}
}

func TestImportPreflightDoctorJSON(t *testing.T) {
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: "web.qcow2", fromJSON: "-"}, strings.NewReader(doctorJSON), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Fail != 0 {
		t.Fatalf("unexpected fail: %+v", rep.Checks)
	}
	got := checkMap(rep)
	if got["virtio"] != pfWarn {
		t.Fatalf("virtio: %+v", got)
	}
	if got["fstab"] != pfPass {
		t.Fatalf("fstab: %+v", got)
	}
	if got["cloud-init"] != pfWarn {
		t.Fatalf("cloud-init absence should be warn: %+v", got)
	}
	if got["firmware"] != pfPass {
		t.Fatalf("BIOS firmware note should be pass: %+v", got)
	}
	if got["guestkit"] != pfPass || rep.Score == nil || *rep.Score != 78 {
		t.Fatalf("score: %+v %v", got, rep.Score)
	}
	if _, ok := got["boot-011"]; ok {
		t.Fatalf("passed non-checklist checks must be omitted: %+v", got)
	}
	if rep.Summary.Warn < 1 {
		t.Fatalf("want warnings: %+v", rep)
	}
}

func TestImportPreflightUIFindingsAndInspectFstab(t *testing.T) {
	doc := `{
	  "doctor": {
	    "score": 60,
	    "target": "kvm",
	    "decision": "REVIEW — fix blockers first",
	    "findings": [
	      {"severity": "high", "title": "Missing virtio drivers in initramfs", "detail": "First boot on KVM will hang waiting for root disk.", "fix": "guestkit plan generate disk.qcow2 -p virtio-initramfs"},
	      {"severity": "low", "title": "Filesystem clean", "detail": "ext4 root fsck clean"}
	    ]
	  },
	  "inspect": {
	    "source": "guestkit.inspect",
	    "operating_system": {"product_name": "Ubuntu 22.04", "distro": "ubuntu"},
	    "storage": {"mounts": [
	      {"mount": "/", "device": "/dev/sda1", "fstype": "ext4"},
	      {"mount": "/boot", "device": "UUID=abcd", "fstype": "ext4"}
	    ]},
	    "packages": {"count": 428, "sample": [{"name": "openssh-server", "version": "1"}]}
	  }
	}`
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: "web.qcow2", fromJSON: "-"}, strings.NewReader(doc), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	got := checkMap(rep)
	if got["virtio"] != pfFail {
		t.Fatalf("high virtio finding: %+v", checkByName(rep, "virtio"))
	}
	if got["fstab"] != pfWarn {
		t.Fatalf("inspect /dev/sda1 should warn: %+v", checkByName(rep, "fstab"))
	}
	if _, ok := got["guest-agent"]; ok {
		t.Fatalf("must not invent guest-agent absence from a package sample: %+v", rep.Checks)
	}
}

func TestImportPreflightBootInspectSummary(t *testing.T) {
	doc := `{"os_release":"Ubuntu 22.04","fstab_valid":false,"bootloader":"grub","cloud_init_present":false,"message":"fstab root is missing"}`
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{fromJSON: "-"}, strings.NewReader(doc), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	got := checkMap(rep)
	if got["fstab"] != pfFail || got["cloud-init"] != pfWarn || got["os"] != pfPass {
		t.Fatalf("summary mapping: %+v", got)
	}
}

func TestImportPreflightUnsupportedFormat(t *testing.T) {
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: "disk.vdi", fromJSON: "-"}, strings.NewReader(`{"bootability":{"score":90,"checks":[],"blockers":[],"warnings":[]}}`), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if checkMap(rep)["format"] != pfFail {
		t.Fatalf("vdi must fail format: %+v", rep.Checks)
	}
}

func TestImportPreflightOVFFirmwareAndWindows(t *testing.T) {
	dir := t.TempDir()
	ovfPath := filepath.Join(dir, "win.ovf")
	ovf := strings.ReplaceAll(testOVF, "ubuntu64Guest", "windows9_64Guest")
	if err := os.WriteFile(ovfPath, []byte(ovf), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: ovfPath, firmware: "bios"}, strings.NewReader(""), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	got := checkMap(rep)
	if got["firmware"] != pfFail {
		t.Fatalf("efi vs --firmware bios: %+v", checkByName(rep, "firmware"))
	}
	if got["windows-virtio"] != pfWarn {
		t.Fatalf("windows OVF: %+v", checkByName(rep, "windows-virtio"))
	}
	if got["format"] != pfPass {
		t.Fatalf("ovf format: %+v", got)
	}
	if _, ok := got["guestkit"]; !ok {
		t.Fatalf("OVA/OVF should note that doctor was skipped: %+v", rep.Checks)
	}

	ovaPath := filepath.Join(dir, "web.ova")
	if err := os.WriteFile(ovaPath, makeOVA(testOVF), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err = runImportPreflight(context.Background(), importPreflightOpts{source: ovaPath}, strings.NewReader(""), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	got = checkMap(rep)
	if got["firmware"] != pfWarn || !strings.Contains(checkByName(rep, "firmware").Message, "UEFI") {
		t.Fatalf("efi OVA should warn: %+v", checkByName(rep, "firmware"))
	}
	if got["disks"] != pfWarn {
		t.Fatalf("multi-disk OVA: %+v", checkByName(rep, "disks"))
	}
}

func TestImportPreflightGuestkitMissing(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(img, []byte("qcow"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := importPreflightEnv{
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: img}, nil, env)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByName(rep, "guestkit")
	if c.Status != pfFail || !strings.Contains(c.Message, "guestkit not found") {
		t.Fatalf("missing guestkit: %+v", c)
	}
}

func TestImportPreflightRunsDoctor(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(img, []byte("qcow"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotBin, gotImg string
	env := importPreflightEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/guestkit", nil },
		runDoctor: func(_ context.Context, bin, image string) ([]byte, error) {
			gotBin, gotImg = bin, image
			return []byte(doctorJSON), nil
		},
	}
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: img}, nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if gotBin != "/usr/bin/guestkit" || gotImg != img {
		t.Fatalf("doctor invoked as %s %s", gotBin, gotImg)
	}
	if checkMap(rep)["virtio"] != pfWarn {
		t.Fatalf("doctor findings missing: %+v", rep.Checks)
	}
}

func TestImportPreflightDoctorFailureAndBadJSON(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(img, []byte("qcow"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := importPreflightEnv{
		lookPath:  func(string) (string, error) { return "/usr/bin/guestkit", nil },
		runDoctor: func(context.Context, string, string) ([]byte, error) { return nil, errors.New("boom") },
	}
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: img}, nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if checkMap(rep)["guestkit"] != pfFail {
		t.Fatalf("doctor error: %+v", checkByName(rep, "guestkit"))
	}
	if _, err := runImportPreflight(context.Background(), importPreflightOpts{fromJSON: "-"}, strings.NewReader("not-json"), importPreflightEnv{}); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestImportPreflightWindowsVirtioFromDoctor(t *testing.T) {
	doc := `{
	  "bootability": {
	    "score": 50,
	    "blockers": [{"check_id":"BOOT-006","title":"Virtio driver readiness","message":"Windows guest has no virtio-win drivers"}],
	    "warnings": [],
	    "checks": []
	  }
	}`
	rep, err := runImportPreflight(context.Background(), importPreflightOpts{source: "win.qcow2", fromJSON: "-"}, strings.NewReader(doc), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if checkMap(rep)["windows-virtio"] != pfFail {
		t.Fatalf("windows virtio: %+v", rep.Checks)
	}
}

func TestImportPreflightWriteAndExit(t *testing.T) {
	rep := &importPreflightReport{}
	rep.add(pfWarn, "virtio", "missing modules", "guestkit", "BOOT-006")
	var buf bytes.Buffer
	if err := writeImportPreflight(&buf, rep, "table"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "STATUS") || !strings.Contains(out, "WARN") || !strings.Contains(out, "0 fail, 1 warn, 0 pass") {
		t.Fatalf("table:\n%s", out)
	}
	buf.Reset()
	if err := writeImportPreflight(&buf, rep, "json"); err != nil {
		t.Fatal(err)
	}
	var decoded importPreflightReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil || decoded.Summary.Warn != 1 {
		t.Fatalf("json: %v %s", err, buf.String())
	}
}

func TestLooksLikeWindowsOS(t *testing.T) {
	for _, s := range []string{"windows9_64Guest", "windows2019srv_64Guest", "Windows Server 2022", "winNetLonghorn64Guest"} {
		if !looksLikeWindowsOS(s) {
			t.Errorf("%q should be windows", s)
		}
	}
	for _, s := range []string{"", "ubuntu64Guest", "darwin64Guest", "rhel9_64Guest"} {
		if looksLikeWindowsOS(s) {
			t.Errorf("%q should not be windows", s)
		}
	}
}

func TestClassifyAndFormatHelpers(t *testing.T) {
	if classifyGuestkitIssue("BOOT-001", "", "", false) != "fstab" {
		t.Fatal("BOOT-001")
	}
	if f, ok := sourceFormat("https://files.example.com/web.vmdk"); !ok || f != "vmdk" {
		t.Fatalf("url format: %s %v", f, ok)
	}
	if f, ok := sourceFormat("disk.qcow"); !ok || f != "qcow2" {
		t.Fatalf("qcow: %s %v", f, ok)
	}
	if !isUnstableDiskName("/dev/sdb1") || isUnstableDiskName("UUID=abc") || isUnstableDiskName("/dev/vda1") {
		t.Fatal("unstable disk name")
	}
}

func TestImportPreflightArgsExitCode(t *testing.T) {
	// A warning-only report exits 0; --strict and FAIL exit 1.
	opts, err := parseImportPreflightArgs([]string{"--from-json", "-", "disk.qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := runImportPreflight(context.Background(), opts, strings.NewReader(doctorJSON), importPreflightEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Fail != 0 || rep.Summary.Warn == 0 {
		t.Fatalf("setup: %+v", rep.Summary)
	}
}

func checkMap(rep *importPreflightReport) map[string]string {
	out := map[string]string{}
	for _, c := range rep.Checks {
		// First (usually more severe) status wins only if not already fail.
		if cur, ok := out[c.Check]; ok && (cur == pfFail || (cur == pfWarn && c.Status == pfPass)) {
			continue
		}
		out[c.Check] = c.Status
	}
	return out
}

func checkByName(rep *importPreflightReport, name string) importPreflightCheck {
	for _, c := range rep.Checks {
		if c.Check == name {
			return c
		}
	}
	return importPreflightCheck{}
}

func makeOVA(ovf string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "web01.ovf", Mode: 0o644, Size: int64(len(ovf)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(ovf))
	_ = tw.Close()
	return buf.Bytes()
}
