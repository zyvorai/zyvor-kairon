// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

// guestRun runs a command through the guest agent (kairon-ui's exec route)
// and returns its result; a non-zero exit is not an error here.
func guestRun(ctx context.Context, ns, name string, body map[string]any) (execResult, error) {
	var res execResult
	if _, ok := body["timeoutSeconds"]; !ok {
		body["timeoutSeconds"] = 30
	}
	raw, err := uiRaw(ctx, http.MethodPost, machinePath(ns, name, "exec"), body)
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(raw, &res)
	return res, err
}

// guestScript runs a POSIX shell script in the guest, falling back to a
// PowerShell script (Windows guests) when the shell is missing or fails.
// Either script may be empty to skip that platform.
func guestScript(ctx context.Context, ns, name, sh, ps string) (out string, windows bool, err error) {
	var firstErr error
	if sh != "" {
		res, err := guestRun(ctx, ns, name, map[string]any{"path": "/bin/sh", "args": []string{"-c", sh}})
		if err == nil && res.ExitCode == 0 {
			return res.Stdout, false, nil
		}
		if err == nil {
			err = fmt.Errorf("guest exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		firstErr = err
	}
	if ps != "" {
		res, err := guestRun(ctx, ns, name, map[string]any{"powershell": ps})
		if err == nil && res.ExitCode == 0 {
			return res.Stdout, true, nil
		}
		if firstErr == nil {
			if err == nil {
				err = fmt.Errorf("guest exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			firstErr = err
		}
	}
	return "", false, firstErr
}

// GuestOSInfo is what `guest osinfo` reports.
type GuestOSInfo struct {
	Name          string `json:"name"`
	ID            string `json:"id,omitempty"`
	Version       string `json:"version,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	UptimeSeconds int64  `json:"uptimeSeconds,omitempty"`
}

const osInfoSh = `cat /etc/os-release 2>/dev/null; echo @@; uname -sr; echo @@; uname -m; echo @@; hostname; echo @@; cut -d' ' -f1 /proc/uptime 2>/dev/null`

const osInfoPS = `$o=Get-CimInstance Win32_OperatingSystem; [pscustomobject]@{Caption=$o.Caption;Version=$o.Version;Arch=$o.OSArchitecture;Host=$o.CSName;Uptime=[int64]((Get-Date)-$o.LastBootUpTime).TotalSeconds} | ConvertTo-Json -Compress`

func parseLinuxOSInfo(out string) GuestOSInfo {
	parts := strings.Split(out, "@@\n")
	for len(parts) < 5 {
		parts = append(parts, "")
	}
	rel := map[string]string{}
	for _, line := range strings.Split(parts[0], "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			rel[k] = strings.Trim(v, `"'`)
		}
	}
	info := GuestOSInfo{
		Name:     rel["PRETTY_NAME"],
		ID:       rel["ID"],
		Version:  rel["VERSION_ID"],
		Kernel:   strings.TrimSpace(parts[1]),
		Arch:     strings.TrimSpace(parts[2]),
		Hostname: strings.TrimSpace(parts[3]),
	}
	if info.Name == "" {
		info.Name = strings.TrimSpace(rel["NAME"] + " " + rel["VERSION"])
	}
	if up, err := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64); err == nil {
		info.UptimeSeconds = int64(up)
	}
	return info
}

func parseWindowsOSInfo(out string) (GuestOSInfo, error) {
	var w struct {
		Caption, Version, Arch, Host string
		Uptime                       int64
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &w); err != nil {
		return GuestOSInfo{}, fmt.Errorf("decode Windows OS info: %w", err)
	}
	return GuestOSInfo{Name: w.Caption, ID: "windows", Version: w.Version, Kernel: "Windows NT " + w.Version, Arch: w.Arch, Hostname: w.Host, UptimeSeconds: w.Uptime}, nil
}

// GuestFS is one mounted filesystem in `guest fslist`.
type GuestFS struct {
	Device      string `json:"device"`
	Type        string `json:"type"`
	MountPoint  string `json:"mountPoint"`
	TotalBytes  int64  `json:"totalBytes"`
	UsedBytes   int64  `json:"usedBytes"`
	AvailBytes  int64  `json:"availableBytes"`
	UsedPercent int    `json:"usedPercent"`
}

const fsListPS = `Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3' | Select-Object DeviceID,FileSystem,Size,FreeSpace | ConvertTo-Json -Compress`

var pseudoFS = map[string]bool{"tmpfs": true, "devtmpfs": true, "squashfs": true, "overlay": true, "efivarfs": true, "ramfs": true}

// parseDF parses `df -PTk`. Without all, pseudo filesystems are dropped.
func parseDF(out string, all bool) []GuestFS {
	var list []GuestFS
	sc := bufio.NewScanner(strings.NewReader(out))
	first := true
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if first {
			first = false
			if len(f) > 0 && f[0] == "Filesystem" {
				continue
			}
		}
		if len(f) < 7 {
			continue
		}
		if !all && pseudoFS[f[1]] {
			continue
		}
		total, _ := strconv.ParseInt(f[2], 10, 64)
		used, _ := strconv.ParseInt(f[3], 10, 64)
		avail, _ := strconv.ParseInt(f[4], 10, 64)
		pct, _ := strconv.Atoi(strings.TrimSuffix(f[5], "%"))
		list = append(list, GuestFS{Device: f[0], Type: f[1], MountPoint: strings.Join(f[6:], " "),
			TotalBytes: total << 10, UsedBytes: used << 10, AvailBytes: avail << 10, UsedPercent: pct})
	}
	return list
}

func parseWindowsFS(out string) ([]GuestFS, error) {
	type disk struct {
		DeviceID, FileSystem string
		Size, FreeSpace      int64
	}
	out = strings.TrimSpace(out)
	var disks []disk
	if strings.HasPrefix(out, "{") {
		var d disk
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			return nil, err
		}
		disks = []disk{d}
	} else if err := json.Unmarshal([]byte(out), &disks); err != nil {
		return nil, fmt.Errorf("decode Windows disks: %w", err)
	}
	list := make([]GuestFS, 0, len(disks))
	for _, d := range disks {
		used := d.Size - d.FreeSpace
		pct := 0
		if d.Size > 0 {
			pct = int((used*100 + d.Size - 1) / d.Size)
		}
		list = append(list, GuestFS{Device: d.DeviceID, Type: d.FileSystem, MountPoint: d.DeviceID + `\`, TotalBytes: d.Size, UsedBytes: used, AvailBytes: d.FreeSpace, UsedPercent: pct})
	}
	return list, nil
}

// GuestUser is one login session in `guest userlist`.
type GuestUser struct {
	User     string `json:"user"`
	Terminal string `json:"terminal,omitempty"`
	Since    string `json:"since,omitempty"`
	From     string `json:"from,omitempty"`
}

const userListPS = `(Get-CimInstance Win32_ComputerSystem).UserName`

// parseWho parses who(1): "alice pts/0 2026-10-14 06:01 (10.0.0.5)".
func parseWho(out string) []GuestUser {
	var list []GuestUser
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		u := GuestUser{User: f[0], Terminal: f[1]}
		rest := f[2:]
		if n := len(rest); n > 0 && strings.HasPrefix(rest[n-1], "(") {
			u.From = strings.Trim(rest[n-1], "()")
			rest = rest[:n-1]
		}
		u.Since = strings.Join(rest, " ")
		list = append(list, u)
	}
	return list
}

func printStructured(w io.Writer, v any, format string) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return printRawStructured(w, raw, format)
}

func checkOutput(format string) error {
	switch format {
	case "", "json", "yaml":
		return nil
	}
	return fmt.Errorf("unknown output format %q (want json or yaml)", format)
}

func formatUptime(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	d := time.Duration(sec) * time.Second
	days := int64(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	s := (d.Truncate(time.Minute)).String()
	s = strings.TrimSuffix(s, "0s")
	if days > 0 {
		return fmt.Sprintf("%dd%s", days, s)
	}
	if s == "" {
		return "<1m"
	}
	return s
}

func writeOSInfo(w io.Writer, info GuestOSInfo) {
	row := func(k, v, c string) {
		fmt.Fprintf(w, "  %s %s\n", style.Paint(w, style.Grey, fmt.Sprintf("%-9s", k)), style.Paint(w, c, orDash(v)))
	}
	row("OS", info.Name, style.White)
	row("Kernel", info.Kernel, style.SoftCyan)
	row("Arch", info.Arch, style.SoftYellow)
	row("Hostname", info.Hostname, style.SoftGreen)
	row("Uptime", formatUptime(info.UptimeSeconds), style.White)
}

func usageBar(w io.Writer, pct int) string {
	const cells = 10
	filled := min(cells, max(0, (pct*cells+50)/100))
	color := style.SoftGreen
	switch {
	case pct >= 90:
		color = style.BrightRed
	case pct >= 75:
		color = style.SoftYellow
	}
	return style.Paint(w, color, strings.Repeat("■", filled)) + style.Paint(w, style.DarkGrey, strings.Repeat("·", cells-filled))
}

func writeFSList(w io.Writer, list []GuestFS) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No filesystems reported.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MOUNT\tTYPE\tSIZE\tUSED\tAVAIL\tUSE%\t\tDEVICE")
	for _, f := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d%%\t%s\t%s\n", f.MountPoint, f.Type, humanBytes(uint64(f.TotalBytes)), humanBytes(uint64(f.UsedBytes)),
			humanBytes(uint64(f.AvailBytes)), f.UsedPercent, usageBar(w, f.UsedPercent), f.Device)
	}
	_ = tw.Flush()
}

func writeUserList(w io.Writer, list []GuestUser) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No users logged in.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tTERMINAL\tSINCE\tFROM")
	for _, u := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.User, orDash(u.Terminal), orDash(u.Since), orDash(u.From))
	}
	_ = tw.Flush()
}

func newGuestInfoCmds(opts *Options) []*cobra.Command {
	var osOut, fsOut, userOut string
	var all bool
	osinfo := &cobra.Command{
		Use:     "osinfo MACHINE",
		Aliases: []string{"guestosinfo"},
		Short:   "Show the guest OS, kernel, hostname and uptime (guest agent)",
		Long: `Like virtctl guestosinfo. Reads /etc/os-release, uname and /proc/uptime through the
guest agent, or Win32_OperatingSystem on a Windows guest. Needs
spec.guestAgent.enabled and an administrator KAIRON_UI_TOKEN.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(osOut); err != nil {
				return err
			}
			out, windows, err := guestScript(cmd.Context(), opts.Namespace, args[0], osInfoSh, osInfoPS)
			if err != nil {
				return err
			}
			info := parseLinuxOSInfo(out)
			if windows {
				if info, err = parseWindowsOSInfo(out); err != nil {
					return err
				}
			}
			if osOut != "" {
				return printStructured(os.Stdout, info, osOut)
			}
			writeOSInfo(os.Stdout, info)
			return nil
		},
	}
	osinfo.Flags().StringVarP(&osOut, "output", "o", "", "json or yaml")

	fslist := &cobra.Command{
		Use:     "fslist MACHINE",
		Aliases: []string{"df"},
		Short:   "List the guest's mounted filesystems and their usage (guest agent)",
		Long: `Like virtctl fslist. Runs df -PTk in the guest (Win32_LogicalDisk on Windows).
Pseudo filesystems (tmpfs, overlay, squashfs, ...) are hidden unless --all.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(fsOut); err != nil {
				return err
			}
			out, windows, err := guestScript(cmd.Context(), opts.Namespace, args[0], "df -PTk", fsListPS)
			if err != nil {
				return err
			}
			list := parseDF(out, all)
			if windows {
				if list, err = parseWindowsFS(out); err != nil {
					return err
				}
			}
			if fsOut != "" {
				return printStructured(os.Stdout, list, fsOut)
			}
			writeFSList(os.Stdout, list)
			return nil
		},
	}
	fslist.Flags().StringVarP(&fsOut, "output", "o", "", "json or yaml")
	fslist.Flags().BoolVar(&all, "all", false, "include pseudo filesystems")

	userlist := &cobra.Command{
		Use:     "userlist MACHINE",
		Aliases: []string{"who"},
		Short:   "List users logged in to the guest (guest agent)",
		Long:    `Like virtctl userlist. Runs who(1) in the guest; on Windows reports the console user.`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(userOut); err != nil {
				return err
			}
			out, windows, err := guestScript(cmd.Context(), opts.Namespace, args[0], "who", userListPS)
			if err != nil {
				return err
			}
			list := parseWho(out)
			if windows {
				list = nil
				if u := strings.TrimSpace(out); u != "" {
					list = []GuestUser{{User: u, Terminal: "console"}}
				}
			}
			if userOut != "" {
				if list == nil {
					list = []GuestUser{}
				}
				return printStructured(os.Stdout, list, userOut)
			}
			writeUserList(os.Stdout, list)
			return nil
		},
	}
	userlist.Flags().StringVarP(&userOut, "output", "o", "", "json or yaml")
	userlist.ValidArgsFunction = completeMachineNames(&opts.Namespace)
	return []*cobra.Command{osinfo, fslist, userlist}
}
