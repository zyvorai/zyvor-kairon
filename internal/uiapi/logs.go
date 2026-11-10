// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// handleLogs streams a Machine's captured serial console output --
// kairon-ui -> kairon-node (internal/consoleproxy) -> FluxVM's own real
// GET /v1/vms/{id}/logs. Kairon's `kubectl logs`/`kaironctl logs`
// equivalent. Unlike guest exec/file access/the QGA diagnostics (all
// admin-only), this uses the same authorization model as the VNC/text
// console -- any authenticated operator by default, restrictable via the
// `kairon.zyvor.dev/console-allowed-users` annotation -- since reading a
// VM's own console output is closer in sensitivity to viewing its display
// than to running arbitrary code or reading/writing arbitrary guest files.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	username := usernameFromContext(r.Context())
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi logs denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to view this machine's logs")
		return
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "logs are not enabled on this deployment")
		return
	}
	// Logs only need a runtime to have existed (its log file lives on
	// the node regardless of whether the guest is currently Running or
	// Paused) -- unlike exec/console, this deliberately doesn't require
	// Phase == "Running".
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = s.nodeTransport()
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/logs/%s", scheme, nodeAddr, s.ConsolePort, m.Status.RuntimeID)
	if q := r.URL.RawQuery; q != "" {
		upstreamURL += "?" + q
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)

	if s.Log != nil {
		s.Log.Info("uiapi logs requested", "username", username, "namespace", namespace, "name", name)
	}
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node logs relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("node logs relay returned HTTP %d", resp.StatusCode)
		writeError(w, http.StatusBadGateway, msg)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF && s.Log != nil {
				s.Log.Warn("uiapi logs stream ended", "namespace", namespace, "name", name, "error", readErr)
			}
			return
		}
	}
}
