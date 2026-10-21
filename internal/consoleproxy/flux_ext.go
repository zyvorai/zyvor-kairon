// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxRelayBody bounds request bodies forwarded to FluxVM.
const maxRelayBody = 64 << 10

// relayFlux forwards to a fixed FluxVM route and writes its JSON reply.
// path is built by the handler from a literal plus url.PathEscape'd IDs.
func (s *Server) relayFlux(w http.ResponseWriter, r *http.Request, what, method, path string, withQuery, withBody bool) {
	if !s.checkToken(w, r) {
		return
	}
	var body []byte
	if withBody {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBody+1))
		if err != nil || len(b) > maxRelayBody {
			http.Error(w, what+": request body unreadable or too large", http.StatusBadRequest)
			return
		}
		body = b
	}
	var q url.Values
	if withQuery {
		q = r.URL.Query()
	}
	out, err := s.Flux.Call(r.Context(), method, path, q, body)
	if err != nil {
		http.Error(w, fmt.Sprintf("%s: %v", what, err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// handleForkPlan forwards to FluxVM's POST /v1/vms/{id}/fork/plan: what a
// fork would be refused for and cost, without doing it.
func (s *Server) handleForkPlan(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "fork plan", http.MethodPost,
		"/v1/vms/"+url.PathEscape(r.PathValue("runtimeID"))+"/fork/plan", false, true)
}

// handleHostMemory forwards to GET /v1/host/memory (node-scoped).
func (s *Server) handleHostMemory(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "host memory", http.MethodGet, "/v1/host/memory", false, false)
}

// handleEvents forwards to GET /v1/events (JSON list; query passes through).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "events", http.MethodGet, "/v1/events", true, false)
}

// handleListBranches forwards to GET /v1/branches.
func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "branches", http.MethodGet, "/v1/branches", true, false)
}

// handleBranchDiff forwards to GET /v1/branches/{a}/diff/{b}.
func (s *Server) handleBranchDiff(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "branch diff", http.MethodGet,
		"/v1/branches/"+url.PathEscape(r.PathValue("a"))+"/diff/"+url.PathEscape(r.PathValue("b")), false, false)
}

// handleBranchStartPlan forwards to POST /v1/branches/{id}/start/plan.
func (s *Server) handleBranchStartPlan(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "branch start plan", http.MethodPost,
		"/v1/branches/"+url.PathEscape(r.PathValue("id"))+"/start/plan", false, true)
}
