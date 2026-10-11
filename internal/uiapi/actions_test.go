// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func runningMachine(name string) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "default", ResourceVersion: "7"},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status:   model.MachineStatus{Phase: "Running"},
	}
	m.Spec.Resources.CPU = "2"
	m.Spec.Resources.Memory = "2Gi"
	return m
}

func TestForkMachineCreatesPinnedChildren(t *testing.T) {
	fk := newFakeKube()
	fk.machines["web"] = runningMachine("web")
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/fork", "", forkRequest{Count: 2, Prefix: "kid"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	for _, n := range []string{"kid-1", "kid-2"} {
		c, ok := fk.machines[n]
		if !ok {
			t.Fatalf("child %s not created", n)
		}
		if c.Metadata.Annotations[model.AnnotationForkFrom] != "web" || c.Metadata.Labels[model.AssignedNodeLabel] != "worker-1" {
			t.Errorf("child %s missing fork metadata: %+v", n, c.Metadata)
		}
		if c.Spec.PowerState != "Running" {
			t.Errorf("child %s power state %q", n, c.Spec.PowerState)
		}
	}
}

func TestForkRefusesNonRunningAndBadCount(t *testing.T) {
	fk := newFakeKube()
	stopped := runningMachine("db")
	stopped.Status.Phase = "Stopped"
	fk.machines["db"] = stopped
	h := newTestServer(t, fk, "").Handler()

	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/db/fork", "", forkRequest{}); rr.Code != http.StatusConflict {
		t.Errorf("stopped machine: status %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/db/fork", "", forkRequest{Count: 99}); rr.Code != http.StatusBadRequest {
		t.Errorf("count 99: status %d", rr.Code)
	}
}

func TestHotplugDiskAttachDetach(t *testing.T) {
	fk := newFakeKube()
	fk.machines["web"] = runningMachine("web")
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/disks", "", hotplugRequest{Action: "attach", Name: "data", Claim: "pvc-1"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("attach: %d %s", rr.Code, rr.Body.String())
	}
	patch := strings.Join(fk.patches["/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web"], "")
	for _, want := range []string{`"resourceVersion":"7"`, `"name":"data"`, `"claimName":"pvc-1"`} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch %s missing %s", patch, want)
		}
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/disks", "", hotplugRequest{Action: "detach", Name: "nope"}); rr.Code != http.StatusConflict {
		t.Errorf("detach unknown: %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/disks", "", hotplugRequest{Action: "attach", Name: "x"}); rr.Code != http.StatusBadRequest {
		t.Errorf("attach without claim: %d", rr.Code)
	}
}

func TestHotplugNICAddRequiresBridge(t *testing.T) {
	fk := newFakeKube()
	tap := runningMachine("web")
	tap.Spec.Network.Mode = "tap"
	fk.machines["web"] = tap
	user := runningMachine("slirp")
	user.Spec.Network.Mode = "user"
	fk.machines["slirp"] = user
	h := newTestServer(t, fk, "").Handler()

	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/slirp/nics", "", hotplugRequest{Action: "add", Name: "eth1", Bridge: "br0"}); rr.Code != http.StatusBadRequest {
		t.Errorf("add on non-tap machine: %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/nics", "", hotplugRequest{Action: "add", Name: "eth1"}); rr.Code != http.StatusBadRequest {
		t.Errorf("add without bridge: %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/nics", "", hotplugRequest{Action: "remove", Name: "eth9"}); rr.Code != http.StatusConflict {
		t.Errorf("remove unknown: %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/nics", "", hotplugRequest{Action: "add", Name: "eth1", Bridge: "br0"}); rr.Code != http.StatusNoContent {
		t.Errorf("add: %d", rr.Code)
	}
}

func TestTriggerSnapshotScheduleSetsAnnotation(t *testing.T) {
	fk := newFakeKube()
	fk.snapshotSchedules["nightly"] = model.MachineSnapshotSchedule{Metadata: model.ObjectMeta{Name: "nightly", Namespace: "default"}}
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/snapshot-schedules/default/nightly/trigger", "", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	patch := strings.Join(fk.patches["/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotschedules/nightly"], "")
	if !strings.Contains(patch, model.AnnotationSnapshotScheduleTriggerNow) {
		t.Errorf("patch %s missing trigger annotation", patch)
	}
}
