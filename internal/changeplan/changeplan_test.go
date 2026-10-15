// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package changeplan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/rehearsal"
)

type testAPI struct {
	mu            sync.Mutex
	objects       map[string]json.RawMessage
	calls         []string
	lost          bool
	blockRollback bool
	external      bool
	failHealth    bool
	client        *kube.Client
	t             *testing.T
}

func resource(kind, name string) string {
	if kind == "MachineNetworkPolicy" {
		return "/apis/kairon.zyvor.dev/v1/namespaces/default/machinenetworkpolicies/" + name
	}
	return "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/" + name
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func newAPI(t *testing.T) *testAPI {
	f := &testAPI{objects: map[string]json.RawMessage{}, t: t}
	f.objects[resource("Machine", "vm")] = raw(model.Machine{Metadata: model.ObjectMeta{Name: "vm", Namespace: "default", UID: "vm-uid", ResourceVersion: "1"}, Spec: model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "qemu"}, Priority: 1, PowerState: "Running", Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root"}}}, Status: model.MachineStatus{Phase: "Running"}})
	f.objects[resource("MachineNetworkPolicy", "edge")] = raw(model.MachineNetworkPolicy{Metadata: model.ObjectMeta{Name: "edge", Namespace: "default", UID: "policy-uid", ResourceVersion: "1"}, Spec: model.MachineNetworkPolicySpec{MachineName: "vm", Policy: model.VmNetworkPolicy{DefaultAllow: true}}})
	f.objects["/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshots/checkpoint"] = raw(model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "checkpoint"}, Spec: model.MachineSnapshotSpec{MachineName: "vm"}, Status: model.MachineSnapshotStatus{Phase: "Succeeded", ReadyToUse: true, SourceUID: "vm-uid"}})
	f.objects["/api/v1/namespaces/default/persistentvolumeclaims/data"] = raw(model.PersistentVolumeClaim{Metadata: model.ObjectMeta{Name: "data"}, Status: model.PersistentVolumeClaimStatus{Phase: "Bound"}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		old, ok := f.objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write(old)
			return
		}
		if r.Method != "PATCH" || r.Header.Get("Content-Type") != "application/json-patch+json" {
			t.Errorf("unsafe request %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var ops []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
			t.Error(err)
			return
		}
		if f.blockRollback && strings.Contains(r.URL.Path, "/machines/") && len(ops) > 2 {
			for _, op := range ops {
				if op["path"] == "/spec/priority" && op["value"] == float64(1) {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
			}
		}
		patch, err := jsonpatch.DecodePatch(raw(ops))
		if err != nil {
			t.Error(err)
			return
		}
		next, err := patch.Apply(old)
		if err != nil {
			http.Error(w, "CAS rejected", http.StatusConflict)
			return
		}
		var obj map[string]any
		_ = json.Unmarshal(next, &obj)
		meta := obj["metadata"].(map[string]any)
		version, _ := strconv.Atoi(meta["resourceVersion"].(string))
		meta["resourceVersion"] = strconv.Itoa(version + 1)
		spec := obj["spec"].(map[string]any)
		if strings.Contains(r.URL.Path, "/machines/") {
			status := map[string]any{"phase": spec["powerState"]}
			if disks, ok := spec["disks"]; ok {
				names := []string{}
				for _, d := range disks.([]any) {
					names = append(names, d.(map[string]any)["name"].(string))
				}
				status["attachedDisks"] = names
			}
			obj["status"] = status
		} else {
			obj["status"] = map[string]any{"effectiveSynced": true, "lastAppliedTime": time.Now().UTC().Add(time.Millisecond)}
		}
		f.objects[r.URL.Path] = raw(obj)
		if f.lost {
			f.lost = false
			http.Error(w, "lost response after commit", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(f.objects[r.URL.Path])
	}))
	t.Cleanup(server.Close)
	f.client = &kube.Client{BaseURL: server.URL, HTTP: server.Client()}
	return f
}
func fixturePlan() Plan {
	probe := func(name string) rehearsal.Step {
		return rehearsal.Step{Name: name, Machine: "vm", Path: "/bin/true", TimeoutSeconds: 5}
	}
	return Plan{APIVersion: "changeplan.kairon.zyvor.dev/v1alpha1", Name: "release", Namespace: "default", TimeoutSeconds: 5, Stages: []Stage{
		{Name: "priority", Kind: "Machine", Target: "vm", Changes: map[string]json.RawMessage{"/spec/priority": raw(9)}, Checks: []rehearsal.Step{probe("vm-health")}, RollbackChecks: []rehearsal.Step{probe("rollback-vm")}},
		{Name: "policy", Kind: "MachineNetworkPolicy", Target: "edge", Changes: map[string]json.RawMessage{"/spec/policy": raw(model.VmNetworkPolicy{DefaultAllow: false})}, Checks: []rehearsal.Step{probe("policy-health")}, RollbackChecks: []rehearsal.Step{probe("rollback-policy")}},
	}}
}
func (f *testAPI) engine() *Engine {
	return &Engine{Kube: f.client, PollInterval: time.Millisecond, RecoveryTimeout: time.Second, Save: func(r Report) error {
		if dir := os.Getenv("KAIRON_OPERATIONS_EVIDENCE"); dir != "" {
			return rehearsal.WriteReport(filepath.Join(dir, strings.ReplaceAll(f.t.Name(), "/", "-")+".json"), r)
		}
		return nil
	}, Exec: func(_ context.Context, _ string, _ string, s rehearsal.Step) (rehearsal.ExecResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.external && s.Name == "policy-health" {
			var o map[string]any
			_ = json.Unmarshal(f.objects[resource("MachineNetworkPolicy", "edge")], &o)
			o["spec"].(map[string]any)["policy"] = map[string]any{"defaultAllow": true, "allowCidrs": []string{"10.0.0.0/8"}}
			f.objects[resource("MachineNetworkPolicy", "edge")] = raw(o)
			return rehearsal.ExecResult{ExitCode: 1}, nil
		}
		if f.failHealth && s.Name == "policy-health" {
			return rehearsal.ExecResult{ExitCode: 1}, nil
		}
		return rehearsal.ExecResult{}, nil
	}}
}
func (f *testAPI) priority() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o map[string]any
	_ = json.Unmarshal(f.objects[resource("Machine", "vm")], &o)
	return o["spec"].(map[string]any)["priority"].(float64)
}

func TestChangePlanCommitAndCompensation(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "commit"
		if failure {
			name = "health-failure-rollback"
		}
		t.Run(name, func(t *testing.T) {
			f := newAPI(t)
			f.failHealth = failure
			e := f.engine()
			p, err := e.Prepare(context.Background(), fixturePlan())
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range f.calls {
				if !strings.HasPrefix(call, "GET ") {
					t.Fatal("preview mutated cluster")
				}
			}
			report, err := e.Apply(context.Background(), p)
			if failure {
				if err == nil || report.Phase != "RolledBack" || f.priority() != 1 {
					t.Fatalf("err=%v report=%+v", err, report)
				}
			} else if err != nil || report.Phase != "Committed" || f.priority() != 9 {
				t.Fatalf("err=%v report=%+v", err, report)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, kind := range []string{"Machine", "MachineNetworkPolicy"} {
				target := "vm"
				if kind != "Machine" {
					target = "edge"
				}
				var o object
				_ = json.Unmarshal(f.objects[resource(kind, target)], &o)
				if o.Metadata.Annotations[Annotation] != "" {
					t.Fatal("reservation leaked")
				}
			}
		})
	}
}
func TestLostForwardResponseIsCompensated(t *testing.T) {
	f := newAPI(t)
	e := f.engine()
	p, err := e.Prepare(context.Background(), fixturePlan())
	if err != nil {
		t.Fatal(err)
	}
	f.lost = true
	report, err := e.Apply(context.Background(), p)
	if err == nil || report.Phase != "RolledBack" || f.priority() != 1 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
}
func TestExternalMutationStopsRollback(t *testing.T) {
	f := newAPI(t)
	e := f.engine()
	p, err := e.Prepare(context.Background(), fixturePlan())
	if err != nil {
		t.Fatal(err)
	}
	f.external = true
	report, err := e.Apply(context.Background(), p)
	if err == nil || report.Phase != "NeedsRecovery" {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	if f.priority() != 9 {
		t.Fatal("compensated earlier dependency past an ambiguous later stage")
	}
}
func TestRollbackRetryAndDurableDecision(t *testing.T) {
	f := newAPI(t)
	f.failHealth = true
	f.blockRollback = true
	e := f.engine()
	p, err := e.Prepare(context.Background(), fixturePlan())
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.Apply(context.Background(), p)
	if err == nil || report.Phase != "NeedsRecovery" {
		t.Fatal(report, err)
	}
	f.blockRollback = false
	if err := e.Recover(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Phase != "RolledBack" || f.priority() != 1 {
		t.Fatal(report)
	}
	// Recovery is idempotent, never replays the original forward actions.
	if err := e.Recover(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
}
func TestDiskCheckpointAndCompensation(t *testing.T) {
	f := newAPI(t)
	e := f.engine()
	p := fixturePlan()
	p.Stages[0].Changes = map[string]json.RawMessage{"/spec/disks": raw([]model.MachineDisk{{Name: "data", ClaimName: "data"}})}
	p.Stages[0].Checkpoint = "checkpoint"
	f.failHealth = true
	p, err := e.Prepare(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.Apply(context.Background(), p)
	if err == nil || report.Phase != "RolledBack" {
		t.Fatal(report, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var o object
	_ = json.Unmarshal(f.objects[resource("Machine", "vm")], &o)
	if _, ok := o.Spec["disks"]; ok {
		t.Fatal("disk attachment not removed")
	}
	if _, ok := f.objects["/api/v1/namespaces/default/persistentvolumeclaims/data"]; !ok {
		t.Fatal("transaction deleted user PVC")
	}
}
func TestPreconditionsTamperAndWriterFailures(t *testing.T) {
	for _, mode := range []string{"tamper", "uid", "version", "writer", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPI(t)
			e := f.engine()
			p, err := e.Prepare(context.Background(), fixturePlan())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			switch mode {
			case "tamper":
				p.Stages[0].Changes["/spec/priority"] = raw(99)
			case "uid", "version":
				f.mu.Lock()
				var o object
				_ = json.Unmarshal(f.objects[resource("Machine", "vm")], &o)
				if mode == "uid" {
					o.Metadata.UID = "replacement"
				} else {
					o.Metadata.ResourceVersion = "42"
				}
				f.objects[resource("Machine", "vm")] = raw(o)
				f.mu.Unlock()
			case "writer":
				e.Save = func(Report) error { return fmt.Errorf("disk full") }
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := e.Apply(ctx, p); err == nil {
				t.Fatal("expected refusal")
			}
			if f.priority() != 1 {
				t.Fatal("changed object after failed preconditions")
			}
		})
	}
}

func TestRecoverFollowsLastDurableDecision(t *testing.T) {
	for _, failAt := range []string{"commit-decision", "released-entry"} {
		t.Run(failAt, func(t *testing.T) {
			f := newAPI(t)
			e := f.engine()
			p, err := e.Prepare(context.Background(), fixturePlan())
			if err != nil {
				t.Fatal(err)
			}
			var durable Report
			failed := false
			e.Save = func(r Report) error {
				trigger := r.Decision == "Commit" && !failed
				if failAt == "released-entry" {
					trigger = trigger && len(r.Entries) > 0 && r.Entries[len(r.Entries)-1].State == "Released"
				}
				if trigger {
					failed = true
					return fmt.Errorf("journal disk unavailable")
				}
				if failed {
					return fmt.Errorf("journal remains unavailable")
				}
				return json.Unmarshal(raw(r), &durable)
			}
			if _, err := e.Apply(context.Background(), p); err == nil || !failed {
				t.Fatal("expected simulated durable-write failure", err)
			}
			// Simulate restarting from disk, not the failed process's report.
			e.Save = func(Report) error { return nil }
			if err := e.Recover(context.Background(), &durable); err != nil {
				t.Fatal(err)
			}
			expected, priority := "Committed", float64(9)
			if failAt == "commit-decision" {
				expected, priority = "RolledBack", 1
			}
			if durable.Phase != expected || f.priority() != priority {
				t.Fatal(durable.Phase, f.priority())
			}
		})
	}
}

func TestRecoverRejectsIncompleteOrCorruptJournal(t *testing.T) {
	f := newAPI(t)
	e := f.engine()
	p, err := e.Prepare(context.Background(), fixturePlan())
	if err != nil {
		t.Fatal(err)
	}
	r := Report{Version: 1, ID: "cp-0123456789abcdef01234567", Plan: p, Decision: "Commit"}
	if err := e.Recover(context.Background(), &r); err == nil {
		t.Fatal("accepted truncated commit journal")
	}
	r.Decision = "Rollback"
	r.Entries = []Entry{{Stage: p.Stages[0], State: "MadeUp", AppliedAt: time.Now()}}
	if err := e.Recover(context.Background(), &r); err == nil {
		t.Fatal("accepted unknown entry state")
	}
	if f.priority() != 1 {
		t.Fatal("corrupt journal mutated the target")
	}
}

func TestChangePlanExamplesAndNestedValidation(t *testing.T) {
	for _, name := range []string{"changeplan.json", "disk-changeplan.json"} {
		b, err := os.ReadFile(filepath.Join("../../examples/operations", name))
		if err != nil {
			t.Fatal(err)
		}
		var p Plan
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(false); err != nil {
			t.Fatal(name, err)
		}
	}
	p := fixturePlan()
	p.Stages[1].Changes["/spec/policy"] = raw(map[string]any{"defaultAllow": false, "misspelledPolicyField": true})
	if err := p.Validate(false); err == nil {
		t.Fatal("nested policy typo accepted")
	}
}

func TestDiskReplacementUnderExistingNameIsRefused(t *testing.T) {
	f := newAPI(t)
	var m model.Machine
	_ = json.Unmarshal(f.objects[resource("Machine", "vm")], &m)
	m.Spec.Disks = []model.MachineDisk{{Name: "data", ClaimName: "old-data"}}
	f.objects[resource("Machine", "vm")] = raw(m)
	p := fixturePlan()
	p.Stages[0].Changes = map[string]json.RawMessage{"/spec/disks": raw([]model.MachineDisk{{Name: "data", ClaimName: "data"}})}
	p.Stages[0].Checkpoint = "checkpoint"
	if _, err := f.engine().Prepare(context.Background(), p); err == nil {
		t.Fatal("ambiguous same-name disk replacement accepted")
	}
}
