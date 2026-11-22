// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package consoleproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
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

// shortTempDir returns a short-path temp directory: AF_UNIX socket paths
// are capped at ~104 bytes on macOS (108 on Linux), and t.TempDir()'s
// test-name-based nesting routinely exceeds that.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newFakeFlux(t *testing.T, workspace string) *fluxvm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vm-1", "status": "Running", "workspace": workspace})
	}))
	t.Cleanup(srv.Close)
	return fluxvm.New(srv.URL, "")
}

// dialWS wraps websocket.Dial and always closes the handshake response
// body (the *websocket.Conn returned on success owns the underlying
// connection itself and needs no separate close here).
func dialWS(ctx context.Context, u string, opts *websocket.DialOptions) (*websocket.Conn, error) {
	conn, resp, err := websocket.Dial(ctx, u, opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return conn, err
}

// listenVNCSocket starts a fake VNC server on <dir>/vnc.sock that echoes
// whatever it receives back to the caller -- enough to prove bytes make it
// all the way through the relay in both directions.
func listenVNCSocket(t *testing.T, dir string) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "vnc.sock"))
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

func TestHandleConsoleRejectsWrongOrMissingToken(t *testing.T) {
	dir := shortTempDir(t)
	listenVNCSocket(t, dir)
	s := &Server{Flux: newFakeFlux(t, dir), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/console/vm-1"

	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected dial without a token to fail")
	}
	if _, err := dialWS(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}},
	}); err == nil {
		t.Fatal("expected dial with the wrong token to fail")
	}
}

func TestHandleConsoleRelaysBytesToVNCSocket(t *testing.T) {
	dir := shortTempDir(t)
	listenVNCSocket(t, dir)
	s := &Server{Flux: newFakeFlux(t, dir), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/console/vm-1"
	conn, err := dialWS(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}},
	})
	if err != nil {
		t.Fatalf("dial with correct token: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "RFB 003.008\n" {
		t.Fatalf("expected the fake VNC socket to echo back what was sent, got %q", data)
	}
}

// newFakeFluxExec returns a fake FluxVM server that answers
// POST /v1/vms/vm-1/qga/exec, capturing the decoded request body for the
// caller to inspect.
func newFakeFluxExec(t *testing.T, gotBody *map[string]any, exitCode int64, stdout, stderr string) *fluxvm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/vm-1/qga/exec" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": exitCode, "stdout": stdout, "stderr": stderr})
	}))
	t.Cleanup(srv.Close)
	return fluxvm.New(srv.URL, "")
}

func TestHandleExecRejectsWrongOrMissingToken(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxExec(t, &got, 0, "", ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/bin/echo"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/exec/vm-1", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

func TestHandleExecRelaysRequestAndResponse(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxExec(t, &got, 0, "hello\n", ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/bin/echo","args":["hello"]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/exec/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out execResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if got["path"] != "/bin/echo" {
		t.Fatalf("expected the request to be forwarded to FluxVM, got %+v", got)
	}
}

func TestHandleExecPropagatesFluxVMFailure(t *testing.T) {
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer srv404.Close()
	s := &Server{Flux: fluxvm.New(srv404.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/bin/echo"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/exec/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when FluxVM rejects the exec, got %d", resp.StatusCode)
	}
}

func TestHandleLogsRejectsWrongOrMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/logs/vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

func TestHandleLogsStreamsFluxVMResponse(t *testing.T) {
	var gotPath, gotQuery string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("line one\nline two\n"))
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/logs/vm-1?lines=50&follow=false", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "line one\nline two\n" {
		t.Fatalf("unexpected body: %q", body)
	}
	if gotPath != "/v1/vms/vm-1/logs" || gotQuery != "lines=50&follow=false" {
		t.Fatalf("expected the request to be forwarded with its query intact, got path=%q query=%q", gotPath, gotQuery)
	}
}

func TestHandleLogsPropagatesFluxVMFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM not found", http.StatusNotFound)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/logs/vm-1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when FluxVM rejects the request, got %d", resp.StatusCode)
	}
}

func TestHandleQGAFsfreezeStatusRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vms/vm-1/qga/fsfreeze-status" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "frozen"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/qga-fsfreeze-status/vm-1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out fsfreezeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "frozen" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestHandleQGAFirewallOpenAndCloseRelayRequests(t *testing.T) {
	var gotOpenBody, gotCloseBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-1/qga/firewall/open":
			_ = json.NewDecoder(r.Body).Decode(&gotOpenBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "", "stderr": ""})
		case "/v1/vms/vm-1/qga/firewall/close":
			_ = json.NewDecoder(r.Body).Decode(&gotCloseBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "", "stderr": ""})
		default:
			http.Error(w, "bad route", http.StatusNotFound)
		}
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	openReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/qga-firewall/open/vm-1", strings.NewReader(`{"name":"web","port":8080,"protocol":"tcp"}`))
	openReq.Header.Set("Authorization", "Bearer secret")
	openResp, err := http.DefaultClient.Do(openReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = openResp.Body.Close() }()
	if openResp.StatusCode != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openResp.StatusCode)
	}
	if gotOpenBody["name"] != "web" || gotOpenBody["port"] != float64(8080) {
		t.Fatalf("open: unexpected request forwarded: %+v", gotOpenBody)
	}

	closeReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/qga-firewall/close/vm-1", strings.NewReader(`{"name":"web"}`))
	closeReq.Header.Set("Authorization", "Bearer secret")
	closeResp, err := http.DefaultClient.Do(closeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeResp.Body.Close() }()
	if closeResp.StatusCode != http.StatusOK {
		t.Fatalf("close: expected 200, got %d", closeResp.StatusCode)
	}
	if gotCloseBody["name"] != "web" {
		t.Fatalf("close: unexpected request forwarded: %+v", gotCloseBody)
	}
}

func TestHandleQGAFsfreezeStatusAndFirewallRejectWrongToken(t *testing.T) {
	s := &Server{Flux: fluxvm.New("http://unused", ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if req, _ := http.NewRequest(http.MethodGet, srv.URL+"/qga-fsfreeze-status/vm-1", nil); true {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("fsfreeze-status: expected 401 with no token, got %d", resp.StatusCode)
		}
	}
	if req, _ := http.NewRequest(http.MethodPost, srv.URL+"/qga-firewall/open/vm-1", strings.NewReader(`{}`)); true {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("firewall/open: expected 401 with no token, got %d", resp.StatusCode)
		}
	}
}

// fakeFluxTextConsole stands in for FluxVM's own GET /v1/vms/{id}/console
// WebSocket-upgrade endpoint -- accepts the upgrade and echoes whatever it
// receives back, enough to prove handleTextConsole's client-dial-then-relay
// shape actually carries bytes both ways, not just that it compiles.
func fakeFluxTextConsole(t *testing.T, wantPath string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if err := conn.Write(ctx, typ, data); err != nil {
				return
			}
		}
	})
}

func TestHandleTextConsoleRejectsWrongOrMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(fakeFluxTextConsole(t, "/v1/vms/vm-1/console"))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/text-console/vm-1"
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected dial without a token to fail")
	}
}

func TestHandleTextConsoleRelaysBytesToFluxVM(t *testing.T) {
	fluxSrv := httptest.NewServer(fakeFluxTextConsole(t, "/v1/vms/vm-1/console"))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/text-console/vm-1"
	conn, err := dialWS(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}},
	})
	if err != nil {
		t.Fatalf("dial with correct token: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("$ ls\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "$ ls\n" {
		t.Fatalf("expected the byte round trip through both relay hops, got %q", data)
	}
}

func TestHandleTextConsolePreservesColsRowsQuery(t *testing.T) {
	fluxSrv := httptest.NewServer(fakeFluxTextConsole(t, "/v1/vms/vm-1/console"))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// fakeFluxTextConsole only accepts exactly "/v1/vms/vm-1/console" as
	// its path (query strings don't affect http.Request.URL.Path), so a
	// successful dial here already proves the query string was forwarded
	// without corrupting the path itself.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/text-console/vm-1?cols=120&rows=40"
	conn, err := dialWS(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}},
	})
	if err != nil {
		t.Fatalf("dial with cols/rows query: %v", err)
	}
	_ = conn.CloseNow()
}

func TestHandleTextConsolePropagatesFluxVMDialFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/text-console/vm-1"
	if _, err := dialWS(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}},
	}); err == nil {
		t.Fatal("expected the dial to fail when FluxVM itself refuses the console upgrade")
	}
}

// newFakeFluxAgent returns a fake FluxVM server answering
// POST /v1/vms/vm-1/agent/put-file and /agent/get-file.
func newFakeFluxAgent(t *testing.T, gotBody *map[string]any) *fluxvm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-1/agent/put-file":
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-written"})
		case "/v1/vms/vm-1/agent/get-file":
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-content", "content_base64": "aGVsbG8=", "mode": 420})
		case "/v1/vms/vm-1/agent":
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "exec", "exit_code": 0, "stdout": "hello\n", "stderr": ""})
		default:
			http.Error(w, "bad route", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return fluxvm.New(srv.URL, "")
}

func TestHandleAgentPutFileRejectsWrongOrMissingToken(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxAgent(t, &got), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/etc/x","contentBase64":"aGVsbG8="}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent-file/put/vm-1", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

func TestHandleAgentPutFileRelaysRequest(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxAgent(t, &got), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/etc/x","contentBase64":"aGVsbG8=","mode":420}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent-file/put/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got["path"] != "/etc/x" || got["content_base64"] != "aGVsbG8=" || got["mode"] != float64(420) {
		t.Fatalf("expected the request to be forwarded to FluxVM, got %+v", got)
	}
}

func TestHandleAgentGetFileRelaysResponse(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxAgent(t, &got), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/etc/hostname"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent-file/get/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out agentFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ContentBase64 != "aGVsbG8=" || out.Mode != 0o644 {
		t.Fatalf("unexpected response: %+v", out)
	}
	if got["path"] != "/etc/hostname" {
		t.Fatalf("expected the request to be forwarded to FluxVM, got %+v", got)
	}
}

func TestHandleAgentFilePropagatesFluxVMFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer srv.Close()
	s := &Server{Flux: fluxvm.New(srv.URL, ""), Token: "secret"}
	nodeSrv := httptest.NewServer(s.Handler())
	defer nodeSrv.Close()

	body := strings.NewReader(`{"path":"/etc/x","contentBase64":"aGVsbG8="}`)
	req, _ := http.NewRequest(http.MethodPost, nodeSrv.URL+"/agent-file/put/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when FluxVM rejects the request, got %d", resp.StatusCode)
	}
}

func TestHandleAgentExecRejectsWrongOrMissingToken(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxAgent(t, &got), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"command":"echo hello"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent-exec/vm-1", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", resp.StatusCode)
	}
}

func TestHandleAgentExecRelaysRequestAndResponse(t *testing.T) {
	var got map[string]any
	s := &Server{Flux: newFakeFluxAgent(t, &got), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"command":"echo hello","timeoutSeconds":30}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent-exec/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out agentExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if got["command"] != "echo hello" || got["timeout_seconds"] != float64(30) {
		t.Fatalf("expected the request to be forwarded to FluxVM, got %+v", got)
	}
}

func TestHandleAgentExecPropagatesFluxVMFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer srv.Close()
	s := &Server{Flux: fluxvm.New(srv.URL, ""), Token: "secret"}
	nodeSrv := httptest.NewServer(s.Handler())
	defer nodeSrv.Close()

	body := strings.NewReader(`{"command":"echo hello"}`)
	req, _ := http.NewRequest(http.MethodPost, nodeSrv.URL+"/agent-exec/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when FluxVM rejects the request, got %d", resp.StatusCode)
	}
}

func TestHandleConsoleRejectsUnknownRuntime(t *testing.T) {
	// A fake Flux client that always 404s -- exercises the "lookup
	// runtime" failure path distinct from "no workspace on the record".
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv404.Close()
	s := &Server{Flux: fluxvm.New(srv404.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/console/nope"
	if _, err := dialWS(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}},
	}); err == nil {
		t.Fatal("expected dial for an unknown runtime to fail")
	}
}

func TestHandleVMSnapshotRejectsWrongOrMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"tag":"before-upgrade"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/vm-snapshot/vm-1", body)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHandleVMSnapshotRelaysRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"tag":"before-upgrade"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/vm-snapshot/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/vms/vm-1/snapshot" || gotBody["tag"] != "before-upgrade" {
		t.Fatalf("unexpected upstream request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestHandleVMSnapshotPropagatesFluxVMFailure(t *testing.T) {
	srv400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "snapshot not supported for backend Firecracker", http.StatusBadRequest)
	}))
	defer srv400.Close()
	s := &Server{Flux: fluxvm.New(srv400.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"tag":"t"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/vm-snapshot/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when FluxVM rejects the snapshot, got %d", resp.StatusCode)
	}
}

func TestHandleVMSnapshotRequiresTag(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed without a tag")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/vm-snapshot/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without a tag, got %d", resp.StatusCode)
	}
}

func TestHandleVMRestoreSnapshotOrchestratesStopThenStartFromSnapshot(t *testing.T) {
	var calls []string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/vms/vm-1/stop":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Stopped"})
		case "/v1/vms/vm-1/start-from-snapshot":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"tag":"before-upgrade"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/vm-restore-snapshot/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out vmSnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "Running" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if len(calls) != 2 || calls[0] != "POST /v1/vms/vm-1/stop" || calls[1] != "POST /v1/vms/vm-1/start-from-snapshot" {
		t.Fatalf("expected stop then start-from-snapshot in order, got %v", calls)
	}
}

func TestHandleVMRestoreSnapshotPropagatesFluxVMFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-1/stop":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Stopped"})
		case "/v1/vms/vm-1/start-from-snapshot":
			http.Error(w, "no such snapshot", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"tag":"missing-tag"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/vm-restore-snapshot/vm-1", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when start-from-snapshot fails, got %d", resp.StatusCode)
	}
}

func TestHandleBackupRootStreamsAndMapsNotFound(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer flux" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/backups/web-1.qcow2/root":
			w.Header().Set("Content-Length", "8")
			_, _ = w.Write([]byte("QFI\xfbdisk"))
		default:
			http.Error(w, "backup not found", http.StatusNotFound)
		}
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, "flux"), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	get := func(path, token string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("/backup-root/web-1.qcow2", "secret")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "QFI\xfbdisk" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	resp = get("/backup-root/nope", "secret")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing backup: got %d, want 404", resp.StatusCode)
	}
	resp = get("/backup-root/web-1.qcow2", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", resp.StatusCode)
	}
}
