// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// genericCluster is a second, more general-purpose fake Kubernetes backend
// than fakeCluster above: fakeCluster hardcodes exactly one Machine and one
// MachineMigration by name (fine for the migration pipeline, which only
// ever needs one of each), but a MachineSet scale test needs to create and
// delete an arbitrary, changing number of Machines by generated name. Kept
// as its own type rather than generalizing fakeCluster, to avoid disturbing
// the already-passing migration pipeline test above.
type genericCluster struct {
	mu           sync.Mutex
	machines     map[string]model.Machine // keyed by "namespace/name"
	machineSets  []model.MachineSet
	nodes        []model.Node
	failNCreates int // when > 0, the next N "create machine" calls fail with 500 and decrement this
}

func (c *genericCluster) machineList() []model.Machine {
	out := make([]model.Machine, 0, len(c.machines))
	for _, m := range c.machines {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (c *genericCluster) handler() http.Handler {
	const base = "/apis/kairon.zyvor.dev/v1/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base+"machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: c.machineList()})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: c.nodes})
		case r.Method == http.MethodGet && r.URL.Path == base+"machinesets":
			_ = json.NewEncoder(w).Encode(model.MachineSetList{Items: c.machineSets})
		case r.Method == http.MethodGet && (r.URL.Path == base+"machinemigrations" ||
			r.URL.Path == base+"machinesnapshots" ||
			r.URL.Path == base+"machinesnapshotrestores" ||
			r.URL.Path == base+"machinequotas"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, base+"namespaces/") && strings.HasSuffix(r.URL.Path, "/machines"):
			if c.failNCreates > 0 {
				c.failNCreates--
				http.Error(w, "injected fault: apiserver unavailable", http.StatusInternalServerError)
				return
			}
			var m model.Machine
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c.machines[m.Namespace()+"/"+m.Metadata.Name] = m
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/machines/"):
			ns, name, ok := splitNamespacedObjectPath(r.URL.Path, "machines")
			if !ok {
				http.NotFound(w, r)
				return
			}
			delete(c.machines, ns+"/"+name)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status") && strings.Contains(r.URL.Path, "/machines/"):
			ns, name, ok := splitNamespacedObjectPath(strings.TrimSuffix(r.URL.Path, "/status"), "machines")
			if !ok {
				http.NotFound(w, r)
				return
			}
			key := ns + "/" + name
			m, ok := c.machines[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if err := applyMergePatch(r, &struct {
				Status *model.MachineStatus `json:"status"`
			}{&m.Status}); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c.machines[key] = m
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/machines/"):
			ns, name, ok := splitNamespacedObjectPath(r.URL.Path, "machines")
			if !ok {
				http.NotFound(w, r)
				return
			}
			key := ns + "/" + name
			m, ok := c.machines[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if err := applyMergePatch(r, &m); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c.machines[key] = m
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status") && strings.Contains(r.URL.Path, "/machinesets/"):
			ns, name, ok := splitNamespacedObjectPath(strings.TrimSuffix(r.URL.Path, "/status"), "machinesets")
			if !ok {
				http.NotFound(w, r)
				return
			}
			for i := range c.machineSets {
				if c.machineSets[i].Namespace() == ns && c.machineSets[i].Metadata.Name == name {
					if err := applyMergePatch(r, &struct {
						Status *model.MachineSetStatus `json:"status"`
					}{&c.machineSets[i].Status}); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusOK)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/machinesets/"):
			// Finalizer add/remove -- reconcileMachineSet adds
			// FinalizerMachineSet on its first pass over a MachineSet, and
			// reconcileMachineSetDeletion clears it once every owned Machine
			// is actually gone.
			ns, name, ok := splitNamespacedObjectPath(r.URL.Path, "machinesets")
			if !ok {
				http.NotFound(w, r)
				return
			}
			for i := range c.machineSets {
				if c.machineSets[i].Namespace() == ns && c.machineSets[i].Metadata.Name == name {
					if err := applyMergePatch(r, &c.machineSets[i]); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusOK)
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

// splitNamespacedObjectPath extracts (namespace, name) from a path of the
// shape ".../namespaces/{ns}/{resource}/{name}", the exact shape
// internal/kube.namespacedObjectPath produces.
func splitNamespacedObjectPath(path, resource string) (ns, name string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/apis/kairon.zyvor.dev/v1/namespaces/"), "/")
	if len(parts) != 3 || parts[1] != resource {
		return "", "", false
	}
	return parts[0], parts[2], true
}

func machineSetTemplate() model.MachineSpec {
	return model.MachineSpec{
		PowerState: "Running",
		Image:      model.ImageSpec{Path: "/images/web.qcow2"},
		Runtime:    model.RuntimeSpec{Backend: "qemu"},
	}
}

// TestMachineSetScalesUpAndDownThroughFullReconcile drives a MachineSet
// through kairon-controller's real, full Controller.Reconcile -- scheduler
// assignment, quota, disruption budgets, and all -- rather than calling
// reconcileMachineSet directly the way internal/controller/machineset_test.go
// already does. That unit-level coverage proves the create/delete counting
// logic is correct in isolation; this proves a scale-up's created replicas
// actually flow through scheduling to real nodes, and a scale-down removes
// the right ones, across the same multi-concern reconcile loop a live
// cluster would run every tick.
func TestMachineSetScalesUpAndDownThroughFullReconcile(t *testing.T) {
	cluster := &genericCluster{
		machines: map[string]model.Machine{},
		nodes:    []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2"), readyCapableNode("worker-3")},
		machineSets: []model.MachineSet{{
			Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"},
			Spec: model.MachineSetSpec{
				Replicas: 12,
				Template: model.MachineTemplate{Spec: machineSetTemplate()},
			},
		}},
	}
	ks := httptest.NewServer(cluster.handler())
	defer ks.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	ctl := &controller.Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: log}

	// Tick 1: scale-up creates all 12 replicas in one pass (createMachineSetReplicas
	// is not itself paced -- only rolling replacement is), but none are
	// scheduled yet: the scheduler assignment loop this same tick already
	// listed Machines *before* the MachineSet step ran.
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	cluster.mu.Lock()
	got := len(cluster.machines)
	cluster.mu.Unlock()
	if got != 12 {
		t.Fatalf("after tick 1: expected 12 replicas created, got %d", got)
	}

	// Tick 2: the 12 now-listed, unscheduled replicas get assigned to nodes.
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	cluster.mu.Lock()
	perNode := map[string]int{}
	unscheduled := 0
	for _, m := range cluster.machines {
		if m.Spec.NodeName == "" {
			unscheduled++
			continue
		}
		perNode[m.Spec.NodeName]++
	}
	cluster.mu.Unlock()
	if unscheduled != 0 {
		t.Fatalf("after tick 2: expected every replica scheduled, %d were not", unscheduled)
	}
	for _, n := range []string{"worker-1", "worker-2", "worker-3"} {
		if perNode[n] != 4 {
			t.Errorf("after tick 2: expected 4 replicas on %s, got %d (distribution=%+v)", n, perNode[n], perNode)
		}
	}

	// Scale down to 5 (simulating an operator's `kubectl edit machineset
	// web` / `kaironctl scale`) -- mutated directly on the fake backend,
	// since this project has no PatchMachineSet-spec client method (only
	// PatchMachineSetStatus, which the controller itself owns).
	cluster.mu.Lock()
	cluster.machineSets[0].Spec.Replicas = 5
	cluster.mu.Unlock()

	// Tick 3: with no template change, excess *current* replicas are
	// deleted in one pass too (only a rolling *replacement* is paced by
	// maxUnavailable -- see stepMachineSetToward's own doc comment).
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	cluster.mu.Lock()
	got = len(cluster.machines)
	cluster.mu.Unlock()
	if got != 5 {
		t.Fatalf("after scale-down tick: expected 5 replicas remaining, got %d", got)
	}
}

// TestMachineSetScaleUpRecoversFromTransientAPIServerFault injects one
// failure into the Kubernetes API server's first create call of a scale-up
// (createMachineSetReplicas returns on its very first error, so a single
// fault yields zero replicas that tick, not a partial batch) -- proving the
// failure surfaces cleanly in MachineSet.status.message rather than being
// swallowed, and that the next tick makes up the full shortfall once the
// fault clears, with no duplicate replicas.
func TestMachineSetScaleUpRecoversFromTransientAPIServerFault(t *testing.T) {
	cluster := &genericCluster{
		machines: map[string]model.Machine{},
		nodes:    []model.Node{readyCapableNode("worker-1")},
		machineSets: []model.MachineSet{{
			Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"},
			Spec: model.MachineSetSpec{
				Replicas: 4,
				Template: model.MachineTemplate{Spec: machineSetTemplate()},
			},
		}},
		failNCreates: 1, // createMachineSetReplicas returns on its first error, so one fault means zero replicas land this tick
	}
	ks := httptest.NewServer(cluster.handler())
	defer ks.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	ctl := &controller.Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: log}

	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 1 (with fault): %v", err)
	}
	cluster.mu.Lock()
	got := len(cluster.machines)
	msg := cluster.machineSets[0].Status.Message
	cluster.mu.Unlock()
	if got != 0 {
		t.Fatalf("after faulted tick: expected zero replicas (createMachineSetReplicas stops on its first error), got %d", got)
	}
	if msg == "" {
		t.Fatal("expected the faulted tick's error to surface in status.message")
	}

	// The fault has cleared (failNCreates decremented to 0 above); the next
	// tick should make up the shortfall to reach the full 4, not
	// duplicate the 2 that already exist.
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatalf("tick 2 (recovered): %v", err)
	}
	cluster.mu.Lock()
	got = len(cluster.machines)
	cluster.mu.Unlock()
	if got != 4 {
		t.Fatalf("after recovery tick: expected 4 replicas total, got %d", got)
	}
}
