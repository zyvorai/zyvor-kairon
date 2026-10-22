// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package consoleproxy is kairon-node's relay for operations that need a
// network path only this node has: it authenticates a request from
// kairon-ui with a shared bearer token, then either (VNC) pumps raw bytes
// between a WebSocket and the VM's otherwise-unreachable local QEMU VNC
// socket (<workspace>/vnc.sock -- see crates/fluxvm-qemu/src/lib.rs in the
// FluxVM repo), (exec) forwards a guest-exec request straight to FluxVM's
// own REST API and relays its synchronous JSON result back, or (text
// console) dials FluxVM's own WebSocket-upgraded interactive shell
// endpoint as a client and relays raw bytes between it and the browser's
// own WebSocket, the same shape as VNC but with FluxVM itself as the
// upstream instead of a Unix socket. kairon-node is always co-located
// with the FluxVM it manages (FLUXVM_URL is a loopback address by
// convention everywhere in this repo), so it's the only place that can
// reach any of the three at all.
package consoleproxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/diskexport"
	"github.com/zyvorai/kairon/internal/fluxvm"
)

// Server relays VNC bytes and guest-exec calls for VMs running on this
// node. Token is a shared secret kairon-ui presents -- there is no
// per-operator identity here, that check already happened in kairon-ui
// (internal/uiapi/console.go) before the request ever reaches this node.
type Server struct {
	Flux    *fluxvm.Client
	Token   string
	Exports *diskexport.Registry
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /console/{runtimeID}", s.handleConsole)
	mux.HandleFunc("POST /exec/{runtimeID}", s.handleExec)
	mux.HandleFunc("GET /text-console/{runtimeID}", s.handleTextConsole)
	mux.HandleFunc("GET /guestfs/{runtimeID}", s.handleGuestfs)
	mux.HandleFunc("GET /portforward/{runtimeID}", s.handlePortForward)
	mux.HandleFunc("POST /agent-file/put/{runtimeID}", s.handleAgentPutFile)
	mux.HandleFunc("POST /agent-file/get/{runtimeID}", s.handleAgentGetFile)
	mux.HandleFunc("POST /agent-exec/{runtimeID}", s.handleAgentExec)
	mux.HandleFunc("GET /qga-fsfreeze-status/{runtimeID}", s.handleQGAFsfreezeStatus)
	mux.HandleFunc("POST /qga-firewall/open/{runtimeID}", s.handleQGAFirewallOpen)
	mux.HandleFunc("POST /qga-firewall/close/{runtimeID}", s.handleQGAFirewallClose)
	mux.HandleFunc("GET /logs/{runtimeID}", s.handleLogs)
	mux.HandleFunc("GET /backup-root/{name}", s.handleBackupRoot)
	mux.HandleFunc("GET /export/{namespace}/{name}", s.handleExport)
	mux.HandleFunc("POST /vm-snapshot/{runtimeID}", s.handleVMSnapshot)
	mux.HandleFunc("POST /vm-restore-snapshot/{runtimeID}", s.handleVMRestoreSnapshot)
	mux.HandleFunc("GET /sandboxes", s.handleListSandboxes)
	mux.HandleFunc("GET /templates", s.handleListTemplates)
	mux.HandleFunc("POST /templates", s.handleBuildTemplate)
	mux.HandleFunc("/sandbox-proxy/{runtimeID}/{port}/{rest...}", s.handleSandboxHTTPProxy)
	mux.HandleFunc("GET /catalog", s.handleListCatalog)
	mux.HandleFunc("POST /catalog", s.handleAddCatalogEntry)
	mux.HandleFunc("DELETE /catalog/{name}", s.handleRemoveCatalogEntry)
	mux.HandleFunc("POST /catalog/{name}/rename", s.handleRenameCatalogEntry)
	mux.HandleFunc("POST /catalog/{name}/clone", s.handleCloneCatalogEntry)
	mux.HandleFunc("POST /catalog/{name}/export", s.handleExportCatalogEntry)
	mux.HandleFunc("POST /catalog/{name}/read-only", s.handleSetCatalogReadOnly)
	mux.HandleFunc("POST /catalog/clean", s.handleCleanCatalogDownloads)
	mux.HandleFunc("POST /egress-check", s.handleEgressCheck)
	mux.HandleFunc("POST /pools", s.handleCreatePool)
	mux.HandleFunc("GET /pools", s.handleListPools)
	mux.HandleFunc("GET /pools/{name}", s.handleGetPool)
	mux.HandleFunc("DELETE /pools/{name}", s.handleDeletePool)
	mux.HandleFunc("POST /pools/{name}/claim", s.handleClaimPool)
	mux.HandleFunc("GET /capabilities", s.handleRuntimeCapabilities)
	mux.HandleFunc("GET /pressure/{runtimeID}", s.handlePressure)
	mux.HandleFunc("GET /cpuset/{runtimeID}", s.handleCPUSet)
	mux.HandleFunc("POST /freeze/{runtimeID}", s.handleFreeze)
	mux.HandleFunc("POST /thaw/{runtimeID}", s.handleThaw)
	mux.HandleFunc("GET /frozen/{runtimeID}", s.handleFrozen)
	mux.HandleFunc("GET /network-effective/{runtimeID}", s.handleNetworkEffective)
	mux.HandleFunc("GET /network-stats/{runtimeID}", s.handleNetworkStats)
	mux.HandleFunc("GET /network-flows/{runtimeID}", s.handleNetworkFlows)
	mux.HandleFunc("GET /network-drop-reasons/{runtimeID}", s.handleNetworkDropReasons)
	mux.HandleFunc("GET /network-drops/{runtimeID}", s.handleNetworkDrops)
	mux.HandleFunc("POST /network-capture/{runtimeID}", s.handleNetworkCapture)
	mux.HandleFunc("GET /network-capture/{runtimeID}", s.handleNetworkCaptures)
	mux.HandleFunc("GET /network-capture/{runtimeID}/{token}", s.handleNetworkCaptureFile)
	mux.HandleFunc("POST /diagnostics/{runtimeID}", s.handleStartDiagnostics)
	mux.HandleFunc("GET /diagnostics/{runtimeID}/{token}", s.handleGetDiagnostics)
	mux.HandleFunc("DELETE /diagnostics/{runtimeID}/{token}", s.handleDeleteDiagnostics)
	mux.HandleFunc("GET /diagnostics/{runtimeID}/{token}/{file}", s.handleDiagnosticsFile)
	mux.HandleFunc("POST /fork-plan/{runtimeID}", s.handleForkPlan)
	mux.HandleFunc("GET /host-memory", s.handleHostMemory)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /vm-screenshot/{runtimeID}", s.handleVMScreenshot)
	mux.HandleFunc("POST /vm-input/{runtimeID}", s.handleVMInput)
	mux.HandleFunc("GET /events/stream", s.handleEventsStream)
	mux.HandleFunc("GET /vm-egress/{runtimeID}", s.handleVMEgress)
	mux.HandleFunc("PUT /vm-egress/{runtimeID}", s.handleVMEgress)
	mux.HandleFunc("DELETE /vm-egress/{runtimeID}", s.handleVMEgress)
	mux.HandleFunc("GET /vm-egress-observed/{runtimeID}", s.handleVMEgressObserved)
	mux.HandleFunc("POST /vm-egress-suggest/{runtimeID}", s.handleVMEgressSuggest)
	mux.HandleFunc("GET /egress-defaults", s.handleEgressDefaults)
	mux.HandleFunc("PUT /egress-defaults", s.handleEgressDefaults)
	mux.HandleFunc("GET /host-topology", s.handleHostTopology)
	mux.HandleFunc("GET /host-pressure", s.handleHostPressure)
	mux.HandleFunc("GET /egress-nftables", s.handleEgressNftables)
	mux.HandleFunc("POST /branches/{id}/start", s.handleBranchStart)
	mux.HandleFunc("DELETE /branches/{id}", s.handleBranchDelete)
	mux.HandleFunc("GET /branches", s.handleListBranches)
	mux.HandleFunc("GET /branches/{a}/diff/{b}", s.handleBranchDiff)
	mux.HandleFunc("POST /branches/{id}/start-plan", s.handleBranchStartPlan)
	mux.HandleFunc("/agent-sessions/{id}", s.handleAgentSession)
	mux.HandleFunc("/agent-sessions/{id}/{rest...}", s.handleAgentSession)
	mux.HandleFunc("/agent-preview/{token}/{rest...}", s.handleAgentPreview)
	return mux
}

// checkToken reports whether r carries the correct shared bearer token,
// writing a 401 and returning false otherwise.
func (s *Server) checkToken(w http.ResponseWriter, r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(got) != len(s.Token) || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
		http.Error(w, "invalid or missing bearer token", http.StatusUnauthorized)
		return false
	}
	return true
}

// execRequest/execResponse mirror internal/fluxvm.QGAExecRequest/Result's
// own JSON shape -- kept as separate types rather than reusing those
// directly so this package's wire contract with kairon-ui doesn't change
// just because internal/fluxvm's Go-side struct layout does.
type execRequest struct {
	Path           string   `json:"path,omitempty"`
	Args           []string `json:"args,omitempty"`
	Powershell     string   `json:"powershell,omitempty"`
	TimeoutSeconds *uint64  `json:"timeoutSeconds,omitempty"`
}

type execResponse struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// handleExec forwards a guest-exec request to FluxVM's own
// POST /v1/vms/{id}/qga/exec and relays its synchronous result back as
// plain JSON -- unlike VNC, there's no long-lived connection to relay
// here, just one request and one response.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	result, err := s.Flux.QGAExec(ctx, r.PathValue("runtimeID"), fluxvm.QGAExecRequest{
		Path: req.Path, Args: req.Args, Powershell: req.Powershell, TimeoutSeconds: req.TimeoutSeconds,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("guest exec: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(execResponse{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr})
}

// fsfreezeStatusResponse/firewallRequest/firewallResponse mirror
// internal/fluxvm's own QGAFsfreezeStatus/QGAFirewallOpen/Close request
// and result shapes -- separate types for the same reason
// execRequest/execResponse above are.
type fsfreezeStatusResponse struct {
	Status string `json:"status"`
}

type firewallOpenRequest struct {
	Name           string  `json:"name"`
	Port           uint16  `json:"port"`
	Protocol       string  `json:"protocol,omitempty"`
	TimeoutSeconds *uint64 `json:"timeoutSeconds,omitempty"`
}

type firewallCloseRequest struct {
	Name           string  `json:"name"`
	TimeoutSeconds *uint64 `json:"timeoutSeconds,omitempty"`
}

type firewallResponse struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// handleQGAFsfreezeStatus forwards to FluxVM's own
// GET /v1/vms/{id}/qga/fsfreeze-status -- a read-only diagnostic
// confirming what qemu-guest-agent itself currently reports, rather than
// inferring guest filesystem state from Kairon's own quiesce annotations
// (internal/agent/quiesce.go).
func (s *Server) handleQGAFsfreezeStatus(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	status, err := s.Flux.QGAFsfreezeStatus(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("qga fsfreeze-status: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(fsfreezeStatusResponse{Status: status})
}

// handleQGAFirewallOpen/handleQGAFirewallClose forward to FluxVM's own
// qemu-guest-agent-backed firewall toggle -- same synchronous
// request/response shape as handleExec, since FluxVM itself implements
// both as a guest-exec call under the hood.
func (s *Server) handleQGAFirewallOpen(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req firewallOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	result, err := s.Flux.QGAFirewallOpen(ctx, r.PathValue("runtimeID"), req.Name, req.Port, req.Protocol, req.TimeoutSeconds)
	if err != nil {
		http.Error(w, fmt.Sprintf("qga firewall open: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(firewallResponse{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr})
}

func (s *Server) handleQGAFirewallClose(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	relayFluxRequest(w, r, "qga firewall close",
		func(ctx context.Context, req firewallCloseRequest) (*fluxvm.QGAExecResult, error) {
			return s.Flux.QGAFirewallClose(ctx, r.PathValue("runtimeID"), req.Name, req.TimeoutSeconds)
		},
		func(result *fluxvm.QGAExecResult) firewallResponse {
			return firewallResponse{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}
		})
}

// relayFluxRequest is shared by handleQGAFirewallClose and
// handleAgentExec (near-identical shape otherwise): decode the request
// body, run call with a bounded timeout, and encode call's result via
// toResponse. errLabel is the http.Error prefix used only on a
// Flux-side failure, matching each handler's own previous message.
func relayFluxRequest[Req, Result, Resp any](w http.ResponseWriter, r *http.Request, errLabel string, call func(ctx context.Context, req Req) (Result, error), toResponse func(Result) Resp) {
	var req Req
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	result, err := call(ctx, req)
	if err != nil {
		http.Error(w, fmt.Sprintf("%s: %v", errLabel, err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(toResponse(result))
}

// agentPutFileRequest/agentGetFileRequest/agentFileResponse mirror
// internal/fluxvm's AgentPutFile/AgentGetFile own request/result shapes --
// separate types for the same reason execRequest/execResponse above are.
type agentPutFileRequest struct {
	Path          string  `json:"path"`
	ContentBase64 string  `json:"contentBase64"`
	Mode          *uint32 `json:"mode,omitempty"`
}

type agentGetFileRequest struct {
	Path string `json:"path"`
}

type agentFileResponse struct {
	ContentBase64 string `json:"contentBase64,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
}

// handleAgentPutFile forwards a file write to FluxVM's own bespoke vsock
// guest agent (POST /v1/vms/{id}/agent/put-file) -- a different channel
// from qemu-guest-agent's guest-exec above, requiring
// spec.guestAgent.console rather than spec.guestAgent.enabled. One
// request, one response, same shape as handleExec.
func (s *Server) handleAgentPutFile(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req agentPutFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	if err := s.Flux.AgentPutFile(ctx, r.PathValue("runtimeID"), req.Path, req.ContentBase64, req.Mode); err != nil {
		http.Error(w, fmt.Sprintf("agent put-file: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(agentFileResponse{})
}

// handleAgentGetFile is handleAgentPutFile's read counterpart, forwarding
// to FluxVM's own POST /v1/vms/{id}/agent/get-file.
func (s *Server) handleAgentGetFile(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req agentGetFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	result, err := s.Flux.AgentGetFile(ctx, r.PathValue("runtimeID"), req.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("agent get-file: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(agentFileResponse{ContentBase64: result.ContentBase64, Mode: result.Mode})
}

// agentExecRequest/agentExecResponse mirror internal/fluxvm's AgentExec's
// own request/result shape -- separate types for the same reason
// execRequest/execResponse are.
type agentExecRequest struct {
	Command        string  `json:"command"`
	TimeoutSeconds *uint64 `json:"timeoutSeconds,omitempty"`
}

type agentExecResponse struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// handleAgentExec forwards a guest-exec request to FluxVM's own bespoke
// vsock guest agent (POST /v1/vms/{id}/agent) -- a different channel from
// qemu-guest-agent's handleExec above (backend-agnostic, requiring
// spec.guestAgent.console rather than spec.guestAgent.enabled). One
// request, one response, same shape as handleExec.
func (s *Server) handleAgentExec(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	relayFluxRequest(w, r, "agent exec",
		func(ctx context.Context, req agentExecRequest) (*fluxvm.AgentExecResult, error) {
			return s.Flux.AgentExec(ctx, r.PathValue("runtimeID"), req.Command, req.TimeoutSeconds)
		},
		func(result *fluxvm.AgentExecResult) agentExecResponse {
			return agentExecResponse{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}
		})
}

// vmSnapshotRequest/vmSnapshotResponse mirror the shape of every other
// request/response pair in this package -- a separate wire type from
// internal/fluxvm's own Snapshot/RestoreSnapshot signatures for the same
// reason execRequest/execResponse are.
type vmSnapshotRequest struct {
	Tag string `json:"tag"`
}

type vmSnapshotResponse struct {
	Status string `json:"status"`
}

// handleVMSnapshot forwards to FluxVM's own POST /v1/vms/{id}/snapshot --
// a full hypervisor-level checkpoint of the VM's running state (RAM, CPU,
// device state), never a disk-content-only copy (that's CSI's own
// CreateSnapshot, internal/csinode, unrelated to this route). The VM is
// never stopped or restarted by this call.
func (s *Server) handleVMSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req vmSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Tag == "" {
		http.Error(w, "tag is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	if err := s.Flux.Snapshot(ctx, r.PathValue("runtimeID"), req.Tag); err != nil {
		http.Error(w, fmt.Sprintf("vm snapshot: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(vmSnapshotResponse{Status: "ok"})
}

// handleVMRestoreSnapshot forwards to fluxvm.Client.RestoreSnapshot, which
// itself orchestrates FluxVM's own stop -> start-from-snapshot sequence
// (see internal/fluxvm/hibernate.go's own doc comments for why a plain
// start-from-snapshot call alone isn't enough). Unlike handleVMSnapshot,
// this always stops the VM first -- the caller (internal/uiapi) gates this
// behind admin authorization given that risk, the same posture as guest
// exec and agent file access.
func (s *Server) handleVMRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req vmSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Tag == "" {
		http.Error(w, "tag is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayTimeout)
	defer cancel()
	rec, err := s.Flux.RestoreSnapshot(ctx, r.PathValue("runtimeID"), req.Tag)
	if err != nil {
		http.Error(w, fmt.Sprintf("vm restore snapshot: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(vmSnapshotResponse{Status: rec.Status})
}

// execRelayTimeout bounds how long this node waits on FluxVM's own
// synchronous guest-exec call -- comfortably above FluxVM's own 60s
// default guest-side timeout (internal/fluxvm.QGAExecRequest.TimeoutSeconds),
// so a caller's explicit, longer timeout still has room to actually apply
// rather than being cut short by this relay hop first.
const execRelayTimeout = 5 * time.Minute

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	runtimeID := r.PathValue("runtimeID")
	rec, err := s.Flux.Get(r.Context(), runtimeID)
	if err != nil {
		http.Error(w, fmt.Sprintf("lookup runtime: %v", err), http.StatusBadGateway)
		return
	}
	if rec.Workspace == "" {
		http.Error(w, "runtime has no workspace (not a QEMU VM, or FluxVM predates workspace reporting)", http.StatusBadRequest)
		return
	}
	sockPath := rec.Workspace + "/vnc.sock"
	sock, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial vnc socket: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = sock.Close() }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// This endpoint is only ever dialed server-to-server by kairon-ui,
		// never directly by a browser, so there is no browser Origin to
		// check against.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ws := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
	relay(ws, sock)
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// fluxWebSocketURL turns c's own BaseURL (http(s)://host:port, exactly
// what every other fluxvm.Client REST call already uses) into the
// equivalent ws(s):// URL for path -- FluxVM upgrades this same base
// address's console route to a WebSocket rather than serving it on a
// separate port.
func fluxWebSocketURL(c *fluxvm.Client, path, rawQuery string) string {
	u := strings.Replace(strings.Replace(c.BaseURL, "https://", "wss://", 1), "http://", "ws://", 1)
	if rawQuery != "" {
		return u + path + "?" + rawQuery
	}
	return u + path
}

// handleTextConsole relays a browser WebSocket straight through to
// FluxVM's own interactive-shell WebSocket
// (GET /v1/vms/{id}/console, requires spec.guestAgent.console --
// FluxVM's proprietary vsock guest agent, a different channel from the
// qemu-guest-agent handleExec above uses) -- kairon-node dials FluxVM as
// a WebSocket client here, the only leg of this whole feature FluxVM
// itself doesn't expose as a plain Unix socket the way VNC's QEMU display
// is.
func (s *Server) handleTextConsole(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	runtimeID := r.PathValue("runtimeID")
	upstreamURL := fluxWebSocketURL(s.Flux, "/v1/vms/"+url.PathEscape(runtimeID)+"/console", r.URL.RawQuery)
	dialOpts := &websocket.DialOptions{HTTPClient: s.Flux.HTTP}
	if s.Flux.Token != "" {
		dialOpts.HTTPHeader = http.Header{"Authorization": {"Bearer " + s.Flux.Token}}
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	upstream, resp, err := websocket.Dial(dialCtx, upstreamURL, dialOpts)
	cancel()
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("dial fluxvm text console: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.CloseNow() }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Server-to-server only, same reasoning as handleConsole's own
		// InsecureSkipVerify above.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	relay(
		websocket.NetConn(r.Context(), conn, websocket.MessageBinary),
		websocket.NetConn(r.Context(), upstream, websocket.MessageBinary),
	)
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// handleLogs streams a VM's captured serial console output straight
// through from FluxVM's own GET /v1/vms/{id}/logs -- Kairon's `kubectl
// logs` equivalent. Unlike every other route in this package, this one
// deliberately isn't a WebSocket: FluxVM's own endpoint is already a
// plain chunked text/plain stream (`?follow=true` never terminates on
// FluxVM's side until the client disconnects), so relaying it as a plain
// HTTP response with periodic flushing is the simplest thing that
// actually preserves that shape -- no framing, no upgrade handshake
// needed for what's inherently a unidirectional text stream. Bounded
// entirely by r.Context(): when the browser tab closes or the fetch is
// aborted, that cancellation propagates through kairon-ui and this hop
// straight to FluxVM's own stream, with no server-side timeout of its own.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	upstreamURL := s.Flux.BaseURL + "/v1/vms/" + url.PathEscape(r.PathValue("runtimeID")) + "/logs"
	if q := r.URL.RawQuery; q != "" {
		upstreamURL += "?" + q
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.Flux.Token != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+s.Flux.Token)
	}
	resp, err := s.Flux.HTTP.Do(upstreamReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial fluxvm logs: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		http.Error(w, fmt.Sprintf("fluxvm logs: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data))), http.StatusBadGateway)
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
			return
		}
	}
}

// relay pumps bytes in both directions until either side closes; it
// returns once the first direction stops, at which point the caller's
// deferred closes tear down the other side too.
func relay(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// handleBackupRoot streams a FluxVM backup's standalone root qcow2 (GET
// /v1/backups/{name}/root), e.g. a MachineBackup Veyron publishes as a
// golden image. Multi-GB, so it bypasses the FluxVM client's request
// timeout and lives only as long as the caller's request.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	s.Exports.Serve(w, r, diskexport.Key(r.PathValue("namespace"), r.PathValue("name")))
}

func (s *Server) handleBackupRoot(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	upstreamURL := s.Flux.BaseURL + "/v1/backups/" + url.PathEscape(r.PathValue("name")) + "/root"
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.Flux.Token != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+s.Flux.Token)
	}
	hc := &http.Client{Transport: s.Flux.HTTP.Transport}
	resp, err := hc.Do(upstreamReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial fluxvm backup: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusNotFound {
			status = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf("fluxvm backup: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data))), status)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}
