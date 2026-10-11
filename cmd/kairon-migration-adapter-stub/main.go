// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Command kairon-migration-adapter-stub is a test double for the Kairon
// migration adapter contract documented in docs/migration-adapter.md. It
// implements the destination and source HTTP endpoints over a Unix socket so
// the live-migration control plane (mTLS peer prepare/commit/abort, source
// start/status/abort) can be exercised end to end without a real
// hypervisor-level adapter, which does not exist yet for FluxVM. It never
// moves any actual VM memory: the "transfer" is a timed simulation.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zyvorai/kairon/internal/migration"
)

const simulatedRAMTotal = 512 << 20 // 512MiB, matches the small test Machine used to exercise this stub.

type destSession struct {
	session migration.Session
	phase   string // "prepared", "committed", "aborted"
}

type transfer struct {
	mu     sync.Mutex
	status migration.TransferStatus
}

type stub struct {
	log *slog.Logger

	mu    sync.Mutex
	dest  map[string]*destSession
	xfers map[string]*transfer // key: sessionID+"/"+transferID
}

func newStub(log *slog.Logger) *stub {
	return &stub{log: log, dest: map[string]*destSession{}, xfers: map[string]*transfer{}}
}

func (s *stub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/destination/prepare", s.prepare)
	mux.HandleFunc("POST /v1/destination/{id}/commit", s.destCommit)
	mux.HandleFunc("POST /v1/destination/{id}/abort", s.destAbort)
	mux.HandleFunc("POST /v1/source/start", s.sourceStart)
	mux.HandleFunc("GET /v1/source/{sessionID}/{transferID}", s.sourceStatus)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/abort", s.sourceAbort)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/tune", s.sourceTune)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/postcopy", s.sourcePostCopy)
	mux.HandleFunc("POST /v1/source/{sessionID}/{transferID}/finish", s.sourceFinish)
	mux.HandleFunc("GET /v1/capabilities", s.capabilities)
	return http.MaxBytesHandler(mux, 1<<20)
}

func (s *stub) prepare(w http.ResponseWriter, r *http.Request) {
	var req migration.PrepareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	s.dest[req.Session.ID] = &destSession{session: req.Session, phase: "prepared"}
	s.mu.Unlock()
	s.log.Info("stub: destination prepare", "session", req.Session.ID, "machine", req.Session.Machine)
	writeJSON(w, http.StatusOK, migration.PrepareResult{
		TransferSupported: true,
		Endpoint:          "stub://" + req.Session.ID,
		Backend:           req.Session.Backend,
	})
}

func (s *stub) destCommit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	d, ok := s.dest[id]
	if ok {
		d.phase = "committed"
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown destination session"))
		return
	}
	s.log.Info("stub: destination commit", "session", id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *stub) destAbort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	if d, ok := s.dest[id]; ok {
		d.phase = "aborted"
	}
	s.mu.Unlock()
	s.log.Info("stub: destination abort", "session", id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *stub) sourceStart(w http.ResponseWriter, r *http.Request) {
	var req migration.SourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	transferID := randomID()
	key := req.Session.ID + "/" + transferID
	t := &transfer{status: migration.TransferStatus{TransferID: transferID, Phase: "running", RAMTotal: simulatedRAMTotal}}
	s.mu.Lock()
	s.xfers[key] = t
	s.mu.Unlock()
	s.log.Info("stub: source start", "session", req.Session.ID, "transferID", transferID, "runtimeID", req.RuntimeID)
	go s.simulateTransfer(t)
	writeJSON(w, http.StatusOK, t.snapshot())
}

func (s *stub) simulateTransfer(t *transfer) {
	const steps = 8
	const stepDelay = 750 * time.Millisecond
	start := time.Now()
	for i := 1; i <= steps; i++ {
		time.Sleep(stepDelay)
		t.mu.Lock()
		t.status.RAMTransferred = uint64(i) * (simulatedRAMTotal / steps)
		t.status.RAMRemaining = simulatedRAMTotal - t.status.RAMTransferred
		t.mu.Unlock()
	}
	t.mu.Lock()
	t.status.Phase = "completed"
	t.status.RAMTransferred = simulatedRAMTotal
	t.status.RAMRemaining = 0
	t.status.TotalTimeMs = uint64(time.Since(start).Milliseconds())
	t.status.DowntimeMs = 50
	t.mu.Unlock()
}

func (t *transfer) snapshot() migration.TransferStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (s *stub) sourceStatus(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("sessionID") + "/" + r.PathValue("transferID")
	s.mu.Lock()
	t, ok := s.xfers[key]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown transfer"))
		return
	}
	writeJSON(w, http.StatusOK, t.snapshot())
}

func (s *stub) sourceAbort(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("sessionID") + "/" + r.PathValue("transferID")
	s.mu.Lock()
	if t, ok := s.xfers[key]; ok {
		t.mu.Lock()
		t.status.Phase = "aborted"
		t.mu.Unlock()
	}
	s.mu.Unlock()
	s.log.Info("stub: source abort", "key", key)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *stub) transferFor(w http.ResponseWriter, r *http.Request) (*transfer, bool) {
	key := r.PathValue("sessionID") + "/" + r.PathValue("transferID")
	s.mu.Lock()
	t, ok := s.xfers[key]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown transfer"))
	}
	return t, ok
}

// sourceTune records the throttle the way QEMU reports it back, so callers
// can see a tune land in the telemetry.
func (s *stub) sourceTune(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferFor(w, r)
	if !ok {
		return
	}
	var req migration.TuneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t.mu.Lock()
	if t.status.Telemetry == nil {
		t.status.Telemetry = &migration.Telemetry{}
	}
	if req.MaxCPUThrottlePct > 0 {
		t.status.Telemetry.CPUThrottlePct = uint64(req.MaxCPUThrottlePct)
	}
	t.mu.Unlock()
	s.log.Info("stub: source tune", "transferID", r.PathValue("transferID"), "tune", req)
	writeJSON(w, http.StatusOK, t.snapshot())
}

func (s *stub) sourcePostCopy(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferFor(w, r)
	if !ok {
		return
	}
	t.mu.Lock()
	t.status.PostCopyActive = true
	t.mu.Unlock()
	s.log.Info("stub: source post-copy", "transferID", r.PathValue("transferID"))
	writeJSON(w, http.StatusOK, t.snapshot())
}

func (s *stub) sourceFinish(w http.ResponseWriter, r *http.Request) {
	s.log.Info("stub: source finish", "transferID", r.PathValue("transferID"))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *stub) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, migration.Capabilities{
		Tune: true, PostCopySwitch: true, AutoConverge: true,
		Compression: []string{"zlib", "zstd"}, Finish: true,
	})
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func main() {
	os.Exit(run())
}

// run returns the process exit code rather than calling os.Exit directly,
// so every deferred cleanup (e.g. cancel()) actually runs before exit.
func run() int {
	socket := flag.String("socket", "", "Unix socket path to listen on (required)")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *socket == "" {
		log.Error("--socket is required")
		return 2
	}

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

	srv := &http.Server{Handler: newStub(log).handler()}
	go func() {
		<-ctx.Done()
		c, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = srv.Shutdown(c)
	}()

	log.Info("kairon-migration-adapter-stub listening", "socket", *socket)
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "error", err)
		return 1
	}
	return 0
}
