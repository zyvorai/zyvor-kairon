// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package consoleproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

// handleNetworkEffective forwards to FluxVM's own
// GET /v1/vms/{id}/network/effective -- raw JSON passthrough, like every
// handler in this file, since FluxVM's own handlers return a dynamic
// serde_json::Value here, not a fixed struct.
func (s *Server) handleNetworkEffective(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	data, err := s.Flux.GetVMNetworkEffective(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("network effective: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// handleNetworkStats forwards to FluxVM's own
// GET /v1/vms/{id}/network/stats.
func (s *Server) handleNetworkStats(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	data, err := s.Flux.GetVMNetworkStats(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("network stats: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func parseLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return limit
}

// handleNetworkFlows forwards to FluxVM's own
// GET /v1/vms/{id}/network/flows?limit=N.
func (s *Server) handleNetworkFlows(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	data, err := s.Flux.GetVMNetworkFlows(r.Context(), r.PathValue("runtimeID"), parseLimit(r))
	if err != nil {
		http.Error(w, fmt.Sprintf("network flows: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// handleNetworkDropReasons forwards to FluxVM's own
// GET /v1/vms/{id}/network/drop-reasons?limit=N.
func (s *Server) handleNetworkDropReasons(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	data, err := s.Flux.GetVMNetworkDropReasons(r.Context(), r.PathValue("runtimeID"), parseLimit(r))
	if err != nil {
		http.Error(w, fmt.Sprintf("network drop-reasons: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// handleNetworkDrops forwards attributed eBPF drops.
// GET /v1/vms/{id}/network/drops?limit=N.
func (s *Server) handleNetworkDrops(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	data, err := s.Flux.AttributedDrops(r.Context(), r.PathValue("runtimeID"), parseLimit(r))
	if err != nil {
		http.Error(w, fmt.Sprintf("network drops: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// handleNetworkCaptures lists capture sessions.
// GET /v1/vms/{id}/network/capture.
func (s *Server) handleNetworkCaptures(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	data, err := s.Flux.CaptureSessions(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("network captures: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// handleNetworkCaptureFile streams a finished capture's pcap, passing on
// FluxVM's 404 / 409. GET /v1/vms/{id}/network/capture/{token}.
func (s *Server) handleNetworkCaptureFile(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	resp, err := s.Flux.CaptureFile(r.Context(), r.PathValue("runtimeID"), r.PathValue("token"))
	if err != nil {
		http.Error(w, fmt.Sprintf("network capture: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyCaptureResponse(w, resp)
}

// copyCaptureResponse relays a pcap download: 200, 404 and 409 keep their
// status, anything else becomes 502.
func copyCaptureResponse(w http.ResponseWriter, resp *http.Response) {
	status := resp.StatusCode
	switch status {
	case http.StatusOK, http.StatusNotFound, http.StatusConflict:
	default:
		status = http.StatusBadGateway
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, resp.Body)
}

// handleNetworkCapture starts a bounded packet capture.
// POST /v1/vms/{id}/network/capture.
func (s *Server) handleNetworkCapture(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var session ebpfedge.CaptureSession
	if err := json.Unmarshal(body, &session); err != nil {
		http.Error(w, "invalid capture session", http.StatusBadRequest)
		return
	}
	data, err := s.Flux.StartCapture(r.Context(), r.PathValue("runtimeID"), session)
	if err != nil {
		http.Error(w, fmt.Sprintf("network capture: %v", err), http.StatusBadGateway)
		return
	}
	if len(data) == 0 {
		data = body
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
