// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const poolAPI = "/apis/kairon.zyvor.dev/v1/namespaces/prod/"

type poolFake struct {
	mu           sync.Mutex
	created      []model.Machine
	deleted      []string
	machinePatch map[string]map[string]any
	specPatch    map[string]map[string]any
	claimStatus  map[string]model.MachineClaimStatus
	poolStatus   model.MachinePoolStatus
	finalizers   map[string][]string
	conflictOn   string
	policies     map[string]model.MachineNetworkPolicy
	policyOps    []string
	snapshots    map[string]model.MachineSnapshot
}

func newPoolTestController(t *testing.T) (*Controller, *poolFake) {
	t.Helper()
	fake := &poolFake{machinePatch: map[string]map[string]any{}, specPatch: map[string]map[string]any{}, claimStatus: map[string]model.MachineClaimStatus{}, finalizers: map[string][]string{}, policies: map[string]model.MachineNetworkPolicy{}, snapshots: map[string]model.MachineSnapshot{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, poolAPI)
		var body map[string]json.RawMessage
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		switch {
		case strings.HasPrefix(path, "machinesnapshots"):
			name := strings.TrimPrefix(strings.TrimPrefix(path, "machinesnapshots"), "/")
			switch r.Method {
			case http.MethodGet:
				s, ok := fake.snapshots[name]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
					return
				}
				_ = json.NewEncoder(w).Encode(s)
			case http.MethodPost:
				var s model.MachineSnapshot
				b, _ := json.Marshal(body)
				_ = json.Unmarshal(b, &s)
				fake.snapshots[s.Metadata.Name] = s
				_, _ = w.Write(b)
			}
		case strings.HasPrefix(path, "machinenetworkpolicies"):
			name := strings.TrimPrefix(strings.TrimPrefix(path, "machinenetworkpolicies"), "/")
			b, _ := json.Marshal(body)
			switch r.Method {
			case http.MethodGet:
				p, ok := fake.policies[name]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
					return
				}
				_ = json.NewEncoder(w).Encode(p)
			case http.MethodPost:
				var p model.MachineNetworkPolicy
				_ = json.Unmarshal(b, &p)
				fake.policies[p.Metadata.Name] = p
				fake.policyOps = append(fake.policyOps, "create/"+p.Metadata.Name)
				_, _ = w.Write(b)
			case http.MethodPatch:
				p := fake.policies[name]
				var patch struct {
					Spec model.MachineNetworkPolicySpec `json:"spec"`
				}
				_ = json.Unmarshal(b, &patch)
				p.Spec = patch.Spec
				fake.policies[name] = p
				fake.policyOps = append(fake.policyOps, "patch/"+name)
				_, _ = io.WriteString(w, `{}`)
			case http.MethodDelete:
				delete(fake.policies, name)
				fake.policyOps = append(fake.policyOps, "delete/"+name)
				_, _ = io.WriteString(w, `{}`)
			}
		case r.Method == http.MethodPost && path == "machines":
			var m model.Machine
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &m)
			fake.created = append(fake.created, m)
			_, _ = w.Write(b)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "machineclaims/"):
			fake.deleted = append(fake.deleted, "claim/"+strings.TrimPrefix(path, "machineclaims/"))
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "machines/"):
			fake.deleted = append(fake.deleted, strings.TrimPrefix(path, "machines/"))
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "machines/"):
			name := strings.TrimPrefix(path, "machines/")
			if name == fake.conflictOn {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"kind":"Status","reason":"Conflict","code":409}`)
				return
			}
			var meta, spec map[string]any
			_ = json.Unmarshal(body["metadata"], &meta)
			fake.machinePatch[name] = meta
			if body["spec"] != nil {
				_ = json.Unmarshal(body["spec"], &spec)
				fake.specPatch[name] = spec
			}
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(path, "/status") && strings.HasPrefix(path, "machineclaims/"):
			var st model.MachineClaimStatus
			_ = json.Unmarshal(body["status"], &st)
			fake.claimStatus[strings.TrimSuffix(strings.TrimPrefix(path, "machineclaims/"), "/status")] = st
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && path == "machinepools/p1/status":
			_ = json.Unmarshal(body["status"], &fake.poolStatus)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && (strings.HasPrefix(path, "machineclaims/") || strings.HasPrefix(path, "machinepools/")):
			var meta struct {
				Finalizers []string `json:"finalizers"`
			}
			_ = json.Unmarshal(body["metadata"], &meta)
			fake.finalizers[path] = meta.Finalizers
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fake
}

func testPool(replicas int) model.MachinePool {
	return model.MachinePool{
		Metadata: model.ObjectMeta{Name: "p1", Namespace: "prod", Finalizers: []string{model.FinalizerMachinePool}},
		Spec: model.MachinePoolSpec{
			Replicas: replicas,
			Template: model.MachineTemplate{Labels: map[string]string{"app": "agent"}, Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}}},
		},
	}
}

func poolMember(name, hash, state, phase string, age time.Duration) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{
			Name: name, Namespace: "prod", ResourceVersion: "rv-" + name,
			CreationTimestamp: time.Now().Add(-age),
			Labels: map[string]string{
				model.LabelMachinePool:         "p1",
				model.LabelMachinePoolTemplate: hash,
				model.LabelPoolState:           state,
			},
		},
		Status: model.MachineStatus{Phase: phase},
	}
}

func testClaim(name string, age time.Duration) model.MachineClaim {
	return model.MachineClaim{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod", CreationTimestamp: time.Now().Add(-age), Finalizers: []string{model.FinalizerMachineClaim}},
		Spec:     model.MachineClaimSpec{PoolName: "p1", Labels: map[string]string{"team": "ml"}},
	}
}

func TestMachinePoolFillsWarmMembersAndReplacesOutdated(t *testing.T) {
	c, fake := newPoolTestController(t)
	pool := testPool(3)
	hash := machineSetTemplateHash(pool.Spec.Template)
	machines := []model.Machine{
		poolMember("p1-a", hash, model.PoolStateWarm, "Running", time.Minute),
		poolMember("p1-old", "stale", model.PoolStateWarm, "Running", time.Hour),
		poolMember("p1-c", hash, model.PoolStateClaimed, "Running", time.Hour),
	}
	c.reconcileMachinePools(t.Context(), []model.MachinePool{pool}, machines, map[string]bool{})

	if len(fake.created) != 2 {
		t.Fatalf("created %d warm machines, want 2", len(fake.created))
	}
	for _, m := range fake.created {
		if m.Metadata.Labels[model.LabelPoolState] != model.PoolStateWarm || m.Metadata.Labels[model.LabelMachinePoolTemplate] != hash || m.Metadata.Labels["app"] != "agent" {
			t.Fatalf("bad member labels: %v", m.Metadata.Labels)
		}
	}
	if strings.Join(fake.deleted, ",") != "p1-old" {
		t.Fatalf("deleted = %v, want the outdated warm member only", fake.deleted)
	}
	st := fake.poolStatus
	if st.Replicas != 1 || st.ReadyReplicas != 1 || st.Claimed != 1 || !strings.Contains(st.Selector, "pool-state=warm") {
		t.Fatalf("status = %+v", st)
	}
}

func TestMachinePoolTrimsBootingMembersFirst(t *testing.T) {
	c, fake := newPoolTestController(t)
	pool := testPool(1)
	hash := machineSetTemplateHash(pool.Spec.Template)
	machines := []model.Machine{
		poolMember("p1-run", hash, model.PoolStateWarm, "Running", time.Minute),
		poolMember("p1-boot", hash, model.PoolStateWarm, "Scheduling", time.Second),
	}
	c.reconcileMachinePools(t.Context(), []model.MachinePool{pool}, machines, map[string]bool{})
	if strings.Join(fake.deleted, ",") != "p1-boot" {
		t.Fatalf("deleted = %v, want the booting member", fake.deleted)
	}
}

func TestMachineClaimBindsOldestReadyMemberAndPoolRefills(t *testing.T) {
	c, fake := newPoolTestController(t)
	pool := testPool(2)
	hash := machineSetTemplateHash(pool.Spec.Template)
	machines := []model.Machine{
		poolMember("p1-new", hash, model.PoolStateWarm, "Running", time.Minute),
		poolMember("p1-old", hash, model.PoolStateWarm, "Running", time.Hour),
		poolMember("p1-boot", hash, model.PoolStateWarm, "Scheduling", time.Second),
	}
	claims := []model.MachineClaim{testClaim("second", time.Second), testClaim("first", time.Minute)}
	fake.conflictOn = "p1-old"

	bound := c.reconcileMachineClaims(t.Context(), claims, machines)

	// first claim (older) hits a 409 on p1-old and falls through to p1-new;
	// the second finds no ready member left and stays Pending.
	if got := fake.claimStatus["first"]; got.Phase != model.ClaimBound || got.MachineName != "p1-new" || got.BoundAt == nil || got.BindMillis <= 0 {
		t.Fatalf("first = %+v", got)
	}
	labels, _ := fake.machinePatch["p1-new"]["labels"].(map[string]any)
	if labels[model.LabelPoolState] != model.PoolStateClaimed || labels[model.LabelMachineClaim] != "first" || labels["team"] != "ml" || fake.machinePatch["p1-new"]["resourceVersion"] != "rv-p1-new" {
		t.Fatalf("bind patch = %v", fake.machinePatch["p1-new"])
	}
	if got := fake.claimStatus["second"]; got.Phase != model.ClaimPending || !strings.Contains(got.Message, "1 warming") {
		t.Fatalf("second = %+v", got)
	}
	if !bound["prod/p1-new"] || len(bound) != 1 {
		t.Fatalf("bound = %v", bound)
	}

	c.reconcileMachinePools(t.Context(), []model.MachinePool{pool}, machines, bound)
	if len(fake.created) != 0 {
		t.Fatalf("pool has p1-old and p1-boot warm (2 = replicas); created %d", len(fake.created))
	}
	if fake.poolStatus.Claimed != 1 || fake.poolStatus.Replicas != 2 {
		t.Fatalf("pool status = %+v", fake.poolStatus)
	}
}

func TestMachineClaimLostWhenMachineGone(t *testing.T) {
	c, fake := newPoolTestController(t)
	claim := testClaim("c1", time.Minute)
	claim.Status = model.MachineClaimStatus{Phase: model.ClaimBound, MachineName: "gone"}
	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, nil)
	if got := fake.claimStatus["c1"]; got.Phase != model.ClaimLost {
		t.Fatalf("status = %+v", got)
	}
}

func TestMachineClaimTTLDeletesClaim(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{{2 * time.Minute, "claim/c1"}, {30 * time.Second, ""}} {
		c, fake := newPoolTestController(t)
		claim := testClaim("c1", time.Hour)
		claim.Spec.TTLSeconds = 60
		bound := time.Now().Add(-tc.age)
		claim.Status = model.MachineClaimStatus{Phase: model.ClaimBound, MachineName: "m1", BoundAt: &bound}
		m := poolMember("m1", "h", model.PoolStateClaimed, "Running", time.Hour)
		c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, []model.Machine{m})
		if strings.Join(fake.deleted, ",") != tc.want {
			t.Fatalf("bound %s ago: deleted = %v, want %q", tc.age, fake.deleted, tc.want)
		}
	}
}

func TestMachineClaimReleaseDeletesOrRetains(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		policy     string
		wantDelete bool
	}{{"", true}, {model.ReclaimRetain, false}} {
		c, fake := newPoolTestController(t)
		claim := testClaim("c1", time.Minute)
		claim.Spec.ReclaimPolicy = tc.policy
		claim.Metadata.DeletionTimestamp = &now
		claim.Status = model.MachineClaimStatus{Phase: model.ClaimBound, MachineName: "m1"}
		m := poolMember("m1", "h", model.PoolStateClaimed, "Running", time.Hour)
		m.Metadata.Labels[model.LabelMachineClaim] = "c1"

		c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, []model.Machine{m})

		if (len(fake.deleted) == 1) != tc.wantDelete {
			t.Fatalf("policy %q: deleted = %v", tc.policy, fake.deleted)
		}
		if !tc.wantDelete {
			labels, _ := fake.machinePatch["m1"]["labels"].(map[string]any)
			if _, ok := labels[model.LabelMachineClaim]; !ok || labels[model.LabelMachineClaim] != nil {
				t.Fatalf("retain must clear the claim label: %v", fake.machinePatch["m1"])
			}
		}
		if f, ok := fake.finalizers["machineclaims/c1"]; !ok || len(f) != 0 {
			t.Fatalf("policy %q: finalizer not removed: %v", tc.policy, fake.finalizers)
		}
	}
}

func TestMachineClaimReleaseWaitsForSnapshot(t *testing.T) {
	now := time.Now()
	c, fake := newPoolTestController(t)
	claim := testClaim("c1", time.Minute)
	claim.Metadata.Annotations = map[string]string{"kairon.zyvor.dev/snapshot-on-release": "true"}
	claim.Metadata.DeletionTimestamp = &now
	claim.Status = model.MachineClaimStatus{Phase: model.ClaimBound, MachineName: "m1"}
	m := poolMember("m1", "h", model.PoolStateClaimed, "Running", time.Hour)
	m.Metadata.Labels[model.LabelMachineClaim] = "c1"

	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, []model.Machine{m})
	snap, ok := fake.snapshots["c1-release"]
	if !ok || snap.Spec.MachineName != "m1" {
		t.Fatalf("snapshots = %v", fake.snapshots)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("machine deleted before snapshot finished: %v", fake.deleted)
	}
	if _, ok := fake.finalizers["machineclaims/c1"]; ok {
		t.Fatalf("finalizer removed before snapshot finished: %v", fake.finalizers)
	}

	snap.Status.Phase = "Succeeded"
	fake.snapshots["c1-release"] = snap
	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, []model.Machine{m})
	if strings.Join(fake.deleted, ",") != "m1" {
		t.Fatalf("deleted = %v, want m1", fake.deleted)
	}
	if f, ok := fake.finalizers["machineclaims/c1"]; !ok || len(f) != 0 {
		t.Fatalf("finalizer not removed: %v", fake.finalizers)
	}
}

func TestMachinePoolDeletionLeavesClaimedMembers(t *testing.T) {
	c, fake := newPoolTestController(t)
	now := time.Now()
	pool := testPool(2)
	pool.Metadata.DeletionTimestamp = &now
	machines := []model.Machine{
		poolMember("w1", "h", model.PoolStateWarm, "Running", time.Hour),
		poolMember("c1", "h", model.PoolStateClaimed, "Running", time.Hour),
	}
	c.reconcileMachinePools(t.Context(), []model.MachinePool{pool}, machines, map[string]bool{})
	if strings.Join(fake.deleted, ",") != "w1" {
		t.Fatalf("deleted = %v", fake.deleted)
	}
	if _, ok := fake.finalizers["machinepools/p1"]; ok {
		t.Fatal("finalizer removed while a warm member still exists")
	}
	c.reconcileMachinePools(t.Context(), []model.MachinePool{pool}, machines[1:], map[string]bool{})
	if f, ok := fake.finalizers["machinepools/p1"]; !ok || len(f) != 0 {
		t.Fatalf("finalizer not removed: %v", fake.finalizers)
	}
}

func TestMachineClaimEgressPolicyLifecycle(t *testing.T) {
	c, fake := newPoolTestController(t)
	pool := testPool(1)
	hash := machineSetTemplateHash(pool.Spec.Template)
	claim := testClaim("agent", time.Second)
	claim.Spec.Egress = &model.ClaimEgress{AllowFqdns: []string{"api.github.com"}, AllowPorts: []string{"443"}}
	machines := []model.Machine{poolMember("p1-a", hash, model.PoolStateWarm, "Running", time.Minute)}

	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, machines)
	st := fake.claimStatus["agent"]
	name := model.ClaimEgressPolicyName("agent")
	if st.Phase != model.ClaimBound || st.EgressPolicy != name {
		t.Fatalf("status = %+v", st)
	}
	p := fake.policies[name]
	if p.Spec.MachineName != "p1-a" || p.Spec.Policy.DefaultAllow || p.Spec.Policy.AllowFqdns[0] != "api.github.com" || p.Metadata.Labels[model.LabelMachineClaim] != "agent" {
		t.Fatalf("policy = %+v", p)
	}

	// A widened allowlist is pushed to the existing policy.
	claim.Status = st
	claim.Spec.Egress.AllowCidrs = []string{"10.0.0.0/8"}
	machines[0].Metadata.Labels[model.LabelPoolState] = model.PoolStateClaimed
	machines[0].Metadata.Labels[model.LabelMachineClaim] = "agent"
	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, machines)
	if got := fake.policies[name].Spec.Policy.AllowCidrs; len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("policy not updated: %+v", fake.policies[name])
	}

	// Release deletes the policy before the Machine.
	now := time.Now()
	claim.Metadata.DeletionTimestamp = &now
	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, machines)
	if _, ok := fake.policies[name]; ok {
		t.Fatal("egress policy should be deleted on release")
	}
	if want := "create/" + name + ",patch/" + name + ",delete/" + name; strings.Join(fake.policyOps, ",") != want {
		t.Fatalf("policy ops = %v, want %s", fake.policyOps, want)
	}
	if strings.Join(fake.deleted, ",") != "p1-a" {
		t.Fatalf("deleted = %v", fake.deleted)
	}
}

func TestMachineClaimWithoutEgressCreatesNoPolicy(t *testing.T) {
	c, fake := newPoolTestController(t)
	pool := testPool(1)
	machines := []model.Machine{poolMember("p1-a", machineSetTemplateHash(pool.Spec.Template), model.PoolStateWarm, "Running", time.Minute)}
	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{testClaim("c1", time.Second)}, machines)
	if len(fake.policyOps) != 0 || fake.claimStatus["c1"].EgressPolicy != "" {
		t.Fatalf("ops = %v status = %+v", fake.policyOps, fake.claimStatus["c1"])
	}
}

func TestPausedPoolPausesAfterWarmupAndBindResumes(t *testing.T) {
	c, fake := newPoolTestController(t)
	pool := testPool(3)
	pool.Spec.WarmPowerState = "Paused"
	pool.Spec.WarmupSeconds = 60
	hash := machineSetTemplateHash(pool.Spec.Template)
	parked := poolMember("p1-parked", hash, model.PoolStateWarm, "Paused", time.Hour)
	parked.Spec.PowerState = "Paused"
	machines := []model.Machine{
		poolMember("p1-warm", hash, model.PoolStateWarm, "Running", 2*time.Minute),
		poolMember("p1-fresh", hash, model.PoolStateWarm, "Running", 10*time.Second),
		parked,
	}
	c.reconcileMachinePools(t.Context(), []model.MachinePool{pool}, machines, map[string]bool{})
	if fake.specPatch["p1-warm"]["powerState"] != "Paused" {
		t.Fatalf("warmed-up member not paused: %v", fake.specPatch)
	}
	if _, ok := fake.specPatch["p1-fresh"]; ok {
		t.Fatalf("member paused before warmup: %v", fake.specPatch["p1-fresh"])
	}
	if fake.poolStatus.ReadyReplicas != 3 {
		t.Fatalf("paused members should count as ready: %+v", fake.poolStatus)
	}

	claims := []model.MachineClaim{testClaim("c1", time.Second)}
	c.reconcileMachineClaims(t.Context(), claims, []model.Machine{parked})
	if got := fake.claimStatus["c1"]; got.Phase != model.ClaimBound || got.MachineName != "p1-parked" {
		t.Fatalf("claim = %+v", got)
	}
	if fake.specPatch["p1-parked"]["powerState"] != "Running" {
		t.Fatalf("bind did not resume: %v", fake.specPatch["p1-parked"])
	}
	ann, _ := fake.machinePatch["p1-parked"]["annotations"].(map[string]any)
	if ann[model.AnnotationLifecycleStart] == nil || ann[model.AnnotationLastActivity] == nil {
		t.Fatalf("bind did not reset lifecycle clocks: %v", ann)
	}
}

func TestRunningPoolResumesParkedMembers(t *testing.T) {
	pool := testPool(1)
	m := poolMember("p1-a", "h", model.PoolStateWarm, "Paused", time.Hour)
	m.Spec.PowerState = "Paused"
	if got := warmPowerTarget(pool.Spec, m, time.Now()); got != "Running" {
		t.Fatalf("got %q", got)
	}
	pool.Spec.Template.Spec.PowerState = "Paused"
	if got := warmPowerTarget(pool.Spec, m, time.Now()); got != "" {
		t.Fatalf("template asks for Paused; got %q", got)
	}
}

func TestMachineClaimAccruesUsage(t *testing.T) {
	c, fake := newPoolTestController(t)
	claim := testClaim("c1", time.Hour)
	bound := time.Now().Add(-2 * time.Minute)
	claim.Status = model.MachineClaimStatus{Phase: model.ClaimBound, MachineName: "m1", BoundAt: &bound}
	m := poolMember("m1", "h", model.PoolStateClaimed, "Running", time.Hour)
	m.Spec.Resources = model.ResourceSpec{CPU: "2", Memory: "2Gi"}
	c.reconcileMachineClaims(t.Context(), []model.MachineClaim{claim}, []model.Machine{m})
	u := fake.claimStatus["c1"].Usage
	if u.ActiveSeconds < 119 || u.VCPUSeconds != 2*float64(u.ActiveSeconds) || u.MemoryGiBSeconds != 2*float64(u.ActiveSeconds) {
		t.Fatalf("usage = %+v", u)
	}
}
