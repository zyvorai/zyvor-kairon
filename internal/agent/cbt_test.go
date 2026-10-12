// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeCBTFlux struct {
	mu       sync.Mutex
	vmStatus string
	job      map[string]any
	held     []string
	calls    []string
	bodies   map[string]map[string]any
	caps     map[string]any
}

func (f *fakeCBTFlux) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if f.bodies == nil {
		f.bodies = map[string]map[string]any{}
	}
	f.bodies[r.URL.Path] = body
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vm-1", "status": f.vmStatus})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/capabilities":
		_ = json.NewEncoder(w).Encode(f.caps)
	case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/cbt-backup") || strings.HasSuffix(r.URL.Path, "/cbt-restore") || r.URL.Path == "/v1/cbt/synthesize"):
		f.calls = append(f.calls, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "state": "running", "phase": "preparing"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/cbt-jobs/job-1":
		_ = json.NewEncoder(w).Encode(f.job)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/cbt-jobs/"):
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/cbt/delete":
		f.calls = append(f.calls, "delete")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": []string{}, "notFound": []string{}, "retentionHeld": f.held})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

type fakeCBTKube struct {
	mu         sync.Mutex
	secrets    map[string]model.Secret
	backups    map[string]model.MachineBackup
	node       model.Node
	disk       []model.DiskBackupStatus
	points     []*time.Time
	finalizers [][]string
	nodePatch  []map[string]any
}

func (f *fakeCBTKube) lastDisk() model.DiskBackupStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.disk) == 0 {
		return model.DiskBackupStatus{}
	}
	return f.disk[len(f.disk)-1]
}

func newCBTAgent(t *testing.T, ff *fakeCBTFlux, fk *fakeCBTKube) *Agent {
	t.Helper()
	fs := httptest.NewServer(http.HandlerFunc(ff.handler))
	t.Cleanup(fs.Close)
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Status struct {
				Disk        model.DiskBackupStatus `json:"disk"`
				PointInTime *time.Time             `json:"pointInTime"`
			} `json:"status"`
			Metadata struct {
				Finalizers []string `json:"finalizers"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(raw, &body)
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/namespaces/prod/secrets/"):
			s, ok := fk.secrets[strings.TrimPrefix(p, "/api/v1/namespaces/prod/secrets/")]
			if !ok {
				http.Error(w, `{"kind":"Status","code":404}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodGet && p == "/api/v1/nodes/n1":
			_ = json.NewEncoder(w).Encode(fk.node)
		case r.Method == http.MethodPatch && p == "/api/v1/nodes/n1":
			var patch map[string]any
			_ = json.Unmarshal(raw, &patch)
			fk.nodePatch = append(fk.nodePatch, patch)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinebackups/"):
			_ = json.NewEncoder(w).Encode(fk.backups[strings.TrimPrefix(p, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinebackups/")])
		case r.Method == http.MethodPatch && strings.HasSuffix(p, "/status"):
			fk.disk = append(fk.disk, body.Status.Disk)
			fk.points = append(fk.points, body.Status.PointInTime)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch:
			fk.finalizers = append(fk.finalizers, body.Metadata.Finalizers)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	return &Agent{NodeName: "n1", Kube: kc, Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func backupSecrets(labelled bool) map[string]model.Secret {
	labels := map[string]string{model.BackupCredentialsLabel: "true"}
	if !labelled {
		labels = nil
	}
	key := make([]byte, 32)
	return map[string]model.Secret{
		"s3":  {Metadata: model.ObjectMeta{Name: "s3", Labels: labels}, Data: map[string][]byte{"accessKeyId": []byte("AK"), "secretAccessKey": []byte("SK\n")}},
		"key": {Metadata: model.ObjectMeta{Name: "key", Labels: labels}, Data: map[string][]byte{"key": key, "keyId": []byte("k1")}},
	}
}

func cbtAgentBackup(mode string) model.MachineBackup {
	b := model.MachineBackup{
		Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod", UID: "0123456789"},
		Spec: model.MachineBackupSpec{
			MachineName: "web", Mode: mode,
			Target:     &model.BackupTarget{S3: &model.S3BackupTarget{Endpoint: "http://minio:9000", Bucket: "vm", CredentialsSecretRef: "s3"}},
			Encryption: &model.BackupEncryption{KeySecretRef: "key"},
		},
		Status: model.MachineBackupStatus{
			NodeName: "n1",
			Chain:    &model.BackupChainStatus{ID: "c-1", Sequence: 1, BackupID: "prod-b1-01234567"},
			Disk:     &model.DiskBackupStatus{Phase: model.BackupPending},
		},
	}
	if mode != model.BackupModeFull && mode != "" {
		b.Spec.Parent = "b0"
		b.Status.Chain.Sequence = 3
		b.Status.Chain.ParentBackupID = "prod-b0"
		b.Status.Chain.ParentManifestKey = "prod/web/c-1/00000002-prod-b0/manifest.json"
	}
	return b
}

func TestCBTBackupStartsFluxVMJobAndFoldsResult(t *testing.T) {
	ff := &fakeCBTFlux{vmStatus: "running"}
	fk := &fakeCBTKube{secrets: backupSecrets(true)}
	a := newCBTAgent(t, ff, fk)
	b := cbtAgentBackup("")
	ctx := context.Background()
	before := time.Now().UTC()
	if err := a.reconcileDiskBackup(ctx, b, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	if pit := fk.points[len(fk.points)-1]; pit == nil || pit.Before(before) || pit.After(time.Now()) {
		t.Fatalf("pointInTime = %v, want the time the FluxVM job started", pit)
	}
	req := ff.bodies["/v1/vms/vm-1/cbt-backup"]
	target, _ := req["target"].(map[string]any)
	enc, _ := req["encryption"].(map[string]any)
	if req["mode"] != "full" || req["prefix"] != "prod/web" || req["chainId"] != "c-1" || req["parent"] != nil ||
		target["type"] != "s3" || target["secretAccessKey"] != "SK" || target["pathStyle"] != true ||
		enc["keyId"] != "k1" || enc["key"] != base64.StdEncoding.EncodeToString(make([]byte, 32)) {
		t.Fatalf("request = %v", req)
	}
	disk := fk.lastDisk()
	if disk.Phase != model.BackupRunning || disk.JobID != "job-1" {
		t.Fatalf("disk = %+v", disk)
	}

	b.Status.Disk = &disk
	ff.job = map[string]any{"id": "job-1", "state": "running", "phase": "uploading", "bytesDone": 1 << 20, "bytesTotal": 4 << 20}
	if err := a.reconcileDiskBackup(ctx, b, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupRunning || got.Message != "uploading: 25% of 4 MiB" {
		t.Fatalf("progress = %+v", got)
	}

	ff.job = map[string]any{"id": "job-1", "state": "succeeded", "phase": "done", "manifestKey": "prod/web/c-1/m.json",
		"manifest": map[string]any{"kind": "full", "quiesced": true, "disks": []map[string]any{
			{"name": "root", "key": "prod/web/c-1/root.qcow2", "storedSize": 100, "changedBytes": 1000},
			{"name": "data", "key": "prod/web/c-1/data.qcow2", "storedSize": 10, "changedBytes": 50},
		}},
		"skipped": []map[string]any{{"name": "cdrom", "reason": "read-only"}}}
	if err := a.reconcileDiskBackup(ctx, b, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	got := fk.lastDisk()
	if got.Phase != model.BackupSucceeded || got.ManifestKey != "prod/web/c-1/m.json" || got.StoredBytes != 110 || got.ChangedBytes != 1050 ||
		len(got.Objects) != 3 || len(got.Disks) != 2 || !got.Quiesced || !strings.Contains(got.Message, "skipped cdrom") {
		t.Fatalf("result = %+v", got)
	}
}

func TestCBTBackupRefusesUnlabelledSecret(t *testing.T) {
	ff := &fakeCBTFlux{vmStatus: "running"}
	fk := &fakeCBTKube{secrets: backupSecrets(false)}
	a := newCBTAgent(t, ff, fk)
	if err := a.reconcileDiskBackup(context.Background(), cbtAgentBackup(""), backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupFailed || !strings.Contains(got.Message, model.BackupCredentialsLabel) {
		t.Fatalf("disk = %+v", got)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("FluxVM called: %v", ff.calls)
	}
}

func TestCBTIncrementalSendsParentAndReportsFullRequired(t *testing.T) {
	ff := &fakeCBTFlux{vmStatus: "running"}
	fk := &fakeCBTKube{secrets: backupSecrets(true)}
	a := newCBTAgent(t, ff, fk)
	b := cbtAgentBackup(model.BackupModeIncremental)
	if err := a.reconcileDiskBackup(context.Background(), b, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	parent, _ := ff.bodies["/v1/vms/vm-1/cbt-backup"]["parent"].(map[string]any)
	if parent["backupId"] != "prod-b0" || parent["sequence"] != float64(2) || parent["manifestKey"] != b.Status.Chain.ParentManifestKey {
		t.Fatalf("parent = %v", parent)
	}
	disk := fk.lastDisk()
	b.Status.Disk = &disk
	ff.job = map[string]any{"id": "job-1", "state": "failed", "phase": "preparing", "error": "disk root has no checkpoint", "fullRequired": true}
	if err := a.reconcileDiskBackup(context.Background(), b, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupFailed || !got.FullRequired || !strings.Contains(got.Message, "must be a full") {
		t.Fatalf("disk = %+v", got)
	}
}

func TestCBTSyntheticFullNeedsNoVM(t *testing.T) {
	ff := &fakeCBTFlux{}
	fk := &fakeCBTKube{secrets: backupSecrets(true)}
	a := newCBTAgent(t, ff, fk)
	b := cbtAgentBackup(model.BackupModeSyntheticFull)
	b.Status.Disk.SyntheticBases = []string{"prod/web/c-1/s/manifest.json"}
	if err := a.reconcileDiskBackup(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	req := ff.bodies["/v1/cbt/synthesize"]
	if req["manifestKey"] != b.Status.Chain.ParentManifestKey || req["backupId"] != "prod-b1-01234567" || len(req["syntheticBases"].([]any)) != 1 {
		t.Fatalf("request = %v", req)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupRunning || got.JobID != "job-1" {
		t.Fatalf("disk = %+v", got)
	}
}

func TestCBTDeleteHonoursObjectLock(t *testing.T) {
	ff := &fakeCBTFlux{}
	fk := &fakeCBTKube{secrets: backupSecrets(true)}
	a := newCBTAgent(t, ff, fk)
	now := time.Now()
	b := cbtAgentBackup("")
	b.Metadata.DeletionTimestamp = &now
	b.Metadata.Finalizers = []string{model.FinalizerCBTBackup}
	b.Status.ManifestKey = "prod/web/c-1/m.json"
	b.Status.Disk = &model.DiskBackupStatus{Phase: model.BackupSucceeded, ManifestKey: b.Status.ManifestKey}
	future := now.Add(time.Hour)
	b.Status.RetentionUntil = &future
	ctx := context.Background()
	if err := a.reconcileDiskBackup(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	if len(ff.calls) != 0 || len(fk.finalizers) != 0 || !strings.Contains(fk.lastDisk().Message, "Object Lock retention until") {
		t.Fatalf("calls=%v finalizers=%v disk=%+v", ff.calls, fk.finalizers, fk.lastDisk())
	}

	past := now.Add(-time.Hour)
	b.Status.RetentionUntil = &past
	ff.held = []string{"prod/web/c-1/root.qcow2"}
	if err := a.reconcileDiskBackup(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	if len(fk.finalizers) != 0 || !strings.Contains(fk.lastDisk().Message, "still under Object Lock") {
		t.Fatalf("finalizers=%v disk=%+v", fk.finalizers, fk.lastDisk())
	}

	ff.held = nil
	if err := a.reconcileDiskBackup(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	keys := ff.bodies["/v1/cbt/delete"]["manifestKeys"].([]any)
	if len(fk.finalizers) != 1 || len(fk.finalizers[0]) != 0 || len(keys) != 1 || keys[0] != b.Status.ManifestKey {
		t.Fatalf("finalizers=%v keys=%v", fk.finalizers, keys)
	}
}

func TestCBTRestoreWaitsForHaltThenRunsJob(t *testing.T) {
	ff := &fakeCBTFlux{vmStatus: "running"}
	b := cbtAgentBackup("")
	fk := &fakeCBTKube{secrets: backupSecrets(true), backups: map[string]model.MachineBackup{"b1": b}}
	a := newCBTAgent(t, ff, fk)
	r := model.MachineBackupRestore{
		Metadata: model.ObjectMeta{Name: "r1", Namespace: "prod"},
		Spec:     model.MachineBackupRestoreSpec{BackupName: "b1"},
		Status: model.MachineBackupRestoreStatus{NodeName: "n1", MachineName: "web", Disk: &model.DiskBackupStatus{
			Phase: model.BackupPending, Name: "b1", ManifestKey: "prod/web/c-1/m.json", SyntheticBases: []string{"s.json"},
		}},
	}
	ctx := context.Background()
	if err := a.reconcileDiskRestore(ctx, r, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); !strings.Contains(got.Message, "halted") || len(ff.calls) != 0 {
		t.Fatalf("disk=%+v calls=%v", got, ff.calls)
	}
	ff.vmStatus = "stopped"
	if err := a.reconcileDiskRestore(ctx, r, backupTestMachine("Halted")); err != nil {
		t.Fatal(err)
	}
	req := ff.bodies["/v1/vms/vm-1/cbt-restore"]
	if req["manifestKey"] != "prod/web/c-1/m.json" || len(req["syntheticBases"].([]any)) != 1 || req["encryption"] == nil {
		t.Fatalf("request = %v", req)
	}
	disk := fk.lastDisk()
	r.Status.Disk = &disk
	ff.job = map[string]any{"id": "job-1", "state": "succeeded", "phase": "done", "manifest": map[string]any{"kind": "full", "disks": []map[string]any{{"name": "root"}}}}
	if err := a.reconcileDiskRestore(ctx, r, backupTestMachine("Halted")); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupSucceeded || len(got.Disks) != 1 || got.ManifestKey != "prod/web/c-1/m.json" {
		t.Fatalf("restored = %+v", got)
	}
}

func TestCBTLostJobFailsTheBackup(t *testing.T) {
	ff := &fakeCBTFlux{}
	fk := &fakeCBTKube{secrets: backupSecrets(true)}
	a := newCBTAgent(t, ff, fk)
	b := cbtAgentBackup("")
	b.Status.Disk = &model.DiskBackupStatus{Phase: model.BackupRunning, JobID: "gone"}
	if err := a.reconcileDiskBackup(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupFailed || !strings.Contains(got.Message, "no record of job gone") {
		t.Fatalf("disk = %+v", got)
	}
}

func TestEncryptionKeyAcceptsRawOrBase64(t *testing.T) {
	raw := make([]byte, 32)
	raw[0] = 7
	b64 := base64.StdEncoding.EncodeToString(raw)
	for _, in := range [][]byte{raw, []byte(b64 + "\n")} {
		got, err := encryptionKey(in)
		if err != nil || got != b64 {
			t.Fatalf("encryptionKey(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := encryptionKey([]byte("c2hvcnQ=")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestPublishCBTCapabilityLabelsNode(t *testing.T) {
	ff := &fakeCBTFlux{caps: map[string]any{"backup": []map[string]any{{"backend": "qemu", "incremental": true, "targets": []string{"s3", "dir"}}}}}
	fk := &fakeCBTKube{}
	a := newCBTAgent(t, ff, fk)
	a.publishCBTCapability(context.Background())
	if len(fk.nodePatch) != 1 {
		t.Fatalf("patches = %v", fk.nodePatch)
	}
	labels := fk.nodePatch[0]["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[model.CBTNodeLabel] != "true" {
		t.Fatalf("labels = %v", labels)
	}
	// Rate-limited: a second call within capabilityEvery does nothing.
	a.publishCBTCapability(context.Background())
	if len(fk.nodePatch) != 1 {
		t.Fatalf("patches = %v", fk.nodePatch)
	}
	var caps *fluxvm.RuntimeCapabilities
	if caps.CBTBackup() {
		t.Fatal("nil capabilities report CBT")
	}
}
