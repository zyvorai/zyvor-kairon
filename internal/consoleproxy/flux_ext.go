// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// handleHostTopology forwards to GET /v1/host/topology (node-scoped).
func (s *Server) handleHostTopology(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "host topology", http.MethodGet, "/v1/host/topology", false, false)
}

// handleHostPressure forwards to GET /v1/host/pressure (node-scoped PSI).
func (s *Server) handleHostPressure(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "host pressure", http.MethodGet, "/v1/host/pressure", false, false)
}

// handleBranchStart forwards to POST /v1/branches/{id}/start. FluxVM requires
// an admin token for it, which the relay's own bearer already is.
func (s *Server) handleBranchStart(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "branch start", http.MethodPost,
		"/v1/branches/"+url.PathEscape(r.PathValue("id"))+"/start", false, true)
}

// handleBranchDelete forwards to DELETE /v1/branches/{id}.
func (s *Server) handleBranchDelete(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "branch delete", http.MethodDelete,
		"/v1/branches/"+url.PathEscape(r.PathValue("id")), false, false)
}

// handleEgressNftables wraps FluxVM's plain-text GET /v1/egress/nftables
// redirect snippet in JSON so every relay route answers the same way.
func (s *Server) handleEgressNftables(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	out, err := s.Flux.Call(r.Context(), http.MethodGet, "/v1/egress/nftables", nil, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("egress nftables: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"snippet": string(out)})
}

// handleEventsStream relays FluxVM's server-sent event stream. Responses are
// flushed as they arrive; the reverse proxy swaps the relay bearer for FluxVM's.
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	s.fluxReverseProxy("/v1/events/stream").ServeHTTP(w, r)
}

// handleVMScreenshot relays FluxVM's GET /v1/vms/{id}/screenshot (a PNG of a running vz
// VM's display; width, height and blank state come back as x-fluxvm-screen-* headers).
// The bytes stream straight through; the reverse proxy swaps the relay bearer for FluxVM's.
func (s *Server) handleVMScreenshot(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	id := r.PathValue("runtimeID")
	if id == "" || strings.ContainsAny(id, "/?#") {
		http.Error(w, "invalid runtime id", http.StatusBadRequest)
		return
	}
	s.fluxReverseProxy("/v1/vms/"+url.PathEscape(id)+"/screenshot").ServeHTTP(w, r)
}

// handleVMInput forwards keyboard and mouse input to FluxVM's POST /v1/vms/{id}/input.
// FluxVM rejects unknown fields itself; the relay only bounds the body size.
func (s *Server) handleVMInput(w http.ResponseWriter, r *http.Request) {
	s.relayFlux(w, r, "vm input", http.MethodPost,
		"/v1/vms/"+url.PathEscape(r.PathValue("runtimeID"))+"/input", false, true)
}
