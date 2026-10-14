// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func mustIssueConsoleTicket(t *testing.T, s *Server, ctx context.Context, username, namespace, name, kind string) string {
	t.Helper()
	ticket, err := s.issueConsoleTicket(ctx, username, namespace, name, kind)
	if err != nil {
		t.Fatalf("issueConsoleTicket: %v", err)
	}
	return ticket
}

func mustKubeClientAt(t *testing.T, u string) *kube.Client {
	t.Helper()
	kc, err := kube.New(u, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	return kc
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

func TestConsoleTicketIsSingleUse(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	ticket := mustIssueConsoleTicket(t, s, ctx, "alice", "default", "vm1", "vnc")
	username, namespace, name, _, ok := s.consumeConsoleTicket(ctx, ticket)
	if !ok {
		t.Fatal("expected a freshly issued ticket to be valid")
	}
	if username != "alice" {
		t.Fatalf("expected ticket to be bound to alice, got %q", username)
	}
	if namespace != "default" || name != "vm1" {
		t.Fatalf("expected ticket to be bound to default/vm1, got %q/%q", namespace, name)
	}
	if _, _, _, _, ok := s.consumeConsoleTicket(ctx, ticket); ok {
		t.Fatal("expected a ticket to be rejected the second time it's presented")
	}
}

func TestConsoleTicketRejectsExpired(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	ticket := mustIssueConsoleTicket(t, s, ctx, "alice", "default", "vm1", "vnc")
	// Overwrite with an already-expired timestamp rather than sleeping
	// past the real (30s) TTL.
	s.consoleTickets.Store(sha256Hex(ticket), consoleTicketState{username: "alice", namespace: "default", name: "vm1", expires: time.Now().Add(-time.Second)})
	if _, _, _, _, ok := s.consumeConsoleTicket(ctx, ticket); ok {
		t.Fatal("expected an expired ticket to be rejected")
	}
}

func TestConsoleTicketRejectsUnknownOrEmpty(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	if _, _, _, _, ok := s.consumeConsoleTicket(ctx, ""); ok {
		t.Fatal("expected an empty ticket to be rejected")
	}
	if _, _, _, _, ok := s.consumeConsoleTicket(ctx, "never-issued"); ok {
		t.Fatal("expected an unissued ticket to be rejected")
	}
}

// shortTempDir mirrors internal/consoleproxy's helper: AF_UNIX socket
// paths are capped at ~104 bytes on macOS, well under what t.TempDir()'s
// test-name-based nesting produces.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cu")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listenEchoVNCSocket(t *testing.T, dir string) {
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

// TestHandleConsoleFullRelay exercises the entire chain this feature
// depends on: browser -> kairon-ui's handleConsole -> a real
// internal/consoleproxy.Server (standing in for kairon-node) -> a real
// Unix socket standing in for QEMU's VNC server. Only the noVNC-in-browser
// leg is out of reach of a Go test.
func TestHandleConsoleTicketIssuesAUsableTicket(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL)}
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Ticket == "" {
		t.Fatal("expected a non-empty ticket")
	}
	if _, _, _, _, ok := s.consumeConsoleTicket(context.Background(), out.Ticket); !ok {
		t.Fatal("expected the issued ticket to be consumable")
	}
}

func TestHandleConsoleTicketBindsToTheAuthenticatedUsername(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}},
		SessionSecret: []byte("test-session-secret"),
	}
	h := s.Handler()

	loginRR := login(t, h, "alice", "correct-horse")
	var loginOut struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(loginRR.Body.Bytes(), &loginOut); err != nil {
		t.Fatalf("decode login response: %v", err)
	}

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket", loginOut.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var ticketOut struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ticketOut); err != nil {
		t.Fatalf("decode: %v", err)
	}
	username, _, _, _, ok := s.consumeConsoleTicket(context.Background(), ticketOut.Ticket)
	if !ok {
		t.Fatal("expected the issued ticket to be consumable")
	}
	if username != "alice" {
		t.Fatalf("expected the ticket to be bound to the authenticated user alice, got %q", username)
	}
}

func TestConsoleAuthorizedUnsetAnnotationAllowsAll(t *testing.T) {
	s := &Server{}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	if !s.consoleAuthorized(context.Background(), m, "") {
		t.Fatal("expected an unset console-allowed-users annotation to allow even an empty (unauthenticated) username")
	}
	if !s.consoleAuthorized(context.Background(), m, "anyone") {
		t.Fatal("expected an unset console-allowed-users annotation to allow any authenticated username")
	}
}

func TestConsoleAuthorizedRestrictsToAllowlist(t *testing.T) {
	s := &Server{}
	m := model.Machine{Metadata: model.ObjectMeta{
		Name: "vm1", Namespace: "default",
		Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "alice, bob"},
	}}
	if !s.consoleAuthorized(context.Background(), m, "alice") {
		t.Fatal("expected alice to be authorized (listed)")
	}
	if !s.consoleAuthorized(context.Background(), m, "bob") {
		t.Fatal("expected bob to be authorized (listed, with surrounding whitespace trimmed)")
	}
	if s.consoleAuthorized(context.Background(), m, "carol") {
		t.Fatal("expected carol to be denied (not listed)")
	}
}

func TestConsoleAuthorizedDeniesEmptyUsernameWhenRestricted(t *testing.T) {
	s := &Server{}
	m := model.Machine{Metadata: model.ObjectMeta{
		Name: "vm1", Namespace: "default",
		Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "alice"},
	}}
	if s.consoleAuthorized(context.Background(), m, "") {
		t.Fatal("expected an empty (legacy shared-token or dev-mode) username to be denied once a Machine restricts console access")
	}
}

func TestConsoleAuthorizedAdminBypassesAllowlist(t *testing.T) {
	s := &Server{Users: []User{{Username: "root", IsAdmin: true}}}
	m := model.Machine{Metadata: model.ObjectMeta{
		Name: "vm1", Namespace: "default",
		Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "alice"},
	}}
	if !s.consoleAuthorized(context.Background(), m, "root") {
		t.Fatal("expected an admin account to bypass the console allowlist for break-glass access")
	}
}

func TestConsoleAuthorizedRBACCheckOffByDefaultUnaffected(t *testing.T) {
	fk := newFakeKube() // fk.sar is nil -- a call would fail the test loudly
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL)}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	if !s.consoleAuthorized(context.Background(), m, "alice") {
		t.Fatal("expected RBACConsoleCheck false (the default) to leave annotation-only behavior unchanged")
	}
}

func TestConsoleAuthorizedRBACCheckAllowed(t *testing.T) {
	fk := newFakeKube()
	fk.sar = func(sar model.SubjectAccessReview) model.SubjectAccessReviewStatus {
		if sar.Spec.User != "alice" || sar.Spec.ResourceAttributes.Subresource != "console" {
			t.Fatalf("unexpected SubjectAccessReview: %+v", sar.Spec)
		}
		return model.SubjectAccessReviewStatus{Allowed: true}
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), RBACConsoleCheck: true}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	if !s.consoleAuthorized(context.Background(), m, "alice") {
		t.Fatal("expected a SubjectAccessReview Allowed=true with no annotation set to grant access")
	}
}

func TestConsoleAuthorizedRBACCheckDenied(t *testing.T) {
	fk := newFakeKube()
	fk.sar = func(model.SubjectAccessReview) model.SubjectAccessReviewStatus {
		return model.SubjectAccessReviewStatus{Allowed: false}
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), RBACConsoleCheck: true}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	if s.consoleAuthorized(context.Background(), m, "alice") {
		t.Fatal("expected a SubjectAccessReview Allowed=false to deny access even with no annotation set")
	}
}

func TestConsoleAuthorizedRBACCheckErrorFailsClosed(t *testing.T) {
	fk := newFakeKube() // fk.sar left nil -- the endpoint itself errors
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), RBACConsoleCheck: true}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	if s.consoleAuthorized(context.Background(), m, "alice") {
		t.Fatal("expected a SubjectAccessReview call failure to fail closed")
	}
}

func TestConsoleAuthorizedRBACCheckDeniesEmptyUsernameWithoutCallingOut(t *testing.T) {
	fk := newFakeKube() // fk.sar left nil -- must not be called for an empty username
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), RBACConsoleCheck: true}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	if s.consoleAuthorized(context.Background(), m, "") {
		t.Fatal("expected an empty username to be denied without a SubjectAccessReview call")
	}
}

func TestConsoleAuthorizedRBACCheckAndAllowlistBothRequired(t *testing.T) {
	fk := newFakeKube()
	fk.sar = func(model.SubjectAccessReview) model.SubjectAccessReviewStatus {
		return model.SubjectAccessReviewStatus{Allowed: true}
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), RBACConsoleCheck: true}
	m := model.Machine{Metadata: model.ObjectMeta{
		Name: "vm1", Namespace: "default",
		Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "someone-else"},
	}}
	if s.consoleAuthorized(context.Background(), m, "alice") {
		t.Fatal("expected RBAC-allowed but allowlist-excluded to still deny -- both checks must pass when both are configured")
	}
}

func TestHandleConsoleTicketDeniesUserNotInAllowlist(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{
			Name: "vm1", Namespace: "default",
			Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "bob"},
		},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}},
		SessionSecret: []byte("test-session-secret"),
	}
	h := s.Handler()

	loginRR := login(t, h, "alice", "correct-horse")
	var loginOut struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(loginRR.Body.Bytes(), &loginOut); err != nil {
		t.Fatalf("decode login response: %v", err)
	}

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket", loginOut.Token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a user not on the console allowlist, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleConsoleRejectsTicketIssuedForADifferentMachine(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.machines["vm2"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm2", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-2"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc, ConsoleToken: "x", ConsolePort: "8090"}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	// Ticket is bound to vm1 at issuance, then replayed against vm2's
	// console endpoint -- must be rejected even though the ticket itself
	// is otherwise still valid and unconsumed.
	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm2/console?ticket=" + ticket
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected a ticket issued for vm1 to be rejected when presented against vm2's console")
	}
}

func TestHandleConfigReflectsConsoleState(t *testing.T) {
	s := &Server{Kube: mustKubeClientAt(t, "http://127.0.0.1:0")}
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/config", "", nil)
	var cfg struct {
		ConsoleEnabled bool `json:"consoleEnabled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.ConsoleEnabled {
		t.Fatal("expected consoleEnabled=false when ConsoleToken/ConsolePort aren't set")
	}

	s.ConsoleToken, s.ConsolePort = "x", "8090"
	rr = doJSON(t, h, http.MethodGet, "/api/v1/config", "", nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.ConsoleEnabled {
		t.Fatal("expected consoleEnabled=true once ConsoleToken/ConsolePort are set")
	}
}

func TestHandleConsoleFullRelay(t *testing.T) {
	workspace := shortTempDir(t)
	listenEchoVNCSocket(t, workspace)

	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "runtime-1", "status": "Running", "workspace": workspace})
	}))
	defer fluxSrv.Close()

	const consoleToken = "shared-node-console-token"
	nodeConsole := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: consoleToken}
	nodeSrv := httptest.NewServer(nodeConsole.Handler())
	defer nodeSrv.Close()
	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "qemu"}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}

	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc, ConsoleToken: consoleToken, ConsolePort: nodePort}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	conn, err := dialWS(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial console: %v", err)
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
		t.Fatalf("expected the byte round trip through both relay hops, got %q", data)
	}
}

// TestHandleConsoleFullRelayOverTLS is TestHandleConsoleFullRelay's TLS
// twin: proves kairon-ui's ConsoleTLS config (built from a CA in
// cmd/kairon-ui's consoleTLSConfig) actually verifies and interoperates
// with a real TLS-serving consoleproxy.Server, not just that both sides
// compile.
func TestHandleConsoleFullRelayOverTLS(t *testing.T) {
	workspace := shortTempDir(t)
	listenEchoVNCSocket(t, workspace)

	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "runtime-1", "status": "Running", "workspace": workspace})
	}))
	defer fluxSrv.Close()

	const consoleToken = "shared-node-console-token"
	nodeConsole := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: consoleToken}
	nodeSrv := httptest.NewUnstartedServer(nodeConsole.Handler())
	nodeSrv.StartTLS()
	defer nodeSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(nodeSrv.Certificate())

	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "qemu"}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}

	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{
		Kube:         kc,
		ConsoleToken: consoleToken,
		ConsolePort:  nodePort,
		ConsoleTLS:   &tls.Config{RootCAs: pool},
	}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	conn, err := dialWS(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial console: %v", err)
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
		t.Fatalf("expected the byte round trip through the TLS relay hop, got %q", data)
	}
}

// TestHandleConsoleTLSRejectsUntrustedCert confirms kairon-ui actually
// verifies kairon-node's certificate rather than just speaking TLS to
// whatever's on the other end -- an empty CertPool must not trust a real,
// otherwise-valid server certificate.
func TestHandleConsoleTLSRejectsUntrustedCert(t *testing.T) {
	workspace := shortTempDir(t)
	listenEchoVNCSocket(t, workspace)

	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "runtime-1", "status": "Running", "workspace": workspace})
	}))
	defer fluxSrv.Close()

	const consoleToken = "shared-node-console-token"
	nodeConsole := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: consoleToken}
	nodeSrv := httptest.NewUnstartedServer(nodeConsole.Handler())
	nodeSrv.StartTLS()
	defer nodeSrv.Close()

	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "qemu"}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}

	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	// An empty pool trusts nothing -- the dial from kairon-ui to
	// kairon-node must fail closed, not fall back to plaintext.
	s := &Server{
		Kube:         kc,
		ConsoleToken: consoleToken,
		ConsolePort:  nodePort,
		ConsoleTLS:   &tls.Config{RootCAs: x509.NewCertPool()},
	}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected the dial to fail when kairon-node's certificate isn't trusted")
	}
}

func TestHandleConsoleRejectsWhenNotRunning(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Stopped"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc, ConsoleToken: "x", ConsolePort: "8090"}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected console on a non-Running machine to fail")
	}
}

func TestHandleConsoleDisabledWhenNotConfigured(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc} // ConsoleToken/ConsolePort left empty
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected console to be refused when ConsoleToken/ConsolePort aren't configured")
	}
}

func TestHandleConsoleRejectsNonQemuBackend(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "firecracker"}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc, ConsoleToken: "x", ConsolePort: "8090"}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "vnc")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected console on a non-qemu backend to fail")
	}
}

func TestHandleConsoleTicketRejectsInvalidKind(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL)}
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket?kind=bogus", "", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid kind, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleConsoleTicketGuestfsIsAdminOnly(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube: mustKubeClientAt(t, kubeSrv.URL),
		Users: []User{
			{Username: "alice", PasswordHash: hashFor(t, "correct-horse")},
			{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true},
		},
		SessionSecret: []byte("test-session-secret"),
	}
	h := s.Handler()
	token := func(user string) string {
		var out struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(login(t, h, user, "correct-horse").Body.Bytes(), &out)
		return out.Token
	}
	for _, kind := range []string{"guestfs", "guestfs-rw"} {
		if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket?kind="+kind, token("alice"), nil); rr.Code != http.StatusForbidden {
			t.Fatalf("%s for non-admin: %d %s", kind, rr.Code, rr.Body.String())
		}
		rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket?kind="+kind, token("root"), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s for admin: %d %s", kind, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleConsoleTicketDefaultsToVNCKind(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL)}
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/console/ticket", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, _, _, kind, ok := s.consumeConsoleTicket(context.Background(), out.Ticket)
	if !ok || kind != "vnc" {
		t.Fatalf("expected a default kind of \"vnc\", got kind=%q ok=%v", kind, ok)
	}
}

func TestHandleConsoleRejectsTextConsoleWithoutGuestAgentConsole(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
		// Spec.GuestAgent.Console left false
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc, ConsoleToken: "x", ConsolePort: "8090"}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "text")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket
	if _, err := dialWS(context.Background(), wsURL, nil); err == nil {
		t.Fatal("expected a text-console dial to fail when spec.guestAgent.console is unset")
	}
}

// fakeFluxTextConsole stands in for FluxVM's own GET /v1/vms/{id}/console
// WebSocket-upgrade endpoint -- accepts the upgrade and echoes whatever it
// receives back. Mirrors internal/consoleproxy's own test helper of the
// same shape (different packages, no shared test-only dependency between
// them).
func fakeFluxTextConsole(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestHandleTextConsoleFullRelay exercises the entire chain: kairon-ui's
// handleConsole (kind="text") -> a real internal/consoleproxy.Server
// (standing in for kairon-node) -> a fake FluxVM WebSocket console
// endpoint. Also proves text console works on a non-qemu backend, unlike
// VNC (TestHandleConsoleRejectsNonQemuBackend above).
func TestHandleTextConsoleFullRelay(t *testing.T) {
	fluxSrv := httptest.NewServer(fakeFluxTextConsole(t))
	defer fluxSrv.Close()

	const relayToken = "shared-node-relay-token"
	nodeRelay := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: relayToken}
	nodeSrv := httptest.NewServer(nodeRelay.Handler())
	defer nodeSrv.Close()
	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "firecracker"}, GuestAgent: model.GuestAgentSpec{Console: true}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	kc := mustKubeClientAt(t, kubeSrv.URL)

	s := &Server{Kube: kc, ConsoleToken: relayToken, ConsolePort: nodePort}
	uiSrv := httptest.NewServer(s.Handler())
	defer uiSrv.Close()

	ticket := mustIssueConsoleTicket(t, s, context.Background(), "tester", "default", "vm1", "text")
	wsURL := "ws" + strings.TrimPrefix(uiSrv.URL, "http") + "/api/v1/machines/default/vm1/console?ticket=" + ticket + "&cols=120&rows=40"
	conn, err := dialWS(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial text console: %v", err)
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
