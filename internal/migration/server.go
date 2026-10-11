// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Store    Store
	Driver   DestinationDriver
	NodeName string
	Now      func() time.Time
	// HeartbeatTTL bounds how long a "Prepared" session may go without a
	// heartbeat before RunReaper/ReapStaleSessions treats the source as
	// gone and self-aborts it, releasing whatever Driver.Prepare reserved
	// -- see the ROADMAP.md entry this closes. Zero (the default) disables
	// reaping entirely: no behavior change from before this existed.
	HeartbeatTTL time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-session-ID, lazily created; never cleaned up (see lockFor)
}

// lockFor serializes transition() (commit/abort) and reapOne for the same
// session ID, so a reap can never abort a session while a legitimate
// commit/abort is concurrently in flight for it (see ReapStaleSessions'
// own doc comment for the exact race this closes). Heartbeat and read-only
// handlers deliberately don't take this lock -- they never call the
// (possibly slow) Driver, and a racing UpdatedAt bump is harmless
// last-write-wins. The lock map is never garbage-collected: session IDs
// are bounded by real migrations that actually occur, not
// attacker-controlled at volume, so this is an acceptable simplicity
// tradeoff rather than a real leak.
func (s *Server) lockFor(id string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/migrations/prepare", s.prepare)
	mux.HandleFunc("GET /internal/v1/migrations/{id}", s.get)
	mux.HandleFunc("POST /internal/v1/migrations/{id}/commit", s.commit)
	mux.HandleFunc("POST /internal/v1/migrations/{id}/abort", s.abort)
	mux.HandleFunc("POST /internal/v1/migrations/{id}/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /internal/v1/migrations/{id}/diagnosis", s.diagnosis)
	return http.MaxBytesHandler(mux, 8<<20)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) dependencies() error {
	if s.Store == nil {
		return errors.New("migration store is not configured")
	}
	if s.Driver == nil {
		return errors.New("migration destination driver is not configured")
	}
	return nil
}

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	if err := s.dependencies(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	var req PrepareRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode prepare request: %w", err))
		return
	}
	if err := ensureEOF(dec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := req.Session.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.NodeName != "" && req.Session.TargetNode != s.NodeName {
		writeError(w, http.StatusForbidden, fmt.Errorf("session targets node %q but this peer is %q", req.Session.TargetNode, s.NodeName))
		return
	}

	existing, err := s.Store.Get(req.Session.ID)
	if err == nil {
		if !existing.SameIdentity(req.Session) {
			writeError(w, http.StatusConflict, errors.New("session id already exists with different immutable identity"))
			return
		}
		writeJSON(w, http.StatusOK, response(existing))
		return
	}
	if !errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	result, err := s.Driver.Prepare(r.Context(), req.Session)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("prepare destination: %w", err))
		return
	}
	if result.TransferSupported && strings.TrimSpace(result.Endpoint) == "" {
		writeError(w, http.StatusBadGateway, errors.New("destination driver claimed support but returned an empty endpoint"))
		return
	}
	now := s.now()
	session := req.Session
	session.CreatedAt, session.UpdatedAt = now, now
	session.TransferSupported = result.TransferSupported
	session.Endpoint = result.Endpoint
	session.Backend = result.Backend
	session.Reason = result.Reason
	session.DataPlaneEncrypted = result.DataPlaneEncrypted
	session.DestinationRuntimeID = result.DestinationRuntimeID
	if result.TransferSupported {
		session.Phase = "Prepared"
	} else {
		session.Phase = "Unsupported"
	}
	if err := s.Store.Put(session); err != nil {
		_ = s.Driver.Abort(r.Context(), session)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, response(session))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("migration store is not configured"))
		return
	}
	session, err := s.Store.Get(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, response(session))
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "Committed", func(session Session) error { return s.Driver.Commit(r.Context(), session) })
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "Aborted", func(session Session) error { return s.Driver.Abort(r.Context(), session) })
}

func (s *Server) transition(w http.ResponseWriter, r *http.Request, desired string, fn func(Session) error) {
	if err := s.dependencies(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer s.lockFor(r.PathValue("id"))()
	session, err := s.Store.Get(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if session.Phase == desired {
		writeJSON(w, http.StatusOK, response(session))
		return
	}
	if desired == "Committed" && session.Phase != "Prepared" {
		writeError(w, http.StatusConflict, fmt.Errorf("cannot commit session in phase %s", session.Phase))
		return
	}
	if desired == "Aborted" && session.Phase == "Committed" {
		writeError(w, http.StatusConflict, errors.New("cannot abort a committed session"))
		return
	}
	if err := fn(session); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	session.Phase = desired
	session.UpdatedAt = s.now()
	if err := s.Store.Put(session); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, response(session))
}

// heartbeat renews a "Prepared" session's UpdatedAt so ReapStaleSessions
// doesn't treat its source as gone. A no-op (not an error) on an
// already-terminal session -- the source learns the real outcome through
// its own Commit/Abort call result or Diagnose, not through this
// best-effort liveness ping. Deliberately doesn't take lockFor: it never
// calls the (possibly slow) Driver, and a racing UpdatedAt write is
// harmless last-write-wins.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("migration store is not configured"))
		return
	}
	id := r.PathValue("id")
	session, err := s.Store.Get(id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if session.Phase == "Prepared" {
		session.UpdatedAt = s.now()
		if err := s.Store.Put(session); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response(session))
}

// ReapStaleSessions aborts every "Prepared" session whose heartbeat is
// older than HeartbeatTTL, on the working theory that a source this quiet
// for this long has crashed or been fenced between a successful Prepare
// and ever calling Commit/Abort -- see ROADMAP.md's entry for the gap this
// closes. A no-op when HeartbeatTTL is zero (the default), Store is nil,
// or Driver is nil.
//
// Safe to call concurrently with itself and with Commit/Abort landing for
// a DIFFERENT session. For the SAME session, reapOne's lockFor call makes
// it impossible to race a commit/abort in flight for that session: without
// it, this function's own List()-then-check could observe a session still
// reading Phase="Prepared" with a stale UpdatedAt at the exact moment
// transition() is between its own Store.Get and Store.Put -- i.e. between
// a real Driver.Commit succeeding and that success being persisted -- and
// wrongly abort a destination that is, at that instant, legitimately
// coming up. reapOne re-fetches and re-checks staleness AFTER acquiring
// the same per-session lock transition() holds for its own entire
// Get-Driver-call-Put critical section, closing that window completely.
func (s *Server) ReapStaleSessions(ctx context.Context) error {
	if s.HeartbeatTTL <= 0 || s.Store == nil || s.Driver == nil {
		return nil
	}
	sessions, err := s.Store.List()
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	now := s.now()
	for _, session := range sessions {
		if session.Phase != "Prepared" || now.Sub(session.UpdatedAt) < s.HeartbeatTTL {
			continue
		}
		s.reapOne(ctx, session.ID, now)
	}
	return nil
}

func (s *Server) reapOne(ctx context.Context, id string, now time.Time) {
	defer s.lockFor(id)()
	// Re-fetch and re-check staleness UNDER the lock -- a heartbeat or a
	// legitimate commit/abort may have landed between ReapStaleSessions'
	// own List() call (lock-free) and here.
	session, err := s.Store.Get(id)
	if err != nil || session.Phase != "Prepared" || now.Sub(session.UpdatedAt) < s.HeartbeatTTL {
		return
	}
	if err := s.Driver.Abort(ctx, session); err != nil {
		return // best-effort; retried next reap tick
	}
	session.Phase = "Aborted"
	session.Reason = fmt.Sprintf("heartbeat timeout: no renewal for over %s, presumed source failure", s.HeartbeatTTL)
	session.UpdatedAt = s.now()
	_ = s.Store.Put(session) // best-effort; retried next tick on failure
}

// RunReaper calls ReapStaleSessions once per interval until ctx is
// cancelled. The caller (cmd/kairon-node) owns this goroutine's lifecycle,
// matching this codebase's existing pattern (see Agent.Run).
func (s *Server) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.ReapStaleSessions(ctx) // errors are transient store I/O; retried next tick
		}
	}
}

// diagnosis is strictly read-only (never mutates Store) so it's always
// safe to poll, including on every NeedsRecovery reconcile tick -- it
// answers "what do we actually know" without requiring or implying any
// recovery action has been decided.
func (s *Server) diagnosis(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("migration store is not configured"))
		return
	}
	session, err := s.Store.Get(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	diagnosable, ok := s.Driver.(Diagnosable)
	if !ok {
		writeJSON(w, http.StatusOK, DiagnosisResult{SessionPhase: session.Phase, ObservedAt: s.now()})
		return
	}
	result, err := diagnosable.Diagnose(r.Context(), session)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("diagnose destination: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func response(session Session) PrepareResponse {
	return PrepareResponse{SessionID: session.ID, Phase: session.Phase, TransferSupported: session.TransferSupported, Endpoint: session.Endpoint, Backend: session.Backend, Reason: session.Reason, DataPlaneEncrypted: session.DataPlaneEncrypted}
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request must contain exactly one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
