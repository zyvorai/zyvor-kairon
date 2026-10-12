// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// drSite is an in-memory Kubernetes API for one site: kairon.zyvor.dev
// objects and core Secrets, with create, get, list, merge-patch and delete.
type drSite struct {
	mu      sync.Mutex
	objects map[string]map[string]any // resource/namespace/name
}

func newDRSite() *drSite { return &drSite{objects: map[string]map[string]any{}} }

func (s *drSite) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		var res, ns, name string
		status := false
		switch {
		case len(parts) >= 5 && parts[0] == "api" && parts[2] == "namespaces":
			ns, res = parts[3], parts[4]
			if len(parts) > 5 {
				name = parts[5]
			}
		case len(parts) >= 6 && parts[0] == "apis" && parts[3] == "namespaces":
			ns, res = parts[4], parts[5]
			if len(parts) > 6 {
				name = parts[6]
			}
			status = len(parts) > 7 && parts[7] == "status"
		case len(parts) == 4 && parts[0] == "apis":
			res = parts[3]
		default:
			http.Error(w, "unsupported path", http.StatusNotFound)
			return
		}
		key := res + "/" + ns + "/" + name
		body := map[string]any{}
		if req.Body != nil {
			_ = json.NewDecoder(req.Body).Decode(&body)
		}
		switch {
		case req.Method == http.MethodGet && name == "":
			var items []any
			for k, o := range s.objects {
				if strings.HasPrefix(k, res+"/") && (ns == "" || strings.HasPrefix(k, res+"/"+ns+"/")) {
					items = append(items, o)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case req.Method == http.MethodGet:
			o, ok := s.objects[key]
			if !ok {
				http.Error(w, `{"reason":"NotFound"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(o)
		case req.Method == http.MethodPost:
			meta, _ := body["metadata"].(map[string]any)
			key = res + "/" + ns + "/" + meta["name"].(string)
			if _, ok := s.objects[key]; ok {
				http.Error(w, `{"reason":"AlreadyExists"}`, http.StatusConflict)
				return
			}
			meta["namespace"] = ns
			meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			delete(body, "status")
			s.objects[key] = body
			_ = json.NewEncoder(w).Encode(body)
		case req.Method == http.MethodPatch:
			o, ok := s.objects[key]
			if !ok {
				http.Error(w, `{"reason":"NotFound"}`, http.StatusNotFound)
				return
			}
			if status {
				body = map[string]any{"status": body["status"]}
			} else {
				delete(body, "status")
			}
			s.objects[key] = mergePatch(o, body).(map[string]any)
			_ = json.NewEncoder(w).Encode(s.objects[key])
		case req.Method == http.MethodDelete:
			if _, ok := s.objects[key]; !ok {
				http.Error(w, `{"reason":"NotFound"}`, http.StatusNotFound)
				return
			}
			delete(s.objects, key)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	})
}

func mergePatch(dst, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	d, ok := dst.(map[string]any)
	if !ok {
		d = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(d, k)
			continue
		}
		d[k] = mergePatch(d[k], v)
	}
	return d
}

func (s *drSite) put(t *testing.T, res, ns, name string, obj any) {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	s.mu.Lock()
	s.objects[res+"/"+ns+"/"+name] = m
	s.mu.Unlock()
}

func (s *drSite) get(t *testing.T, res, ns, name string, out any) bool {
	t.Helper()
	s.mu.Lock()
	o, ok := s.objects[res+"/"+ns+"/"+name]
	s.mu.Unlock()
	if !ok {
		return false
	}
	raw, _ := json.Marshal(o)
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	return true
}

func (s *drSite) names(res string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if strings.HasPrefix(k, res+"/") {
			out = append(out, k[strings.LastIndex(k, "/")+1:])
		}
	}
	slices.Sort(out)
	return out
}

// settle plays kairon-node: every Machine gets a runtime and reaches its
// desired power state, every restore succeeds.
func (s *drSite) settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, o := range s.objects {
		switch {
		case strings.HasPrefix(k, "machines/"):
			spec, _ := o["spec"].(map[string]any)
			phase, _ := spec["powerState"].(string)
			if phase == "" {
				phase = "Running"
			}
			name := o["metadata"].(map[string]any)["name"].(string)
			o["status"] = map[string]any{"phase": phase, "runtimeID": "rt-" + name}
		case strings.HasPrefix(k, "machinebackuprestores/"):
			o["status"] = map[string]any{"phase": model.BackupSucceeded}
		}
	}
}

type drPair struct {
	a, b       *drSite
	ca, cb     *Controller
	urlA, urlB string
}

func newDRPair(t *testing.T) *drPair {
	t.Helper()
	p := &drPair{a: newDRSite(), b: newDRSite()}
	sa, sb := httptest.NewServer(p.a.handler(t)), httptest.NewServer(p.b.handler(t))
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	p.urlA, p.urlB = sa.URL, sb.URL
	clients := map[string]*kube.Client{}
	for _, s := range []*httptest.Server{sa, sb} {
		kc, err := kube.New(s.URL, "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		kc.HTTP = s.Client()
		clients[s.URL] = kc
	}
	peer := func(server string, _ model.Secret) (*kube.Client, error) { return clients[server], nil }
	mk := func(url string) *Controller {
		return &Controller{Kube: clients[url], Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			DR: DisasterRecovery{SecretNamespace: "kairon-system", TestBridges: []string{"drtest0"}, Peer: peer}}
	}
	p.ca, p.cb = mk(sa.URL), mk(sb.URL)
	for _, s := range []*drSite{p.a, p.b} {
		s.put(t, "secrets", "kairon-system", "peer", model.Secret{Metadata: model.ObjectMeta{Name: "peer", Namespace: "kairon-system",
			Labels: map[string]string{model.DRPeerCredentialsLabel: "true"}}, Data: map[string][]byte{"token": []byte("t")}})
	}
	p.a.put(t, "machineprotectionplans", "prod", "shop", drPlan(model.ProtectionRolePrimary, p.urlB))
	p.b.put(t, "machineprotectionplans", "prod", "shop", drPlan(model.ProtectionRoleSecondary, p.urlA))
	for _, name := range []string{"db", "web"} {
		m := model.Machine{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
			Metadata: model.ObjectMeta{Name: name, Namespace: "prod", Labels: map[string]string{"tier": name}},
			Spec:     model.MachineSpec{NodeName: "a1", PowerState: "Running"}}
		m.Spec.Resources.AllocatedCPUSet = []uint32{2, 3}
		m.Status.Phase, m.Status.RuntimeID = "Running", "rt-"+name
		p.a.put(t, "machines", "prod", name, m)
	}
	return p
}

func drPlan(role, peer string) model.MachineProtectionPlan {
	return model.MachineProtectionPlan{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineProtectionPlan},
		Metadata: model.ObjectMeta{Name: "shop", Namespace: "prod"},
		Spec: model.MachineProtectionPlanSpec{
			Role:        role,
			Machines:    []model.ProtectedMachine{{Name: "web", DependsOn: []string{"db"}}, {Name: "db"}},
			Replication: model.ProtectionReplication{Interval: "15m", Target: *s3Target()},
			Objectives:  model.ProtectionObjectives{RPO: "30m", RTO: "10m"},
			Peer:        model.ProtectionPeer{Server: peer, CredentialsSecretRef: "peer"},
			Test:        model.ProtectionTest{Bridge: "drtest0"},
		},
	}
}

func (p *drPair) reconcile(t *testing.T, c *Controller) {
	t.Helper()
	ctx := context.Background()
	machines, err := c.Kube.ListMachines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileProtectionPlans(ctx, machines); err != nil {
		t.Fatal(err)
	}
}

func (p *drPair) plan(t *testing.T, s *drSite) model.MachineProtectionPlan {
	t.Helper()
	var out model.MachineProtectionPlan
	if !s.get(t, "machineprotectionplans", "prod", "shop", &out) {
		t.Fatal("plan missing")
	}
	return out
}

// replicate adds a Succeeded dr-shop backup of machine on site A.
func (p *drPair) replicate(t *testing.T, name, machine string, at time.Time) {
	b := succeeded(cbtBackup(name, "", ""), 1, "c-"+machine, name, at)
	b.Spec.MachineName = machine
	b.Spec.PolicyName = "dr-shop"
	b.Metadata.Labels = map[string]string{model.BackupPolicyLabel: "dr-shop", model.BackupMachineLabel: machine}
	p.a.put(t, "machinebackups", "prod", name, b)
}

func (p *drPair) action(t *testing.T, s *drSite, a model.ProtectionAction) {
	plan := p.plan(t, s)
	plan.Spec.Action = &a
	s.put(t, "machineprotectionplans", "prod", "shop", plan)
}

func TestProtectionPrimaryLabelsMachinesAndOwnsReplicationPolicy(t *testing.T) {
	p := newDRPair(t)
	p.reconcile(t, p.ca)
	st := p.plan(t, p.a).Status
	if !st.Active || st.Epoch != 1 || st.Phase != model.ProtectionPhaseActive || st.Peer == nil || !st.Peer.Reachable {
		t.Fatalf("status = %+v peer=%+v", st, st.Peer)
	}
	var m model.Machine
	p.a.get(t, "machines", "prod", "web", &m)
	if m.Metadata.Labels[model.ProtectionPlanLabel] != "shop" {
		t.Fatalf("web labels = %v", m.Metadata.Labels)
	}
	var pol model.MachineBackupPolicy
	if !p.a.get(t, "machinebackuppolicies", "prod", "dr-shop", &pol) {
		t.Fatal("dr-shop policy not created")
	}
	if pol.Spec.Selector[model.ProtectionPlanLabel] != "shop" || pol.Spec.IncrementalEvery != "15m" || pol.Spec.FullEvery != "24h0m0s" || pol.Spec.Retention.Hourly != 24 {
		t.Fatalf("policy = %+v", pol.Spec)
	}
	if len(p.b.names("machinebackuppolicies")) != 0 {
		t.Fatal("standby site must not replicate")
	}
}

func TestProtectionStandbyMirrorsSpecsAndImportsRestorePoints(t *testing.T) {
	p := newDRPair(t)
	p.reconcile(t, p.ca)
	t0 := time.Now().Add(-10 * time.Minute)
	p.replicate(t, "dr-shop-db-1", "db", t0)
	p.replicate(t, "dr-shop-web-1", "web", t0.Add(time.Minute))
	stray := succeeded(cbtBackup("manual", "", ""), 1, "c-x", "manual", t0)
	p.a.put(t, "machinebackups", "prod", "manual", stray)
	p.reconcile(t, p.cb)

	if got := p.b.names("machinebackups"); !slices.Equal(got, []string{"dr-shop-db-1", "dr-shop-web-1"}) {
		t.Fatalf("imported = %v", got)
	}
	var b model.MachineBackup
	p.b.get(t, "machinebackups", "prod", "dr-shop-db-1", &b)
	if !b.Replicated() || b.Status.Phase != model.BackupSucceeded || b.Status.ManifestKey == "" || b.Metadata.Labels[model.ProtectionPlanLabel] != "shop" ||
		b.Metadata.Labels[model.BackupPolicyLabel] != "" || len(b.Metadata.Finalizers) != 0 {
		t.Fatalf("imported backup = %+v", b)
	}
	// The next pass reports the restore points it imported.
	p.reconcile(t, p.cb)
	st := p.plan(t, p.b).Status
	if st.Active || st.Phase != model.ProtectionPhaseStandby || len(st.Machines) != 2 {
		t.Fatalf("status = %+v", st)
	}
	for _, ms := range st.Machines {
		if ms.Spec == nil || ms.Spec.NodeName != "" || ms.Spec.Resources.AllocatedCPUSet != nil || ms.RestorePoint != "dr-shop-"+ms.Name+"-1" || ms.Labels["tier"] != ms.Name {
			t.Fatalf("machine status = %+v", ms)
		}
	}
	if st.RPOSeconds == nil || *st.RPOSeconds < 590 {
		t.Fatalf("rpo = %v", st.RPOSeconds)
	}
	if cond, ok := model.FindCondition(st.Conditions, "RPOMet"); !ok || cond.Status != "True" {
		t.Fatalf("RPOMet = %+v", st.Conditions)
	}

	// A restore point the active site pruned goes here too.
	p.a.mu.Lock()
	delete(p.a.objects, "machinebackups/prod/dr-shop-db-1")
	p.a.mu.Unlock()
	p.reconcile(t, p.cb)
	if got := p.b.names("machinebackups"); !slices.Equal(got, []string{"dr-shop-web-1"}) {
		t.Fatalf("after prune = %v", got)
	}
}

func TestProtectionFailoverRestoresInDependencyOrderAndFencesTheOldSite(t *testing.T) {
	p := newDRPair(t)
	p.reconcile(t, p.ca)
	t0 := time.Now().Add(-5 * time.Minute)
	p.replicate(t, "dr-shop-db-1", "db", t0)
	p.replicate(t, "dr-shop-web-1", "web", t0)
	p.reconcile(t, p.cb)
	p.action(t, p.b, model.ProtectionAction{Type: model.ProtectionActionFailover, ID: "f1"})

	var webBeforeDB bool
	for i := 0; i < 20; i++ {
		p.reconcile(t, p.cb)
		p.b.settle()
		names := p.b.names("machines")
		if slices.Contains(names, "web") && !slices.Contains(p.b.names("machinebackuprestores"), protectionRestoreName("shop", "f1", "db")) {
			webBeforeDB = true
		}
		if f := p.plan(t, p.b).Status.Failover; f != nil && f.Phase != model.ProtectionRunRunning {
			break
		}
	}
	if webBeforeDB {
		t.Fatal("web was created before db was restored")
	}
	st := p.plan(t, p.b).Status
	f := st.Failover
	if f == nil || f.Phase != model.ProtectionRunSucceeded || !slices.Equal(f.Restored, []string{"db", "web"}) || f.RTOSeconds == nil ||
		f.DataLossSeconds == nil || *f.DataLossSeconds < 290 || !st.Active || st.Epoch != 2 {
		t.Fatalf("status = %+v failover=%+v", st, f)
	}
	var r model.MachineBackupRestore
	if !p.b.get(t, "machinebackuprestores", "prod", protectionRestoreName("shop", "f1", "web"), &r) || r.Spec.BackupName != "dr-shop-web-1" || r.Spec.MachineName != "web" {
		t.Fatalf("restore = %+v", r)
	}
	var web model.Machine
	p.b.get(t, "machines", "prod", "web", &web)
	if web.Spec.PowerState != "Running" || web.Metadata.Labels[model.ProtectionPlanLabel] != "shop" || web.Metadata.Labels["tier"] != "web" || web.Spec.NodeName != "" {
		t.Fatalf("web = %+v", web)
	}

	// The old site sees the higher epoch: it halts and stops replicating.
	p.a.put(t, "machinebackuppolicies", "prod", "dr-shop", model.MachineBackupPolicy{Metadata: model.ObjectMeta{Name: "dr-shop", Namespace: "prod"}})
	p.reconcile(t, p.ca)
	sa := p.plan(t, p.a).Status
	if sa.Active || sa.Epoch != 2 {
		t.Fatalf("old site status = %+v", sa)
	}
	var old model.Machine
	p.a.get(t, "machines", "prod", "db", &old)
	if old.Spec.PowerState != "Halted" || len(p.a.names("machinebackuppolicies")) != 0 {
		t.Fatalf("old db power = %q policies = %v", old.Spec.PowerState, p.a.names("machinebackuppolicies"))
	}
	// The new site now replicates.
	p.reconcile(t, p.cb)
	if !slices.Contains(p.b.names("machinebackuppolicies"), "dr-shop") {
		t.Fatal("new active site has no dr-shop policy")
	}
}

func TestProtectionPlannedFailoverWaitsForHandoffFinalBackups(t *testing.T) {
	p := newDRPair(t)
	p.reconcile(t, p.ca)
	t0 := time.Now().Add(-20 * time.Minute)
	p.replicate(t, "dr-shop-db-1", "db", t0)
	p.replicate(t, "dr-shop-web-1", "web", t0)
	p.reconcile(t, p.cb)
	p.action(t, p.b, model.ProtectionAction{Type: model.ProtectionActionFailover, ID: "p1", Planned: true})

	p.reconcile(t, p.cb) // asks A to hand off
	if pa := p.plan(t, p.a); pa.Metadata.Annotations[model.ProtectionHandoffAnnotation] != "p1" {
		t.Fatalf("handoff annotation = %v", pa.Metadata.Annotations)
	}
	p.reconcile(t, p.ca) // A halts its Machines
	p.a.settle()
	p.reconcile(t, p.ca) // A takes final backups
	h := p.plan(t, p.a).Status.Handoff
	if h == nil || h.Phase != model.ProtectionHandoffBackup || len(h.Backups) != 2 {
		t.Fatalf("handoff = %+v", h)
	}
	var fb model.MachineBackup
	p.a.get(t, "machinebackups", "prod", h.Backups[0], &fb)
	if fb.Spec.Mode != model.BackupModeIncremental || !strings.HasPrefix(fb.Spec.Parent, "dr-shop-") || fb.Spec.Quiesce != model.BackupQuiesceNever {
		t.Fatalf("final backup spec = %+v", fb.Spec)
	}
	if len(p.b.names("machines")) != 0 {
		t.Fatal("standby created Machines before the handoff finished")
	}
	now := time.Now()
	for _, name := range h.Backups {
		var b model.MachineBackup
		p.a.get(t, "machinebackups", "prod", name, &b)
		succeeded(&b, 2, "c-"+b.Spec.MachineName, "dr-shop-"+b.Spec.MachineName+"-1", now)
		p.a.put(t, "machinebackups", "prod", name, b)
	}
	p.reconcile(t, p.ca)
	if h := p.plan(t, p.a).Status.Handoff; h.Phase != model.ProtectionHandoffReady {
		t.Fatalf("handoff = %+v", h)
	}
	for i := 0; i < 20; i++ {
		p.reconcile(t, p.cb)
		p.b.settle()
		if f := p.plan(t, p.b).Status.Failover; f != nil && f.Phase != model.ProtectionRunRunning {
			break
		}
	}
	f := p.plan(t, p.b).Status.Failover
	if f.Phase != model.ProtectionRunSucceeded || f.Points["db"] != h.Backups[0] && f.Points["db"] != h.Backups[1] || *f.DataLossSeconds != 0 {
		t.Fatalf("failover = %+v", f)
	}
}

func TestProtectionTestRunsCopiesOnTheIsolatedBridgeAndCleansUp(t *testing.T) {
	p := newDRPair(t)
	p.reconcile(t, p.ca)
	p.replicate(t, "dr-shop-db-1", "db", time.Now().Add(-time.Minute))
	p.replicate(t, "dr-shop-web-1", "web", time.Now().Add(-time.Minute))
	p.reconcile(t, p.cb)

	plan := p.plan(t, p.b)
	plan.Spec.Test.Bridge = "br-prod"
	plan.Spec.Action = &model.ProtectionAction{Type: model.ProtectionActionTest, ID: "t0"}
	p.b.put(t, "machineprotectionplans", "prod", "shop", plan)
	p.reconcile(t, p.cb)
	if st := p.plan(t, p.b).Status; st.Test != nil || !strings.Contains(st.Message, "--dr-test-bridges") {
		t.Fatalf("test on a non-allowlisted bridge: %+v", st)
	}

	plan = p.plan(t, p.b)
	plan.Spec.Test.Bridge = "drtest0"
	plan.Spec.Action = &model.ProtectionAction{Type: model.ProtectionActionTest, ID: "t1"}
	p.b.put(t, "machineprotectionplans", "prod", "shop", plan)
	for i := 0; i < 20; i++ {
		p.reconcile(t, p.cb)
		p.b.settle()
		if run := p.plan(t, p.b).Status.Test; run != nil && run.Phase != model.ProtectionRunRunning {
			break
		}
	}
	st := p.plan(t, p.b).Status
	if st.Test == nil || st.Test.Phase != model.ProtectionRunSucceeded || st.Active || st.Epoch != 1 {
		t.Fatalf("status = %+v test=%+v", st, st.Test)
	}
	if got := p.b.names("machines"); !slices.Equal(got, []string{"db-drtest", "web-drtest"}) {
		t.Fatalf("machines = %v", got)
	}
	var copyM model.Machine
	p.b.get(t, "machines", "prod", "web-drtest", &copyM)
	if copyM.Spec.Network.Mode != "tap" || copyM.Spec.Network.Bridge != "drtest0" || copyM.Metadata.Labels[protectionTestLabel] != "shop" || copyM.Metadata.Labels[model.ProtectionPlanLabel] != "" {
		t.Fatalf("test copy = %+v", copyM)
	}
	// The primary keeps running and replicating throughout.
	p.reconcile(t, p.ca)
	if sa := p.plan(t, p.a).Status; !sa.Active || sa.Epoch != 1 {
		t.Fatalf("primary = %+v", sa)
	}

	p.action(t, p.b, model.ProtectionAction{Type: model.ProtectionActionEndTest, ID: "e1"})
	p.reconcile(t, p.cb)
	if run := p.plan(t, p.b).Status.Test; run.Phase != model.ProtectionRunEnded || len(p.b.names("machines")) != 0 || len(p.b.names("machinebackuprestores")) != 0 {
		t.Fatalf("after end: test=%+v machines=%v restores=%v", run, p.b.names("machines"), p.b.names("machinebackuprestores"))
	}
}

func TestProtectionUnreachablePeerKeepsStandbyAndReportsIt(t *testing.T) {
	p := newDRPair(t)
	p.b.mu.Lock()
	delete(p.b.objects, "secrets/kairon-system/peer")
	p.b.mu.Unlock()
	p.reconcile(t, p.cb)
	st := p.plan(t, p.b).Status
	if st.Active || st.Peer == nil || st.Peer.Reachable {
		t.Fatalf("status = %+v", st)
	}
	if c, ok := model.FindCondition(st.Conditions, "PeerReachable"); !ok || c.Status != "False" {
		t.Fatalf("PeerReachable = %+v", c)
	}
	// Disabled: no --dr-secret-namespace, plans are left alone.
	p.cb.DR.SecretNamespace = ""
	p.b.put(t, "machineprotectionplans", "prod", "shop", drPlan(model.ProtectionRoleSecondary, p.urlA))
	p.reconcile(t, p.cb)
	if st := p.plan(t, p.b).Status; st.Phase != "" {
		t.Fatalf("disabled controller touched the plan: %+v", st)
	}
}
