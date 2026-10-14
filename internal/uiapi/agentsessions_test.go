// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	parentSessionID = "11111111-2222-4333-8444-555555555555"
	childSessionID  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

var testPreviewToken = strings.Repeat("c", 64)

// sessionKube is a minimal apiserver for AgentSessions and Nodes.
type sessionKube struct {
	mu       sync.Mutex
	sessions map[string]model.AgentSession
	nodeIP   string
}

func (k *sessionKube) handler() http.Handler {
	const prefix = "/apis/kairon.zyvor.dev/v1/namespaces/default/agentsessions"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
		switch {
		case r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{{
				Metadata: model.ObjectMeta{Name: "mac-1"},
				Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: k.nodeIP}}},
			}}})
		case !strings.HasPrefix(r.URL.Path, prefix):
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		case r.Method == http.MethodGet && name == "":
			list := model.AgentSessionList{}
			for _, s := range k.sessions {
				list.Items = append(list.Items, s)
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet:
			s, ok := k.sessions[name]
			if !ok {
				http.Error(w, `{"reason":"NotFound"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPost:
			var s model.AgentSession
			_ = json.NewDecoder(r.Body).Decode(&s)
			k.sessions[s.Metadata.Name] = s
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodDelete:
			delete(k.sessions, name)
			_, _ = io.WriteString(w, "{}")
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	})
}

type sessionWorld struct {
	kube    *sessionKube
	server  *Server
	handler http.Handler
	admin   string
	op      string
	flux    []*http.Request
}

func newSessionWorld(t *testing.T) *sessionWorld {
	t.Helper()
	world := &sessionWorld{}
	var fluxMu sync.Mutex
	flux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fluxMu.Lock()
		world.flux = append(world.flux, r.Clone(r.Context()))
		fluxMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		base := "/v1/agents/sessions/" + parentSessionID
		switch {
		case r.URL.Path == base+"/exec":
			_, _ = io.WriteString(w, `{"exit_code":0,"stdout":"hi\n","stderr":""}`)
		case r.URL.Path == base+"/fork":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"items":[{"id":"`+childSessionID+`","parent":"`+parentSessionID+`"}],"total_ms":12}`)
		case r.URL.Path == base+"/preview":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"`+testPreviewToken+`","url":"/preview/`+testPreviewToken+`/","port":3000}`)
		case r.URL.Path == base+"/previews":
			_, _ = io.WriteString(w, `{"items":[{"token":"`+testPreviewToken+`","port":3000}]}`)
		case r.URL.Path == base+"/trajectory":
			_, _ = io.WriteString(w, `{"steps":[]}`)
		case strings.HasPrefix(r.URL.Path, base+"/approvals/"), r.URL.Path == base+"/grants":
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		case r.URL.Path == base+"/pty":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_, msg, err := c.Read(r.Context())
			if err == nil {
				_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte("echo:"), msg...))
			}
			_ = c.Close(websocket.StatusNormalClosure, "exit=0")
		case strings.HasPrefix(r.URL.Path, "/preview/"+testPreviewToken+"/"):
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Set-Cookie", "app=1; Path=/")
			_, _ = io.WriteString(w, "<h1>app</h1>")
		default:
			http.Error(w, "bad route "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(flux.Close)

	const relayToken = "node-relay-token"
	node := httptest.NewServer((&consoleproxy.Server{Flux: fluxvm.New(flux.URL, ""), Token: relayToken}).Handler())
	t.Cleanup(node.Close)
	nodeURL, _ := url.Parse(node.URL)
	nodeHost, nodePort, _ := net.SplitHostPort(nodeURL.Host)

	world.kube = &sessionKube{sessions: map[string]model.AgentSession{}, nodeIP: nodeHost}
	world.kube.sessions["work"] = model.AgentSession{
		Metadata: model.ObjectMeta{Name: "work", Namespace: "default"},
		Spec:     model.AgentSessionSpec{Agent: "claude", NodeName: "mac-1"},
		Status:   model.AgentSessionStatus{Phase: model.AgentSessionRunning, NodeName: "mac-1", SessionID: parentSessionID},
	}
	kubeSrv := httptest.NewServer(world.kube.handler())
	t.Cleanup(kubeSrv.Close)

	world.server = &Server{
		Kube: mustKubeClientAt(t, kubeSrv.URL),
		Users: []User{
			{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true},
			{Username: "op", PasswordHash: hashFor(t, "battery-staple")},
		},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken,
		ConsolePort:   nodePort,
	}
	world.handler = world.server.Handler()
	world.admin = loginToken(t, world.handler, "root", "correct-horse")
	world.op = loginToken(t, world.handler, "op", "battery-staple")
	return world
}

func TestAgentSessionExecRelaysToFluxVM(t *testing.T) {
	w := newSessionWorld(t)
	rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/exec", w.admin, map[string]any{"command": "echo hi"})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"stdout":"hi\n"`) {
		t.Fatalf("exec = %d %s", rr.Code, rr.Body.String())
	}
	if got := w.flux[0]; got.URL.Path != "/v1/agents/sessions/"+parentSessionID+"/exec" || got.Header.Get("Cookie") != "" {
		t.Fatalf("FluxVM saw %s", got.URL.Path)
	}
}

func TestAgentSessionVerbsNeedExecGrant(t *testing.T) {
	w := newSessionWorld(t)
	if rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/exec", w.op, map[string]any{"command": "id"}); rr.Code != http.StatusForbidden {
		t.Fatalf("operator without sandboxOperatorExec = %d", rr.Code)
	}
	w.server.SandboxOperatorExec = true
	if rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/exec", w.op, map[string]any{"command": "id"}); rr.Code != http.StatusOK {
		t.Fatalf("operator with sandboxOperatorExec = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAgentSessionApprovalsAndGrantsNeedAdmin(t *testing.T) {
	w := newSessionWorld(t)
	w.server.SandboxOperatorExec = true
	approve := "/api/v1/agentsessions/default/work/approvals/" + childSessionID + "/approve"
	if rr := doJSON(t, w.handler, http.MethodPost, approve, w.op, map[string]any{}); rr.Code != http.StatusForbidden {
		t.Fatalf("operator approving = %d", rr.Code)
	}
	if rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/grants", w.op, map[string]any{"secret_ref": "gh"}); rr.Code != http.StatusForbidden {
		t.Fatalf("operator granting = %d", rr.Code)
	}
	rr := doJSON(t, w.handler, http.MethodPost, approve, w.admin, map[string]any{"note": "looks fine"})
	var got map[string]any
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got["note"] != "by root via kairon-ui: looks fine" {
		t.Fatalf("admin approve = %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/approvals/"+childSessionID+"/deny", w.admin, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"note":"by root via kairon-ui"`) {
		t.Fatalf("admin deny = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAgentSessionPtyWebSocketThroughRelays(t *testing.T) {
	w := newSessionWorld(t)
	ui := httptest.NewServer(w.handler)
	t.Cleanup(ui.Close)
	ctx := t.Context()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ui.URL, "http")+"/api/v1/agentsessions/default/work/pty?cols=80&rows=24",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + w.admin}}})
	if err != nil {
		t.Fatalf("dial pty: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := c.Read(ctx); err != nil || string(msg) != "echo:ls\n" {
		t.Fatalf("pty read = %q, %v", msg, err)
	}
	_, _, err = c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != "exit=0" {
		t.Fatalf("close = %v", err)
	}
}

func TestAgentSessionEndedAllowsOnlyLedgerReads(t *testing.T) {
	w := newSessionWorld(t)
	s := w.kube.sessions["work"]
	s.Status.Phase = model.AgentSessionEnded
	w.kube.sessions["work"] = s
	if rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/exec", w.admin, map[string]any{"command": "id"}); rr.Code != http.StatusConflict {
		t.Fatalf("exec on Ended = %d", rr.Code)
	}
	if rr := doJSON(t, w.handler, http.MethodGet, "/api/v1/agentsessions/default/work/trajectory", w.admin, nil); rr.Code != http.StatusOK {
		t.Fatalf("trajectory on Ended = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAgentSessionForkCreatesChildResources(t *testing.T) {
	w := newSessionWorld(t)
	rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/fork", w.admin, map[string]any{"count": 1})
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork = %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		AgentSessions []string `json:"agentSessions"`
		TotalMs       int      `json:"total_ms"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.AgentSessions) != 1 || out.AgentSessions[0] != "work-aaaaaaaa" || out.TotalMs != 12 {
		t.Fatalf("fork response = %s", rr.Body.String())
	}
	child, ok := w.kube.sessions["work-aaaaaaaa"]
	if !ok {
		t.Fatal("child AgentSession not created")
	}
	if child.Adopted() != childSessionID || child.Spec.NodeName != "mac-1" || child.Spec.Agent != "claude" {
		t.Fatalf("child = %+v", child)
	}
	if child.Metadata.Labels[model.LabelForkedFrom] != "work" || len(child.Metadata.Finalizers) != 1 {
		t.Fatalf("child metadata = %+v", child.Metadata)
	}
}

func TestAgentSessionPreviewURLsRewritten(t *testing.T) {
	w := newSessionWorld(t)
	want := "/preview/default/work/" + testPreviewToken + "/"
	rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions/default/work/preview", w.admin, map[string]any{"port": 3000})
	if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"url":"`+want+`"`) || !strings.Contains(rr.Body.String(), `"served":false`) {
		t.Fatalf("preview = %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, w.handler, http.MethodGet, "/api/v1/agentsessions/default/work/previews", w.admin, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"url":"`+want+`"`) {
		t.Fatalf("previews = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAgentSessionPublicPreview(t *testing.T) {
	w := newSessionWorld(t)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Cookie", "kairon-session=secret")
		rr := httptest.NewRecorder()
		w.handler.ServeHTTP(rr, req)
		return rr
	}
	page := "/preview/default/work/" + testPreviewToken + "/index.html"
	if rr := get(page); rr.Code != http.StatusNotFound {
		t.Fatalf("preview with the flag off = %d", rr.Code)
	}
	w.server.AgentSessionPreviews = true
	rr := get(page)
	if rr.Code != http.StatusOK || rr.Body.String() != "<h1>app</h1>" {
		t.Fatalf("preview = %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Security-Policy") != previewCSP || rr.Header().Get("Set-Cookie") != "" || rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("preview headers = %v", rr.Header())
	}
	last := w.flux[len(w.flux)-1]
	if last.URL.Path != "/preview/"+testPreviewToken+"/index.html" || last.Header.Get("Cookie") != "" {
		t.Fatalf("FluxVM saw %s cookie=%q", last.URL.Path, last.Header.Get("Cookie"))
	}
	if rr := get("/preview/default/work/" + testPreviewToken); rr.Code != http.StatusMovedPermanently {
		t.Fatalf("preview root = %d", rr.Code)
	}
	for _, path := range []string{
		"/preview/default/work/not-a-token/",
		"/preview/default/missing/" + testPreviewToken + "/",
	} {
		if rr := get(path); rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rr.Code)
		}
	}
	s := w.kube.sessions["work"]
	s.Status.Phase = model.AgentSessionEnded
	w.kube.sessions["work"] = s
	if rr := get(page); rr.Code != http.StatusNotFound {
		t.Fatalf("preview of an Ended session = %d", rr.Code)
	}
}

func TestAgentSessionCreate(t *testing.T) {
	w := newSessionWorld(t)
	rr := doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions?namespace=default", w.op,
		map[string]any{"name": "new", "spec": map[string]any{"agent": "codex", "ttlSeconds": 600}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := w.kube.sessions["new"]; !ok {
		t.Fatal("AgentSession not stored")
	}
	rr = doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions?namespace=default", w.op,
		map[string]any{"name": "pinned", "spec": map[string]any{"nodeName": "mac-1"}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("operator pinning a node = %d", rr.Code)
	}
	rr = doJSON(t, w.handler, http.MethodPost, "/api/v1/agentsessions?namespace=default", w.admin,
		map[string]any{"name": "bad", "spec": map[string]any{"agent": "has space"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid spec = %d %s", rr.Code, rr.Body.String())
	}
}
