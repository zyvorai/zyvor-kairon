// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
)

// kaironComponent returns the Kairon component a pod belongs to
// (kairon-controller, kairon-node, kairon-ui, ...) or "" for any other pod.
func kaironComponent(p kube.PodSummary) string {
	if n := p.Labels["app.kubernetes.io/name"]; strings.HasPrefix(n, "kairon-") {
		return n
	}
	return ""
}

// visiblePods lists pods the caller may see: every pod when namespace
// scoping is off, otherwise only pods in namespaces the caller is granted.
func (s *Server) visiblePods(r *http.Request) ([]kube.PodSummary, error) {
	pods, err := s.Kube.ListPods(r.Context(), "")
	if err != nil {
		return nil, err
	}
	out := pods[:0]
	for _, p := range pods {
		if s.authorizedForNamespace(r.Context(), p.Namespace) {
			out = append(out, p)
		}
	}
	return out, nil
}

type podRow struct {
	kube.PodSummary
	Component string `json:"component,omitempty"`
}

// handlePods serves GET /api/v1/pods: Kairon's own pods by default,
// ?scope=all for every pod the caller may see, ?node=<name> to restrict to
// one node. Sorted by namespace then name.
func (s *Server) handlePods(w http.ResponseWriter, r *http.Request) {
	pods, err := s.visiblePods(r)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	all := r.URL.Query().Get("scope") == "all"
	node := r.URL.Query().Get("node")
	out := make([]podRow, 0, len(pods))
	for _, p := range pods {
		comp := kaironComponent(p)
		if !all && comp == "" {
			continue
		}
		if node != "" && p.Node != node {
			continue
		}
		out = append(out, podRow{PodSummary: p, Component: comp})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

type componentHealth struct {
	Component string `json:"component"`
	Pods      int    `json:"pods"`
	Ready     int    `json:"ready"`
	Restarts  int    `json:"restarts"`
	Healthy   bool   `json:"healthy"`
}

// handleHealth serves GET /api/v1/health: a per-component rollup of Kairon's
// own pods (the dashboard form of `kaironctl doctor`'s workload check).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	pods, err := s.visiblePods(r)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	by := map[string]*componentHealth{}
	for _, p := range pods {
		comp := kaironComponent(p)
		if comp == "" {
			continue
		}
		h := by[comp]
		if h == nil {
			h = &componentHealth{Component: comp}
			by[comp] = h
		}
		h.Pods++
		h.Restarts += p.Restarts
		if p.Ready && p.Phase == "Running" {
			h.Ready++
		}
	}
	out := make([]componentHealth, 0, len(by))
	for _, h := range by {
		h.Healthy = h.Pods > 0 && h.Ready == h.Pods
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	writeJSON(w, http.StatusOK, out)
}
