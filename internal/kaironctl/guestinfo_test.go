// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseLinuxOSInfo(t *testing.T) {
	out := "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n@@\nLinux 6.8.0-45-generic\n@@\nx86_64\n@@\nweb-1\n@@\n273601.52\n"
	info := parseLinuxOSInfo(out)
	if info.Name != "Ubuntu 24.04.1 LTS" || info.ID != "ubuntu" || info.Version != "24.04" || info.Kernel != "Linux 6.8.0-45-generic" ||
		info.Arch != "x86_64" || info.Hostname != "web-1" || info.UptimeSeconds != 273601 {
		t.Fatalf("%+v", info)
	}
	if got := formatUptime(info.UptimeSeconds); got != "3d4h0m" {
		t.Fatalf("uptime %q", got)
	}
	if got := formatUptime(42); got != "<1m" {
		t.Fatalf("short uptime %q", got)
	}
}

func TestParseWindowsOSInfo(t *testing.T) {
	info, err := parseWindowsOSInfo(`{"Caption":"Microsoft Windows Server 2022 Standard","Version":"10.0.20348","Arch":"64-bit","Host":"WIN-1","Uptime":3600}`)
	if err != nil || info.Name != "Microsoft Windows Server 2022 Standard" || info.Hostname != "WIN-1" || info.UptimeSeconds != 3600 {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestParseDF(t *testing.T) {
	out := `Filesystem     Type     1024-blocks    Used Available Capacity Mounted on
/dev/vda1      ext4        20480000 8192000  12288000      40% /
tmpfs          tmpfs         512000       0    512000       0% /run
/dev/vdb       xfs        104857600 99614720  5242880      95% /var/lib/my data
`
	list := parseDF(out, false)
	if len(list) != 2 {
		t.Fatalf("pseudo fs not hidden: %+v", list)
	}
	if list[0].MountPoint != "/" || list[0].TotalBytes != 20480000<<10 || list[0].UsedPercent != 40 {
		t.Fatalf("%+v", list[0])
	}
	if list[1].MountPoint != "/var/lib/my data" || list[1].Type != "xfs" {
		t.Fatalf("mount with a space: %+v", list[1])
	}
	if len(parseDF(out, true)) != 3 {
		t.Fatal("--all must keep tmpfs")
	}
	var buf strings.Builder
	writeFSList(&buf, list)
	if !strings.Contains(buf.String(), "■■■■") || !strings.Contains(buf.String(), "95%") {
		t.Fatalf("table:\n%s", buf.String())
	}
}

func TestParseWindowsFS(t *testing.T) {
	one, err := parseWindowsFS(`{"DeviceID":"C:","FileSystem":"NTFS","Size":100,"FreeSpace":25}`)
	if err != nil || len(one) != 1 || one[0].UsedPercent != 75 || one[0].MountPoint != `C:\` {
		t.Fatalf("%+v %v", one, err)
	}
	two, err := parseWindowsFS(`[{"DeviceID":"C:","Size":10,"FreeSpace":5},{"DeviceID":"D:","Size":10,"FreeSpace":10}]`)
	if err != nil || len(two) != 2 || two[1].UsedPercent != 0 {
		t.Fatalf("%+v %v", two, err)
	}
}

func TestParseWho(t *testing.T) {
	list := parseWho("alice    pts/0        2026-10-14 06:01 (10.0.0.5)\nroot     tty1         2026-10-13 22:10\n")
	if len(list) != 2 || list[0].User != "alice" || list[0].From != "10.0.0.5" || list[0].Since != "2026-10-14 06:01" || list[1].From != "" {
		t.Fatalf("%+v", list)
	}
}

func TestGuestOSInfoFallsBackToPowerShell(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if b["powershell"] != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": `{"Caption":"Windows 11","Version":"10.0","Arch":"64-bit","Host":"W","Uptime":60}`})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 1, "stderr": "no /bin/sh"})
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "tok")

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	root := NewRootCmd(nil)
	root.SetArgs([]string{"guest", "osinfo", "win-1", "-o", "json"})
	err := root.Execute()
	_ = w.Close()
	os.Stdout = stdout
	got, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0]["path"] != "/bin/sh" {
		t.Fatalf("exec calls %v", bodies)
	}
	var info GuestOSInfo
	if err := json.Unmarshal(got, &info); err != nil || info.Name != "Windows 11" || info.ID != "windows" {
		t.Fatalf("output %s: %v", got, err)
	}
}
