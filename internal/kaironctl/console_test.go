// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

func TestRenderConsoleFrame(t *testing.T) {
	var buf bytes.Buffer // not a TTY: no ANSI
	info := consoleInfo{Machine: "web-1", Namespace: "prod", Node: "worker-2", Phase: "Running", Backend: "qemu", IP: "10.0.0.7", CPU: "2", Memory: "4Gi", Log: "web.log"}
	out := renderConsoleFrame(&buf, info, 64)
	if strings.Contains(out, "\033[") {
		t.Fatalf("frame has ANSI without a TTY:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	box := lines[:len(lines)-1]
	for _, l := range box {
		if n := utf8.RuneCountInString(l); n != 64 {
			t.Fatalf("line width %d, want 64: %q", n, l)
		}
	}
	for _, want := range []string{"● ● ●", "web-1 — prod", "worker-2", "Running", "qemu", "10.0.0.7", "2 vCPU 4Gi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("frame lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(box[0], "╭") || !strings.HasPrefix(box[len(box)-1], "╰") {
		t.Fatalf("frame corners:\n%s", out)
	}
	if hint := lines[len(lines)-1]; !strings.Contains(hint, "Ctrl+] to detach") || !strings.Contains(hint, "logging to web.log") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestRenderConsoleFrameLongNames(t *testing.T) {
	var buf bytes.Buffer
	info := consoleInfo{Machine: strings.Repeat("m", 80), Namespace: "ns", Node: strings.Repeat("n", 60)}
	for _, l := range strings.Split(strings.TrimRight(renderConsoleFrame(&buf, info, 200), "\n"), "\n")[:7] {
		if n := utf8.RuneCountInString(l); n != 76 {
			t.Fatalf("line width %d, want 76 (capped): %q", n, l)
		}
	}
}

type rw struct {
	io.Reader
	io.Writer
}

func TestRunConsoleSessionDetach(t *testing.T) {
	var sent bytes.Buffer
	guest := rw{Reader: blockingReader{}, Writer: &sent}
	in := strings.NewReader("ls -l\r" + string(consoleEscape) + "ignored")
	why := runConsoleSession(context.Background(), guest, in, io.Discard, consoleEscape)
	if why != endDetached {
		t.Fatalf("end = %v, want detached", why)
	}
	if sent.String() != "ls -l\r" {
		t.Fatalf("sent %q; bytes after Ctrl+] must not reach the guest", sent.String())
	}
}

func TestRunConsoleSessionRemoteClose(t *testing.T) {
	var out bytes.Buffer
	guest := rw{Reader: strings.NewReader("login: "), Writer: io.Discard}
	why := runConsoleSession(context.Background(), guest, blockingReader{}, &out, consoleEscape)
	if why != endRemote || out.String() != "login: " {
		t.Fatalf("end = %v, out = %q", why, out.String())
	}
}

func TestRunConsoleSessionCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	guest := rw{Reader: blockingReader{}, Writer: io.Discard}
	if why := runConsoleSession(ctx, guest, blockingReader{}, io.Discard, consoleEscape); why != endCancelled {
		t.Fatalf("end = %v", why)
	}
}

type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestVNCViewer(t *testing.T) {
	none := func(string) (string, error) { return "", errors.New("missing") }
	if name, args, ok := vncViewer("darwin", "127.0.0.1", 5901, none); !ok || name != "open" || args[0] != "vnc://127.0.0.1:5901" {
		t.Fatalf("darwin: %s %v %v", name, args, ok)
	}
	only := func(want string) func(string) (string, error) {
		return func(n string) (string, error) {
			if n == want {
				return "/usr/bin/" + n, nil
			}
			return "", errors.New("missing")
		}
	}
	if name, args, ok := vncViewer("linux", "127.0.0.1", 5901, only("remote-viewer")); !ok || name != "/usr/bin/remote-viewer" || args[0] != "vnc://127.0.0.1:5901" {
		t.Fatalf("remote-viewer: %s %v", name, args)
	}
	if _, args, ok := vncViewer("linux", "127.0.0.1", 5901, only("vncviewer")); !ok || args[0] != "127.0.0.1::5901" {
		t.Fatalf("vncviewer: %v", args)
	}
	if _, _, ok := vncViewer("linux", "127.0.0.1", 5901, none); ok {
		t.Fatal("expected no viewer")
	}
}

func TestDialConsoleTicketFlow(t *testing.T) {
	var gotKind, gotTicket, gotCols, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines/prod/web-1/console/ticket":
			gotKind = r.URL.Query().Get("kind")
			gotAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"ticket":"t-123"}`)
		case "/api/v1/machines/prod/web-1/console":
			gotTicket, gotCols = r.URL.Query().Get("ticket"), r.URL.Query().Get("cols")
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
			_, _ = nc.Write([]byte("hello"))
			_ = nc.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "secret")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConsole(ctx, "prod", "web-1", "text", 120, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	b, _ := io.ReadAll(conn)
	if string(b) != "hello" {
		t.Fatalf("read %q", b)
	}
	if gotKind != "text" || gotTicket != "t-123" || gotCols != "120" || gotAuth != "Bearer secret" {
		t.Fatalf("kind=%q ticket=%q cols=%q auth=%q", gotKind, gotTicket, gotCols, gotAuth)
	}
	var _ net.Conn = conn
}
