// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package rehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// fixture serves Kubernetes REST and simulates CSI snapshot/restore and VM
// readiness. Guest commands are REAL Python/SQLite/HTTP subprocesses against
// the restored files; this is deliberately not advertised as a KVM lab test.
type fixture struct {
	t           *testing.T
	objects     map[string]json.RawMessage
	files       map[string][]byte
	deleted     []string
	execTargets []string
	requests    []string
	fail        string
	dir         string
	client      *kube.Client
	saveCount   int
	report      Report
}

func validPlan() Plan {
	step := func(name, action string) Step {
		return Step{Name: name, Machine: "db", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", action}, TimeoutSeconds: 10}
	}
	return Plan{APIVersion: "rehearsal.kairon.zyvor.dev/v1alpha1", Name: "upgrade", Namespace: "default", Node: "lab", Bridge: "krh-demo", TTLSeconds: 180, TimeoutSeconds: 30, Consistency: "crash", SnapshotClass: "csi-snapshots", Members: []Member{{Name: "db"}}, Baseline: []Step{step("before", "probe")}, Changes: []Step{step("upgrade", "compatible")}, Verify: []Step{step("after", "probe")}}
}
func objectPath(kind, name string) string {
	prefix := "/apis/kairon.zyvor.dev/v1/namespaces/default/"
	if kind == "persistentvolumeclaims" {
		prefix = "/api/v1/namespaces/default/"
	}
	if kind == "volumesnapshots" {
		prefix = "/apis/snapshot.storage.k8s.io/v1/namespaces/default/"
	}
	return prefix + kind + "/" + name
}
func (f *fixture) put(path string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	f.objects[path] = b
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("scenario tests require Python 3")
	}
	f := &fixture{t: t, objects: map[string]json.RawMessage{}, files: map[string][]byte{}, dir: t.TempDir()}
	db := filepath.Join(f.dir, "source.db")
	if b, err := exec.Command("python3", "../../examples/rehearsal/app.py", "init", "--db", db).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, b)
	}
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	f.files["root"] = b
	src := model.Machine{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: "Machine"}, Metadata: model.ObjectMeta{Name: "db", Namespace: "default", UID: "source-uid", ResourceVersion: "42"}, Spec: model.MachineSpec{
		Runtime: model.RuntimeSpec{Backend: "qemu"}, Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}, GuestAgent: model.GuestAgentSpec{Enabled: true},
		Network:   model.NetworkSpec{Mode: "tap", Bridge: "production", MAC: "52:54:00:00:00:01", ExtraInterfaces: []model.ExtraInterface{{Name: "management", Bridge: "prod"}}},
		CloudInit: model.CloudInitSpec{RunCmd: []string{"do-not-rerun-production-bootstrap"}}, Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root"}},
	}, Status: model.MachineStatus{Phase: "Running", RuntimeID: "prod-runtime"}}
	f.put(objectPath("machines", "db"), src)
	sc := "csi"
	f.put(objectPath("persistentvolumeclaims", "root"), model.PersistentVolumeClaim{Metadata: model.ObjectMeta{Name: "root", UID: "source-pvc"}, Spec: model.PersistentVolumeClaimSpec{StorageClassName: &sc}, Status: model.PersistentVolumeClaimStatus{Phase: "Bound"}})
	node := model.Node{Metadata: model.ObjectMeta{Name: "lab", ResourceVersion: "1", Annotations: map[string]string{BridgeAnnotation: "krh-demo", GuardAnnotation: "v1"}}}
	node.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
	f.put("/api/v1/nodes/lab", node)
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.client = &kube.Client{BaseURL: server.URL, HTTP: server.Client()}
	return f
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		b, ok := f.objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
		return
	}
	if r.Method == "PATCH" && r.URL.Path == "/api/v1/nodes/lab" {
		var patch []struct {
			Op    string
			Path  string
			Value json.RawMessage
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		var node model.Node
		_ = json.Unmarshal(f.objects[r.URL.Path], &node)
		var version string
		_ = json.Unmarshal(patch[0].Value, &version)
		if version != node.Metadata.ResourceVersion {
			http.Error(w, "stale version", http.StatusConflict)
			return
		}
		_ = json.Unmarshal(patch[1].Value, &node.Metadata.Annotations)
		// JSON unmarshalling into an existing map merges keys; replace it first.
		node.Metadata.Annotations = nil
		_ = json.Unmarshal(patch[1].Value, &node.Metadata.Annotations)
		f.put(r.URL.Path, node)
		_, _ = w.Write(f.objects[r.URL.Path])
		return
	}
	if r.Method == "DELETE" {
		b, ok := f.objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var obj struct{ Metadata model.ObjectMeta }
		_ = json.Unmarshal(b, &obj)
		var options struct{ Preconditions struct{ UID string } }
		_ = json.NewDecoder(r.Body).Decode(&options)
		if options.Preconditions.UID != obj.Metadata.UID {
			http.Error(w, "UID precondition", http.StatusConflict)
			return
		}
		if f.fail == "cleanup" {
			http.Error(w, "cannot delete", http.StatusServiceUnavailable)
			return
		}
		delete(f.objects, r.URL.Path)
		f.deleted = append(f.deleted, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
		return
	}
	if r.Method != "POST" {
		f.t.Errorf("unexpected source mutation: %s %s", r.Method, r.URL.Path)
		http.Error(w, "mutation", 400)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		f.t.Error(err)
		w.WriteHeader(400)
		return
	}
	var meta model.ObjectMeta
	_ = json.Unmarshal(raw["metadata"], &meta)
	path := r.URL.Path + "/" + meta.Name
	if _, ok := f.objects[path]; ok {
		w.WriteHeader(409)
		return
	}
	meta.UID = "uid-" + meta.Name
	raw["metadata"], _ = json.Marshal(meta)
	switch {
	case strings.HasSuffix(r.URL.Path, "/volumesnapshots"):
		var spec model.VolumeSnapshotSpec
		_ = json.Unmarshal(raw["spec"], &spec)
		f.files[meta.Name] = bytes.Clone(f.files[*spec.Source.PersistentVolumeClaimName])
		ready := f.fail != "snapshot-timeout"
		size := "1Gi"
		st := model.VolumeSnapshotStatus{ReadyToUse: &ready, RestoreSize: &size}
		if f.fail == "snapshot" {
			msg := "driver failed"
			st.Error = &model.VolumeSnapshotError{Message: &msg}
		}
		raw["status"], _ = json.Marshal(st)
	case strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims"):
		var spec model.PersistentVolumeClaimSpec
		_ = json.Unmarshal(raw["spec"], &spec)
		f.files[meta.Name] = bytes.Clone(f.files[spec.DataSource.Name])
		raw["status"] = json.RawMessage(`{"phase":"Pending"}`) // WFFC: consumer must be created.
	case strings.HasSuffix(r.URL.Path, "/machines"):
		var spec model.MachineSpec
		_ = json.Unmarshal(raw["spec"], &spec)
		if spec.Volumes[0].ClaimName == "root" || spec.Network.Bridge != "krh-demo" || len(spec.Network.ExtraInterfaces) != 0 || len(spec.CloudInit.RunCmd) != 0 {
			f.t.Error("unsafe copy")
		}
		for _, volume := range spec.Volumes {
			var pvc model.PersistentVolumeClaim
			_ = json.Unmarshal(f.objects[objectPath("persistentvolumeclaims", volume.ClaimName)], &pvc)
			pvc.Status.Phase = "Bound"
			f.put(objectPath("persistentvolumeclaims", volume.ClaimName), pvc)
		}
		raw["status"] = json.RawMessage(`{"phase":"Running","runtimeID":"test-runtime"}`)
		if f.fail == "boot" {
			raw["status"] = json.RawMessage(`{"phase":"Failed"}`)
		}
		if f.fail == "foreign" {
			meta.Labels = map[string]string{}
			raw["metadata"], _ = json.Marshal(meta)
		}
	default:
		f.t.Errorf("unexpected POST %s", r.URL.Path)
	}
	b, _ := json.Marshal(raw)
	f.objects[path] = b
	if f.fail == "ambiguous" && strings.HasSuffix(r.URL.Path, "/machines") {
		http.Error(w, "lost create response", 500)
		return
	}
	_, _ = w.Write(b)
}
func (f *fixture) runner() *Runner {
	return &Runner{Kube: f.client, PollInterval: time.Millisecond, CleanupTimeout: time.Second, Save: func(r Report) error {
		f.saveCount++
		b, _ := json.Marshal(r)
		_ = json.Unmarshal(b, &f.report)
		if dir := os.Getenv("KAIRON_REHEARSAL_TEST_EVIDENCE"); dir != "" {
			name := strings.ReplaceAll(f.t.Name(), "/", "-") + ".json"
			return WriteReport(filepath.Join(dir, name), r)
		}
		return nil
	}, Exec: func(ctx context.Context, ns, name string, s Step) (ExecResult, error) {
		f.execTargets = append(f.execTargets, name)
		if s.Path == "/bin/true" {
			return ExecResult{}, nil
		}
		if name == "db" {
			f.t.Fatal("production guest execution")
		}
		var m model.Machine
		_ = json.Unmarshal(f.objects[objectPath("machines", name)], &m)
		path := filepath.Join(f.dir, name+".db")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, f.files[m.Spec.Volumes[0].ClaimName], 0600); err != nil {
				return ExecResult{}, err
			}
		}
		args := append([]string{"../../examples/rehearsal/app.py"}, s.Args[1:]...)
		args = append(args, "--db", path)
		cmd := exec.CommandContext(ctx, "python3", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				res.ExitCode = int64(exit.ExitCode())
				return res, nil
			}
			return res, err
		}
		return res, nil
	}}
}

func TestUpgradeScenarios(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		pass         bool
	}{{"compatible", "compatible", true}, {"breaking", "breaking", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p := validPlan()
			p.Changes[0].Args[1] = tc.action
			source := bytes.Clone(f.objects[objectPath("machines", "db")])
			disk := bytes.Clone(f.files["root"])
			report, err := f.runner().Run(context.Background(), p)
			if (err == nil) != tc.pass {
				t.Fatalf("error=%v report=%+v", err, report)
			}
			if !report.Cleaned || len(report.Results) != 3 || len(report.Resources) != 3 {
				t.Fatalf("incomplete evidence: %+v", report)
			}
			if !bytes.Equal(source, f.objects[objectPath("machines", "db")]) || !bytes.Equal(disk, f.files["root"]) {
				t.Fatal("production changed")
			}
			for _, target := range f.execTargets {
				if target == "db" {
					t.Fatal("executed on source")
				}
			}
			if len(f.deleted) != 3 || !strings.Contains(f.deleted[0], "/machines/") || !strings.Contains(f.deleted[1], "/persistentvolumeclaims/") || !strings.Contains(f.deleted[2], "/volumesnapshots/") {
				t.Fatalf("unsafe cleanup ordering: %v", f.deleted)
			}
			if f.report.Phase != report.Phase || f.report.Cleaned != report.Cleaned {
				t.Fatal("journal not finalized")
			}
		})
	}
}

func TestFailurePaths(t *testing.T) {
	for _, failure := range []string{"snapshot", "boot", "ambiguous", "cleanup", "foreign", "snapshot-timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.fail = failure
			r := f.runner()
			ctx := context.Background()
			if failure == "snapshot-timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			out, err := r.Run(ctx, validPlan())
			if err == nil {
				t.Fatal("expected failure")
			}
			cleanupBlocked := failure == "cleanup" || failure == "foreign"
			if out.Cleaned == cleanupBlocked {
				t.Fatalf("cleaned=%t failure=%s err=%v", out.Cleaned, failure, err)
			}
			if failure != "cleanup" && len(out.Results) != 0 {
				t.Fatal("ran application steps after infrastructure failure")
			}
			if cleanupBlocked && len(f.deleted) != 0 {
				t.Fatal("deleted storage before copy removal")
			}
			if failure == "cleanup" {
				f.fail = ""
				if err := r.Cleanup(context.Background(), &out); err != nil {
					t.Fatal(err)
				}
				if !out.Cleaned {
					t.Fatal("cleanup retry did not finish")
				}
			}
		})
	}
}

func TestPreflightRejectsUnsafeSourcesWithoutWrites(t *testing.T) {
	for _, test := range []string{"device", "atlas", "block", "no-guard", "same-bridge", "no-agent", "no-pvc", "bad-existing", "pinned", "shared-claim"} {
		t.Run(test, func(t *testing.T) {
			f := newFixture(t)
			p := validPlan()
			var m model.Machine
			_ = json.Unmarshal(f.objects[objectPath("machines", "db")], &m)
			switch test {
			case "shared-claim":
				m.Spec.Volumes = append(m.Spec.Volumes, model.MachineVolume{Name: "also-root", ClaimName: "root"})
			case "pinned":
				m.Spec.Resources.CPUPinning = true
			case "device":
				m.Spec.Devices = []model.MachineDevice{{}}
			case "atlas":
				m.Spec.Volumes[0].Atlas = &model.AtlasVolumeSource{}
			case "block":
				var pvc model.PersistentVolumeClaim
				_ = json.Unmarshal(f.objects[objectPath("persistentvolumeclaims", "root")], &pvc)
				mode := "Block"
				pvc.Spec.VolumeMode = &mode
				f.put(objectPath("persistentvolumeclaims", "root"), pvc)
			case "no-guard":
				f.put("/api/v1/nodes/lab", model.Node{})
			case "same-bridge":
				m.Spec.Network.Bridge = p.Bridge
			case "no-agent":
				m.Spec.GuestAgent.Enabled = false
			case "no-pvc":
				m.Spec.Volumes = nil
			case "bad-existing":
				p.Consistency = "existing"
				p.Members[0].Snapshots = map[string]string{"root": "missing"}
			}
			f.put(objectPath("machines", "db"), m)
			out, err := f.runner().Run(context.Background(), p)
			if err == nil {
				t.Fatal("unsafe source accepted")
			}
			if len(out.Resources) != 0 {
				t.Fatal("created resources before complete preflight")
			}
			for _, request := range f.requests {
				if !strings.HasPrefix(request, "GET ") {
					t.Fatalf("preflight wrote: %s", request)
				}
			}
		})
	}
}

func TestExistingSnapshotsRetained(t *testing.T) {
	f := newFixture(t)
	p := validPlan()
	p.Consistency = "existing"
	p.Members[0].Snapshots = map[string]string{"root": "coordinated"}
	ready := true
	claim := "root"
	size := "1Gi"
	f.put(objectPath("volumesnapshots", "coordinated"), model.VolumeSnapshot{Metadata: model.ObjectMeta{Name: "coordinated", UID: "existing"}, Spec: model.VolumeSnapshotSpec{Source: model.VolumeSnapshotSource{PersistentVolumeClaimName: &claim}}, Status: model.VolumeSnapshotStatus{ReadyToUse: &ready, RestoreSize: &size}})
	f.files["coordinated"] = bytes.Clone(f.files["root"])
	out, err := f.runner().Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 2 || len(f.objects[objectPath("volumesnapshots", "coordinated")]) == 0 {
		t.Fatal("existing recovery point was deleted")
	}
}

func TestReportDurabilityAndFailClosedWriter(t *testing.T) {
	f := newFixture(t)
	r := f.runner()
	path := filepath.Join(t.TempDir(), "report.json")
	r.Save = func(out Report) error { return WriteReport(path, out) }
	out, err := r.Run(context.Background(), validPlan())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RunID != out.RunID || saved.FinishedAt == nil || !saved.Cleaned {
		t.Fatal("incomplete report")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("report not private")
	}
	f = newFixture(t)
	r = f.runner()
	r.Save = func(Report) error { return fmt.Errorf("disk full") }
	if _, err := r.Run(context.Background(), validPlan()); err == nil {
		t.Fatal("missing persistence failure")
	}
	if len(f.requests) != 0 {
		t.Fatal("contacted cluster without durable report")
	}
}

func TestDecodeRejectsTyposAndInvalidPlans(t *testing.T) {
	raw, _ := json.Marshal(validPlan())
	if _, err := Decode(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{string(raw) + `{}`, strings.Replace(string(raw), `"node":"lab"`, `"nodd":"lab"`, 1), strings.Replace(string(raw), `"consistency":"crash"`, `"consistency":"magic"`, 1)} {
		if _, err := Decode(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted invalid input %s", bad)
		}
	}
}

func TestApplicationGroupRestoresAllVolumesBeforeBoot(t *testing.T) {
	f := newFixture(t)
	var db model.Machine
	_ = json.Unmarshal(f.objects[objectPath("machines", "db")], &db)
	web := db
	web.Metadata.Name = "web"
	web.Metadata.UID = "web-source"
	web.Spec.Volumes = append([]model.MachineVolume(nil), web.Spec.Volumes...)
	web.Spec.Volumes[0].ClaimName = "web-root"
	var webPVC model.PersistentVolumeClaim
	_ = json.Unmarshal(f.objects[objectPath("persistentvolumeclaims", "root")], &webPVC)
	webPVC.Metadata.Name = "web-root"
	webPVC.Metadata.UID = "web-root-uid"
	f.put(objectPath("persistentvolumeclaims", "web-root"), webPVC)
	f.files["web-root"] = bytes.Clone(f.files["root"])
	f.put(objectPath("machines", "web"), web)
	// A second independent filesystem data volume must be restored as well.
	db.Spec.Volumes = append(db.Spec.Volumes, model.MachineVolume{Name: "data", ClaimName: "data", GuestPath: "/srv/data"})
	f.put(objectPath("machines", "db"), db)
	var pvc model.PersistentVolumeClaim
	_ = json.Unmarshal(f.objects[objectPath("persistentvolumeclaims", "root")], &pvc)
	pvc.Metadata.Name = "data"
	pvc.Metadata.UID = "data-source"
	f.put(objectPath("persistentvolumeclaims", "data"), pvc)
	f.files["data"] = bytes.Clone(f.files["root"])
	p := validPlan()
	p.Members = append(p.Members, Member{Name: "web"})
	p.Verify = append(p.Verify, Step{Name: "web-health", Machine: "web", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", "probe"}, TimeoutSeconds: 10})
	out, err := f.runner().Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 8 || len(out.Sources) != 2 || len(out.Results) != 4 {
		t.Fatalf("missing application resources: %+v", out)
	}
	claims := 0
	for _, req := range f.requests {
		if req == "POST /api/v1/namespaces/default/persistentvolumeclaims" {
			claims++
		}
		if req == "POST /apis/kairon.zyvor.dev/v1/namespaces/default/machines" && claims != 3 {
			t.Fatal("booted before all three independent restored claims existed")
		}
	}
	var source model.Machine
	_ = json.Unmarshal(f.objects[objectPath("machines", "db")], &source)
	if source.Spec.Volumes[1].ClaimName != "data" {
		t.Fatal("production data reference changed")
	}
}

func TestReservationDoesNotStealAnotherRun(t *testing.T) {
	f := newFixture(t)
	var node model.Node
	_ = json.Unmarshal(f.objects["/api/v1/nodes/lab"], &node)
	node.Metadata.Annotations[ReservationAnnotation] = "rh-existing"
	f.put("/api/v1/nodes/lab", node)
	out, err := f.runner().Run(context.Background(), validPlan())
	if err == nil {
		t.Fatal("stole reservation")
	}
	if len(out.Resources) != 0 {
		t.Fatal("created resources without exclusive reservation")
	}
	_ = json.Unmarshal(f.objects["/api/v1/nodes/lab"], &node)
	if node.Metadata.Annotations[ReservationAnnotation] != "rh-existing" {
		t.Fatal("cleared another run's reservation")
	}
}

func TestCleanupRefusesReplacedUIDAndUnknownResources(t *testing.T) {
	f := newFixture(t)
	r := f.runner()
	f.fail = "cleanup"
	out, err := r.Run(context.Background(), validPlan())
	if err == nil {
		t.Fatal("expected blocked cleanup")
	}
	f.fail = ""
	var m model.Machine
	last := out.Resources[len(out.Resources)-1]
	_ = json.Unmarshal(f.objects[objectPath("machines", last.Name)], &m)
	m.Metadata.UID = "replaced"
	f.put(objectPath("machines", last.Name), m)
	if err := r.Cleanup(context.Background(), &out); err == nil {
		t.Fatal("deleted replacement")
	}
	if len(f.deleted) != 0 {
		t.Fatal("deleted storage behind replacement VM")
	}
	out.Resources = []Resource{{Kind: "unknown", Name: out.RunID + "-foreign"}}
	if err := r.Cleanup(context.Background(), &out); err == nil {
		t.Fatal("accepted unknown cleanup resource")
	}
}

func TestSetupAndBaselineFailuresPreventUpgrade(t *testing.T) {
	for _, stage := range []string{"setup", "baseline"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			p := validPlan()
			bad := Step{Name: "bad-precondition", Machine: "db", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", "invalid-action"}, TimeoutSeconds: 10}
			if stage == "setup" {
				p.Setup = []Step{bad}
			} else {
				p.Baseline = []Step{bad}
			}
			out, err := f.runner().Run(context.Background(), p)
			if err == nil || !out.Cleaned {
				t.Fatalf("err=%v cleaned=%t", err, out.Cleaned)
			}
			if len(out.Results) != 1 {
				t.Fatalf("ran upgrade after failed %s", stage)
			}
		})
	}
}

// Two separate guests are represented by subprocesses and restored SQLite
// files. The web probe calls a persistent HTTP service reading the DB copy;
// renaming a table on the DB copy must break that cross-process API check.
func TestTwoTierHTTPRehearsal(t *testing.T) {
	for _, action := range []string{"compatible", "breaking"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			p := validPlan()
			var web model.Machine
			_ = json.Unmarshal(f.objects[objectPath("machines", "db")], &web)
			web.Metadata.Name = "web"
			web.Metadata.UID = "web-uid"
			web.Spec.Volumes = append([]model.MachineVolume(nil), web.Spec.Volumes...)
			web.Spec.Volumes[0].ClaimName = "web-root"
			var webPVC model.PersistentVolumeClaim
			_ = json.Unmarshal(f.objects[objectPath("persistentvolumeclaims", "root")], &webPVC)
			webPVC.Metadata.Name = "web-root"
			webPVC.Metadata.UID = "web-root-uid"
			f.put(objectPath("persistentvolumeclaims", "web-root"), webPVC)
			f.files["web-root"] = bytes.Clone(f.files["root"])
			f.put(objectPath("machines", "web"), web)
			p.Members = append(p.Members, Member{Name: "web"})
			p.Setup = []Step{{Name: "start-db-api", Machine: "db", Path: "/usr/bin/python3", Args: []string{"/opt/kairon-rehearsal/app.py", "probe"}, TimeoutSeconds: 10}}
			p.Baseline[0].Machine = "web"
			p.Verify[0].Machine = "web"
			p.Changes[0].Args[1] = action
			r := f.runner()
			guestExec := r.Exec
			var server *exec.Cmd
			var endpoint string
			defer func() {
				if server != nil {
					_ = server.Process.Kill()
					_ = server.Wait()
				}
			}()
			r.Exec = func(ctx context.Context, ns, name string, s Step) (ExecResult, error) {
				if s.Name == "start-db-api" {
					res, err := guestExec(ctx, ns, name, s)
					if err != nil || res.ExitCode != 0 {
						return res, err
					}
					portFile := filepath.Join(f.dir, "api-port")
					server = exec.Command("python3", "../../examples/rehearsal/app.py", "serve", "--db", filepath.Join(f.dir, name+".db"), "--port-file", portFile)
					if err := server.Start(); err != nil {
						return res, err
					}
					if err := r.poll(ctx, func() (bool, error) {
						b, e := os.ReadFile(portFile)
						if os.IsNotExist(e) {
							return false, nil
						}
						if e != nil {
							return false, e
						}
						port, parseErr := strconv.Atoi(string(b))
						if parseErr != nil || port < 1 || port > 65535 {
							return false, nil // writer may have created but not filled the file.
						}
						endpoint = "http://127.0.0.1:" + string(b) + "/health"
						return true, nil
					}); err != nil {
						return res, err
					}
					return res, nil
				}
				if s.Machine == "web" {
					s.Args = append(s.Args, "--url", endpoint)
				}
				return guestExec(ctx, ns, name, s)
			}
			out, err := r.Run(context.Background(), p)
			if (err == nil) != (action == "compatible") || !out.Cleaned {
				t.Fatalf("action=%s err=%v report=%+v", action, err, out)
			}
			if out.Results[len(out.Results)-1].Stage != "Verify" {
				t.Fatalf("did not reach API verification: err=%v report=%+v", err, out)
			}
		})
	}
}

func TestGuestReadinessRetriesBeforeUpgrade(t *testing.T) {
	f := newFixture(t)
	r := f.runner()
	guestExec := r.Exec
	attempts := 0
	r.Exec = func(ctx context.Context, ns, name string, s Step) (ExecResult, error) {
		if s.Path == "/bin/true" {
			attempts++
			if attempts == 1 {
				return ExecResult{}, fmt.Errorf("QGA not ready yet")
			}
		}
		return guestExec(ctx, ns, name, s)
	}
	out, err := r.Run(context.Background(), validPlan())
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || !out.Sources[0].GuestReady {
		t.Fatalf("readiness attempts=%d", attempts)
	}
}
