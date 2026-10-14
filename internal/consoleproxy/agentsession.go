// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
)

// agentSessionVerbs are the FluxVM agent-session routes kairon-ui may
// reach through this relay, keyed by the first path segment after the
// session id. Deleting the session itself is not here: that goes through
// the AgentSession resource so the finalizer and quota stay in step.
// tokens and ssh are left out on purpose: a session token only works on
// FluxVM's node-local port, and ssh hands out the host's shared sandbox
// key. pty-ticket is for browsers talking to FluxVM directly.
var agentSessionVerbs = map[string]bool{
	"approvals":  true,
	"grants":     true,
	"probe":      true,
	"pty":        true,
	"screenshot": true,
	"input":      true,
	"uploads":    true,
	"exec":       true,
	"checkpoint": true,
	"restore":    true,
	"fork":       true,
	"promote":    true,
	"events":     true,
	"artifacts":  true,
	"trajectory": true,
	"preview":    true,
	"previews":   true,
	"jobs":       true,
	"files":      true,
	"git":        true,
}

var (
	sessionIDPattern    = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	previewTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// handleAgentSession relays one request to FluxVM's
// /v1/agents/sessions/{id}[/{rest}]. Streaming responses (job SSE) are
// flushed as they arrive, and WebSocket upgrades pass through.
func (s *Server) handleAgentSession(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	id, rest := r.PathValue("id"), r.PathValue("rest")
	if !sessionIDPattern.MatchString(id) {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	verb, _, _ := strings.Cut(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
	case agentSessionVerbs[verb] && !hasDotSegment(rest):
	default:
		http.Error(w, "agent session route not relayed", http.StatusNotFound)
		return
	}
	path := "/v1/agents/sessions/" + id
	if rest != "" {
		path += "/" + rest
	}
	s.fluxReverseProxy(path).ServeHTTP(w, r)
}

// handleAgentPreview relays one request to FluxVM's unauthenticated
// /preview/{token}/{rest}. The token is the capability; kairon-ui has
// already checked the session is Running before calling here.
func (s *Server) handleAgentPreview(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	token, rest := r.PathValue("token"), r.PathValue("rest")
	if !previewTokenPattern.MatchString(token) || hasDotSegment(rest) {
		http.NotFound(w, r)
		return
	}
	s.fluxReverseProxy("/preview/"+token+"/"+rest).ServeHTTP(w, r)
}

func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return true
		}
	}
	return false
}

// fluxReverseProxy forwards to FluxVM at path, keeping the query string
// and swapping the relay's bearer for FluxVM's own. Cookies never reach
// FluxVM or the guest.
func (s *Server) fluxReverseProxy(path string) *httputil.ReverseProxy {
	base, _ := url.Parse(s.Flux.BaseURL)
	transport := http.DefaultTransport
	if s.Flux.HTTP != nil && s.Flux.HTTP.Transport != nil {
		transport = s.Flux.HTTP.Transport
	}
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.URL.Path = strings.TrimSuffix(base.Path, "/") + path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			if s.Flux.Token != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+s.Flux.Token)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "dial fluxvm: "+err.Error(), http.StatusBadGateway)
		},
	}
}
