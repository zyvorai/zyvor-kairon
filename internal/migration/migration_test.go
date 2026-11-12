// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeDestination struct {
	prepare PrepareResult
	prepN   int
	commitN int
	abortN  int
	commitE error
}

func (f *fakeDestination) Prepare(context.Context, Session) (PrepareResult, error) {
	f.prepN++
	return f.prepare, nil
}
func (f *fakeDestination) Commit(context.Context, Session) error { f.commitN++; return f.commitE }
func (f *fakeDestination) Abort(context.Context, Session) error  { f.abortN++; return nil }

func testSession() Session {
	return Session{ID: "kmm-123", Namespace: "prod", Machine: "db", SourceNode: "node-a", TargetNode: "node-b", RuntimeID: "vm-1"}
}

func TestPrepareIsIdempotentAndConflictsOnIdentityChange(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123", Backend: "fake"}}
	s := &Server{NodeName: "node-b", Store: NewFileStore(t.TempDir()), Driver: d}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := NewClient(ts.Client())

	first, err := c.Prepare(context.Background(), ts.URL, testSession())
	if err != nil || !first.TransferSupported || first.Phase != "Prepared" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := c.Prepare(context.Background(), ts.URL, testSession())
	if err != nil || second.SessionID != first.SessionID || d.prepN != 1 {
		t.Fatalf("second=%+v err=%v prepares=%d", second, err, d.prepN)
	}
	changed := testSession()
	changed.Machine = "other"
	if _, err := c.Prepare(context.Background(), ts.URL, changed); err == nil {
		t.Fatal("expected identity conflict")
	}
}

func TestPrepareRejectsWrongTargetNode(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	ts := httptest.NewServer((&Server{NodeName: "node-c", Store: NewFileStore(t.TempDir()), Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err == nil {
		t.Fatal("expected wrong target node to be rejected")
	}
	if d.prepN != 0 {
		t.Fatalf("driver prepare called %d times", d.prepN)
	}
}

func TestCommitAndAbortLifecycle(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	ts := httptest.NewServer((&Server{Store: store, Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(context.Background(), ts.URL, "kmm-123"); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(context.Background(), ts.URL, "kmm-123"); err != nil {
		t.Fatalf("commit must be idempotent: %v", err)
	}
	if d.commitN != 1 {
		t.Fatalf("commit count=%d", d.commitN)
	}
	if err := c.Abort(context.Background(), ts.URL, "kmm-123"); err == nil {
		t.Fatal("committed session must not be abortable")
	}
}

type fakeDiagnosableDestination struct {
	fakeDestination
	diagnosis DiagnosisResult
	diagErr   error
	diagCalls int
}

func (f *fakeDiagnosableDestination) Diagnose(context.Context, Session) (DiagnosisResult, error) {
	f.diagCalls++
	return f.diagnosis, f.diagErr
}

func TestDiagnosisFallsBackToSessionPhaseWhenDriverIsNotDiagnosable(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	ts := httptest.NewServer((&Server{Store: store, Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
		t.Fatal(err)
	}
	result, err := c.Diagnose(context.Background(), ts.URL, "kmm-123")
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionPhase != "Prepared" {
		t.Errorf("expected SessionPhase=Prepared, got %+v", result)
	}
	if result.DestinationRuntimeFound {
		t.Errorf("a non-Diagnosable driver must not report DestinationRuntimeFound, got %+v", result)
	}
}

func TestDiagnosisUsesDriverWhenAvailable(t *testing.T) {
	d := &fakeDiagnosableDestination{
		fakeDestination: fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}},
		diagnosis:       DiagnosisResult{SessionPhase: "Prepared", DestinationRuntimeFound: true, DestinationRuntimeStatus: "Running", DestinationRuntimeID: "vm-2"},
	}
	store := NewFileStore(t.TempDir())
	ts := httptest.NewServer((&Server{Store: store, Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
		t.Fatal(err)
	}
	result, err := c.Diagnose(context.Background(), ts.URL, "kmm-123")
	if err != nil {
		t.Fatal(err)
	}
	if d.diagCalls != 1 {
		t.Errorf("expected exactly one Diagnose call, got %d", d.diagCalls)
	}
	if result != d.diagnosis {
		t.Errorf("expected the driver's diagnosis to be returned verbatim, got %+v", result)
	}
}

func TestDiagnosisOnUnknownSessionIsNotFound(t *testing.T) {
	d := &fakeDestination{}
	ts := httptest.NewServer((&Server{Store: NewFileStore(t.TempDir()), Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Diagnose(context.Background(), ts.URL, "does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown session id")
	}
}

func TestUnsupportedDestinationIsPersistedWithoutEndpoint(t *testing.T) {
	d := UnsupportedDestinationDriver{Reason: "adapter absent"}
	store := NewFileStore(t.TempDir())
	ts := httptest.NewServer((&Server{Store: store, Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	got, err := c.Prepare(context.Background(), ts.URL, testSession())
	if err != nil {
		t.Fatal(err)
	}
	if got.TransferSupported || got.Phase != "Unsupported" || got.Endpoint != "" || got.Reason != "adapter absent" {
		t.Fatalf("response=%+v", got)
	}
}

func TestFileStoreRoundTripAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	s := testSession()
	s.Phase = "Prepared"
	if err := store.Put(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(s.ID)
	if err != nil || !got.SameIdentity(s) || got.Phase != "Prepared" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, s.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if _, err := store.Get("../escape"); err == nil {
		t.Fatal("expected traversal id rejection")
	}
}

func TestFileStoreList(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	if sessions, err := store.List(); err != nil || len(sessions) != 0 {
		t.Fatalf("expected empty list on a not-yet-created store, got %+v err=%v", sessions, err)
	}
	a, b := testSession(), testSession()
	a.ID, b.ID = "kmm-a", "kmm-b"
	a.Phase, b.Phase = "Prepared", "Committed"
	if err := store.Put(a); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(b); err != nil {
		t.Fatal(err)
	}
	// A corrupt/unparseable session file must be skipped, not fail the
	// whole listing.
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions (corrupt file skipped), got %d: %+v", len(sessions), sessions)
	}
	ids := map[string]bool{}
	for _, s := range sessions {
		ids[s.ID] = true
	}
	if !ids["kmm-a"] || !ids["kmm-b"] {
		t.Fatalf("missing expected session ids in %+v", sessions)
	}
}

func TestServerHeartbeatRenewsPreparedSession(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	ts := httptest.NewServer((&Server{Store: store, Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get("kmm-123")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := c.Heartbeat(context.Background(), ts.URL, "kmm-123"); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get("kmm-123")
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("expected UpdatedAt to advance: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
	if after.Phase != "Prepared" {
		t.Fatalf("heartbeat must not change phase, got %q", after.Phase)
	}
}

func TestServerHeartbeatIsANoOpOnTerminalPhases(t *testing.T) {
	for _, phase := range []string{"Committed", "Aborted", "Unsupported"} {
		t.Run(phase, func(t *testing.T) {
			d := &fakeDestination{prepare: PrepareResult{TransferSupported: phase != "Unsupported", Endpoint: "opaque://session/123"}}
			store := NewFileStore(t.TempDir())
			ts := httptest.NewServer((&Server{Store: store, Driver: d}).Handler())
			defer ts.Close()
			c := NewClient(ts.Client())
			if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "Committed":
				if err := c.Commit(context.Background(), ts.URL, "kmm-123"); err != nil {
					t.Fatal(err)
				}
			case "Aborted":
				if err := c.Abort(context.Background(), ts.URL, "kmm-123"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Get("kmm-123")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Heartbeat(context.Background(), ts.URL, "kmm-123"); err != nil {
				t.Fatalf("heartbeat on a terminal-phase session must not error: %v", err)
			}
			after, err := store.Get("kmm-123")
			if err != nil {
				t.Fatal(err)
			}
			if after.Phase != before.Phase || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("heartbeat must be a true no-op on a terminal phase: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestServerHeartbeat404OnUnknownSession(t *testing.T) {
	d := &fakeDestination{}
	ts := httptest.NewServer((&Server{Store: NewFileStore(t.TempDir()), Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if err := c.Heartbeat(context.Background(), ts.URL, "does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown session id")
	}
}

func TestReapStaleSessionsNoOpWhenHeartbeatTTLIsZero(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	s := &Server{Store: store, Driver: d, Now: func() time.Time { return time.Now().Add(24 * time.Hour) }}
	sess := testSession()
	sess.Phase = "Prepared"
	if err := s.Store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapStaleSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.abortN != 0 {
		t.Fatalf("HeartbeatTTL=0 must disable reaping entirely, got %d aborts", d.abortN)
	}
}

func TestReapStaleSessionsIgnoresFreshSessions(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	now := time.Now()
	s := &Server{Store: store, Driver: d, HeartbeatTTL: time.Minute, Now: func() time.Time { return now }}
	sess := testSession()
	sess.Phase = "Prepared"
	sess.UpdatedAt = now.Add(-10 * time.Second) // well within the 1-minute TTL
	if err := store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapStaleSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.abortN != 0 {
		t.Fatalf("a fresh session must not be reaped, got %d aborts", d.abortN)
	}
}

func TestReapStaleSessionsSkipsNonPreparedPhases(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	now := time.Now()
	s := &Server{Store: store, Driver: d, HeartbeatTTL: time.Minute, Now: func() time.Time { return now }}
	for _, phase := range []string{"Committed", "Aborted", "Unsupported"} {
		sess := testSession()
		sess.ID = "kmm-" + phase
		sess.Phase = phase
		sess.UpdatedAt = now.Add(-time.Hour) // ancient, but not "Prepared"
		if err := store.Put(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReapStaleSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.abortN != 0 {
		t.Fatalf("only Prepared sessions may be reaped, got %d aborts", d.abortN)
	}
}

func TestReapStaleSessionsAbortsExpiredPreparedSession(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	store := NewFileStore(t.TempDir())
	now := time.Now()
	s := &Server{Store: store, Driver: d, HeartbeatTTL: time.Minute, Now: func() time.Time { return now }}
	sess := testSession()
	sess.Phase = "Prepared"
	sess.UpdatedAt = now.Add(-2 * time.Minute) // stale: older than HeartbeatTTL
	if err := store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapStaleSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.abortN != 1 {
		t.Fatalf("expected exactly one abort, got %d", d.abortN)
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "Aborted" {
		t.Fatalf("expected Phase=Aborted after reaping, got %q", got.Phase)
	}
	if got.Reason == "" {
		t.Fatal("expected a Reason explaining the reap")
	}
}

// blockingDestination lets a test control exactly when Commit returns, so
// a concurrent ReapStaleSessions call can be forced to run WHILE a commit
// is genuinely in flight -- the exact interleaving the per-session lock
// (Server.lockFor) must make impossible to observe as a wrongly-aborted,
// actually-succeeding migration.
type blockingDestination struct {
	fakeDestination
	commitStarted chan struct{}
	releaseCommit chan struct{}
}

func (f *blockingDestination) Commit(ctx context.Context, session Session) error {
	close(f.commitStarted)
	<-f.releaseCommit
	return f.fakeDestination.Commit(ctx, session)
}

func TestReapStaleSessionsDoesNotRaceWithConcurrentCommit(t *testing.T) {
	d := &blockingDestination{
		fakeDestination: fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}},
		commitStarted:   make(chan struct{}),
		releaseCommit:   make(chan struct{}),
	}
	store := NewFileStore(t.TempDir())
	now := time.Now()
	s := &Server{Store: store, Driver: d, HeartbeatTTL: time.Minute, Now: func() time.Time { return now }}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
		t.Fatal(err)
	}
	// Artificially age the session well past HeartbeatTTL, as if its
	// source had gone silent -- except a commit is about to land for it.
	sess, err := store.Get("kmm-123")
	if err != nil {
		t.Fatal(err)
	}
	sess.UpdatedAt = now.Add(-2 * time.Minute)
	if err := store.Put(sess); err != nil {
		t.Fatal(err)
	}

	commitErr := make(chan error, 1)
	go func() { commitErr <- c.Commit(context.Background(), ts.URL, "kmm-123") }()

	select {
	case <-d.commitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("commit never started")
	}

	// Commit is now blocked inside Driver.Commit, holding the per-session
	// lock (transition() acquires it before calling fn()). A concurrent
	// reap pass's own reapOne must block on that SAME lock rather than
	// proceeding to see a stale-looking, still-"Prepared" session and
	// abort it out from under the in-flight commit -- that's precisely the
	// race this whole feature exists to close. So ReapStaleSessions is
	// expected to BLOCK here, not return immediately.
	reapDone := make(chan error, 1)
	go func() { reapDone <- s.ReapStaleSessions(context.Background()) }()

	select {
	case err := <-reapDone:
		t.Fatalf("ReapStaleSessions returned (err=%v) before the commit it should be blocked behind released its lock -- the per-session lock isn't working", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: still blocked on lockFor.
	}
	if d.abortN != 0 {
		t.Fatalf("reap must not abort a session with a commit genuinely in flight, got %d aborts", d.abortN)
	}

	close(d.releaseCommit)
	if err := <-commitErr; err != nil {
		t.Fatalf("commit failed: %v", err)
	}

	select {
	case err := <-reapDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReapStaleSessions never returned after the commit's lock was released")
	}

	final, err := store.Get("kmm-123")
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != "Committed" {
		t.Fatalf("expected the legitimate commit to win, never clobbered back to Aborted, got Phase=%q", final.Phase)
	}
	if d.commitN != 1 {
		t.Fatalf("expected exactly one commit, got %d", d.commitN)
	}
	if d.abortN != 0 {
		t.Fatalf("expected zero aborts: the reap re-checked under the lock and found the session already Committed, got %d", d.abortN)
	}
}

func TestServerTLSRequiresClientCertificate(t *testing.T) {
	dir := t.TempDir()
	ca, serverCert, serverKey, clientCert, clientKey := makeTLSFiles(t, dir)
	watcher, err := NewCertWatcher(slog.New(slog.NewTextHandler(io.Discard, nil)), ca, serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := ServerTLSConfig(ca, watcher)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.MinVersion != 0x0304 || tlsConfig.ClientAuth != 4 { // TLS1.3 / RequireAndVerifyClientCert
		t.Fatalf("unexpected tls config: min=%x auth=%v", tlsConfig.MinVersion, tlsConfig.ClientAuth)
	}

	// httptest.Server.StartTLS() auto-injects its own dummy certificate
	// whenever tlsConfig.Certificates is empty -- true here since the
	// production ServerTLSConfig now serves via the GetCertificate
	// callback instead, same as internal/tlsreload's whole point. Wire up
	// a real net.Listen + tls.NewListener server by hand instead, exactly
	// the same construction cmd/kairon-node/main.go's configureMigration
	// actually uses, so this test exercises the real GetCertificate path
	// rather than httptest's unrelated auto-cert convenience behavior.
	driver := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://ok"}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: (&Server{NodeName: "node-b", Store: NewFileStore(t.TempDir()), Driver: driver}).Handler(), TLSConfig: tlsConfig}
	tlsListener := tls.NewListener(listener, tlsConfig)
	go func() { _ = srv.Serve(tlsListener) }()
	defer func() { _ = srv.Close() }()
	url := "https://" + listener.Addr().String()

	pool := x509.NewCertPool()
	pemData, _ := os.ReadFile(ca)
	pool.AppendCertsFromPEM(pemData)
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLSForTest(pool, nil)}}
	if _, err := NewClient(noCert).Prepare(context.Background(), url, testSession()); err == nil {
		t.Fatal("expected peer without client certificate to be rejected")
	}
	pair, err := tlsLoad(clientCert, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLSForTest(pool, &pair)}}
	if _, err := NewClient(withCert).Prepare(context.Background(), url, testSession()); err != nil {
		t.Fatalf("mTLS client failed: %v", err)
	}
}

func clientTLSForTest(pool *x509.CertPool, cert *tls.Certificate) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return cfg
}

func tlsLoad(cert, key string) (tls.Certificate, error) { return tls.LoadX509KeyPair(cert, key) }

func makeTLSFiles(t *testing.T, dir string) (string, string, string, string, string) {
	t.Helper()
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now().Add(-time.Minute)
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kairon-test-ca"}, NotBefore: now, NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, caPath, "CERTIFICATE", caDER, 0o600)

	issue := func(serial int64, cn string, ips []net.IP, eku x509.ExtKeyUsage) (string, string) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		certT := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: now, NotAfter: now.Add(time.Hour), IPAddresses: ips, ExtKeyUsage: []x509.ExtKeyUsage{eku}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
		der, err := x509.CreateCertificate(rand.Reader, certT, caT, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		certPath := filepath.Join(dir, cn+".crt")
		keyPath := filepath.Join(dir, cn+".key")
		writePEM(t, certPath, "CERTIFICATE", der, 0o600)
		writePEM(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), 0o600)
		return certPath, keyPath
	}
	serverCert, serverKey := issue(2, "server", []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	clientCert, clientKey := issue(3, "client", nil, x509.ExtKeyUsageClientAuth)
	return caPath, serverCert, serverKey, clientCert, clientKey
}

func writePEM(t *testing.T, path, typ string, der []byte, mode os.FileMode) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// gateStore blocks the first Get after arm() until released, to hold a
// heartbeat between its read and write.
type gateStore struct {
	Store
	mu      sync.Mutex
	armed   bool
	reached chan struct{}
	release chan struct{}
}

func (g *gateStore) Get(id string) (Session, error) {
	sess, err := g.Store.Get(id)
	g.mu.Lock()
	hold := g.armed
	g.armed = false
	g.mu.Unlock()
	if hold {
		close(g.reached)
		<-g.release
	}
	return sess, err
}

// A heartbeat that read "Prepared" must not write it back over a commit that
// finished in between.
func TestHeartbeatDoesNotRevertConcurrentCommit(t *testing.T) {
	d := &fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://session/123"}}
	gs := &gateStore{Store: NewFileStore(t.TempDir()), reached: make(chan struct{}), release: make(chan struct{})}
	srv := &Server{Store: gs, Driver: d}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	if _, err := c.Prepare(context.Background(), ts.URL, testSession()); err != nil {
		t.Fatal(err)
	}
	gs.mu.Lock()
	gs.armed = true
	gs.mu.Unlock()
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); _ = c.Heartbeat(context.Background(), ts.URL, "kmm-123") }()
	<-gs.reached
	commitDone := make(chan struct{})
	go func() { defer close(commitDone); _ = c.Commit(context.Background(), ts.URL, "kmm-123") }()
	select {
	case <-commitDone:
	case <-time.After(300 * time.Millisecond):
	}
	close(gs.release)
	<-hbDone
	<-commitDone
	got, err := gs.Store.Get("kmm-123")
	if err != nil || got.Phase != "Committed" {
		t.Fatalf("phase=%q err=%v, want Committed", got.Phase, err)
	}
}

type countingDestination struct {
	fakeDestination
	mu sync.Mutex
	n  int
}

func (c *countingDestination) Prepare(ctx context.Context, s Session) (PrepareResult, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	return c.prepare, nil
}

func TestConcurrentPreparePreparesDestinationOnce(t *testing.T) {
	d := &countingDestination{fakeDestination: fakeDestination{prepare: PrepareResult{TransferSupported: true, Endpoint: "opaque://s"}}}
	ts := httptest.NewServer((&Server{Store: NewFileStore(t.TempDir()), Driver: d}).Handler())
	defer ts.Close()
	c := NewClient(ts.Client())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Prepare(context.Background(), ts.URL, testSession()) }()
	}
	wg.Wait()
	if d.n != 1 {
		t.Fatalf("Driver.Prepare ran %d times for one session", d.n)
	}
}
