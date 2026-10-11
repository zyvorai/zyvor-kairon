// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/model"
)

// tuningSource is a source adapter with the optional adaptive routes.
type tuningSource struct {
	fakeSourceMigrator
	caps         migration.Capabilities
	tunes        []migration.TuneRequest
	postCopies   int
	postCopyErr  error
	finishes     int
	statusPhase  string
	statusRemain uint64
}

func (f *tuningSource) Status(context.Context, migration.Session, string) (migration.TransferStatus, error) {
	st := f.startStatus
	if f.statusPhase != "" {
		st.Phase = f.statusPhase
	}
	if f.statusRemain != 0 {
		st.RAMRemaining = f.statusRemain
	}
	return st, nil
}
func (f *tuningSource) Capabilities(context.Context) (migration.Capabilities, error) {
	return f.caps, nil
}
func (f *tuningSource) Tune(_ context.Context, _ migration.Session, _ string, req migration.TuneRequest) (migration.TransferStatus, error) {
	f.tunes = append(f.tunes, req)
	return f.startStatus, nil
}
func (f *tuningSource) StartPostCopy(context.Context, migration.Session, string) (migration.TransferStatus, error) {
	f.postCopies++
	return f.startStatus, f.postCopyErr
}
func (f *tuningSource) Finish(context.Context, migration.Session, string) error {
	f.finishes++
	return nil
}

var allCaps = migration.Capabilities{Tune: true, PostCopySwitch: true, AutoConverge: true, Compression: []string{"zlib", "zstd"}, PostCopyWithMultifd: true, Finish: true}

type tickResult struct {
	status        model.MachineMigrationStatus
	networkResume bool
}

// runMigrationTick runs one agent reconcile for item against a fake
// cluster, FluxVM and destination peer.
func runMigrationTick(t *testing.T, item model.MachineMigration, machine model.Machine, source migration.SourceDriver, destination *fakePeerDestination, store migration.Store) tickResult {
	t.Helper()
	if store == nil {
		store = migration.NewFileStore(t.TempDir())
	}
	peerServer := httptest.NewServer((&migration.Server{NodeName: "worker-2", Store: store, Driver: destination}).Handler())
	defer peerServer.Close()
	var res tickResult
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db/status":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{item}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			_ = json.NewEncoder(w).Encode(machine)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			res.status = p.Status
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer ks.Close()
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			rec := fluxvm.Record{UUID: "vm-1", Name: machine.RuntimeName(), Status: "Running"}
			if len(machine.Spec.DeviceClaims) > 0 {
				rec.Request.VFIODevices = []string{"0000:65:00.0"}
			}
			_ = json.NewEncoder(w).Encode(rec)
		case r.URL.Path == "/v1/vms/vm-1/network/migration/resume":
			res.networkResume = true
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/v1/vms/vm-1/network/migration/"):
			_, _ = w.Write([]byte("{}"))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fs.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{
		NodeName: "worker-1", Kube: kc, Flux: fc, DefaultBackend: "qemu",
		MigrationPeer: migration.NewClient(peerServer.Client()), SourceMigrator: source,
		MigrationPeerURL: func(context.Context, string) (string, error) { return peerServer.URL, nil },
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return res
}

func dbMachine() model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer}},
		Spec:     model.MachineSpec{NodeName: "worker-1", Image: model.ImageSpec{Path: "/images/db.qcow2"}, Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}, Runtime: model.RuntimeSpec{Backend: "qemu"}, PowerState: "Running"},
		Status:   model.MachineStatus{RuntimeID: "vm-1", Phase: "Running", NodeName: "worker-1"},
	}
}

func adaptiveMigration(phase string) model.MachineMigration {
	item := model.MachineMigration{
		Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod", UID: "migration-uid-1"},
		Spec: model.MachineMigrationSpec{
			MachineName: "db", Strategy: "live", Mode: "pre-copy", BandwidthMbps: 1000, MaxDowntimeMs: 400, MultifdChannels: 4,
			Adaptive: &model.MachineMigrationAdaptiveSpec{
				ProgressTimeout:         "30s",
				CompletionTimeoutPerGiB: "120s",
				AutoConverge:            &model.MigrationAutoConverge{Enabled: true},
				PostCopy:                &model.MigrationPostCopy{Enabled: true},
				Compression:             &model.MigrationCompression{Algorithm: "zstd", Level: 3},
			},
		},
		Status: model.MachineMigrationStatus{Phase: phase, SourceNode: "worker-1", TargetNode: "worker-2", EffectiveStrategy: "live"},
	}
	if phase == "Running" {
		item.Status.RuntimeID = "vm-1"
		item.Status.SessionID = migrationSessionID(item)
		item.Status.TransferID = "xfer-1"
	}
	return item
}

// runningStore holds the destination session a Running migration expects.
func runningStore(t *testing.T, item model.MachineMigration) migration.Store {
	t.Helper()
	store := migration.NewFileStore(t.TempDir())
	if err := store.Put(migration.Session{ID: item.Status.SessionID, Namespace: "prod", Machine: "db", SourceNode: "worker-1", TargetNode: "worker-2", Phase: "Prepared"}); err != nil {
		t.Fatal(err)
	}
	return store
}

func acceptingDestination() *fakePeerDestination {
	return &fakePeerDestination{result: migration.PrepareResult{TransferSupported: true, Endpoint: "tcp:10.0.0.2:4444", Backend: "qemu"}}
}

func TestAdaptiveStartNegotiatesStreamAndArmsPostCopy(t *testing.T) {
	destination := acceptingDestination()
	level := uint8(3)
	destination.result.Stream = &migration.StreamCapabilities{MultifdChannels: 4, Compression: &migration.Compression{Algorithm: "zstd", Level: &level}, PostCopy: true}
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running", RAMTotal: 2 << 30, RAMRemaining: 2 << 30}
	res := runMigrationTick(t, adaptiveMigration("Starting"), dbMachine(), source, destination, nil)

	sent := destination.lastSession.Stream
	if sent == nil || sent.MultifdChannels != 4 || sent.Compression == nil || sent.Compression.Algorithm != "zstd" || !sent.PostCopy {
		t.Fatalf("requested stream = %+v", sent)
	}
	if len(destination.lastSession.SourceRecord) == 0 {
		t.Fatal("source FluxVM record was not sent to the destination")
	}
	opts := source.last.Options
	if opts.PostCopy != migration.PostCopyArmed || opts.Compression == nil || opts.AutoConverge == nil || opts.AutoConverge.MaxPct != 1 || opts.MaxDowntimeMs != 200 {
		t.Fatalf("start options = %+v", opts)
	}
	ad := res.status.Adaptive
	if res.status.Phase != "Running" || ad == nil || ad.Stage != model.AdaptiveStagePreCopy || !ad.PostCopyArmed || ad.PostCopyStarted || ad.DowntimeLimitMs != 200 {
		t.Fatalf("status = %+v adaptive = %+v", res.status, ad)
	}
}

func TestAdaptiveStartWithHostDevicesNeverArmsPostCopy(t *testing.T) {
	destination := acceptingDestination()
	destination.result.Stream = &migration.StreamCapabilities{MultifdChannels: 4}
	machine := dbMachine()
	machine.Spec.DeviceClaims = []model.DeviceClaimReference{{Name: "gpu"}}
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running"}
	res := runMigrationTick(t, adaptiveMigration("Starting"), machine, source, destination, nil)
	if destination.lastSession.Stream.PostCopy || source.last.Options.PostCopy == migration.PostCopyArmed {
		t.Fatalf("post-copy requested for a Machine with host devices: %+v / %+v", destination.lastSession.Stream, source.last.Options)
	}
	if ad := res.status.Adaptive; ad == nil || ad.PostCopyArmed || !strings.Contains(ad.Message, "host devices") {
		t.Fatalf("adaptive = %+v", res.status.Adaptive)
	}
}

func TestImmediatePostCopyWithHostDevicesIsBlocked(t *testing.T) {
	destination := acceptingDestination()
	machine := dbMachine()
	machine.Spec.DeviceClaims = []model.DeviceClaimReference{{Name: "gpu"}}
	item := adaptiveMigration("Starting")
	item.Spec.Mode = "post-copy"
	source := &tuningSource{caps: allCaps}
	res := runMigrationTick(t, item, machine, source, destination, nil)
	if res.status.Phase != "Blocked" || destination.prepares != 0 || source.starts != 0 || !res.networkResume {
		t.Fatalf("status=%+v prepares=%d starts=%d resumed=%v", res.status, destination.prepares, source.starts, res.networkResume)
	}
}

func TestLegacyDestinationKeepsLegacyStream(t *testing.T) {
	destination := acceptingDestination()
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running"}
	res := runMigrationTick(t, adaptiveMigration("Starting"), dbMachine(), source, destination, nil)
	opts := source.last.Options
	if opts.Compression != nil || opts.PostCopy != "" || opts.MultifdChannels != 4 {
		t.Fatalf("legacy destination got negotiated options: %+v", opts)
	}
	if ad := res.status.Adaptive; ad == nil || ad.PostCopyArmed || !strings.Contains(ad.Message, "predates stream negotiation") {
		t.Fatalf("adaptive = %+v", res.status.Adaptive)
	}
}

func TestAdapterWithoutTunerRecordsUnsupported(t *testing.T) {
	destination := acceptingDestination()
	destination.result.Stream = &migration.StreamCapabilities{MultifdChannels: 4}
	source := &fakeSourceMigrator{startStatus: migration.TransferStatus{TransferID: "xfer-1", Phase: "running"}}
	res := runMigrationTick(t, adaptiveMigration("Starting"), dbMachine(), source, destination, nil)
	if ad := res.status.Adaptive; ad == nil || ad.Stage != model.AdaptiveStageUnsupported || !strings.Contains(ad.Message, "unsupported by adapter") {
		t.Fatalf("adaptive = %+v", res.status.Adaptive)
	}
	if source.last.Options.AutoConverge != nil {
		t.Fatal("auto-converge enabled on an adapter that can't tune it")
	}
}

func runningAdaptive(item *model.MachineMigration, since time.Time) {
	t := since.UTC()
	item.Status.Adaptive = &model.MachineMigrationAdaptiveStatus{
		Stage: model.AdaptiveStagePreCopy, StartedAt: &t, StageSince: &t, BestProgressAt: &t,
		BestRAMRemaining: 1 << 30, BandwidthMbps: 1000, DowntimeLimitMs: 200, PostCopyArmed: true,
	}
}

func TestRunningAdaptiveStallTunesAutoConverge(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now().Add(-45*time.Second))
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running", RAMTotal: 2 << 30, RAMRemaining: 1 << 30,
		Telemetry: &migration.Telemetry{Mbps: 900, DirtyPagesRate: 1000, PageSize: 4096, ExpectedDowntimeMs: 900}}
	res := runMigrationTick(t, item, dbMachine(), source, acceptingDestination(), runningStore(t, item))
	if len(source.tunes) != 1 || source.tunes[0].MaxCPUThrottlePct != 50 {
		t.Fatalf("tunes = %+v", source.tunes)
	}
	if res.status.Adaptive.Stage != model.AdaptiveStageAutoConverge || !strings.Contains(res.status.Message, "AutoConverge") {
		t.Fatalf("status = %+v", res.status)
	}
	if p := res.status.Progress; p == nil || p.ThroughputMbps != 900 || p.ExpectedDowntimeMs != 900 || p.EstimatedCompletionSeconds == 0 {
		t.Fatalf("progress = %+v", res.status.Progress)
	}
}

func TestRunningAdaptiveDeadlineAbortsAndKeepsSource(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now().Add(-10*time.Minute))
	destination := acceptingDestination()
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running", RAMTotal: 2 << 30, RAMRemaining: 1 << 30}
	res := runMigrationTick(t, item, dbMachine(), source, destination, runningStore(t, item))
	if res.status.Phase != "Failed" || !strings.Contains(res.status.Message, "completion deadline") {
		t.Fatalf("status = %+v", res.status)
	}
	if source.aborts != 1 || destination.aborts != 1 || !res.networkResume {
		t.Fatalf("source aborts=%d destination aborts=%d resumed=%v", source.aborts, destination.aborts, res.networkResume)
	}
}

func TestPostCopyRefusedDisarms(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now().Add(-45*time.Second))
	item.Status.Adaptive.Stage = model.AdaptiveStageRelaxDowntime
	item.Status.Adaptive.DowntimeLimitMs = 400
	item.Status.Adaptive.MaxThrottlePct = 99
	source := &tuningSource{caps: allCaps, postCopyErr: rejectedErr(t)}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running", RAMTotal: 2 << 30, RAMRemaining: 1 << 30}
	res := runMigrationTick(t, item, dbMachine(), source, acceptingDestination(), runningStore(t, item))
	ad := res.status.Adaptive
	if source.postCopies != 1 || ad.PostCopyStarted || ad.PostCopyArmed || ad.Stage != model.AdaptiveStageRelaxDowntime {
		t.Fatalf("postCopies=%d adaptive=%+v", source.postCopies, ad)
	}
}

func TestPostCopyUncertainFailureLatches(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now().Add(-45*time.Second))
	item.Status.Adaptive.Stage = model.AdaptiveStageRelaxDowntime
	item.Status.Adaptive.DowntimeLimitMs = 400
	source := &tuningSource{caps: allCaps, postCopyErr: io.ErrUnexpectedEOF}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running", RAMTotal: 2 << 30, RAMRemaining: 1 << 30}
	res := runMigrationTick(t, item, dbMachine(), source, acceptingDestination(), runningStore(t, item))
	if !res.status.Adaptive.PostCopyStarted {
		t.Fatalf("an ambiguous post-copy failure must be treated as started: %+v", res.status.Adaptive)
	}
}

// rejectedErr returns the error the adapter client produces for an
// adapter 4xx reply.
func rejectedErr(t *testing.T) error {
	t.Helper()
	dir, err := os.MkdirTemp("", "kag")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not armed", http.StatusConflict)
	})}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()
	_, err = migration.NewAdapter(sock).StartPostCopy(context.Background(), migration.Session{ID: "s"}, "x")
	if !migration.IsRejected(err) {
		t.Fatalf("expected a rejection, got %v", err)
	}
	return err
}

func TestFailureAfterPostCopyGoesToNeedsRecovery(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now().Add(-time.Minute))
	item.Status.Adaptive.PostCopyStarted = true
	item.Status.Adaptive.Stage = model.AdaptiveStagePostCopy
	destination := acceptingDestination()
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "failed", Message: "connection reset"}
	res := runMigrationTick(t, item, dbMachine(), source, destination, runningStore(t, item))
	if res.status.Phase != "NeedsRecovery" || !strings.Contains(res.status.Message, "source was not resumed") {
		t.Fatalf("status = %+v", res.status)
	}
	if destination.aborts != 0 || res.networkResume {
		t.Fatalf("post-copy failure aborted the destination (%d) or resumed the source network (%v)", destination.aborts, res.networkResume)
	}
}

func TestPostCopyActiveTelemetryLatchesWithoutAdaptiveSpec(t *testing.T) {
	item := adaptiveMigration("Running")
	item.Spec.Adaptive = nil
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running", PostCopyActive: true}
	res := runMigrationTick(t, item, dbMachine(), source, acceptingDestination(), runningStore(t, item))
	if res.status.Adaptive == nil || !res.status.Adaptive.PostCopyStarted {
		t.Fatalf("adaptive = %+v", res.status.Adaptive)
	}
}

func TestImmediatePostCopyLatchesAtStart(t *testing.T) {
	destination := acceptingDestination()
	destination.result.Stream = &migration.StreamCapabilities{PostCopy: true}
	item := adaptiveMigration("Starting")
	item.Spec.Adaptive = nil
	item.Spec.Mode = "post-copy"
	item.Spec.MultifdChannels = 0
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "running"}
	res := runMigrationTick(t, item, dbMachine(), source, destination, nil)
	if source.last.Options.PostCopy != migration.PostCopyImmediate || res.status.Adaptive == nil || !res.status.Adaptive.PostCopyStarted {
		t.Fatalf("options=%+v adaptive=%+v", source.last.Options, res.status.Adaptive)
	}
}

func TestCancelIgnoredAfterPostCopy(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now())
	item.Status.Adaptive.PostCopyStarted = true
	item.Spec.Cancel = true
	destination := acceptingDestination()
	source := &tuningSource{caps: allCaps}
	res := runMigrationTick(t, item, dbMachine(), source, destination, runningStore(t, item))
	if res.status.Phase != "Running" || !strings.HasPrefix(res.status.Message, "cancel ignored") || source.aborts != 0 || destination.aborts != 0 {
		t.Fatalf("status=%+v source aborts=%d destination aborts=%d", res.status, source.aborts, destination.aborts)
	}
}

func TestCommitReleasesSourceRuntime(t *testing.T) {
	item := adaptiveMigration("Running")
	runningAdaptive(&item, time.Now())
	source := &tuningSource{caps: allCaps}
	source.startStatus = migration.TransferStatus{TransferID: "xfer-1", Phase: "completed"}
	res := runMigrationTick(t, item, dbMachine(), source, acceptingDestination(), runningStore(t, item))
	if res.status.Phase != "Cutover" || source.finishes != 1 {
		t.Fatalf("phase=%s finishes=%d", res.status.Phase, source.finishes)
	}
}
