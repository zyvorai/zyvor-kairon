// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// relayRawGET is relayToNodeGet's raw-passthrough counterpart -- for a
// node relay route whose response is dynamic JSON with no fixed shape
// (network effective/stats/flows/drop-reasons, all backed by FluxVM's
// own serde_json::Value handlers), copied straight through to the caller
// rather than decoded into a Go struct.
func (s *Server) relayRawGET(w http.ResponseWriter, ctx context.Context, nodeAddr, nodePath string) {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = s.nodeTransport()
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath)
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("node relay returned HTTP %d", resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

// handleNetworkEffective returns a Machine's fully-resolved effective
// network policy (after NetworkSecurityGroup/label merging): kairon-ui ->
// kairon-node -> FluxVM's own GET /v1/vms/{id}/network/effective. The
// underlying fluxvm.Client.GetVMNetworkEffective existed with no caller
// anywhere in this codebase until this handler -- the single most direct
// answer to "what policy is actually being enforced right now," as
// opposed to GetVMNetworkPolicy's own view of what was last *sent*.
func (s *Server) handleNetworkEffective(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	s.relayRawGET(w, ctx, nodeAddr, "network-effective/"+runtimeID)
}

// handleNetworkStats returns a Machine's real, eBPF-dataplane-derived
// network byte/packet counters: kairon-ui -> kairon-node -> FluxVM's own
// GET /v1/vms/{id}/network/stats.
func (s *Server) handleNetworkStats(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	s.relayRawGET(w, ctx, nodeAddr, "network-stats/"+runtimeID)
}

// handleNetworkFlows returns a Machine's most recent eBPF-observed
// network flows: kairon-ui -> kairon-node -> FluxVM's own
// GET /v1/vms/{id}/network/flows?limit=N.
func (s *Server) handleNetworkFlows(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	nodePath := "network-flows/" + runtimeID
	if limit := r.URL.Query().Get("limit"); limit != "" {
		nodePath += "?limit=" + limit
	}
	s.relayRawGET(w, ctx, nodeAddr, nodePath)
}

// handleNetworkDropReasons returns why the eBPF dataplane most recently
// dropped packets for a Machine: kairon-ui -> kairon-node -> FluxVM's own
// GET /v1/vms/{id}/network/drop-reasons?limit=N -- the most direct
// troubleshooting tool for "why is my MachineNetworkPolicy/
// NetworkSecurityGroup blocking traffic I expected to be allowed."
func (s *Server) handleNetworkDropReasons(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	nodePath := "network-drop-reasons/" + runtimeID
	if limit := r.URL.Query().Get("limit"); limit != "" {
		nodePath += "?limit=" + limit
	}
	s.relayRawGET(w, ctx, nodeAddr, nodePath)
}

// handleNetworkDrops relays attributed drops: kairon-ui -> kairon-node -> FluxVM.
func (s *Server) handleNetworkDrops(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	nodePath := "network-drops/" + runtimeID
	if limit := r.URL.Query().Get("limit"); limit != "" {
		nodePath += "?limit=" + limit
	}
	s.relayRawGET(w, ctx, nodeAddr, nodePath)
}

// handleNetworkCapture relays a bounded tap: kairon-ui -> kairon-node -> FluxVM.
func (s *Server) handleNetworkCapture(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	s.relayRawPOST(w, ctx, nodeAddr, "network-capture/"+runtimeID, body)
}

// handleNetworkCaptures lists capture sessions: kairon-ui -> kairon-node -> FluxVM.
func (s *Server) handleNetworkCaptures(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	s.relayRawGET(w, ctx, nodeAddr, "network-capture/"+runtimeID)
}

// handleNetworkCaptureFile streams a finished capture's pcap. 404 (unknown
// token) and 409 (still running) are passed through so a client can poll.
func (s *Server) handleNetworkCaptureFile(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	token := r.PathValue("token")
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = s.nodeTransport()
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/network-capture/%s/%s", scheme, nodeAddr, s.ConsolePort, url.PathEscape(runtimeID), url.PathEscape(token))
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", token+".pcap"))
		_, _ = io.Copy(w, resp.Body)
	case http.StatusNotFound:
		writeError(w, http.StatusNotFound, "capture not found")
	case http.StatusConflict:
		writeError(w, http.StatusConflict, "capture is still running")
	default:
		writeError(w, http.StatusBadGateway, fmt.Sprintf("node relay returned HTTP %d", resp.StatusCode))
	}
}

func (s *Server) relayRawPOST(w http.ResponseWriter, ctx context.Context, nodeAddr, nodePath string, body []byte) {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = s.nodeTransport()
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath)
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	upstreamReq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("node relay returned HTTP %d", resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}
