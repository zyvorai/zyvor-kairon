// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/model"
)

// consoleTicketTTL is deliberately short: a ticket only needs to survive
// the few hundred milliseconds between the browser's ticket-issuing
// fetch() (which can carry the real Authorization header) and its
// follow-up WebSocket handshake (which can't -- browsers don't allow
// custom headers on a native WebSocket upgrade).
const consoleTicketTTL = 30 * time.Second

// consoleTicketState binds a ticket to the operator it was issued to (so
// handleConsole's audit log can attribute a console session to a person,
// not just "someone who had a valid ticket") and to the one Machine it
// was issued for (so a ticket minted for one Machine can't be replayed
// against a different one's console endpoint within its short TTL --
// see consumeConsoleTicket/handleConsole). kind is "vnc" or "text" --
// which of the two consoleproxy relay routes the caller asked for at
// issuance; handleConsole still separately checks the Machine's own
// eligibility for that kind at dial time (Running, the right backend/
// guestAgent field), the same as it always has for VNC.
type consoleTicketState struct {
	username  string
	namespace string
	name      string
	kind      string
	expires   time.Time
}

// issueConsoleTicket mints a single-use ticket for handleConsole, so the
// real session/token credential never has to appear in a URL or access
// log -- only this short-lived, narrow-purpose value does. With
// SharedStateConfigMapName unset (a single replica, the default), this is
// exactly the original purely-local implementation: stored in
// consoleTickets, keyed by the ticket's own hash rather than the ticket
// itself (see sha256Hex in sharedstate.go). With it set, the shared
// ConfigMap becomes the single source of truth instead -- see
// consumeConsoleTicket for why a ticket can't be trusted from both places
// at once.
//
// Returns an error rather than silently proceeding on a rand.Read failure
// -- matching randomNonce's own established handling of the identical
// crypto/rand call in oidc.go, and unlike that one, a discarded error here
// wouldn't just fail a later comparison: crypto/rand.Read only guarantees
// n == len(b) when err is nil, so a failure can leave buf (and therefore
// the minted ticket) partially or entirely at its zero value -- a
// predictable credential granting a live VNC/text console session, not
// merely a rejected login.
func (s *Server) issueConsoleTicket(ctx context.Context, username, namespace, name, kind string) (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate console ticket: %w", err)
	}
	ticket := hex.EncodeToString(buf)
	expires := time.Now().Add(consoleTicketTTL)
	if s.SharedStateConfigMapName == "" {
		s.consoleTickets.Store(sha256Hex(ticket), consoleTicketState{username: username, namespace: namespace, name: name, kind: kind, expires: expires})
		return ticket, nil
	}
	s.writeSharedState(ctx, sharedTicketKey(ticket), sharedTicketEntry{Username: username, Namespace: namespace, Name: name, Kind: kind, Expires: expires})
	return ticket, nil
}

// consumeConsoleTicket validates and immediately deletes a ticket --
// presenting the same ticket twice always fails the second time -- and
// returns the username it was issued to.
//
// With SharedStateConfigMapName set, this always goes straight to the
// shared ConfigMap rather than checking consoleTickets first: the
// browser's ticket-issuing fetch() and its follow-up WebSocket upgrade
// are two separate connections, and with more than one kairon-ui replica
// behind one Service, nothing guarantees they land on the same pod. If
// this replica happened to be the one that minted the ticket, trusting
// its own local copy first would let IT keep accepting the ticket even
// after a DIFFERENT replica already consumed the shared one -- a local
// cache that's optimistically ahead of the shared source of truth is
// exactly what breaks single-use here, so once sharing is enabled the
// shared ConfigMap is authoritative, full stop, and consoleTickets is
// never written to at all (see issueConsoleTicket). This also means a
// concurrent consume from two replicas within the same few milliseconds
// can still both read the entry before either delete lands -- a narrow,
// documented (docs/guides/kairon-ui-ha.md) limitation of a
// read-then-delete that isn't atomic, not a design this "first cut" tries
// to fully close.
//
// consoleTicketTTL (30s) is far shorter than any reasonable poll
// interval, which is why this can't just wait for the periodic sync to
// pick a remote ticket up (see RunSharedStateSync in sharedstate.go) --
// it has to be a direct, synchronous lookup instead.
func (s *Server) consumeConsoleTicket(ctx context.Context, ticket string) (username, namespace, name, kind string, ok bool) {
	if ticket == "" {
		return "", "", "", "", false
	}
	hash := sha256Hex(ticket)
	if s.SharedStateConfigMapName == "" {
		v, found := s.consoleTickets.LoadAndDelete(hash)
		if !found {
			return "", "", "", "", false
		}
		st, _ := v.(consoleTicketState)
		if time.Now().After(st.expires) {
			return "", "", "", "", false
		}
		return st.username, st.namespace, st.name, st.kind, true
	}
	cm, err := s.Kube.GetConfigMap(ctx, s.SharedStateNamespace, s.SharedStateConfigMapName)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi console ticket lookup failed", "error", err)
		}
		return "", "", "", "", false
	}
	key := sharedStateTicketPrefix + hash
	raw, found := cm.Data[key]
	if !found {
		return "", "", "", "", false
	}
	s.deleteSharedState(ctx, key)
	var e sharedTicketEntry
	if json.Unmarshal([]byte(raw), &e) != nil || time.Now().After(e.Expires) {
		return "", "", "", "", false
	}
	return e.Username, e.Namespace, e.Name, e.Kind, true
}

func (s *Server) handleConsoleTicket(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "vnc"
	}
	guestfs := kind == "guestfs" || kind == "guestfs-rw"
	if kind != "vnc" && kind != "text" && !guestfs {
		writeError(w, http.StatusBadRequest, "kind must be \"vnc\", \"text\", \"guestfs\" or \"guestfs-rw\"")
		return
	}
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	username := usernameFromContext(r.Context())
	if guestfs && !s.isAdminIdentity(r.Context(), username) {
		writeError(w, http.StatusForbidden, "guestfs opens the Machine's disk directly and is admin-only")
		return
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi console ticket denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to open this machine's console")
		return
	}
	ticket, err := s.issueConsoleTicket(r.Context(), username, namespace, name, kind)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("uiapi console ticket generation failed", "username", username, "namespace", namespace, "name", name, "error", err)
		}
		writeError(w, http.StatusInternalServerError, "failed to generate console ticket")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
}

// consoleAuthorized reports whether username may open m's console. Unset
// kairon.zyvor.dev/console-allowed-users (the default) means every
// authenticated operator may -- unchanged from before this annotation
// existed. Set, it's a comma-separated allowlist of usernames; an admin
// account (ui.auth.users[].admin, the same elevated-privilege property
// that already lets an admin reset another operator's password) can
// always open any console regardless, for break-glass access an
// allowlist author didn't have to anticipate. An empty username -- the
// legacy shared ui.token has no per-operator identity at all, and
// neither does fully unauthenticated dev mode -- is denied whenever the
// Machine restricts console access, since there's no real identity to
// check against an allowlist: fail closed rather than silently allow.
//
// When s.RBACConsoleCheck is also true, a real Kubernetes
// SubjectAccessReview (see rbacAllowsConsole) must additionally allow it
// -- checked first, so a real Kubernetes RBAC denial can't be overridden
// by the annotation allowlist or an admin account, and so "RBAC alone,
// no annotation set" is enough on its own to grant access.
func (s *Server) consoleAuthorized(ctx context.Context, m model.Machine, username string) bool {
	if s.RBACConsoleCheck && !s.rbacAllowsConsole(ctx, m, username) {
		return false
	}
	allowed := strings.TrimSpace(m.Metadata.Annotations[model.AnnotationConsoleAllowedUsers])
	if allowed == "" {
		return true
	}
	if username == "" {
		return false
	}
	if s.isAdminIdentity(ctx, username) {
		return true
	}
	for _, u := range strings.Split(allowed, ",") {
		if strings.TrimSpace(u) == username {
			return true
		}
	}
	return false
}

// rbacAllowsConsole posts a SubjectAccessReview checking "get" on the
// machines/console subresource for username -- a subresource this
// project's CRD can't actually serve (see model.SubjectAccessReview's own
// doc comment), but RBAC resource strings with a subresource suffix are
// just strings the authorizer compares (the same nodes/proxy precedent
// real Kubernetes RBAC already uses), so `kubectl create clusterrole
// --resource=machines/console --verb=get` is meaningful with zero CRD
// changes. An empty username (no real per-operator identity at all) is
// denied without even calling out -- there's nothing for a
// SubjectAccessReview to check. Any SAR error also denies (fail closed,
// matching every other auth-adjacent error path in this file) --
// including for a local ui.auth.users[] account with no real Kubernetes
// User to check against, since kube-apiserver simply reports no matching
// RoleBinding for an unrecognized subject rather than erroring.
func (s *Server) rbacAllowsConsole(ctx context.Context, m model.Machine, username string) bool {
	if username == "" {
		return false
	}
	status, err := s.Kube.SubjectAccessReview(ctx, model.SubjectAccessReview{Spec: model.SubjectAccessReviewSpec{
		User: username,
		ResourceAttributes: &model.ResourceAttributes{
			Verb: "get", Group: "kairon.zyvor.dev", Resource: "machines",
			Subresource: "console", Namespace: m.Namespace(), Name: m.Metadata.Name,
		},
	}})
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi console RBAC check failed", "username", username, "namespace", m.Namespace(), "name", m.Metadata.Name, "error", err)
		}
		return false
	}
	return status.Allowed
}

// handleConfig reports small, non-sensitive feature toggles the frontend
// needs to decide what to render before the user acts -- e.g. whether to
// show a Machine's "Console" button at all, rather than showing it
// unconditionally and only failing after a click.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"consoleEnabled": s.ConsoleToken != "" && s.ConsolePort != "",
		"atlasEnabled":   s.atlasEnabled(),

		"agentSessionPreviews": s.AgentSessionPreviews,
	}
	if s.AtlasConsoleURL != "" {
		resp["atlasConsoleURL"] = s.AtlasConsoleURL
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConsole relays a browser WebSocket to the target Machine's console:
// kairon-ui -> kairon-node (internal/consoleproxy) -> either FluxVM's
// local, unix-socket-only QEMU VNC server ("vnc", the graphical display)
// or FluxVM's own WebSocket-upgraded interactive shell endpoint ("text",
// requiring spec.guestAgent.console -- a different, FluxVM-proprietary
// vsock channel from the VNC display). See docs/architecture.md for the
// full chain and its trust boundary.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	username, ticketNamespace, ticketName, kind, ok := s.consumeConsoleTicket(r.Context(), r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or expired console ticket")
		return
	}
	// A ticket only ever authorizes the one Machine handleConsoleTicket
	// issued it for -- reject outright rather than relaying to a
	// different Machine's console just because the ticket happens to be
	// otherwise valid. This is what actually makes consoleAuthorized's
	// per-Machine check (above, at issuance) meaningful: without this,
	// a ticket for a Machine the caller IS authorized to view could be
	// replayed here against one they aren't, within the ticket's 30s TTL.
	if ticketNamespace != namespace || ticketName != name {
		if s.Log != nil {
			s.Log.Warn("uiapi console ticket machine mismatch", "username", username, "ticketNamespace", ticketNamespace, "ticketName", ticketName, "requestedNamespace", namespace, "requestedName", name)
		}
		writeError(w, http.StatusUnauthorized, "invalid or expired console ticket")
		return
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "console is not enabled on this deployment")
		return
	}
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	guestfs := kind == "guestfs" || kind == "guestfs-rw"
	if !guestfs {
		if m, _, err = s.ensureAwake(r.Context(), m); err != nil {
			writeWakeError(w, err)
			return
		}
		if m.Status.Phase != "Running" {
			writeError(w, http.StatusConflict, "machine is not Running")
			return
		}
	}
	var nodePath string
	switch kind {
	case "guestfs":
		nodePath = "guestfs"
	case "guestfs-rw":
		if m.Status.Phase != "Halted" {
			writeError(w, http.StatusConflict, "guestfs --rw needs the Machine Halted (it is "+m.Status.Phase+")")
			return
		}
		nodePath = "guestfs"
	case "text":
		if !m.Spec.GuestAgent.Console {
			writeError(w, http.StatusBadRequest, "text console requires spec.guestAgent.console")
			return
		}
		nodePath = "text-console"
	case "vnc":
		if backend := m.Spec.Runtime.Backend; backend != "" && backend != "auto" && backend != "qemu" {
			writeError(w, http.StatusBadRequest, "VNC console is only available for the qemu backend")
			return
		}
		nodePath = "console"
	default:
		writeError(w, http.StatusUnauthorized, "invalid or expired console ticket")
		return
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	scheme := "ws"
	dialOpts := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + s.ConsoleToken}}}
	if s.ConsoleTLS != nil {
		scheme = "wss"
		dialOpts.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: s.ConsoleTLS}}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath, m.Status.RuntimeID)
	if kind == "text" || guestfs {
		// Forward the browser's requested terminal size straight through
		// to FluxVM's own ConsoleQuery (cols/rows) -- cosmetic only, a
		// missing/invalid value just falls back to FluxVM's own 80x24
		// default, nothing here validates them.
		forward := url.Values{}
		if q := r.URL.Query(); q.Has("cols") || q.Has("rows") {
			if v := q.Get("cols"); v != "" {
				forward.Set("cols", v)
			}
			if v := q.Get("rows"); v != "" {
				forward.Set("rows", v)
			}
		}
		if kind == "guestfs-rw" {
			forward.Set("rw", "1")
		}
		if len(forward) > 0 {
			upstreamURL += "?" + forward.Encode()
		}
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	upstream, resp, err := websocket.Dial(dialCtx, upstreamURL, dialOpts)
	cancel()
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		writeRelayDialError(w, "connect to node console relay", resp, err)
		return
	}
	defer func() { _ = upstream.CloseNow() }()

	downstream, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = downstream.CloseNow() }()

	if s.Log != nil {
		s.Log.Info("uiapi console opened", "username", username, "namespace", namespace, "name", name, "kind", kind, "remoteAddr", r.RemoteAddr)
	}
	start := time.Now()
	ctx := r.Context()
	defer s.keepActive(m)()
	relay(
		websocket.NetConn(ctx, downstream, websocket.MessageBinary),
		websocket.NetConn(ctx, upstream, websocket.MessageBinary),
	)
	if s.Log != nil {
		s.Log.Info("uiapi console closed", "username", username, "namespace", namespace, "name", name, "kind", kind, "duration", time.Since(start).Round(time.Second).String())
	}
	_ = downstream.Close(websocket.StatusNormalClosure, "")
}

// nodeInternalIP mirrors internal/agent's targetControlURL node-address
// resolution (used there for the migration peer control plane) -- same
// "look up the Node object, take its InternalIP" logic, duplicated rather
// than shared since internal/agent and internal/uiapi are different
// binaries' packages with no existing common dependency between them.
func (s *Server) nodeInternalIP(ctx context.Context, nodeName string) (string, error) {
	nodes, err := s.Kube.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, node := range nodes {
		if node.Metadata.Name != nodeName {
			continue
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == "InternalIP" && strings.TrimSpace(addr.Address) != "" {
				return strings.TrimSpace(addr.Address), nil
			}
		}
	}
	return "", fmt.Errorf("node %q has no InternalIP", nodeName)
}

// relay pumps bytes in both directions until either side closes; it
// returns once the first direction stops, at which point the caller's
// deferred closes tear down the other side too. Same small helper as
// internal/consoleproxy's (kairon-node's half of this same relay) --
// duplicated rather than shared across the two binaries' packages.
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
