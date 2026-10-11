// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Command kairon-migration-adapter-fluxvm is a real implementation of the
// Kairon migration adapter contract (docs/migration-adapter.md), backed by
// FluxVM's adopt-mode migration receivers (POST /v1/migration/receivers
// with the source's VM record, then activate, then adopt at commit) and
// its source-side QEMU migration REST API
// (/v1/vms/{id}/migration/{start,status,cancel,tune,postcopy,finish}).
// Unlike kairon-migration-adapter-stub, this adapter drives a real QEMU
// migration. FluxVM's storage contract v1 requires shared storage: the
// same disk path must be reachable from both the source and target FluxVM
// instances (see fluxvm's docs/runtime-boundary.md).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zyvorai/kairon/internal/migration"
)

// --- FluxVM REST API client (only the endpoints this adapter needs) ---

type fluxClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newFluxClient(baseURL, token string) *fluxClient {
	return &fluxClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *fluxClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("fluxvm %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode fluxvm response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// receiverRequest mirrors the fields of FluxVM's MigrationReceiverRequest
// this adapter sets. With `record` (the source's own VM record) FluxVM
// launches an adopt-mode receiver with the source's device model, which
// `adopt` later turns into a running VM.
type receiverRequest struct {
	Record           json.RawMessage    `json:"record"`
	ListenHost       string             `json:"listen_host,omitempty"`
	AdvertiseHost    string             `json:"advertise_host,omitempty"`
	ExpiresInSeconds uint64             `json:"expires_in_seconds,omitempty"`
	Tls              *migrationTLS      `json:"tls,omitempty"`
	Capabilities     streamCapabilities `json:"capabilities"`
}

// streamCapabilities mirrors FluxVM's MigrationStreamCapabilities.
type streamCapabilities struct {
	MultifdChannels uint8        `json:"multifd_channels,omitempty"`
	Compression     *compression `json:"compression,omitempty"`
	PostCopy        bool         `json:"post_copy,omitempty"`
}

type compression struct {
	Algorithm string `json:"algorithm"`
	Level     *uint8 `json:"level,omitempty"`
}

// receiverInfo is the subset of FluxVM's MigrationReceiver this adapter reads.
type receiverInfo struct {
	ID    string `json:"id"`
	URI   string `json:"uri"`
	Token string `json:"token"`
	// Capabilities is absent from FluxVM releases that predate stream
	// negotiation.
	Capabilities *streamCapabilities `json:"capabilities"`
}

type receiverToken struct {
	Token string `json:"token"`
}

type autoConverge struct {
	InitialPct   uint8 `json:"initial_pct,omitempty"`
	IncrementPct uint8 `json:"increment_pct,omitempty"`
	MaxPct       uint8 `json:"max_pct,omitempty"`
}

type tuneRequest struct {
	BandwidthMbps           uint64 `json:"bandwidth_mbps,omitempty"`
	MaxDowntimeMs           uint64 `json:"max_downtime_ms,omitempty"`
	CPUThrottleInitialPct   uint8  `json:"cpu_throttle_initial_pct,omitempty"`
	CPUThrottleIncrementPct uint8  `json:"cpu_throttle_increment_pct,omitempty"`
	MaxCPUThrottlePct       uint8  `json:"max_cpu_throttle_pct,omitempty"`
}

// runtimeCapabilities is the part of FluxVM's GET /v1/runtime/capabilities
// this adapter reads.
type runtimeCapabilities struct {
	Migration []struct {
		Backend             string   `json:"backend"`
		Live                bool     `json:"live"`
		AutoConverge        bool     `json:"autoConverge"`
		Compression         []string `json:"compression"`
		PostCopySwitch      bool     `json:"postCopySwitch"`
		LiveTune            bool     `json:"liveTune"`
		PostCopyWithMultifd bool     `json:"postCopyWithMultifd"`
	} `json:"migration"`
}

// migrationTLS mirrors FluxVM's MigrationTlsSpec. TLSHostname is meaningful
// only for the source (client) side -- the receiver never verifies a
// hostname, so prepare() leaves it empty.
type migrationTLS struct {
	CaPath      string `json:"ca_path"`
	CertPath    string `json:"cert_path"`
	KeyPath     string `json:"key_path"`
	TLSHostname string `json:"tls_hostname,omitempty"`
}

type migrationStartRequest struct {
	Destination     string        `json:"destination"`
	Mode            string        `json:"mode,omitempty"`
	BandwidthMbps   uint64        `json:"bandwidth_mbps,omitempty"`
	MaxDowntimeMs   uint64        `json:"max_downtime_ms,omitempty"`
	MultifdChannels uint8         `json:"multifd_channels,omitempty"`
	Tls             *migrationTLS `json:"tls,omitempty"`
	AutoConverge    *autoConverge `json:"auto_converge,omitempty"`
	Compression     *compression  `json:"compression,omitempty"`
	PostCopy        string        `json:"post_copy,omitempty"`
}

// migrationStatus mirrors FluxVM's MigrationStatus JSON shape.
type migrationStatus struct {
	Phase          string `json:"phase"`
	Status         string `json:"status"`
	RAMTransferred uint64 `json:"ram_transferred,omitempty"`
	RAMRemaining   uint64 `json:"ram_remaining,omitempty"`
	RAMTotal       uint64 `json:"ram_total,omitempty"`
	TotalTimeMs    uint64 `json:"total_time_ms,omitempty"`
	DowntimeMs     uint64 `json:"downtime_ms,omitempty"`
	Error          string `json:"error,omitempty"`

	DirtyPagesRate     uint64  `json:"dirty_pages_rate,omitempty"`
	DirtySyncCount     uint64  `json:"dirty_sync_count,omitempty"`
	PageSize           uint64  `json:"page_size,omitempty"`
	DuplicatePages     uint64  `json:"duplicate_pages,omitempty"`
	NormalBytes        uint64  `json:"normal_bytes,omitempty"`
	Mbps               float64 `json:"mbps,omitempty"`
	ExpectedDowntimeMs uint64  `json:"expected_downtime_ms,omitempty"`
	CPUThrottlePct     uint64  `json:"cpu_throttle_pct,omitempty"`
	SetupTimeMs        uint64  `json:"setup_time_ms,omitempty"`
	PostcopyRequests   uint64  `json:"postcopy_requests,omitempty"`
}

// fluxPhaseToKairon translates FluxVM's MigrationPhase (capitalized, e.g.
// "Completed") into the lowercase phase strings agent.go's projectTransfer
// already understands ("completed"/"transferred", "failed",
// "cancelled"/"canceled", "aborted", else treated as still running).
func fluxPhaseToKairon(phase string) string {
	switch strings.ToLower(phase) {
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "cancelled", "canceled":
		return "cancelled"
	default:
		return "running"
	}
}

func toTransferStatus(transferID string, s migrationStatus) migration.TransferStatus {
	return migration.TransferStatus{
		TransferID:     transferID,
		Phase:          fluxPhaseToKairon(s.Phase),
		Message:        s.Error,
		RAMTransferred: s.RAMTransferred,
		RAMRemaining:   s.RAMRemaining,
		RAMTotal:       s.RAMTotal,
		TotalTimeMs:    s.TotalTimeMs,
		DowntimeMs:     s.DowntimeMs,
		PostCopyActive: strings.EqualFold(s.Phase, "postcopy-active") || strings.HasPrefix(s.Status, "postcopy"),
		Telemetry: &migration.Telemetry{
			DirtyPagesRate:     s.DirtyPagesRate,
			DirtySyncCount:     s.DirtySyncCount,
			PageSize:           s.PageSize,
			DuplicatePages:     s.DuplicatePages,
			NormalBytes:        s.NormalBytes,
			Mbps:               s.Mbps,
			ExpectedDowntimeMs: s.ExpectedDowntimeMs,
			CPUThrottlePct:     s.CPUThrottlePct,
			SetupTimeMs:        s.SetupTimeMs,
			PostcopyRequests:   s.PostcopyRequests,
		},
	}
}

// --- adapter server: implements the documented Unix-socket contract ---

type adapter struct {
	log            *slog.Logger
	flux           *fluxClient
	advertiseHost  string
	bridge         string
	receiverTTLSec uint64
	// migrationNetworks maps a migration network name (MachineMigrationSpec.MigrationNetwork)
	// to the literal IP the receiver should bind/advertise on, configured
	// via repeated -migration-network name=ip flags. A session with no
	// MigrationNetwork set ignores this map entirely (advertiseHost/0.0.0.0
	// default behavior, unchanged).
	migrationNetworks map[string]string
	// tlsCA/tlsCert/tlsKey, when all non-empty (gated by -migration-data-tls),
	// are paths readable on THIS host that both prepare() (receiver) and
	// sourceStart() (source) hand to FluxVM, which materializes them into
	// the filenames QEMU's tls-creds-x509 object expects. nil/empty means
	// today's plain-transport behavior, unchanged.
	tlsCA, tlsCert, tlsKey string

	mu         sync.Mutex
	destByID   map[string]*destReceiver // session.ID -> fluxvm receiver
	sourceRTID map[string]string        // session.ID -> source runtimeID (for status/abort)

	// adoptWait bounds how long commit retries adopt while FluxVM reports
	// the incoming side still finishing (409).
	adoptWait time.Duration
}

type destReceiver struct {
	id        string
	token     string
	committed bool
}

func newAdapter(log *slog.Logger, flux *fluxClient, advertiseHost, bridge string, ttl uint64, migrationNetworks map[string]string, tlsCA, tlsCert, tlsKey string) *adapter {
	return &adapter{
		log: log, flux: flux, advertiseHost: advertiseHost, bridge: bridge, receiverTTLSec: ttl,
		migrationNetworks: migrationNetworks,
		tlsCA:             tlsCA, tlsCert: tlsCert, tlsKey: tlsKey,
		destByID:   map[string]*destReceiver{},
		sourceRTID: map[string]string{},
		adoptWait:  15 * time.Second,
	}
}

// migrationTLSSpec builds a *migrationTLS from the adapter's configured
// cert paths, or nil when TLS is disabled (tlsCA empty). hostname is only
// meaningful for the source side; pass "" for the receiver.
func (a *adapter) migrationTLSSpec(hostname string) *migrationTLS {
	if a.tlsCA == "" {
		return nil
	}
	return &migrationTLS{CaPath: a.tlsCA, CertPath: a.tlsCert, KeyPath: a.tlsKey, TLSHostname: hostname}
}

func (a *adapter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/destination/prepare", a.prepare)
	mux.HandleFunc("POST /v1/destination/{id}/commit", a.destCommit)
	mux.HandleFunc("POST /v1/destination/{id}/abort", a.destAbort)
	mux.HandleFunc("POST /v1/source/start", a.sourceStart)
	mux.HandleFunc("GET /v1/source/{sessionID}/{transferID}", a.sourceStatus)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/abort", a.sourceAbort)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/tune", a.sourceTune)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/postcopy", a.sourcePostCopy)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/finish", a.sourceFinish)
	mux.HandleFunc("GET /v1/capabilities", a.capabilities)
	return http.MaxBytesHandler(mux, 1<<20)
}

func (a *adapter) prepare(w http.ResponseWriter, r *http.Request) {
	var req migration.PrepareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	session := req.Session
	if len(session.SourceRecord) == 0 {
		writeJSON(w, http.StatusOK, migration.PrepareResult{
			TransferSupported: false,
			Reason:            "session carries no sourceRecord; the source kairon-node must be upgraded to send its FluxVM record",
		})
		return
	}
	backend := session.Backend
	if backend == "" {
		backend = "qemu"
	}
	advertiseHost := a.advertiseHost
	listenHost := ""
	if session.MigrationNetwork != "" {
		ip, ok := a.migrationNetworks[session.MigrationNetwork]
		if !ok {
			writeJSON(w, http.StatusOK, migration.PrepareResult{
				TransferSupported: false,
				Reason:            fmt.Sprintf("unknown migrationNetwork %q (not configured via -migration-network on this adapter)", session.MigrationNetwork),
			})
			return
		}
		listenHost = ip
		advertiseHost = ip
	}
	receiverReq := receiverRequest{
		Record:           session.SourceRecord,
		ListenHost:       listenHost,
		AdvertiseHost:    advertiseHost,
		ExpiresInSeconds: a.receiverTTLSec,
		Tls:              a.migrationTLSSpec(""),
		Capabilities:     toStreamCapabilities(session.Stream),
	}
	var info receiverInfo
	if _, err := a.flux.do(r.Context(), http.MethodPost, "/v1/migration/receivers", receiverReq, &info); err != nil {
		a.log.Error("fluxvm: create receiver failed", "session", session.ID, "error", err)
		writeJSON(w, http.StatusOK, migration.PrepareResult{TransferSupported: false, Reason: err.Error()})
		return
	}
	// The receiver only listens once activated; the source dials right
	// after prepare returns.
	if _, err := a.flux.do(r.Context(), http.MethodPost, "/v1/migration/receivers/"+info.ID+"/activate", receiverToken{Token: info.Token}, nil); err != nil {
		a.log.Error("fluxvm: activate receiver failed", "session", session.ID, "receiverID", info.ID, "error", err)
		a.deleteReceiver(r.Context(), session.ID, info.ID)
		writeJSON(w, http.StatusOK, migration.PrepareResult{TransferSupported: false, Reason: "activate receiver: " + err.Error()})
		return
	}
	a.mu.Lock()
	a.destByID[session.ID] = &destReceiver{id: info.ID, token: info.Token}
	a.mu.Unlock()
	a.log.Info("fluxvm: receiver armed", "session", session.ID, "receiverID", info.ID, "uri", info.URI, "migrationNetwork", session.MigrationNetwork)
	writeJSON(w, http.StatusOK, migration.PrepareResult{
		TransferSupported:    true,
		Endpoint:             info.URI,
		Backend:              backend,
		DataPlaneEncrypted:   a.tlsCA != "",
		DestinationRuntimeID: info.ID,
		Stream:               fromStreamCapabilities(info.Capabilities),
	})
}

func fromStreamCapabilities(s *streamCapabilities) *migration.StreamCapabilities {
	if s == nil {
		return nil
	}
	out := &migration.StreamCapabilities{MultifdChannels: s.MultifdChannels, PostCopy: s.PostCopy}
	if s.Compression != nil {
		out.Compression = &migration.Compression{Algorithm: s.Compression.Algorithm, Level: s.Compression.Level}
	}
	return out
}

func toStreamCapabilities(s *migration.StreamCapabilities) streamCapabilities {
	if s == nil {
		return streamCapabilities{}
	}
	out := streamCapabilities{MultifdChannels: s.MultifdChannels, PostCopy: s.PostCopy}
	if s.Compression != nil {
		out.Compression = &compression{Algorithm: s.Compression.Algorithm, Level: s.Compression.Level}
	}
	return out
}

func (a *adapter) deleteReceiver(ctx context.Context, sessionID, receiverID string) {
	if _, err := a.flux.do(ctx, http.MethodDelete, "/v1/migration/receivers/"+receiverID, nil, nil); err != nil {
		a.log.Warn("fluxvm: receiver delete failed", "session", sessionID, "receiverID", receiverID, "error", err)
	}
}

// destCommit adopts the receiver: FluxVM turns it into a running VM under
// the receiver's id. Idempotent; retries while the incoming side is still
// finishing (409).
func (a *adapter) destCommit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	recv, ok := a.destByID[id]
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown destination session %q", id))
		return
	}
	if recv.committed {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	deadline := time.Now().Add(a.adoptWait)
	for {
		code, err := a.flux.do(r.Context(), http.MethodPost, "/v1/migration/receivers/"+recv.id+"/adopt", receiverToken{Token: recv.token}, nil)
		if err == nil {
			break
		}
		if code != http.StatusConflict || time.Now().After(deadline) {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		select {
		case <-r.Context().Done():
			writeError(w, http.StatusBadGateway, r.Context().Err())
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	a.mu.Lock()
	recv.committed = true
	a.mu.Unlock()
	a.log.Info("fluxvm: receiver adopted", "session", id, "runtimeID", recv.id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *adapter) destAbort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	recv, ok := a.destByID[id]
	if ok && recv.committed {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("destination session %q is already committed", id))
		return
	}
	delete(a.destByID, id)
	a.mu.Unlock()
	if ok {
		a.deleteReceiver(r.Context(), id, recv.id)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *adapter) sourceStart(w http.ResponseWriter, r *http.Request) {
	var req migration.SourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.RuntimeID == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("source request missing runtimeID"))
		return
	}
	mode := req.Options.Mode
	if mode == "" {
		mode = "pre-copy"
	}
	startReq := migrationStartRequest{
		Destination:     req.Endpoint,
		Mode:            mode,
		BandwidthMbps:   req.Options.BandwidthMbps,
		MaxDowntimeMs:   req.Options.MaxDowntimeMs,
		MultifdChannels: req.Options.MultifdChannels,
		Tls:             a.migrationTLSSpec(endpointHost(req.Endpoint)),
		PostCopy:        req.Options.PostCopy,
	}
	if ac := req.Options.AutoConverge; ac != nil {
		startReq.AutoConverge = &autoConverge{InitialPct: ac.InitialPct, IncrementPct: ac.IncrementPct, MaxPct: ac.MaxPct}
	}
	if c := req.Options.Compression; c != nil {
		startReq.Compression = &compression{Algorithm: c.Algorithm, Level: c.Level}
	}
	var status migrationStatus
	if _, err := a.flux.do(r.Context(), http.MethodPost, "/v1/vms/"+req.RuntimeID+"/migration/start", startReq, &status); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	transferID := req.Session.ID // FluxVM's migration state is keyed by the source VM id, not a separate transfer id -- reuse the session id as our own handle.
	a.mu.Lock()
	a.sourceRTID[transferID] = req.RuntimeID
	a.mu.Unlock()
	a.log.Info("fluxvm: source migration started", "session", req.Session.ID, "runtimeID", req.RuntimeID, "destination", req.Endpoint)
	writeJSON(w, http.StatusOK, toTransferStatus(transferID, status))
}

func (a *adapter) sourceStatus(w http.ResponseWriter, r *http.Request) {
	transferID := r.PathValue("transferID")
	a.mu.Lock()
	runtimeID, ok := a.sourceRTID[transferID]
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown transfer %q", transferID))
		return
	}
	var status migrationStatus
	if code, err := a.flux.do(r.Context(), http.MethodGet, "/v1/vms/"+runtimeID+"/migration/status", nil, &status); err != nil {
		// A 4xx means FluxVM no longer has a running source VM; kairon-node
		// treats that as a lost source once post-copy has started.
		if code >= 400 && code < 500 {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransferStatus(transferID, status))
}

func (a *adapter) sourceAbort(w http.ResponseWriter, r *http.Request) {
	transferID := r.PathValue("transferID")
	a.mu.Lock()
	runtimeID, ok := a.sourceRTID[transferID]
	a.mu.Unlock()
	if ok {
		if _, err := a.flux.do(r.Context(), http.MethodPost, "/v1/vms/"+runtimeID+"/migration/cancel", nil, nil); err != nil {
			a.log.Warn("fluxvm: source migration cancel failed", "runtimeID", runtimeID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *adapter) sourceRuntime(w http.ResponseWriter, r *http.Request) (string, bool) {
	transferID := r.PathValue("transferID")
	a.mu.Lock()
	runtimeID, ok := a.sourceRTID[transferID]
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown transfer %q", transferID))
	}
	return runtimeID, ok
}

func (a *adapter) sourceTune(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := a.sourceRuntime(w, r)
	if !ok {
		return
	}
	var req migration.TuneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.IsZero() {
		writeError(w, http.StatusBadRequest, fmt.Errorf("tune request sets nothing"))
		return
	}
	var status migrationStatus
	if _, err := a.flux.do(r.Context(), http.MethodPost, "/v1/vms/"+runtimeID+"/migration/tune", tuneRequest(req), &status); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	a.log.Info("fluxvm: migration tuned", "runtimeID", runtimeID, "tune", req)
	writeJSON(w, http.StatusOK, toTransferStatus(r.PathValue("transferID"), status))
}

func (a *adapter) sourcePostCopy(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := a.sourceRuntime(w, r)
	if !ok {
		return
	}
	var status migrationStatus
	if code, err := a.flux.do(r.Context(), http.MethodPost, "/v1/vms/"+runtimeID+"/migration/postcopy", map[string]any{}, &status); err != nil {
		// A FluxVM 4xx means it refused before touching QEMU; anything else
		// may have switched, and Kairon must assume it did.
		if code >= 400 && code < 500 {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	a.log.Info("fluxvm: migration switched to post-copy", "runtimeID", runtimeID)
	writeJSON(w, http.StatusOK, toTransferStatus(r.PathValue("transferID"), status))
}

// sourceFinish stops the paused source QEMU and removes its FluxVM record
// once the destination has committed. The shared disk stays.
func (a *adapter) sourceFinish(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := a.sourceRuntime(w, r)
	if !ok {
		return
	}
	code, err := a.flux.do(r.Context(), http.MethodPost, "/v1/vms/"+runtimeID+"/migration/finish", map[string]any{}, nil)
	if err != nil && code != http.StatusNotFound {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	a.mu.Lock()
	delete(a.sourceRTID, r.PathValue("transferID"))
	a.mu.Unlock()
	a.log.Info("fluxvm: source runtime finished", "runtimeID", runtimeID)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *adapter) capabilities(w http.ResponseWriter, r *http.Request) {
	var rc runtimeCapabilities
	if _, err := a.flux.do(r.Context(), http.MethodGet, "/v1/runtime/capabilities", nil, &rc); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	out := migration.Capabilities{Finish: true}
	for _, m := range rc.Migration {
		if m.Backend != "qemu" {
			continue
		}
		out.Tune = m.LiveTune
		out.PostCopySwitch = m.PostCopySwitch
		out.AutoConverge = m.AutoConverge
		out.Compression = m.Compression
		out.PostCopyWithMultifd = m.PostCopyWithMultifd
	}
	writeJSON(w, http.StatusOK, out)
}

// endpointHost extracts the host from a "tcp:host:port" migration endpoint
// URI (the shape prepare() builds and sourceStart() receives back), for use
// as the TLS client's expected server hostname. Returns "" if endpoint
// isn't a recognizable tcp: URI (e.g. a non-FluxVM adapter's own scheme) --
// the caller then sends no tls_hostname, which FluxVM's receiver-side
// verify-peer still enforces via the CA, just without hostname pinning.
func endpointHost(endpoint string) string {
	rest, ok := strings.CutPrefix(endpoint, "tcp:")
	if !ok {
		return ""
	}
	host, _, err := net.SplitHostPort(rest)
	if err != nil {
		return ""
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// migrationNetworkFlag accumulates repeated -migration-network name=ip
// flags into a name -> IP map.
type migrationNetworkFlag map[string]string

func (m migrationNetworkFlag) String() string {
	parts := make([]string, 0, len(m))
	for name, ip := range m {
		parts = append(parts, name+"="+ip)
	}
	return strings.Join(parts, ",")
}

func (m migrationNetworkFlag) Set(value string) error {
	name, ip, ok := strings.Cut(value, "=")
	if !ok || name == "" || ip == "" {
		return fmt.Errorf("expected name=ip, got %q", value)
	}
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP %q for migration network %q", ip, name)
	}
	m[name] = ip
	return nil
}

func main() {
	os.Exit(run())
}

// run returns the process exit code rather than calling os.Exit directly,
// so every deferred cleanup (e.g. cancel()) actually runs before exit.
func run() int {
	socket := flag.String("socket", "", "Unix socket path to listen on (required)")
	fluxURL := flag.String("fluxvm-url", "http://127.0.0.1:8080", "local FluxVM REST API base URL")
	fluxToken := flag.String("fluxvm-token", os.Getenv("FLUXVM_TOKEN"), "FluxVM admin Bearer token (default: $FLUXVM_TOKEN)")
	advertiseHost := flag.String("advertise-host", "127.0.0.1", "address migration peers should use to reach this host's receivers")
	bridge := flag.String("bridge", "virbr0", "ignored; receivers reuse the source VM's own network spec")
	receiverTTL := flag.Uint64("receiver-ttl-seconds", 300, "how long an unclaimed receiver is kept before FluxVM reclaims it")
	migrationNetworks := make(migrationNetworkFlag)
	flag.Var(migrationNetworks, "migration-network", "name=ip mapping a MachineMigration's migrationNetwork to the address the receiver binds/advertises (repeatable)")
	dataTLS := flag.Bool("migration-data-tls", false, "authenticate/encrypt the QEMU migration data stream via TLS (requires -migration-ca/-migration-cert/-migration-key)")
	tlsCA := flag.String("migration-ca", os.Getenv("KAIRON_MIGRATION_CA"), "CA cert path for migration data-plane TLS (default: $KAIRON_MIGRATION_CA)")
	tlsCert := flag.String("migration-cert", os.Getenv("KAIRON_MIGRATION_CERT"), "cert path for migration data-plane TLS (default: $KAIRON_MIGRATION_CERT)")
	tlsKey := flag.String("migration-key", os.Getenv("KAIRON_MIGRATION_KEY"), "key path for migration data-plane TLS (default: $KAIRON_MIGRATION_KEY)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *socket == "" {
		log.Error("--socket is required")
		return 2
	}
	if *dataTLS && (*tlsCA == "" || *tlsCert == "" || *tlsKey == "") {
		log.Error("-migration-data-tls is fail-closed: -migration-ca, -migration-cert and -migration-key must all be set")
		return 2
	}
	if !*dataTLS {
		// Never silently enable TLS just because cert flags happen to be
		// set -- rollout is opt-in via -migration-data-tls (see ROADMAP
		// v0.4's ordering: flip clusters over once every node's adapter
		// and FluxVM binary is upgraded, not implicitly).
		*tlsCA, *tlsCert, *tlsKey = "", "", ""
		log.Warn("migration data-plane TLS is DISABLED: QEMU RAM/state transfer will cross the network in CLEARTEXT " +
			"(this is separate from kairon-node's own control-plane RPCs, which are always mTLS-encrypted); " +
			"set -migration-data-tls plus -migration-ca/-migration-cert/-migration-key to enable it")
	}

	flux := newFluxClient(*fluxURL, *fluxToken)
	a := newAdapter(log, flux, *advertiseHost, *bridge, *receiverTTL, migrationNetworks, *tlsCA, *tlsCert, *tlsKey)

	_ = os.Remove(*socket)
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		log.Error("listen", "error", err)
		return 1
	}
	if err := os.Chmod(*socket, 0o660); err != nil {
		log.Error("chmod socket", "error", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	srv := &http.Server{Handler: a.handler()}
	go func() {
		<-ctx.Done()
		c, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = srv.Shutdown(c)
	}()

	log.Info("kairon-migration-adapter-fluxvm listening", "socket", *socket, "fluxvmURL", *fluxURL)
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "error", err)
		return 1
	}
	return 0
}
