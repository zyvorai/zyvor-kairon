// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// agentSessionReadVerbs still answer once a session has Ended: FluxVM
// keeps the ledger after the VM is gone.
var agentSessionReadVerbs = map[string]bool{"events": true, "artifacts": true, "trajectory": true}

var previewTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// previewCSP runs preview pages in an opaque origin: they are served from
// kairon-ui's own origin, so without it guest JavaScript could read the
// dashboard's sessionStorage token.
const previewCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"

const forkResponseLimit = 8 << 20

type createAgentSessionRequest struct {
	Name   string                 `json:"name"`
	Labels map[string]string      `json:"labels,omitempty"`
	Spec   model.AgentSessionSpec `json:"spec"`
}

func (s *Server) handleListAgentSessions(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListAgentSessionsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetAgentSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Kube.GetAgentSession(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleCreateAgentSession(w http.ResponseWriter, r *http.Request) {
	var req createAgentSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	// A pinned node skips the controller's quota admission.
	if req.Spec.NodeName != "" && !s.isAdminIdentity(r.Context(), usernameFromContext(r.Context())) {
		writeError(w, http.StatusForbidden, "only an admin may pin spec.nodeName")
		return
	}
	if err := model.ValidateAgentSessionSpec(req.Spec); err != nil {
		writeError(w, http.StatusBadRequest, "spec."+err.Error())
		return
	}
	if req.Name == "" {
		req.Name = "session-" + randomSuffix()
	}
	ns := namespaceParam(r)
	sess := model.AgentSession{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindAgentSession},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns, Labels: req.Labels},
		Spec:     req.Spec,
	}
	created, err := s.Kube.CreateAgentSession(r.Context(), ns, sess)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi agent session created", "username", usernameFromContext(r.Context()), "namespace", ns, "name", req.Name)
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleDeleteAgentSession(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	if err := s.Kube.DeleteAgentSession(r.Context(), ns, name); err != nil {
		writeUpstreamError(w, err)
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi agent session deleted", "username", usernameFromContext(r.Context()), "namespace", ns, "name", name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentSessionAccess runs the checks every relayed verb needs. The verbs
// run arbitrary commands and read or write the workspace, so they take the
// same grant as guest exec: an admin, or any named operator when
// ui.auth.sandboxOperatorExec is on.
func (s *Server) agentSessionAccess(w http.ResponseWriter, r *http.Request, verb string) (sess model.AgentSession, nodeAddr, username string, ok bool) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	sess, err := s.Kube.GetAgentSession(r.Context(), ns, name)
	if err != nil {
		writeUpstreamError(w, err)
		return sess, "", "", false
	}
	username = usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) && (!s.SandboxOperatorExec || username == "") {
		if s.Log != nil {
			s.Log.Warn("uiapi agent session denied", "username", username, "namespace", ns, "name", name, "verb", verb)
		}
		writeError(w, http.StatusForbidden, "agent session verbs require an admin account (or ui.auth.sandboxOperatorExec)")
		return sess, "", "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "the node relay is not enabled on this deployment")
		return sess, "", "", false
	}
	if sess.Status.SessionID == "" || sess.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "agent session has no FluxVM session yet")
		return sess, "", "", false
	}
	readable := r.Method == http.MethodGet && agentSessionReadVerbs[verb] && sess.Status.Phase == model.AgentSessionEnded
	if sess.Status.Phase != model.AgentSessionRunning && !readable {
		writeError(w, http.StatusConflict, "agent session is "+strings.ToLower(orDefault(sess.Status.Phase, "pending")))
		return sess, "", "", false
	}
	nodeAddr, err = s.nodeInternalIP(r.Context(), sess.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return sess, "", "", false
	}
	return sess, nodeAddr, username, true
}

// handleAgentSessionProxy relays one FluxVM session verb through the
// session's node. Fork answers also create one AgentSession per child, and
// preview URLs are rewritten to kairon-ui's /preview route.
func (s *Server) handleAgentSessionProxy(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	verb, _, _ := strings.Cut(rest, "/")
	if verb == "" {
		writeError(w, http.StatusNotFound, "agent session verb required")
		return
	}
	sess, nodeAddr, username, ok := s.agentSessionAccess(w, r, verb)
	if !ok {
		return
	}
	rewrite := s.agentSessionResponseRewrite(r, sess, rest)
	rp := s.nodeReverseProxy(nodeAddr, "/agent-sessions/"+sess.Status.SessionID+"/"+rest, rewrite != nil)
	rp.ModifyResponse = rewrite
	if s.Log != nil {
		s.Log.Info("uiapi agent session verb", "username", username, "namespace", sess.Metadata.Namespace, "name", sess.Metadata.Name, "verb", verb, "method", r.Method)
	}
	rp.ServeHTTP(w, r)
}

func (s *Server) agentSessionResponseRewrite(r *http.Request, sess model.AgentSession, rest string) func(*http.Response) error {
	switch {
	case r.Method == http.MethodPost && rest == "fork":
		return func(resp *http.Response) error {
			if resp.StatusCode != http.StatusCreated {
				return nil
			}
			return rewriteJSONBody(resp, func(doc map[string]any) {
				names, errs := s.createForkChildren(resp.Request.Context(), sess, doc)
				doc["agentSessions"] = names
				if len(errs) > 0 {
					doc["agentSessionErrors"] = errs
				}
			})
		}
	case r.Method == http.MethodPost && rest == "preview":
		return func(resp *http.Response) error {
			if resp.StatusCode/100 != 2 {
				return nil
			}
			return rewriteJSONBody(resp, func(doc map[string]any) { s.rewritePreview(sess, doc) })
		}
	case r.Method == http.MethodGet && rest == "previews":
		return func(resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			return rewriteJSONBody(resp, func(doc map[string]any) {
				items, _ := doc["items"].([]any)
				for _, it := range items {
					if p, ok := it.(map[string]any); ok {
						s.rewritePreview(sess, p)
					}
				}
			})
		}
	}
	return nil
}

func (s *Server) rewritePreview(sess model.AgentSession, p map[string]any) {
	token, _ := p["token"].(string)
	if token == "" {
		return
	}
	p["url"] = "/preview/" + sess.Metadata.Namespace + "/" + sess.Metadata.Name + "/" + token + "/"
	p["served"] = s.AgentSessionPreviews
}

// createForkChildren creates one AgentSession per child FluxVM made,
// pinned to the parent's node and adopting the child's session id. The
// finalizer is set up front so deleting a child before kairon-node first
// sees it still removes the FluxVM session.
func (s *Server) createForkChildren(ctx context.Context, parent model.AgentSession, doc map[string]any) (names, errs []string) {
	items, _ := doc["items"].([]any)
	names, errs = []string{}, nil
	for _, it := range items {
		child, _ := it.(map[string]any)
		id, _ := child["id"].(string)
		if len(id) < 8 {
			continue
		}
		name := truncateName(parent.Metadata.Name, 244) + "-" + id[:8]
		labels := map[string]string{model.AssignedNodeLabel: parent.Status.NodeName}
		if len(parent.Metadata.Name) <= 63 {
			labels[model.LabelForkedFrom] = parent.Metadata.Name
		}
		spec := parent.Spec
		spec.NodeName = parent.Status.NodeName
		cr := model.AgentSession{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindAgentSession},
			Metadata: model.ObjectMeta{
				Name:        name,
				Namespace:   parent.Metadata.Namespace,
				Labels:      labels,
				Annotations: map[string]string{model.AnnotationAdoptSession: id},
				Finalizers:  []string{model.FinalizerAgentSession},
			},
			Spec: spec,
		}
		if _, err := s.Kube.CreateAgentSession(ctx, parent.Metadata.Namespace, cr); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
			if s.Log != nil {
				s.Log.Warn("uiapi fork child AgentSession create failed", "namespace", parent.Metadata.Namespace, "parent", parent.Metadata.Name, "session", id, "err", err)
			}
			continue
		}
		names = append(names, name)
	}
	return names, errs
}

// handleAgentSessionPreview serves a session preview on kairon-ui's own
// origin. Unauthenticated: the 256-bit token is the credential, as in
// FluxVM. Off unless ui.agentSessions.previews is set, and every failure
// is the same 404 so the route does not reveal which sessions exist.
func (s *Server) handleAgentSessionPreview(w http.ResponseWriter, r *http.Request) {
	notFound := func() { http.Error(w, "preview not found", http.StatusNotFound) }
	token := r.PathValue("token")
	if !s.AgentSessionPreviews || s.ConsoleToken == "" || s.ConsolePort == "" || !previewTokenPattern.MatchString(token) {
		notFound()
		return
	}
	sess, err := s.Kube.GetAgentSession(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil || sess.Status.Phase != model.AgentSessionRunning || sess.Status.NodeName == "" {
		notFound()
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), sess.Status.NodeName)
	if err != nil {
		notFound()
		return
	}
	rp := s.nodeReverseProxy(nodeAddr, "/agent-preview/"+token+"/"+r.PathValue("rest"), false)
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Set-Cookie")
		resp.Header.Set("Content-Security-Policy", previewCSP)
		resp.Header.Set("Referrer-Policy", "no-referrer")
		return nil
	}
	rp.ServeHTTP(w, r)
}

// handleAgentSessionPreviewRoot sends /preview/{ns}/{name}/{token} to the
// trailing-slash form so the app's relative URLs resolve under the token.
func (s *Server) handleAgentSessionPreviewRoot(w http.ResponseWriter, r *http.Request) {
	if !s.AgentSessionPreviews || !previewTokenPattern.MatchString(r.PathValue("token")) {
		http.Error(w, "preview not found", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
}

// nodeReverseProxy forwards to kairon-node's relay at path with the
// console token. Callers' cookies and credentials stop here. identity asks
// the node for an uncompressed body so ModifyResponse can rewrite it.
func (s *Server) nodeReverseProxy(nodeAddr, path string, identity bool) *httputil.ReverseProxy {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = nodeAddr + ":" + s.ConsolePort
			pr.Out.URL.Path = path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
			if identity {
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "connect to node relay: "+err.Error())
		},
	}
}

func rewriteJSONBody(resp *http.Response, edit func(map[string]any)) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, forkResponseLimit))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		edit(doc)
		if out, err := json.Marshal(doc); err == nil {
			body = out
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func truncateName(name string, n int) string {
	if len(name) <= n {
		return name
	}
	return strings.TrimRight(name[:n], "-.")
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
