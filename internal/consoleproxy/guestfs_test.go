// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestHandleGuestfs(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "guestfish")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"args: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := guestfishBin
	t.Cleanup(func() { guestfishBin = orig })

	status := "running"
	flux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fluxvm.Record{IDValue: "vm-1", Status: status, Disk: "/var/lib/fluxvm/vm-1/disk.qcow2"})
	}))
	defer flux.Close()
	s := &Server{Flux: fluxvm.New(flux.URL, ""), Token: "t"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(query string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/guestfs/vm-1"+query, nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}

	guestfishBin = filepath.Join(dir, "missing")
	if resp := get(""); resp.StatusCode != http.StatusNotImplemented { //nolint:bodyclose // helper closes the body
		t.Fatalf("missing guestfish: %d", resp.StatusCode)
	}
	guestfishBin = fake
	if resp := get("?rw=1"); resp.StatusCode != http.StatusConflict { //nolint:bodyclose // helper closes the body
		t.Fatalf("rw on a running VM: %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/guestfs/vm-1", //nolint:bodyclose // websocket upgrade response body is hijacked
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer t"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	out, _ := io.ReadAll(websocket.NetConn(ctx, conn, websocket.MessageBinary))
	if !strings.Contains(string(out), "args: --ro -a /var/lib/fluxvm/vm-1/disk.qcow2 -i") {
		t.Fatalf("output %q", out)
	}
}
