// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// newNamespaceScopingTestServer builds a Server wired for a real
// per-operator session login against fk, with NamespaceScopingEnabled set
// as requested -- same httptest.Server + fakeKube fixture convention
// every other test in this package uses (see newUserTestServer).
func newNamespaceScopingTestServer(t *testing.T, fk *fakeKube, users []User, oidc *OIDCAuth, scopingEnabled bool) *Server {
	t.Helper()
	srv := httptest.NewServer(fk.handler())
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	return &Server{
		Kube:                    kc,
		Users:                   users,
		SessionSecret:           []byte("test-session-secret"),
		OIDC:                    oidc,
		NamespaceScopingEnabled: scopingEnabled,
	}
}

// loginToken is defined in exec_test.go -- reused here as-is.

// TestNamespaceScopingQueryParamRoute proves the namespaceParam extractor
// wiring: a non-admin, namespace-scoped user is rejected against an
// out-of-scope namespace and allowed against an in-scope one, on a route
// that reads its namespace from the "?namespace=" query string
// (GET /api/v1/machines, handleListMachines).
func TestNamespaceScopingQueryParamRoute(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newNamespaceScopingTestServer(t, fk, []User{
		{Username: "bob", PasswordHash: hashFor(t, "bob-pw"), Namespaces: []string{"default"}},
	}, nil, true)
	h := s.Handler()
	token := loginToken(t, h, "bob", "bob-pw")

	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=default", token, nil); rr.Code != http.StatusOK {
		t.Fatalf("in-scope namespace: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=other", token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope namespace: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if !strings.Contains(body["error"], `"other"`) {
		t.Fatalf("expected the 403 error to name the rejected namespace, got %q", body["error"])
	}
}

// TestNamespaceScopingPathSegmentRoute proves the namespaceFromPath
// extractor wiring: the same allow/deny shape as
// TestNamespaceScopingQueryParamRoute, but on a route that reads its
// namespace from a "{namespace}" path segment
// (DELETE /api/v1/machines/{namespace}/{name}, handleDeleteMachine).
func TestNamespaceScopingPathSegmentRoute(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newNamespaceScopingTestServer(t, fk, []User{
		{Username: "bob", PasswordHash: hashFor(t, "bob-pw"), Namespaces: []string{"default"}},
	}, nil, true)
	h := s.Handler()
	token := loginToken(t, h, "bob", "bob-pw")

	// Out-of-scope path-segment namespace: rejected before ever reaching
	// the fake apiserver (which has no /namespaces/other/... route at
	// all, so a 403 -- not a 404/502 -- proves the rejection happened in
	// requireNamespace, not downstream).
	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/machines/other/db", token, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope namespace: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	_, stillThere := fk.machines["db"]
	fk.mu.Unlock()
	if !stillThere {
		t.Fatal("expected the out-of-scope delete to never reach the fake apiserver")
	}

	// In-scope path-segment namespace: allowed, same 204 every other
	// delete test in this package expects.
	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/machines/default/db", token, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("in-scope namespace: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestNamespaceScopingAdminIsUnrestricted proves isAdminIdentity always
// wins over Namespaces, even when Namespaces is empty (the "no
// namespaces" default) -- an admin account is never scoped.
func TestNamespaceScopingAdminIsUnrestricted(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newNamespaceScopingTestServer(t, fk, []User{
		{Username: "root", PasswordHash: hashFor(t, "root-pw"), IsAdmin: true},
	}, nil, true)
	h := s.Handler()
	token := loginToken(t, h, "root", "root-pw")

	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=default", token, nil); rr.Code != http.StatusOK {
		t.Fatalf("admin against namespace %q: expected 200, got %d: %s", "default", rr.Code, rr.Body.String())
	}
	// The fake apiserver only models the "default" namespace, so a real
	// namespace it doesn't know about 404s downstream -- the point here
	// is only that requireNamespace itself never 403s an admin identity;
	// a 404 (reached the fake apiserver) proves that just as well as a
	// 200 would, and unlike "default" this doesn't require the fixture to
	// model an arbitrary second namespace.
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=some-other-namespace", token, nil); rr.Code == http.StatusForbidden {
		t.Fatalf("admin against an unscoped namespace: expected requireNamespace to never 403 an admin identity, got 403: %s", rr.Body.String())
	}
}

func TestNamespaceScopingLegacySharedTokenDenied(t *testing.T) {
	fk := newFakeKube()
	s := newNamespaceScopingTestServer(t, fk, nil, nil, true)
	s.Token = "shared-secret"
	for _, path := range []string{"/api/v1/machines?namespace=default", "/api/v1/overview", "/api/v1/nodes"} {
		if rr := doJSON(t, s.Handler(), http.MethodGet, path, "shared-secret", nil); rr.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d", path, rr.Code)
		}
	}
}

// TestNamespaceScopingOIDCGroupGrantsNamespace proves
// OIDCAuth.NamespaceGroups' own allow path: a session carrying a group
// mapped to a namespace is allowed against it and rejected against one
// that isn't, mirroring AdminGroups' shape exactly. Builds the session
// token directly via signSession (as TestSessionRejectsExpiredAndTamperedTokens
// already does in this package) rather than driving a full OIDC
// redirect/callback round trip, which needs a real identity provider.
func TestNamespaceScopingOIDCGroupGrantsNamespace(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	sessionSecret := []byte("test-session-secret")
	s := newNamespaceScopingTestServer(t, fk, nil, &OIDCAuth{
		NamespaceGroups: map[string][]string{"team-bob": {"default"}},
	}, true)
	s.SessionSecret = sessionSecret
	h := s.Handler()

	token, _, err := signSession(sessionSecret, "carol", []string{"team-bob"}, sessionTTL)
	if err != nil {
		t.Fatalf("signSession: %v", err)
	}

	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=default", token, nil); rr.Code != http.StatusOK {
		t.Fatalf("in-scope namespace via OIDC group: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=other", token, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope namespace via OIDC group: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestNamespaceScopingDisabledLeavesNonAdminUnrestricted proves
// NamespaceScopingEnabled's own off switch: false (the default, the zero
// value) means a non-admin user with an empty Namespaces list -- which
// would deny everything once scoping is on -- still reaches every
// namespace exactly as kairon-ui behaved before this feature existed.
// This is the single most important behavior this feature must preserve:
// every pre-existing deployment must see byte-for-byte identical behavior
// with the flag unset.
func TestNamespaceScopingDisabledLeavesNonAdminUnrestricted(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newNamespaceScopingTestServer(t, fk, []User{
		{Username: "bob", PasswordHash: hashFor(t, "bob-pw")}, // no Namespaces at all
	}, nil, false) // NamespaceScopingEnabled left false
	h := s.Handler()
	token := loginToken(t, h, "bob", "bob-pw")

	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=default", token, nil); rr.Code != http.StatusOK {
		t.Fatalf("scoping disabled, namespace %q: expected 200, got %d: %s", "default", rr.Code, rr.Body.String())
	}
	// See TestNamespaceScopingAdminIsUnrestricted's own comment on why a
	// non-403 (not necessarily 200) is the right assertion against a
	// namespace the fake apiserver fixture doesn't model.
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=some-other-namespace", token, nil); rr.Code == http.StatusForbidden {
		t.Fatalf("scoping disabled, unmodeled namespace: expected requireNamespace to never 403 with scoping off, got 403: %s", rr.Body.String())
	}
}

// containsIdent reports whether ident occurs in s as a whole identifier
// (bounded by non-identifier characters on both sides, or the start/end
// of s) -- e.g. containsIdent("s.handleListMachines)", "s.handleListMachines")
// is true, but containsIdent("s.handleListMachineSets)", "s.handleListMachines")
// is false, even though the former is a textual prefix of a substring of
// the latter. Used by TestEveryNamespacedRouteIsWrappedByRequireNamespace
// to check a route registration's source text for a specific handler
// function reference without false-positiving on one handler name being a
// substring of an unrelated, longer one.
func containsIdent(s, ident string) bool {
	if ident == "" {
		return false
	}
	isIdentByte := func(b byte) bool {
		return b == '_' || b == '.' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for start := 0; ; {
		idx := strings.Index(s[start:], ident)
		if idx < 0 {
			return false
		}
		idx += start
		before := byte(' ')
		if idx > 0 {
			before = s[idx-1]
		}
		afterIdx := idx + len(ident)
		after := byte(' ')
		if afterIdx < len(s) {
			after = s[afterIdx]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			return true
		}
		start = idx + 1
	}
}

// handlersUsingNamespaceParam statically parses every non-test .go file in
// this package and returns the set of "s.<funcName>" strings for every
// *Server method whose body calls namespaceParam(r) -- the query-param
// namespace extractor. Used by TestEveryNamespacedRouteIsWrappedByRequireNamespace
// to cross-reference Handler()'s own route table against the handlers
// that actually need requireNamespace wrapping, so a future handler that
// starts reading namespaceParam without its route registration being
// updated to match fails this test immediately.
func handlersUsingNamespaceParam(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil {
				return true
			}
			usesNamespaceParam := false
			ast.Inspect(fd.Body, func(n2 ast.Node) bool {
				call, ok := n2.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "namespaceParam" {
					usesNamespaceParam = true
				}
				return true
			})
			if usesNamespaceParam {
				found["s."+fd.Name.Name] = true
			}
			return true
		})
	}
	return found
}

// TestEveryNamespacedRouteIsWrappedByRequireNamespace is a static-analysis
// regression test directly targeting the "missed a call site" failure
// mode this session has already hit twice for RBAC coverage (see
// scripts/check_rbac_coverage.py): it inspects Handler()'s own source
// text in server.go and asserts every api.HandleFunc(...) registration
// whose path carries a literal "{namespace}" segment, and every
// registration whose handler is one handlersUsingNamespaceParam found
// (i.e. reads namespaceParam(r)), is wrapped by s.requireNamespace(...).
// A future route added to Handler() without this wrapper -- the exact
// mistake this test exists to catch -- fails immediately, rather than
// silently reintroducing the cross-namespace authorization gap this
// feature closes.
func TestEveryNamespacedRouteIsWrappedByRequireNamespace(t *testing.T) {
	namespaceParamHandlers := handlersUsingNamespaceParam(t)
	if len(namespaceParamHandlers) == 0 {
		t.Fatal("expected to find at least one handler using namespaceParam(r) -- this test's own static analysis is broken")
	}

	serverSrc, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("ReadFile server.go: %v", err)
	}
	src := string(serverSrc)
	start := strings.Index(src, "func (s *Server) Handler() http.Handler {")
	end := strings.Index(src, "\nfunc routePattern(")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate Handler()'s own body in server.go -- this test's own extraction is broken")
	}
	handlerSrc := src[start:end]

	// Matches one api.HandleFunc("[METHOD ]PATH", REST-OF-LINE) route
	// registration per line -- Handler() registers exactly one route per
	// source line throughout, by convention.
	lineRe := regexp.MustCompile(`(?m)^\s*api\.HandleFunc\("([A-Z]+ )?([^"]+)",\s*(.+)\)\s*$`)
	matches := lineRe.FindAllStringSubmatch(handlerSrc, -1)
	if len(matches) < 60 {
		t.Fatalf("expected to find dozens of api.HandleFunc registrations in Handler(), found %d -- this test's own regex is broken", len(matches))
	}

	pathChecked, handlerChecked := 0, 0
	for _, m := range matches {
		path, rest := m[2], m[3]
		wrapped := strings.Contains(rest, "s.requireNamespace(")

		if strings.Contains(path, "{namespace}") {
			pathChecked++
			if !wrapped {
				t.Errorf("route %q has a {namespace} path segment but its registration is not wrapped by s.requireNamespace: %s", path, strings.TrimSpace(rest))
			}
		}

		for handlerName := range namespaceParamHandlers {
			if containsIdent(rest, handlerName) {
				handlerChecked++
				if !wrapped {
					t.Errorf("route %q is served by %s, which reads namespaceParam(r), but its registration is not wrapped by s.requireNamespace: %s", path, handlerName, strings.TrimSpace(rest))
				}
			}
		}
	}
	if pathChecked == 0 {
		t.Fatal("expected at least one {namespace} path-segment route registration to check -- this test's own path matching is broken")
	}
	if handlerChecked == 0 {
		t.Fatal("expected at least one namespaceParam-handler route registration to check -- this test's own handler cross-referencing is broken")
	}
}

func TestFleetViewerCannotMutate(t *testing.T) {
	s := newNamespaceScopingTestServer(t, newFakeKube(), []User{{Username: "viewer", PasswordHash: hashFor(t, "pw"), Namespaces: []string{"default"}, Role: "viewer"}}, nil, true)
	h := s.Handler()
	token := loginToken(t, h, "viewer", "pw")
	for _, path := range []string{"/api/v1/machines?namespace=default", "/api/v1/fleet/machinetemplateclaims?namespace=default"} {
		rr := doJSON(t, h, http.MethodPost, path, token, map[string]any{})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("viewer %s got %d", path, rr.Code)
		}
	}
	rr := doJSON(t, h, http.MethodGet, "/api/v1/namespaces", token, nil)
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[\"default\"]" {
		t.Fatalf("namespace discovery: %d %s", rr.Code, rr.Body.String())
	}
}

// TestListMachinesAllNamespaces covers ?namespace=_all: every namespace the
// caller may see, never one they may not.
func TestListMachinesAllNamespaces(t *testing.T) {
	fk := newFakeKube()
	fk.machines["a-vm"] = model.Machine{Metadata: model.ObjectMeta{Name: "a-vm", Namespace: "default"}}
	fk.machines["b-vm"] = model.Machine{Metadata: model.ObjectMeta{Name: "b-vm", Namespace: "b"}}
	fk.machines["c-vm"] = model.Machine{Metadata: model.ObjectMeta{Name: "c-vm", Namespace: "c"}}

	names := func(t *testing.T, rr *httptest.ResponseRecorder) []string {
		t.Helper()
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var ms []model.Machine
		if err := json.Unmarshal(rr.Body.Bytes(), &ms); err != nil {
			t.Fatalf("decode: %v: %s", err, rr.Body.String())
		}
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			out = append(out, m.Metadata.Name)
		}
		sortStrings(out)
		return out
	}

	t.Run("scoped user sees only their namespaces", func(t *testing.T) {
		s := newNamespaceScopingTestServer(t, fk, []User{
			{Username: "bob", PasswordHash: hashFor(t, "bob-pw"), Namespaces: []string{"default", "c"}},
		}, nil, true)
		h := s.Handler()
		token := loginToken(t, h, "bob", "bob-pw")
		got := names(t, doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=_all", token, nil))
		if strings.Join(got, ",") != "a-vm,c-vm" {
			t.Fatalf("got %v, want [a-vm c-vm]", got)
		}
	})

	t.Run("admin sees every namespace", func(t *testing.T) {
		s := newNamespaceScopingTestServer(t, fk, []User{
			{Username: "root", PasswordHash: hashFor(t, "root-pw"), IsAdmin: true},
		}, nil, true)
		h := s.Handler()
		token := loginToken(t, h, "root", "root-pw")
		got := names(t, doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=_all", token, nil))
		if strings.Join(got, ",") != "a-vm,b-vm,c-vm" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("scoping off lists everything", func(t *testing.T) {
		s := newNamespaceScopingTestServer(t, fk, []User{
			{Username: "bob", PasswordHash: hashFor(t, "bob-pw"), Namespaces: []string{"default"}},
		}, nil, false)
		h := s.Handler()
		token := loginToken(t, h, "bob", "bob-pw")
		got := names(t, doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=_all", token, nil))
		if len(got) != 3 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("single-namespace requests are unchanged", func(t *testing.T) {
		s := newNamespaceScopingTestServer(t, fk, []User{
			{Username: "bob", PasswordHash: hashFor(t, "bob-pw"), Namespaces: []string{"default"}},
		}, nil, true)
		h := s.Handler()
		token := loginToken(t, h, "bob", "bob-pw")
		// (The fake apiserver does not filter namespaced lists, so only the
		// authorization outcome of the single-namespace route is asserted.)
		if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=default", token, nil); rr.Code != http.StatusOK {
			t.Fatalf("in-scope namespace must stay 200, got %d", rr.Code)
		}
		if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines?namespace=b", token, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("out-of-scope namespace must stay 403, got %d", rr.Code)
		}
	})
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestListMigrationsAndSnapshotsAllNamespaces covers ?namespace=_all on the
// Migrations and Snapshots lists: same visibility rule as Machines.
func TestListMigrationsAndSnapshotsAllNamespaces(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m-a"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m-a", Namespace: "default"}}
	fk.migrations["m-b"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m-b", Namespace: "b"}}
	fk.snapshots["s-a"] = model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "s-a", Namespace: "default"}}
	fk.snapshots["s-b"] = model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "s-b", Namespace: "b"}}

	count := func(t *testing.T, rr *httptest.ResponseRecorder) int {
		t.Helper()
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode: %v: %s", err, rr.Body.String())
		}
		return len(raw)
	}

	for _, path := range []string{"/api/v1/migrations?namespace=_all", "/api/v1/snapshots?namespace=_all"} {
		t.Run("scoped user "+path, func(t *testing.T) {
			s := newNamespaceScopingTestServer(t, fk, []User{
				{Username: "bob", PasswordHash: hashFor(t, "bob-pw"), Namespaces: []string{"default"}},
			}, nil, true)
			h := s.Handler()
			token := loginToken(t, h, "bob", "bob-pw")
			if n := count(t, doJSON(t, h, http.MethodGet, path, token, nil)); n != 1 {
				t.Fatalf("scoped user got %d items, want 1", n)
			}
		})
		t.Run("admin "+path, func(t *testing.T) {
			s := newNamespaceScopingTestServer(t, fk, []User{
				{Username: "root", PasswordHash: hashFor(t, "root-pw"), IsAdmin: true},
			}, nil, true)
			h := s.Handler()
			token := loginToken(t, h, "root", "root-pw")
			if n := count(t, doJSON(t, h, http.MethodGet, path, token, nil)); n != 2 {
				t.Fatalf("admin got %d items, want 2", n)
			}
		})
	}
}
