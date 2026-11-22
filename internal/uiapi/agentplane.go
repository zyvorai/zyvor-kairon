// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"context"
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/agentplane/facts"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/llm"
)

// mountAgentPlane registers the read-only agent routes. They do not
// read a namespace query param; the body is a proposal, not a write.
func (s *Server) mountAgentPlane(api *routeMux) {
	api.HandleFunc("POST /api/v1/agent/compile-policy", s.handleAgentCompile)
	api.HandleFunc("POST /api/v1/agent/explain-drops", s.handleAgentDrops)
	api.HandleFunc("POST /api/v1/agent/anomalies", s.handleAgentAnomalies)
	api.HandleFunc("POST /api/v1/agent/cpu-label", s.handleAgentCPU)
	api.HandleFunc("POST /api/v1/agent/claims/step", s.handleAgentStep)
	api.HandleFunc("POST /api/v1/agent/confidential", s.handleAgentConfidential)
	api.HandleFunc("POST /api/v1/agent/gateway", s.handleAgentGateway)
	api.HandleFunc("POST /api/v1/agent/ask", s.handleAgentAsk)
	api.HandleFunc("GET /api/v1/agent/diagnose/{namespace}/{kind}/{name}", s.requireNamespace(namespaceFromPath, s.handleAgentDiagnose))
}

func (s *Server) handleAgentDiagnose(w http.ResponseWriter, r *http.Request) {
	if s.Kube == nil {
		writeError(w, http.StatusServiceUnavailable, "kubernetes client not configured")
		return
	}
	sub, err := facts.Gather(r.Context(), s.Kube, r.PathValue("kind"), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		if kube.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d := agentplane.Diagnose(sub.Facts)
	if client := llm.FromEnv(); client != nil && !d.Healthy && r.URL.Query().Get("ai") != "false" {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if out, err := agentplane.Summarize(ctx, client, d); err == nil {
			d = out
		}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleAgentAsk(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentCompile(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentDrops(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentAnomalies(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentCPU(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentStep(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentConfidential(w http.ResponseWriter, r *http.Request) {
	agentplane.Handler().ServeHTTP(w, r)
}

func (s *Server) handleAgentGateway(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = "/api/v1/agent/gateway"
	agentplane.Handler().ServeHTTP(w, r)
}
