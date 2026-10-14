// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

// portForwardTarget is the address kairon-node dials for a TCP port in
// the guest. The address always comes from FluxVM's record of the VM or
// from the guest agent (agentIPs), never from the caller: hint (the
// Machine's status.guestIP) only picks among those, so this relay cannot
// be pointed anywhere else. A user-mode (SLIRP) guest is not routable
// from the host: only ports it forwards (spec.network.forwards) are
// reachable, through loopback.
func portForwardTarget(rec *fluxvm.Record, port uint16, agentIPs []string, hint string) (string, error) {
	mode := rec.Request.Network.Mode
	if mode == "" || mode == "user" {
		for _, f := range rec.Request.Network.Forwards {
			if f.GuestPort == port && (f.Protocol == "" || f.Protocol == "tcp") {
				return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(f.HostPort))), nil
			}
		}
		return "", fmt.Errorf("guest port %d is not forwarded: a user-mode Machine needs a spec.network.forwards entry for it", port)
	}
	if rec.Request.Network.NetNS {
		return "", fmt.Errorf("the guest sits in its own network namespace; port-forward needs a guest reachable from the host")
	}
	var candidates []string
	if rec.GuestIP != "" {
		candidates = append(candidates, rec.GuestIP)
	}
	candidates = append(candidates, agentIPs...)
	if len(candidates) == 0 {
		return "", fmt.Errorf("the guest has no known address yet")
	}
	ip := candidates[0]
	for _, c := range candidates {
		if hint != "" && c == hint {
			ip = c
			break
		}
	}
	return net.JoinHostPort(ip, strconv.Itoa(int(port))), nil
}

// handlePortForward relays one TCP connection to a port in the guest over
// a WebSocket, for kaironctl port-forward/ssh/scp through kairon-ui.
func (s *Server) handlePortForward(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil || port == 0 {
		http.Error(w, "port must be 1-65535", http.StatusBadRequest)
		return
	}
	rec, err := s.Flux.Get(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("lookup runtime: %v", err), http.StatusBadGateway)
		return
	}
	var agentIPs []string
	if rec.GuestIP == "" {
		if ifaces, err := s.Flux.QGANetworkInterfaces(r.Context(), rec.ID()); err == nil {
			agentIPs = fluxvm.AllGuestIPs(ifaces)
		}
	}
	addr, err := portForwardTarget(rec, uint16(port), agentIPs, r.URL.Query().Get("ip"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	upstream, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial guest %s: %v", addr, err), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Dialed server-to-server by kairon-ui only, like the VNC relay.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	relay(websocket.NetConn(r.Context(), conn, websocket.MessageBinary), upstream)
	_ = conn.Close(websocket.StatusNormalClosure, "")
}
