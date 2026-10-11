// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package selftest

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// fakeKube simulates just enough of the controller: a created Machine goes
// Running, powerState patches move the phase, and a cpu patch is applied.
type fakeKube struct {
	mu        sync.Mutex
	machines  map[string]*model.Machine
	failBoot  bool
	nodes     []model.Node
	migrate   bool
	deleted   []string
	createErr error
}

func newFakeKube() *fakeKube {
	ready := []model.NodeCondition{{Type: "Ready", Status: "True"}}
	node := func(name string) model.Node {
		return model.Node{
			Metadata: model.ObjectMeta{Name: name, Labels: map[string]string{model.CapableLabel: "true", "kubernetes.io/arch": "amd64"}},
			Status:   model.NodeStatus{Conditions: ready, Addresses: []model.NodeAddress{{Type: "InternalIP", Address: "10.0.0." + name[len(name)-1:]}}},
		}
	}
	return &fakeKube{machines: map[string]*model.Machine{}, nodes: []model.Node{node("n1")}}
}

func (f *fakeKube) CreateMachine(_ context.Context, ns string, m model.Machine) (model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return model.Machine{}, f.createErr
	}
	m.Metadata.Namespace = ns
	m.Status.NodeName = "n1"
	m.Status.Phase = "Running"
	if f.failBoot {
		m.Status.Phase, m.Status.Message = "Failed", "image digest mismatch"
	}
	m.Status.AppliedVCPUs = 1
	m.Status.ResourceUsage = &model.ResourceUsage{CPUPercent: 3, MemoryBytes: 64 << 20}
	m.Status.Conditions = []model.Condition{{Type: "Ready", Status: "True"}}
	f.machines[m.Metadata.Name] = &m
	return m, nil
}

func (f *fakeKube) GetMachine(_ context.Context, _, name string) (model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[name]
	if !ok {
		return model.Machine{}, &kube.APIError{StatusCode: 404}
	}
	return *m, nil
}

func (f *fakeKube) DeleteMachine(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.machines, name)
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeKube) PatchMachine(_ context.Context, _, name string, patch map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[name]
	if !ok {
		return &kube.APIError{StatusCode: 404}
	}
	spec, _ := patch["spec"].(map[string]any)
	if ps, ok := spec["powerState"].(string); ok {
		m.Status.Phase = ps
	}
	if res, ok := spec["resources"].(map[string]any); ok && res["cpu"] == "2" {
		m.Status.AppliedVCPUs = 2
	}
	return nil
}

func (f *fakeKube) ListNodes(context.Context) ([]model.Node, error) { return f.nodes, nil }
func (f *fakeKube) ListEventsFor(context.Context, string, string, string) ([]model.Event, error) {
	return []model.Event{{Reason: "Scheduled", Message: "placed", Type: "Normal"}}, nil
}
func (f *fakeKube) CreateMachineMigration(_ context.Context, _ string, m model.MachineMigration) (model.MachineMigration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrate = true
	if vm, ok := f.machines[m.Spec.MachineName]; ok {
		vm.Status.NodeName = m.Spec.TargetNode
	}
	return m, nil
}
func (f *fakeKube) GetMachineMigration(context.Context, string, string) (model.MachineMigration, error) {
	return model.MachineMigration{Status: model.MachineMigrationStatus{Phase: "Succeeded"}}, nil
}

type recorder struct {
	logs    []string
	results []Result
}

func (r *recorder) Log(l Level, m string)      { r.logs = append(r.logs, string(l)+": "+m) }
func (r *recorder) StepStart(id, title string) {}
func (r *recorder) StepDone(res Result)        { r.results = append(r.results, res) }
func (r *recorder) status(id string) Status {
	for _, x := range r.results {
		if x.ID == id {
			return x.Status
		}
	}
	return ""
}

func baseEnv(f *fakeKube) Env {
	return Env{
		Kube: f, Namespace: "default", PollInterval: time.Millisecond, BootTimeout: time.Second, StepTimeout: time.Second,
		RunID: "t1",
		ControlPlane: func(context.Context) []Check {
			return []Check{{Name: "controller", Status: Pass, Detail: "ready"}}
		},
		SerialLog: func(context.Context, string, string) (string, error) {
			return "booting\ncirros login: ", nil
		},
		Dial: func(context.Context, string) (string, error) { return "SSH-2.0-dropbear_2022.83\r\n", nil },
	}
}

func TestStandardProfileAllPassAndCleansUp(t *testing.T) {
	f := newFakeKube()
	rec := &recorder{}
	res := Run(context.Background(), baseEnv(f), rec, Options{Profile: ProfileStandard})

	for _, id := range []string{"control-plane", "boot", "console-log", "power", "network", "pause-resume", "hotplug-cpu", "observability"} {
		if got := rec.status(id); got != Pass {
			t.Errorf("%s = %q, want pass; logs:\n%s", id, got, strings.Join(rec.logs, "\n"))
		}
	}
	if s := Summarize(res); s.Fail != 0 {
		t.Errorf("summary = %+v", s)
	}
	if ExitCode(res, false) != 0 {
		t.Error("exit code should be 0")
	}
	if len(f.machines) != 0 {
		t.Errorf("machines left behind: %v", f.machines)
	}
}

func TestBootFailureSkipsDependentsAndCleansUp(t *testing.T) {
	f := newFakeKube()
	f.failBoot = true
	rec := &recorder{}
	res := Run(context.Background(), baseEnv(f), rec, Options{Profile: ProfileStandard})

	if rec.status("boot") != Fail {
		t.Fatalf("boot = %q", rec.status("boot"))
	}
	for _, id := range []string{"console-log", "power", "network", "pause-resume", "hotplug-cpu", "observability"} {
		if rec.status(id) != Skip {
			t.Errorf("%s = %q, want skip after boot failure", id, rec.status(id))
		}
	}
	if ExitCode(res, false) != 1 {
		t.Error("exit code should be 1")
	}
	var boot Result
	for _, r := range res {
		if r.ID == "boot" {
			boot = r
		}
	}
	if !strings.Contains(boot.Reason, "image digest mismatch") {
		t.Errorf("boot reason lost the controller message: %q", boot.Reason)
	}
	if len(f.machines) != 0 {
		t.Errorf("machines left behind: %v", f.machines)
	}
}

func TestCancelledRunStillCleansUp(t *testing.T) {
	f := newFakeKube()
	ctx, cancel := context.WithCancel(context.Background())
	env := baseEnv(f)
	env.SerialLog = func(context.Context, string, string) (string, error) {
		cancel() // Ctrl-C arrives while waiting for the login prompt
		return "", nil
	}
	rec := &recorder{}
	Run(ctx, env, rec, Options{Profile: ProfileQuick})
	if len(f.machines) != 0 {
		t.Errorf("machines left behind after cancel: %v", f.machines)
	}
	if len(f.deleted) == 0 {
		t.Error("expected the test Machine to be deleted")
	}
}

func TestKeepLeavesMachine(t *testing.T) {
	f := newFakeKube()
	env := baseEnv(f)
	env.Keep = true
	Run(context.Background(), env, &recorder{}, Options{Profile: ProfileQuick})
	if len(f.machines) != 1 {
		t.Errorf("--keep should leave one Machine, have %d", len(f.machines))
	}
}

func TestNetworkSkipsWhenUnreachable(t *testing.T) {
	f := newFakeKube()
	env := baseEnv(f)
	env.Dial = func(context.Context, string) (string, error) { return "", errors.New("i/o timeout") }
	rec := &recorder{}
	Run(context.Background(), env, rec, Options{Profile: ProfileStandard, Only: []string{"control-plane", "boot", "network"}})
	if rec.status("network") != Skip {
		t.Fatalf("network = %q, want skip", rec.status("network"))
	}
	if rec.status("boot") != Pass {
		t.Errorf("boot = %q", rec.status("boot"))
	}
}

func TestNoVMRunsOnlyControlPlane(t *testing.T) {
	f := newFakeKube()
	env := baseEnv(f)
	env.NoVM = true
	rec := &recorder{}
	Run(context.Background(), env, rec, Options{Profile: ProfileStandard})
	if rec.status("control-plane") != Pass || rec.status("boot") != Skip {
		t.Errorf("control-plane=%q boot=%q", rec.status("control-plane"), rec.status("boot"))
	}
	if len(f.machines) != 0 {
		t.Error("--no-vm must not create Machines")
	}
}

func TestOnlySkipAndProfileFiltering(t *testing.T) {
	f := newFakeKube()
	rec := &recorder{}
	Run(context.Background(), baseEnv(f), rec, Options{Profile: ProfileQuick, Skip: []string{"console-log"}})
	if rec.status("console-log") != "" {
		t.Error("console-log should be filtered out by --skip")
	}
	if rec.status("network") != "" {
		t.Error("network is a standard-profile scenario and must not run in quick")
	}
	if rec.status("power") != Pass {
		t.Errorf("power = %q", rec.status("power"))
	}
}

func TestFullProfileSkipsWithReasons(t *testing.T) {
	f := newFakeKube() // one node: no migration; no bridge/pvc
	rec := &recorder{}
	res := Run(context.Background(), baseEnv(f), rec, Options{Profile: ProfileFull})
	reasons := map[string]string{}
	for _, r := range res {
		reasons[r.ID] = r.Reason
	}
	for id, want := range map[string]string{
		"nic-hotplug":    "--bridge",
		"disk-hotplug":   "--pvc",
		"cold-migration": "two capable",
	} {
		if rec.status(id) != Skip || !strings.Contains(reasons[id], want) {
			t.Errorf("%s: status %q reason %q, want skip mentioning %q", id, rec.status(id), reasons[id], want)
		}
	}
}

func TestColdMigrationWithTwoNodes(t *testing.T) {
	f := newFakeKube()
	n2 := f.nodes[0]
	n2.Metadata = model.ObjectMeta{Name: "n2", Labels: f.nodes[0].Metadata.Labels}
	f.nodes = append(f.nodes, n2)
	rec := &recorder{}
	Run(context.Background(), baseEnv(f), rec, Options{Profile: ProfileFull, Only: []string{"control-plane", "boot", "cold-migration"}})
	if rec.status("cold-migration") != Pass || !f.migrate {
		t.Errorf("cold-migration = %q (migrate=%v)\n%s", rec.status("cold-migration"), f.migrate, strings.Join(rec.logs, "\n"))
	}
}

func TestPinnedCirrosDigestsAreWellFormed(t *testing.T) {
	re := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	for arch, p := range cirros {
		if !re.MatchString(p.Digest) {
			t.Errorf("%s digest %q is not sha256:<64 hex>", arch, p.Digest)
		}
		if !strings.HasPrefix(p.URL, "https://download.cirros-cloud.net/") {
			t.Errorf("%s URL %q", arch, p.URL)
		}
	}
}

func TestParseProfileAndUnknownIDs(t *testing.T) {
	if p, err := ParseProfile(""); err != nil || p != ProfileStandard {
		t.Errorf("default profile = %q, %v", p, err)
	}
	if _, err := ParseProfile("huge"); err == nil {
		t.Error("want error for unknown profile")
	}
	if bad := UnknownIDs([]string{"boot", "nope"}); len(bad) != 1 || bad[0] != "nope" {
		t.Errorf("UnknownIDs = %v", bad)
	}
}
