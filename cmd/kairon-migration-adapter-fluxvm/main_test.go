// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/migration"
)

func testAdapter(t *testing.T, fluxHandler http.HandlerFunc, migrationNetworks map[string]string) (*adapter, *httptest.Server) {
	t.Helper()
	flux := httptest.NewServer(fluxHandler)
	t.Cleanup(flux.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := newAdapter(log, newFluxClient(flux.URL, ""), "203.0.113.1", "virbr0", 300, migrationNetworks, "", "", "")
	return a, flux
}

func testAdapterWithTLS(t *testing.T, fluxHandler http.HandlerFunc) *adapter {
	t.Helper()
	flux := httptest.NewServer(fluxHandler)
	t.Cleanup(flux.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newAdapter(log, newFluxClient(flux.URL, ""), "203.0.113.1", "virbr0", 300, nil, "/etc/ca.pem", "/etc/cert.pem", "/etc/key.pem")
}

func prepareSession() migration.Session {
	return migration.Session{
		ID:           "sess-1",
		Namespace:    "default",
		Machine:      "vm-1",
		SourceNode:   "node-a",
		TargetNode:   "node-b",
		DiskPath:     "/var/lib/fluxvm/shared/vm-1.qcow2",
		MAC:          "52:54:00:aa:bb:01",
		VCPUs:        2,
		MemoryMiB:    2048,
		SourceRecord: json.RawMessage(`{"id":"src-vm","name":"default-vm-1"}`),
	}
}

// fakeFlux serves FluxVM's receiver create (recording the request) and
// activate routes; the receiver uri is built from the advertise host the
// adapter sent, as FluxVM does.
type fakeFlux struct {
	t         *testing.T
	created   receiverRequest
	activated receiverToken
	calls     []string
}

func (f *fakeFlux) handler(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/migration/receivers":
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		writeJSON(w, http.StatusCreated, receiverInfo{ID: "recv-1", URI: "tcp:" + f.created.AdvertiseHost + ":45001", Token: "tok-1"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/migration/receivers/recv-1/activate":
		_ = json.NewDecoder(r.Body).Decode(&f.activated)
		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		f.t.Errorf("unexpected FluxVM call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func doPrepare(t *testing.T, a *adapter, session migration.Session) migration.PrepareResult {
	t.Helper()
	body, err := json.Marshal(migration.PrepareRequest{Session: session})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/destination/prepare", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.prepare(rec, req)
	var result migration.PrepareResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result
}

func TestPrepareCreatesAdoptModeReceiverAndActivatesIt(t *testing.T) {
	f := &fakeFlux{t: t}
	a, _ := testAdapter(t, f.handler, nil)
	session := prepareSession()
	session.Stream = &migration.StreamCapabilities{MultifdChannels: 4, PostCopy: true, Compression: &migration.Compression{Algorithm: "zstd"}}
	result := doPrepare(t, a, session)
	if !result.TransferSupported {
		t.Fatalf("expected TransferSupported=true, got %+v", result)
	}
	if string(f.created.Record) != string(session.SourceRecord) {
		t.Errorf("receiver record = %s, want the source record", f.created.Record)
	}
	if f.created.ListenHost != "" || f.created.AdvertiseHost != "203.0.113.1" {
		t.Errorf("listen/advertise = %q/%q", f.created.ListenHost, f.created.AdvertiseHost)
	}
	if c := f.created.Capabilities; c.MultifdChannels != 4 || !c.PostCopy || c.Compression == nil || c.Compression.Algorithm != "zstd" {
		t.Errorf("receiver capabilities = %+v", c)
	}
	if f.activated.Token != "tok-1" {
		t.Errorf("activate token = %q, want tok-1", f.activated.Token)
	}
	if result.Endpoint != "tcp:203.0.113.1:45001" {
		t.Errorf("endpoint = %q", result.Endpoint)
	}
	if result.DestinationRuntimeID != "recv-1" {
		t.Errorf("DestinationRuntimeID = %q, want recv-1", result.DestinationRuntimeID)
	}
}

func TestPrepareWithoutSourceRecordIsUnsupportedWithoutCallingFluxVM(t *testing.T) {
	f := &fakeFlux{t: t}
	a, _ := testAdapter(t, f.handler, nil)
	session := prepareSession()
	session.SourceRecord = nil
	result := doPrepare(t, a, session)
	if result.TransferSupported || result.Reason == "" {
		t.Fatalf("expected unsupported with a reason, got %+v", result)
	}
	if len(f.calls) != 0 {
		t.Errorf("FluxVM was called: %v", f.calls)
	}
}

func TestPrepareWithResolvedMigrationNetworkOverridesAddress(t *testing.T) {
	f := &fakeFlux{t: t}
	a, _ := testAdapter(t, f.handler, map[string]string{"migration-fast": "10.10.10.5"})
	session := prepareSession()
	session.MigrationNetwork = "migration-fast"
	result := doPrepare(t, a, session)
	if !result.TransferSupported {
		t.Fatalf("expected TransferSupported=true, got %+v", result)
	}
	if f.created.ListenHost != "10.10.10.5" {
		t.Errorf("expected listen_host 10.10.10.5, got %q", f.created.ListenHost)
	}
	if result.Endpoint != "tcp:10.10.10.5:45001" {
		t.Errorf("expected migration-network address in endpoint, got %q", result.Endpoint)
	}
}

func TestPrepareWithUnresolvableMigrationNetworkFailsWithoutCallingFluxVM(t *testing.T) {
	f := &fakeFlux{t: t}
	a, _ := testAdapter(t, f.handler, map[string]string{"migration-fast": "10.10.10.5"})
	session := prepareSession()
	session.MigrationNetwork = "does-not-exist"
	result := doPrepare(t, a, session)
	if result.TransferSupported {
		t.Fatalf("expected TransferSupported=false, got %+v", result)
	}
	if result.Reason == "" {
		t.Error("expected a non-empty Reason explaining the unresolvable network")
	}
	if len(f.calls) != 0 {
		t.Error("expected FluxVM not to be called for an unresolvable migrationNetwork")
	}
}

func TestPrepareDeletesReceiverWhenActivateFails(t *testing.T) {
	var deleted bool
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/migration/receivers":
			writeJSON(w, http.StatusCreated, receiverInfo{ID: "recv-1", URI: "tcp:203.0.113.1:1", Token: "t"})
		case r.URL.Path == "/v1/migration/receivers/recv-1/activate":
			writeError(w, http.StatusBadRequest, io.ErrUnexpectedEOF)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/migration/receivers/recv-1":
			deleted = true
			writeJSON(w, http.StatusOK, map[string]any{})
		}
	}, nil)
	result := doPrepare(t, a, prepareSession())
	if result.TransferSupported {
		t.Fatalf("expected unsupported, got %+v", result)
	}
	if !deleted {
		t.Error("receiver was not deleted after the failed activate")
	}
}

func TestPrepareWithoutTLSConfiguredSendsNoTls(t *testing.T) {
	f := &fakeFlux{t: t}
	a, _ := testAdapter(t, f.handler, nil)
	result := doPrepare(t, a, prepareSession())
	if f.created.Tls != nil {
		t.Errorf("expected nil Tls, got %+v", f.created.Tls)
	}
	if result.DataPlaneEncrypted {
		t.Error("expected DataPlaneEncrypted=false when no TLS is configured")
	}
}

func TestPrepareWithTLSConfiguredSendsCertPathsAndNoHostname(t *testing.T) {
	f := &fakeFlux{t: t}
	a := testAdapterWithTLS(t, f.handler)
	result := doPrepare(t, a, prepareSession())
	tls := f.created.Tls
	if tls == nil {
		t.Fatal("expected non-nil Tls")
	}
	if tls.CaPath != "/etc/ca.pem" || tls.CertPath != "/etc/cert.pem" || tls.KeyPath != "/etc/key.pem" {
		t.Errorf("unexpected cert paths: %+v", tls)
	}
	if tls.TLSHostname != "" {
		t.Errorf("expected empty TLSHostname on the receiver side, got %q", tls.TLSHostname)
	}
	if !result.DataPlaneEncrypted {
		t.Error("expected DataPlaneEncrypted=true when TLS is configured")
	}
}

func TestCommitAdoptsRetryingWhileIncomingFinishesAndIsIdempotent(t *testing.T) {
	adopts := 0
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/migration/receivers/recv-1/adopt" {
			t.Errorf("unexpected call %s", r.URL.Path)
			return
		}
		var tok receiverToken
		_ = json.NewDecoder(r.Body).Decode(&tok)
		if tok.Token != "tok-1" {
			t.Errorf("adopt token = %q", tok.Token)
		}
		adopts++
		if adopts < 3 {
			writeError(w, http.StatusConflict, io.EOF)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "recv-1"})
	}, nil)
	a.destByID["sess-1"] = &destReceiver{id: "recv-1", token: "tok-1"}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/destination/sess-1/commit", nil)
		req.SetPathValue("id", "sess-1")
		rec := httptest.NewRecorder()
		a.destCommit(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("commit %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if adopts != 3 {
		t.Errorf("adopt called %d times, want 3 (two 409s, one success, then idempotent)", adopts)
	}
	abort := httptest.NewRequest(http.MethodPost, "/v1/destination/sess-1/abort", nil)
	abort.SetPathValue("id", "sess-1")
	rec := httptest.NewRecorder()
	a.destAbort(rec, abort)
	if rec.Code != http.StatusConflict {
		t.Errorf("abort after commit = %d, want 409", rec.Code)
	}
}

func TestCommitFailsOnNonConflictError(t *testing.T) {
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, io.EOF)
	}, nil)
	a.destByID["sess-1"] = &destReceiver{id: "recv-1", token: "tok-1"}
	req := httptest.NewRequest(http.MethodPost, "/v1/destination/sess-1/commit", nil)
	req.SetPathValue("id", "sess-1")
	rec := httptest.NewRecorder()
	a.destCommit(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("commit = %d, want 502", rec.Code)
	}
}

func sourceCall(t *testing.T, a *adapter, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.SetPathValue("sessionID", "sess-1")
	req.SetPathValue("transferID", "sess-1")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestSourceStartMapsAdaptiveOptions(t *testing.T) {
	var got migrationStartRequest
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, migrationStatus{Phase: "active", Status: "active"})
	}, nil)
	level := uint8(3)
	rec := sourceCall(t, a, a.sourceStart, "/v1/source/start", migration.SourceRequest{
		Session: prepareSession(), RuntimeID: "vm-1", Endpoint: "tcp:10.0.0.5:1",
		Options: migration.SourceOptions{
			MultifdChannels: 4,
			AutoConverge:    &migration.AutoConverge{InitialPct: 1, IncrementPct: 1, MaxPct: 1},
			Compression:     &migration.Compression{Algorithm: "zstd", Level: &level},
			PostCopy:        migration.PostCopyArmed,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	if got.PostCopy != "armed" || got.AutoConverge == nil || got.AutoConverge.MaxPct != 1 || got.Compression == nil || *got.Compression.Level != 3 {
		t.Errorf("start request = %+v", got)
	}
}

func TestSourceStartRepeatedReportsTheExistingTransfer(t *testing.T) {
	starts := 0
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-1/migration/start":
			starts++
			writeJSON(w, http.StatusOK, migrationStatus{Phase: "active", Status: "active"})
		case "/v1/vms/vm-1/migration/status":
			writeJSON(w, http.StatusOK, migrationStatus{Phase: "completed", Status: "completed"})
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	}, nil)
	req := migration.SourceRequest{Session: prepareSession(), RuntimeID: "vm-1", Endpoint: "tcp:10.0.0.5:1"}
	if rec := sourceCall(t, a, a.sourceStart, "/v1/source/start", req); rec.Code != http.StatusOK {
		t.Fatalf("first start = %d %s", rec.Code, rec.Body.String())
	}
	rec := sourceCall(t, a, a.sourceStart, "/v1/source/start", req)
	var ts migration.TransferStatus
	_ = json.NewDecoder(rec.Body).Decode(&ts)
	if rec.Code != http.StatusOK || ts.Phase != "completed" || starts != 1 {
		t.Fatalf("repeat start = %d %+v, starts = %d", rec.Code, ts, starts)
	}
	req.RuntimeID = "vm-2"
	if rec := sourceCall(t, a, a.sourceStart, "/v1/source/start", req); rec.Code != http.StatusConflict {
		t.Errorf("start for another runtime = %d, want 409", rec.Code)
	}
}

func TestSourceTunePostCopyAndFinishUseTheSourceRuntime(t *testing.T) {
	var paths []string
	var tune tuneRequest
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/vms/vm-1/migration/tune":
			_ = json.NewDecoder(r.Body).Decode(&tune)
			writeJSON(w, http.StatusOK, migrationStatus{Phase: "active", Status: "active", CPUThrottlePct: 20})
		case "/v1/vms/vm-1/migration/postcopy":
			writeJSON(w, http.StatusOK, migrationStatus{Phase: "postcopy-active", Status: "postcopy-active"})
		default:
			writeJSON(w, http.StatusOK, map[string]any{})
		}
	}, nil)
	a.sourceRTID["sess-1"] = "vm-1"

	rec := sourceCall(t, a, a.sourceTune, "/v1/source/sess-1/sess-1/tune", migration.TuneRequest{MaxCPUThrottlePct: 40, BandwidthMbps: 5000})
	var ts migration.TransferStatus
	_ = json.NewDecoder(rec.Body).Decode(&ts)
	if rec.Code != http.StatusOK || ts.Telemetry == nil || ts.Telemetry.CPUThrottlePct != 20 {
		t.Fatalf("tune = %d %+v", rec.Code, ts)
	}
	if tune.MaxCPUThrottlePct != 40 || tune.BandwidthMbps != 5000 {
		t.Errorf("tune request = %+v", tune)
	}
	if rec := sourceCall(t, a, a.sourceTune, "/v1/source/sess-1/sess-1/tune", migration.TuneRequest{}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty tune = %d, want 400", rec.Code)
	}

	rec = sourceCall(t, a, a.sourcePostCopy, "/v1/source/sess-1/sess-1/postcopy", nil)
	ts = migration.TransferStatus{}
	_ = json.NewDecoder(rec.Body).Decode(&ts)
	if !ts.PostCopyActive || ts.Phase != "running" {
		t.Errorf("postcopy status = %+v", ts)
	}

	if rec := sourceCall(t, a, a.sourceFinish, "/v1/source/sess-1/sess-1/finish", nil); rec.Code != http.StatusOK {
		t.Errorf("finish = %d", rec.Code)
	}
	if _, ok := a.sourceRTID["sess-1"]; ok {
		t.Error("finished transfer is still tracked")
	}
	want := []string{"/v1/vms/vm-1/migration/tune", "/v1/vms/vm-1/migration/postcopy", "/v1/vms/vm-1/migration/finish"}
	if len(paths) != len(want) {
		t.Fatalf("FluxVM paths = %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("FluxVM path %d = %s, want %s", i, paths[i], want[i])
		}
	}
}

func TestSourceStatusMapsAGoneSourceTo409(t *testing.T) {
	code := http.StatusNotFound
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"vm not running"}`, code)
	}, nil)
	a.sourceRTID["sess-1"] = "vm-1"

	if rec := sourceCall(t, a, a.sourceStatus, "/v1/source/sess-1/sess-1", nil); rec.Code != http.StatusConflict {
		t.Errorf("status with FluxVM 404 = %d, want 409", rec.Code)
	}
	code = http.StatusInternalServerError
	if rec := sourceCall(t, a, a.sourceStatus, "/v1/source/sess-1/sess-1", nil); rec.Code != http.StatusBadGateway {
		t.Errorf("status with FluxVM 500 = %d, want 502", rec.Code)
	}
}

func TestCapabilitiesReportsTheQEMUEntry(t *testing.T) {
	a, _ := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"migration":[
			{"backend":"qemu","liveTune":true,"postCopySwitch":true,"autoConverge":true,"compression":["zlib","zstd"],"postCopyWithMultifd":false},
			{"backend":"cloud-hypervisor","liveTune":false}]}`))
	}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	rec := httptest.NewRecorder()
	a.capabilities(rec, req)
	var c migration.Capabilities
	_ = json.NewDecoder(rec.Body).Decode(&c)
	if !c.Tune || !c.PostCopySwitch || !c.AutoConverge || !c.Finish || !c.SupportsCompression("zstd") || c.PostCopyWithMultifd {
		t.Errorf("capabilities = %+v", c)
	}
}

func TestSourceStartWithTLSConfiguredSendsCertPathsAndParsedHostname(t *testing.T) {
	a := testAdapterWithTLS(t, func(w http.ResponseWriter, r *http.Request) {
		var req migrationStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Tls == nil {
			t.Fatal("expected non-nil Tls")
		}
		if req.Tls.CaPath != "/etc/ca.pem" || req.Tls.CertPath != "/etc/cert.pem" || req.Tls.KeyPath != "/etc/key.pem" {
			t.Errorf("unexpected cert paths: %+v", req.Tls)
		}
		if req.Tls.TLSHostname != "10.10.10.5" {
			t.Errorf("expected TLSHostname parsed from endpoint, got %q", req.Tls.TLSHostname)
		}
		writeJSON(w, http.StatusOK, migrationStatus{Phase: "active", Status: "active"})
	})

	body, err := json.Marshal(migration.SourceRequest{
		Session:   prepareSession(),
		RuntimeID: "vm-1",
		Endpoint:  "tcp:10.10.10.5:45001",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/source/start", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	a.sourceStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEndpointHost(t *testing.T) {
	cases := map[string]string{
		"tcp:10.0.0.5:4444":       "10.0.0.5",
		"tcp:[::1]:4444":          "::1",
		"unix:/run/incoming.sock": "",
		"not-a-uri":               "",
	}
	for endpoint, want := range cases {
		if got := endpointHost(endpoint); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", endpoint, got, want)
		}
	}
}
