// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package rehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func recoveryFixture(t *testing.T, offline bool) (*fixture, Plan) {
	f := newFixture(t)
	p := validPlan()
	p.Purpose = "recovery"
	p.Consistency = "existing"
	p.Baseline = nil
	p.Changes = nil
	p.Objectives = &RecoveryObjectives{MaxRTOSeconds: 10, MaxRPOSeconds: 7200}
	p.Members[0].SourceSnapshot = "saved"
	p.Members[0].RestoreStorageClass = map[string]string{"root": "csi"}
	var src model.Machine
	_ = json.Unmarshal(f.objects[objectPath("machines", "db")], &src)
	f.put("/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshots/saved", model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "saved", UID: "snapshot-uid"}, Spec: model.MachineSnapshotSpec{MachineName: "db"}, Status: model.MachineSnapshotStatus{Phase: "Succeeded", ReadyToUse: true, SourceSpec: &src.Spec, SourceUID: src.Metadata.UID, VolumeSnapshots: []model.VolumeSnapshotReference{{VolumeName: "root", VolumeSnapshotName: "saved-root", ReadyToUse: true}}}})
	ready := true
	claim := "root"
	size := "1Gi"
	class := "snapclass"
	at := time.Now().UTC().Add(-time.Hour)
	f.put(objectPath("volumesnapshots", "saved-root"), model.VolumeSnapshot{Metadata: model.ObjectMeta{Name: "saved-root", UID: "saved-root-uid"}, Spec: model.VolumeSnapshotSpec{Source: model.VolumeSnapshotSource{PersistentVolumeClaimName: &claim}, VolumeSnapshotClassName: &class}, Status: model.VolumeSnapshotStatus{ReadyToUse: &ready, RestoreSize: &size, CreationTime: &at, BoundVolumeSnapshotContentName: "content-root"}})
	f.files["saved-root"] = bytes.Clone(f.files["root"])
	f.put("/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/content-root", map[string]any{"spec": map[string]string{"sourceVolumeMode": "Filesystem"}})
	f.put("/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses", map[string]any{"items": []any{map[string]any{"metadata": map[string]string{"name": "snapclass"}, "driver": "example.csi"}}})
	f.put("/apis/storage.k8s.io/v1/storageclasses", map[string]any{"items": []any{map[string]any{"metadata": map[string]string{"name": "csi"}, "provisioner": "example.csi"}}})
	if offline {
		delete(f.objects, objectPath("machines", "db"))
		delete(f.objects, objectPath("persistentvolumeclaims", "root"))
	}
	return f, p
}
func TestRecoveryVerifiesApplicationWithoutSourceMachineOrPVC(t *testing.T) {
	f, p := recoveryFixture(t, true)
	out, err := f.runner().Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if out.ApplicationReadyAt == nil || out.ApplicationRTOSeconds == nil || out.RecoveryPointAgeSeconds == nil || *out.RecoveryPointAgeSeconds < 3600 || !out.Cleaned || out.Phase != "Succeeded" {
		t.Fatalf("missing measured evidence: %+v", out)
	}
	if len(out.Results) != 1 || out.Results[0].Stage != "Verify" {
		t.Fatal("recovery ran an upgrade")
	}
	if len(f.objects[objectPath("volumesnapshots", "saved-root")]) == 0 {
		t.Fatal("deleted external recovery point")
	}
}
func TestRecoveryRejectsStaleUnknownOrMismatchedPoints(t *testing.T) {
	for _, test := range []string{"old", "future-time", "slow-rto", "unknown-time", "wrong-driver", "raw-block", "missing-uid", "restore-failed"} {
		t.Run(test, func(t *testing.T) {
			f, p := recoveryFixture(t, true)
			var snap model.VolumeSnapshot
			_ = json.Unmarshal(f.objects[objectPath("volumesnapshots", "saved-root")], &snap)
			switch test {
			case "old":
				at := time.Now().UTC().Add(-24 * time.Hour)
				snap.Status.CreationTime = &at
			case "future-time":
				at := time.Now().UTC().Add(time.Hour)
				snap.Status.CreationTime = &at
			case "slow-rto":
				p.Objectives.MaxRTOSeconds = 0.000001
			case "unknown-time":
				snap.Status.CreationTime = nil
			case "wrong-driver":
				f.put("/apis/storage.k8s.io/v1/storageclasses", map[string]any{"items": []any{map[string]any{"metadata": map[string]string{"name": "csi"}, "provisioner": "wrong.csi"}}})
			case "raw-block":
				f.put("/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/content-root", map[string]any{"spec": map[string]string{"sourceVolumeMode": "Block"}})
			case "missing-uid":
				p.Purpose = "incident"
				p.Objectives = nil
				p.Incident = &IncidentReference{SHA256: string(bytes.Repeat([]byte("a"), 64)), Machine: "db", SourceUID: "wrong", CollectedAt: time.Now().UTC().Format(time.RFC3339)}
				p.Baseline = []Step{{Name: "reproduce", Machine: "db", Path: "/bin/true", TimeoutSeconds: 5, ExpectedExitCode: 1}}
				p.Changes = []Step{{Name: "fix", Machine: "db", Path: "/bin/true", TimeoutSeconds: 5}}
			case "restore-failed":
				f.fail = "boot"
			}
			f.put(objectPath("volumesnapshots", "saved-root"), snap)
			out, err := f.runner().Run(context.Background(), p)
			if err == nil || !out.Cleaned {
				t.Fatalf("test=%s err=%v report=%+v", test, err, out)
			}
		})
	}
}
func TestIncidentReproductionDiagnosisAndFix(t *testing.T) {
	f, p := recoveryFixture(t, false)
	p.Purpose = "incident"
	p.Objectives = nil
	p.Incident = &IncidentReference{SHA256: string(bytes.Repeat([]byte("a"), 64)), Machine: "db", SourceUID: "source-uid", CollectedAt: time.Now().UTC().Format(time.RFC3339)}
	p.Baseline = []Step{{Name: "reproduce-http-503", Machine: "db", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", "probe", "--expect-schema-failure"}, TimeoutSeconds: 5, ExpectedExitCode: 1}}
	p.Changes = []Step{{Name: "repair-schema", Machine: "db", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", "repair"}, TimeoutSeconds: 5}}
	broken := filepath.Join(t.TempDir(), "broken.db")
	if err := os.WriteFile(broken, f.files["root"], 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("python3", "../../examples/rehearsal/app.py", "breaking", "--db", broken).CombinedOutput(); err != nil {
		t.Fatal(err, string(b))
	}
	b, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	f.files["saved-root"] = b
	original := bytes.Clone(f.files["root"])
	r := f.runner()
	r.Investigate = func(_ context.Context, _ string, name string) (json.RawMessage, error) {
		if name == "db" {
			t.Fatal("diagnosed source")
		}
		return json.RawMessage(`{"cause":"schema mismatch","proposal":"restore the users table name"}`), nil
	}
	out, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Investigation) != 1 || len(out.Results) != 3 || out.Results[0].ExitCode != 1 || out.Results[0].ExpectedExitCode != 1 || out.Results[2].ExitCode != 0 || !out.Cleaned {
		t.Fatal(out)
	}
	if !bytes.Equal(original, f.files["root"]) {
		t.Fatal("incident fix touched production")
	}
}
func TestIncidentMustReproduceBeforeFix(t *testing.T) {
	f, p := recoveryFixture(t, false)
	p.Purpose = "incident"
	p.Objectives = nil
	p.Incident = &IncidentReference{SHA256: string(bytes.Repeat([]byte("a"), 64)), Machine: "db", SourceUID: "source-uid"}
	p.Baseline = []Step{{Name: "must-fail", Machine: "db", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", "probe", "--expect-schema-failure"}, TimeoutSeconds: 5, ExpectedExitCode: 1}}
	p.Changes = []Step{{Name: "must-not-run", Machine: "db", Path: "/bin/true", TimeoutSeconds: 5}}
	out, err := f.runner().Run(context.Background(), p)
	if err == nil || len(out.Results) != 1 || !out.Cleaned {
		t.Fatal(out, err)
	}
}

func TestOperationalExamplePlans(t *testing.T) {
	for _, name := range []string{"recovery", "incident"} {
		b, err := os.ReadFile("../../examples/operations/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var p Plan
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if name == "incident" {
			p.Incident = &IncidentReference{SHA256: string(bytes.Repeat([]byte("a"), 64)), Machine: "demo-db", SourceUID: "fixture"}
		}
		if err := p.Validate(); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestIncidentProbeDistinguishesConnectivityFailure(t *testing.T) {
	// An invalid URL scheme has no HTTP schema-failure signature.
	cmd := exec.Command("python3", "../../examples/rehearsal/app.py", "probe", "--url", "invalid://missing", "--expect-schema-failure")
	err := cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 {
		t.Fatal("connectivity failure could impersonate schema reproduction", err)
	}
}
