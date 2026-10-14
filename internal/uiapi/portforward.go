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
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// handlePortForward relays a WebSocket to one TCP port in a Machine's
// guest: kaironctl -> kairon-ui -> kairon-node (consoleproxy
// /portforward) -> the guest. kaironctl is not a browser, so it sends its
// bearer token on the upgrade itself and no console ticket is needed.
// Access follows the console rules (consoleAuthorized): reaching a guest
// port is at least as sensitive as watching its screen.
func (s *Server) handlePortForward(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil || port == 0 {
		writeError(w, http.StatusBadRequest, "port must be 1-65535")
		return
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "the node relay is not enabled on this deployment (needs the console relay)")
		return
	}
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	username := usernameFromContext(r.Context())
	if !s.consoleAuthorized(r.Context(), m, username) {
		writeError(w, http.StatusForbidden, "not authorized to reach this machine")
		return
	}
	if m, _, err = s.ensureAwake(r.Context(), m); err != nil {
		writeWakeError(w, err)
		return
	}
	if m.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machine is not Running")
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
	q := url.Values{"port": {strconv.FormatUint(port, 10)}}
	if m.Status.GuestIP != "" {
		q.Set("ip", m.Status.GuestIP)
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/portforward/%s?%s", scheme, nodeAddr, s.ConsolePort, url.PathEscape(m.Status.RuntimeID), q.Encode())
	dialCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	upstream, resp, err := websocket.Dial(dialCtx, upstreamURL, dialOpts)
	cancel()
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		writeRelayDialError(w, "connect to node relay", resp, err)
		return
	}
	defer func() { _ = upstream.CloseNow() }()

	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Non-browser clients only: the route needs a bearer token, which
		// a browser cannot put on a WebSocket upgrade.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = downstream.CloseNow() }()
	if s.Log != nil {
		s.Log.Info("uiapi port-forward opened", "username", username, "namespace", namespace, "name", name, "port", port)
	}
	start := time.Now()
	ctx := r.Context()
	defer s.keepActive(m)()
	relay(
		websocket.NetConn(ctx, downstream, websocket.MessageBinary),
		websocket.NetConn(ctx, upstream, websocket.MessageBinary),
	)
	if s.Log != nil {
		s.Log.Info("uiapi port-forward closed", "username", username, "namespace", namespace, "name", name, "port", port, "duration", time.Since(start).Round(time.Second).String())
	}
	_ = downstream.Close(websocket.StatusNormalClosure, "")
}

// writeRelayDialError reports a failed kairon-node relay upgrade with the
// node's own explanation (e.g. "guest port 22 is not forwarded"), which the
// WebSocket error alone drops. A node 4xx keeps its status.
func writeRelayDialError(w http.ResponseWriter, prefix string, resp *http.Response, err error) {
	status, msg := http.StatusBadGateway, prefix+": "+err.Error()
	if resp != nil {
		if resp.Body != nil {
			if b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)); len(bytes.TrimSpace(b)) > 0 {
				msg = prefix + ": " + strings.TrimSpace(string(b))
			}
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			status = resp.StatusCode
		}
	}
	writeError(w, status, msg)
}
