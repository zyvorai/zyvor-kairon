// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// guestfishBin is a variable so tests can substitute a fake.
var guestfishBin = "guestfish"

// liveRuntimeStatus is every FluxVM status in which the VMM may still have
// the disk open; guestfs --rw refuses all of them.
var liveRuntimeStatus = map[string]bool{
	"": true, "running": true, "started": true, "ready": true, "paused": true,
	"creating": true, "starting": true, "pending": true,
}

func guestfishArgs(disk string, rw bool) []string {
	mode := "--ro"
	if rw {
		mode = "--rw"
	}
	return []string{mode, "-a", disk, "-i"}
}

// handleGuestfs runs guestfish on a runtime's disk under a PTY and relays
// it over a WebSocket. kairon-ui only issues the ticket to admins; --rw
// is refused here too while the VM may still be running.
func (s *Server) handleGuestfs(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	q := r.URL.Query()
	rw := q.Get("rw") == "1"
	bin, err := exec.LookPath(guestfishBin)
	if err != nil {
		http.Error(w, "libguestfs is not installed on this node: guestfish is not on kairon-node's PATH (install libguestfs-tools)", http.StatusNotImplemented)
		return
	}
	rec, err := s.Flux.Get(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("fluxvm get runtime: %v", err), http.StatusBadGateway)
		return
	}
	if rec.Disk == "" {
		http.Error(w, "runtime reports no disk path", http.StatusConflict)
		return
	}
	if rw && liveRuntimeStatus[strings.ToLower(rec.Status)] {
		http.Error(w, fmt.Sprintf("runtime is %q; --rw needs the Machine halted", rec.Status), http.StatusConflict)
		return
	}
	size := &pty.Winsize{Cols: 80, Rows: 24}
	if c, err := strconv.ParseUint(q.Get("cols"), 10, 16); err == nil && c > 0 {
		size.Cols = uint16(c)
	}
	if rr, err := strconv.ParseUint(q.Get("rows"), 10, 16); err == nil && rr > 0 {
		size.Rows = uint16(rr)
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	cmd := exec.CommandContext(r.Context(), bin, guestfishArgs(rec.Disk, rw)...)
	cmd.Env = os.Environ()
	if os.Getenv("LIBGUESTFS_BACKEND") == "" {
		cmd.Env = append(cmd.Env, "LIBGUESTFS_BACKEND=direct")
	}
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "start guestfish")
		return
	}
	defer func() { _ = ptmx.Close() }()
	relay(websocket.NetConn(r.Context(), conn, websocket.MessageBinary), ptmx)
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	_ = conn.Close(websocket.StatusNormalClosure, "")
}
