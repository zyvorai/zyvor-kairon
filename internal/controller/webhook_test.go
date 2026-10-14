// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/conversion"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
)

// newWebhookTestController wires a *Controller to an in-memory fake
// Kubernetes API serving exactly the namespaced list endpoints
// validateMachine/validateMachineMigration call -- mirrors
// TestReconcileSchedulesMachine's own inline-httptest-server convention
// above rather than introducing a new test-double abstraction.
func newWebhookTestController(t *testing.T, ns string, quotas []model.MachineQuota, machines []model.Machine, budgets []model.MachineDisruptionBudget, migrations []model.MachineMigration) *Controller {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/"+ns+"/machinequotas":
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: quotas})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/"+ns+"/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: machines})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/"+ns+"/machinedisruptionbudgets":
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: budgets})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/"+ns+"/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: migrations})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/"+ns+"/machineinstancetypes/small":
			_ = json.NewEncoder(w).Encode(model.MachineInstanceType{Metadata: model.ObjectMeta{Namespace: ns, Name: "small"}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machineclusterpreferences/linux":
			_ = json.NewEncoder(w).Encode(model.MachineClusterPreference{Metadata: model.ObjectMeta{Name: "linux"}})
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
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func admissionReq(t *testing.T, resource, namespace, operation string, obj any) (*http.Request, *admission.Request) {
	t.Helper()
	return admissionReqWithOld(t, resource, namespace, operation, obj, nil)
}

// admissionReqWithOld is admissionReq plus oldObj, marshaled into
// OldObject -- the real API server only ever sets this for UPDATE/DELETE
// (see admission.Request's own doc comment), so tests exercising
// validateMachineResize need this instead of the plain admissionReq
// CREATE-shaped helper. A nil oldObj leaves OldObject unset.
func admissionReqWithOld(t *testing.T, resource, namespace, operation string, obj, oldObj any) (*http.Request, *admission.Request) {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	req := &admission.Request{
		UID:       "test-uid",
		Resource:  admission.GroupVersionResource{Group: "kairon.zyvor.dev", Version: "v1alpha1", Resource: resource},
		Namespace: namespace,
		Operation: operation,
		Object:    raw,
	}
	if oldObj != nil {
		oldRaw, err := json.Marshal(oldObj)
		if err != nil {
			t.Fatalf("marshal old object: %v", err)
		}
		req.OldObject = oldRaw
	}
	r := httptest.NewRequest(http.MethodPost, "/validate", nil).WithContext(context.Background())
	return r, req
}

func TestValidateMachineIgnoresNonMachineResources(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{{Spec: model.MachineQuotaSpec{MaxMachines: intPtr(0)}, Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}}}, nil, nil, nil)

	r, req := admissionReq(t, "machinemigrations", "prod", admission.OperationCreate, model.Machine{})
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected non-machines resource to be ignored, got denied: %s", d.Reason)
	}
}

func TestValidateMachineIgnoresOtherOperations(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{{Spec: model.MachineQuotaSpec{MaxMachines: intPtr(0)}, Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}}}, nil, nil, nil)
	r, req := admissionReq(t, "machines", "prod", "DELETE", model.Machine{Metadata: model.ObjectMeta{Namespace: "prod"}})
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected an operation this webhook doesn't handle to be ignored, got denied: %s", d.Reason)
	}
}

func TestValidateMachineResizeAllowsUnscheduledMachine(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "1"}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, nil, nil, nil)

	old := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "1"}}} // NodeName unset: not yet scheduled
	updated := old
	updated.Spec.Resources.CPU = "100"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, updated, old)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected a resize of an unscheduled Machine to be allowed (the reconcile loop's own scheduling-time check backstops it), got denied: %s", d.Reason)
	}
}

func TestValidateMachineUpdateCdromsRemoveOnly(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	old := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "win"}, Spec: model.MachineSpec{
		Cdroms: []model.MachineCdrom{{Name: "install", ImageRef: "ws2022-iso"}, {Name: "virtio", ImageRef: "virtio-win"}},
	}}
	ejected := old
	ejected.Spec.Cdroms = old.Spec.Cdroms[1:]
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, ejected, old)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("removing a cdrom (eject) should be allowed, got denied: %s", d.Reason)
	}
	added := old
	added.Spec.Cdroms = append(append([]model.MachineCdrom(nil), old.Spec.Cdroms...), model.MachineCdrom{Name: "tools", ImageRef: "tools-iso"})
	r, req = admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, added, old)
	if d := ctl.validateMachine(r, req); d.Allowed || !strings.Contains(d.Reason, "can't be added") {
		t.Fatalf("adding a cdrom after creation should be denied, got %+v", d)
	}
}

func TestValidateMachineResizeAllowsShrinkOrNoChange(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "2"}}
	existing := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "2"}}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, []model.Machine{existing}, nil, nil)

	shrunk := existing
	shrunk.Spec.Resources.CPU = "1"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, shrunk, existing)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected a shrink to be allowed without even listing quotas, got denied: %s", d.Reason)
	}

	r, req = admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, existing, existing)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected a no-change update to be allowed, got denied: %s", d.Reason)
	}
}

func TestValidateMachineResizeDeniesGrowthOverMaxTotalCPU(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "4"}}
	// Two Machines already scheduled: vm-1 (being resized) at 2 vCPU, and
	// an unrelated vm-2 at 2 vCPU -- together already at the 4 vCPU cap,
	// so growing vm-1 at all must be denied.
	vm1 := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "2"}}}
	vm2 := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-2"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "2"}}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, []model.Machine{vm1, vm2}, nil, nil)

	grown := vm1
	grown.Spec.Resources.CPU = "3"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, grown, vm1)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected growth past maxTotalCpu to be denied")
	}
	if d.Reason == "" {
		t.Fatal("expected a non-empty denial reason")
	}
}

func TestValidateMachineResizeAllowsGrowthWithinMaxTotalCPU(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "4"}}
	vm1 := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "2"}}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, []model.Machine{vm1}, nil, nil)

	grown := vm1
	grown.Spec.Resources.CPU = "4"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, grown, vm1)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected growth within maxTotalCpu to be allowed, got denied: %s", d.Reason)
	}
}

func TestValidateMachineResizeDeniesGrowthOverMaxTotalMemory(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalMemory: "4Gi"}}
	vm1 := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{Memory: "4Gi"}}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, []model.Machine{vm1}, nil, nil)

	grown := vm1
	grown.Spec.Resources.Memory = "8Gi"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, grown, vm1)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected growth past maxTotalMemory to be denied")
	}
}

func TestValidateMachineDeniesCreateWithMalformedImageSource(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec: model.MachineSpec{Image: model.ImageSpec{
			Source: &model.ImageSource{HTTPURL: "http://example.invalid/x.qcow2"},
			// Digest deliberately unset.
		}},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected a spec.image.source with no digest to be denied")
	}
}

func TestValidateMachineAllowsCreateWithWellFormedImageSource(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec: model.MachineSpec{Image: model.ImageSpec{
			Source: &model.ImageSource{HTTPURL: "https://example.invalid/x.qcow2"},
			Digest: "sha256:" + strings.Repeat("a", 64),
		}},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected a well-formed spec.image.source to be allowed, got denied: %s", d.Reason)
	}
}

func TestValidateResourceQuantities(t *testing.T) {
	cases := []struct {
		name    string
		res     model.ResourceSpec
		wantErr bool
	}{
		{"empty resources allowed (awaiting instance-type resolution)", model.ResourceSpec{}, false},
		{"well-formed cpu and memory", model.ResourceSpec{CPU: "2", Memory: "4Gi"}, false},
		{"well-formed with maxCpu/maxMemory", model.ResourceSpec{CPU: "2", Memory: "4Gi", MaxCPU: "4", MaxMemory: "8Gi"}, false},
		{"malformed cpu", model.ResourceSpec{CPU: "abc", Memory: "4Gi"}, true},
		{"malformed memory", model.ResourceSpec{CPU: "2", Memory: "4Xi"}, true},
		{"malformed maxCpu", model.ResourceSpec{CPU: "2", Memory: "4Gi", MaxCPU: "not-a-number"}, true},
		{"malformed maxMemory", model.ResourceSpec{CPU: "2", Memory: "4Gi", MaxMemory: "not-a-quantity"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResourceQuantities(tc.res)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestValidateMachineDeniesCreateWithMalformedCPU(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "abc", Memory: "4Gi"}},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected a malformed spec.resources.cpu to be denied")
	}
}

func TestValidateMachineDeniesCreateWithMalformedMemory(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "not-a-quantity"}},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected a malformed spec.resources.memory to be denied")
	}
}

// TestValidateMachineAllowsCreateWithEmptyResourcesAwaitingInstanceType
// confirms validateResourceQuantities' one deliberate exception doesn't
// regress: a Machine naming spec.instanceTypeName instead of setting
// spec.resources directly is expected to leave CPU/Memory unset until
// resolveInstanceTypes (internal/controller/instancetype.go) fills them
// in from the referenced MachineInstanceType -- this must be allowed
// through, not treated as "empty means invalid".
func TestValidateMachineAllowsCreateWithEmptyResourcesAwaitingInstanceType(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec:     model.MachineSpec{InstanceTypeName: "small"},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected an empty spec.resources awaiting instance-type resolution to be allowed, got denied: %s", d.Reason)
	}
}

func TestValidateMachineInstanceTypeAndPreferenceRefs(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	cases := []struct {
		name  string
		spec  model.MachineSpec
		allow bool
	}{
		{"existing type and cluster preference", model.MachineSpec{InstanceTypeName: "small", PreferenceName: "linux", PreferenceKind: model.KindMachineClusterPreference}, true},
		{"missing type", model.MachineSpec{InstanceTypeName: "huge"}, false},
		{"missing cluster type", model.MachineSpec{InstanceTypeName: "small", InstanceTypeKind: model.KindMachineClusterInstanceType}, false},
		{"missing namespaced preference", model.MachineSpec{InstanceTypeName: "small", PreferenceName: "linux"}, false},
		{"bad kind", model.MachineSpec{InstanceTypeName: "small", InstanceTypeKind: "Bogus"}, false},
		{"explicit resources skip the type lookup", model.MachineSpec{InstanceTypeName: "huge", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			incoming := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: tc.spec}
			r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
			if d := ctl.validateMachine(r, req); d.Allowed != tc.allow {
				t.Fatalf("allowed=%v, want %v (%s)", d.Allowed, tc.allow, d.Reason)
			}
		})
	}
}

func TestValidateNetworkMAC(t *testing.T) {
	cases := []struct {
		name    string
		mac     string
		wantErr bool
	}{
		{"unset mac allowed", "", false},
		{"well-formed colon-separated mac", "52:54:00:12:34:56", false},
		{"well-formed hyphen-separated mac", "52-54-00-12-34-56", false},
		{"malformed: not hex", "gg:54:00:12:34:56", true},
		{"malformed: wrong separator", "52.54.00.12.34.56", true},
		{"malformed: too few octets", "52:54:00:12:34", true},
		{"rejected: EUI-64 width", "52:54:00:ff:fe:12:34:56", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNetworkMAC(model.NetworkSpec{MAC: tc.mac})
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestValidateMachineDeniesCreateWithMalformedMAC(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{MAC: "not-a-mac"}},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected a malformed spec.network.mac to be denied")
	}
}

func TestValidateMachineAllowsCreateWithWellFormedMAC(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	incoming := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{MAC: "52:54:00:12:34:56"}},
	}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected a well-formed spec.network.mac to be allowed, got denied: %s", d.Reason)
	}
}

// TestValidateMachineResizeDeniesMalformedMACOnEdit confirms
// validateNetworkMAC is also enforced on UPDATE, not just CREATE:
// spec.network.mac can be introduced or changed by a plain Machine edit
// (there's no dedicated "resize" API distinct from a generic UPDATE --
// validateMachineResize handles every Machine UPDATE), and
// docs/network-fabric.md's field-mapping table lists spec.network as
// valid on "VM create / edit" both.
func TestValidateMachineResizeDeniesMalformedMACOnEdit(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	old := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{NodeName: "worker-1", Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"}}}
	updated := old
	updated.Spec.Network.MAC = "not-a-mac"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, updated, old)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected an edit introducing a malformed spec.network.mac to be denied")
	}
}

func TestValidateMachineResizeDeniesMalformedMaxCPU(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	old := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}, Spec: model.MachineSpec{NodeName: "worker-1", Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"}}}
	updated := old
	updated.Spec.Resources.MaxCPU = "not-a-number"
	r, req := admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, updated, old)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected a resize introducing a malformed spec.resources.maxCpu to be denied")
	}
}

func TestValidateMachineAllowsCreateWithNoQuotasConfigured(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}})
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected allow with no MachineQuota in the namespace, got denied: %s", d.Reason)
	}
}

func TestValidateMachineDeniesCreateOverMaxMachines(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxMachines: intPtr(1)}}
	existing := []model.Machine{{Metadata: model.ObjectMeta{Namespace: "prod", Name: "already-scheduled"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, existing, nil, nil)

	incoming := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "new-vm"}}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	d := ctl.validateMachine(r, req)
	if d.Allowed {
		t.Fatal("expected the second Machine to be denied once maxMachines=1 is already used")
	}
	if d.Reason == "" {
		t.Fatal("expected a non-empty denial reason")
	}
}

func TestValidateMachineAllowsCreateUnderQuota(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxMachines: intPtr(5)}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, nil, nil, nil)
	incoming := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, incoming)
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("expected allow under quota, got denied: %s", d.Reason)
	}
}

func TestValidateMachineMigrationIgnoresNonMigrationResourcesAndUpdates(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, model.MachineMigration{})
	if d := ctl.validateMachineMigration(r, req); !d.Allowed {
		t.Fatalf("expected non-migration resource to be ignored, got denied: %s", d.Reason)
	}
	r, req = admissionReq(t, "machinemigrations", "prod", admission.OperationUpdate, model.MachineMigration{})
	if d := ctl.validateMachineMigration(r, req); !d.Allowed {
		t.Fatalf("expected UPDATE to be ignored, got denied: %s", d.Reason)
	}
}

func TestValidateMachineMigrationAllowsWithNoBudgetsConfigured(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	mig := model.MachineMigration{Metadata: model.ObjectMeta{Namespace: "prod", Name: "m1"}, Spec: model.MachineMigrationSpec{MachineName: "web-1"}}
	r, req := admissionReq(t, "machinemigrations", "prod", admission.OperationCreate, mig)
	if d := ctl.validateMachineMigration(r, req); !d.Allowed {
		t.Fatalf("expected allow with no MachineDisruptionBudget in the namespace, got denied: %s", d.Reason)
	}
}

func TestValidateMachineMigrationAllowsWhenTargetMachineNotFound(t *testing.T) {
	budget := model.MachineDisruptionBudget{Metadata: model.ObjectMeta{Namespace: "prod", Name: "b"}, Spec: model.MachineDisruptionBudgetSpec{Selector: map[string]string{"tier": "web"}, MinAvailable: "100"}}
	ctl := newWebhookTestController(t, "prod", nil, nil, []model.MachineDisruptionBudget{budget}, nil)
	mig := model.MachineMigration{Metadata: model.ObjectMeta{Namespace: "prod", Name: "m1"}, Spec: model.MachineMigrationSpec{MachineName: "no-such-machine"}}
	r, req := admissionReq(t, "machinemigrations", "prod", admission.OperationCreate, mig)
	if d := ctl.validateMachineMigration(r, req); !d.Allowed {
		t.Fatalf("expected allow when the referenced Machine doesn't exist (nothing to check a budget's selector against), got denied: %s", d.Reason)
	}
}

func TestValidateMachineMigrationDeniesWhenBudgetExhausted(t *testing.T) {
	machines := []model.Machine{
		{Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-1", Labels: map[string]string{"tier": "web"}}, Status: model.MachineStatus{Phase: "Running"}},
		{Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-2", Labels: map[string]string{"tier": "web"}}, Status: model.MachineStatus{Phase: "Running"}},
	}
	budget := model.MachineDisruptionBudget{Metadata: model.ObjectMeta{Namespace: "prod", Name: "b"}, Spec: model.MachineDisruptionBudgetSpec{Selector: map[string]string{"tier": "web"}, MinAvailable: "2"}}
	ctl := newWebhookTestController(t, "prod", nil, machines, []model.MachineDisruptionBudget{budget}, nil)
	mig := model.MachineMigration{Metadata: model.ObjectMeta{Namespace: "prod", Name: "m1"}, Spec: model.MachineMigrationSpec{MachineName: "web-1"}}
	r, req := admissionReq(t, "machinemigrations", "prod", admission.OperationCreate, mig)
	d := ctl.validateMachineMigration(r, req)
	if d.Allowed {
		t.Fatal("expected deny: minAvailable=2 with only 2 healthy web machines allows zero disruptions")
	}
}

func TestValidateMachineMigrationAllowsWithinBudget(t *testing.T) {
	machines := []model.Machine{
		{Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-1", Labels: map[string]string{"tier": "web"}}, Status: model.MachineStatus{Phase: "Running"}},
		{Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-2", Labels: map[string]string{"tier": "web"}}, Status: model.MachineStatus{Phase: "Running"}},
		{Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-3", Labels: map[string]string{"tier": "web"}}, Status: model.MachineStatus{Phase: "Running"}},
	}
	budget := model.MachineDisruptionBudget{Metadata: model.ObjectMeta{Namespace: "prod", Name: "b"}, Spec: model.MachineDisruptionBudgetSpec{Selector: map[string]string{"tier": "web"}, MinAvailable: "2"}}
	ctl := newWebhookTestController(t, "prod", nil, machines, []model.MachineDisruptionBudget{budget}, nil)
	mig := model.MachineMigration{Metadata: model.ObjectMeta{Namespace: "prod", Name: "m1"}, Spec: model.MachineMigrationSpec{MachineName: "web-1"}}
	r, req := admissionReq(t, "machinemigrations", "prod", admission.OperationCreate, mig)
	if d := ctl.validateMachineMigration(r, req); !d.Allowed {
		t.Fatalf("expected allow: minAvailable=2 with 3 healthy web machines allows 1 disruption, got denied: %s", d.Reason)
	}
}

// TestValidateMachineNetworkPolicyIgnoresNonMatchingResourcesAndOperations
// confirms validateMachineNetworkPolicy is scoped exactly like
// validateMachine/validateMachineMigration -- a no-op Kubernetes lookup
// away, since unlike those two this validator never touches c.Kube at
// all (there's no reconcile-loop decision function to reuse here, see
// WebhookHandler's own doc comment), so the *Controller in these tests
// never needs a fake API server.
func TestValidateMachineNetworkPolicyIgnoresNonMatchingResourcesAndOperations(t *testing.T) {
	ctl := &Controller{}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, model.MachineNetworkPolicy{})
	if d := ctl.validateMachineNetworkPolicy(r, req); !d.Allowed {
		t.Fatalf("expected non-matching resource to be ignored, got denied: %s", d.Reason)
	}
	r, req = admissionReq(t, "machinenetworkpolicies", "prod", "DELETE", model.MachineNetworkPolicy{})
	if d := ctl.validateMachineNetworkPolicy(r, req); !d.Allowed {
		t.Fatalf("expected DELETE to be ignored, got denied: %s", d.Reason)
	}
}

func TestValidateMachineNetworkPolicyAllowsWellFormedPolicy(t *testing.T) {
	ctl := &Controller{}
	p := model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-edge"},
		Spec: model.MachineNetworkPolicySpec{
			Selector: map[string]string{"app": "web"},
			Policy:   model.VmNetworkPolicy{AllowCidrs: []string{"10.0.0.0/8"}, AllowPorts: []string{"tcp/443"}},
		},
	}
	for _, op := range []string{admission.OperationCreate, admission.OperationUpdate} {
		r, req := admissionReq(t, "machinenetworkpolicies", "prod", op, p)
		if d := ctl.validateMachineNetworkPolicy(r, req); !d.Allowed {
			t.Fatalf("%s: expected a well-formed policy to be allowed, got denied: %s", op, d.Reason)
		}
	}
}

func TestValidateMachineNetworkPolicyDeniesMalformedCIDR(t *testing.T) {
	ctl := &Controller{}
	p := model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-edge"},
		Spec:     model.MachineNetworkPolicySpec{Policy: model.VmNetworkPolicy{AllowCidrs: []string{"10.0.0.0"}}},
	}
	r, req := admissionReq(t, "machinenetworkpolicies", "prod", admission.OperationCreate, p)
	d := ctl.validateMachineNetworkPolicy(r, req)
	if d.Allowed {
		t.Fatal("expected a CIDR missing /prefix to be denied")
	}
	if d.Reason == "" {
		t.Fatal("expected a non-empty denial reason")
	}
}

func TestValidateMachineNetworkPolicyDeniesMalformedPortRuleOnUpdate(t *testing.T) {
	ctl := &Controller{}
	p := model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "web-edge"},
		Spec:     model.MachineNetworkPolicySpec{Policy: model.VmNetworkPolicy{AllowPorts: []string{"http/443"}}},
	}
	r, req := admissionReq(t, "machinenetworkpolicies", "prod", admission.OperationUpdate, p)
	if d := ctl.validateMachineNetworkPolicy(r, req); d.Allowed {
		t.Fatal("expected `kubectl edit` introducing an unsupported protocol to be denied on UPDATE, not just CREATE")
	}
}

func TestValidateNetworkSecurityGroupIgnoresNonMatchingResourcesAndOperations(t *testing.T) {
	ctl := &Controller{}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, model.NetworkSecurityGroup{})
	if d := ctl.validateNetworkSecurityGroup(r, req); !d.Allowed {
		t.Fatalf("expected non-matching resource to be ignored, got denied: %s", d.Reason)
	}
	r, req = admissionReq(t, "networksecuritygroups", "prod", "DELETE", model.NetworkSecurityGroup{})
	if d := ctl.validateNetworkSecurityGroup(r, req); !d.Allowed {
		t.Fatalf("expected DELETE to be ignored, got denied: %s", d.Reason)
	}
}

func TestValidateNetworkSecurityGroupAllowsWellFormedPolicy(t *testing.T) {
	ctl := &Controller{}
	g := model.NetworkSecurityGroup{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "frontend"},
		Spec:     model.NetworkSecurityGroupSpec{Policy: model.VmNetworkPolicy{AllowCidrs: []string{"10.0.0.0/8"}, AllowPorts: []string{"tcp/443", "udp/53"}}},
	}
	r, req := admissionReq(t, "networksecuritygroups", "prod", admission.OperationCreate, g)
	if d := ctl.validateNetworkSecurityGroup(r, req); !d.Allowed {
		t.Fatalf("expected a well-formed group policy to be allowed, got denied: %s", d.Reason)
	}
}

func TestValidateNetworkSecurityGroupDeniesZeroMaxEgressMbps(t *testing.T) {
	ctl := &Controller{}
	zero := uint32(0)
	g := model.NetworkSecurityGroup{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "frontend"},
		Spec:     model.NetworkSecurityGroupSpec{Policy: model.VmNetworkPolicy{MaxEgressMbps: &zero}},
	}
	r, req := admissionReq(t, "networksecuritygroups", "prod", admission.OperationCreate, g)
	if d := ctl.validateNetworkSecurityGroup(r, req); d.Allowed {
		t.Fatal("expected an explicit maxEgressMbps: 0 to be denied")
	}
}

func TestValidateMachineQuotaObjectIgnoresNonMatchingResourcesAndOperations(t *testing.T) {
	ctl := &Controller{}
	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, model.MachineQuota{})
	if d := ctl.validateMachineQuotaObject(r, req); !d.Allowed {
		t.Fatalf("expected non-matching resource to be ignored, got denied: %s", d.Reason)
	}
	r, req = admissionReq(t, "machinequotas", "prod", "DELETE", model.MachineQuota{})
	if d := ctl.validateMachineQuotaObject(r, req); !d.Allowed {
		t.Fatalf("expected DELETE to be ignored, got denied: %s", d.Reason)
	}
}

func TestValidateMachineQuotaObjectAllowsWellFormedQuota(t *testing.T) {
	ctl := &Controller{}
	q := model.MachineQuota{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(10), MaxTotalCPU: "16", MaxTotalMemory: "64Gi"},
	}
	for _, op := range []string{admission.OperationCreate, admission.OperationUpdate} {
		r, req := admissionReq(t, "machinequotas", "prod", op, q)
		if d := ctl.validateMachineQuotaObject(r, req); !d.Allowed {
			t.Fatalf("%s: expected a well-formed MachineQuota to be allowed, got denied: %s", op, d.Reason)
		}
	}
}

func TestValidateMachineQuotaObjectAllowsUnsetLimits(t *testing.T) {
	ctl := &Controller{}
	q := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxMachines: intPtr(10)}}
	r, req := admissionReq(t, "machinequotas", "prod", admission.OperationCreate, q)
	if d := ctl.validateMachineQuotaObject(r, req); !d.Allowed {
		t.Fatalf("expected unset maxTotalCpu/maxTotalMemory (unlimited) to be allowed, got denied: %s", d.Reason)
	}
}

func TestValidateMachineQuotaObjectDeniesMalformedMaxTotalCPU(t *testing.T) {
	ctl := &Controller{}
	q := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "not-a-number"}}
	r, req := admissionReq(t, "machinequotas", "prod", admission.OperationCreate, q)
	if d := ctl.validateMachineQuotaObject(r, req); d.Allowed {
		t.Fatal("expected a malformed maxTotalCpu to be denied")
	}
}

func TestValidateMachineQuotaObjectDeniesMalformedMaxTotalMemoryOnUpdate(t *testing.T) {
	ctl := &Controller{}
	q := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalMemory: "not-a-quantity"}}
	r, req := admissionReq(t, "machinequotas", "prod", admission.OperationUpdate, q)
	if d := ctl.validateMachineQuotaObject(r, req); d.Allowed {
		t.Fatal("expected a malformed maxTotalMemory to be denied on UPDATE too")
	}
}

// TestWebhookHandlerEndToEnd exercises the real AdmissionReview HTTP
// envelope (internal/admission), not just the Validator functions directly
// -- confirms encode/decode round-trips through WebhookHandler correctly.
func TestWebhookHandlerEndToEnd(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxMachines: intPtr(0)}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, nil, nil, nil)
	h := ctl.WebhookHandler()

	incoming := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}}
	raw, _ := json.Marshal(incoming)
	review := admission.Review{
		APIVersion: admission.APIVersion,
		Kind:       "AdmissionReview",
		Request: &admission.Request{
			UID:       "abc-123",
			Resource:  admission.GroupVersionResource{Group: "kairon.zyvor.dev", Version: "v1alpha1", Resource: "machines"},
			Namespace: "prod",
			Operation: admission.OperationCreate,
			Object:    raw,
		},
	}
	body, _ := json.Marshal(review)
	httpReq := httptest.NewRequest(http.MethodPost, "/validate-machine", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (AdmissionReview responses are always 200, the decision is in the body), got %d", rr.Code)
	}
	var out admission.Review
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Response == nil {
		t.Fatal("expected a response")
	}
	if out.Response.UID != "abc-123" {
		t.Fatalf("expected the response UID to echo the request UID, got %q", out.Response.UID)
	}
	if out.Response.Allowed {
		t.Fatal("expected deny (maxMachines=0)")
	}
	if out.Response.Status == nil || out.Response.Status.Message == "" {
		t.Fatal("expected a denial message")
	}
}

// TestWebhookHandlerRecordsDecisionMetrics confirms WebhookHandler's
// observeWebhookDecision wiring actually reaches
// kairon_webhook_decisions_total -- exercised through the real HTTP
// envelope (like TestWebhookHandlerEndToEnd above) so this also covers
// admission.Handler's observe callback, not just the Controller-side
// method in isolation.
func TestWebhookHandlerRecordsDecisionMetrics(t *testing.T) {
	quota := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxMachines: intPtr(0)}}
	ctl := newWebhookTestController(t, "prod", []model.MachineQuota{quota}, nil, nil, nil)
	rec := metrics.NewRecorder()
	ctl.Metrics = rec
	h := ctl.WebhookHandler()

	post := func(t *testing.T, path string, req admission.Request) {
		t.Helper()
		body, _ := json.Marshal(admission.Review{APIVersion: admission.APIVersion, Kind: "AdmissionReview", Request: &req})
		httpReq := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httpReq)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s: got %d", path, rr.Code)
		}
	}

	deniedRaw, _ := json.Marshal(model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-1"}})
	post(t, "/validate-machine", admission.Request{
		Resource:  admission.GroupVersionResource{Group: "kairon.zyvor.dev", Version: "v1alpha1", Resource: "machines"},
		Namespace: "prod",
		Operation: admission.OperationCreate,
		Object:    deniedRaw,
	})
	// Not "machines"/"machinemigrations" -> always allowed (WebhookHandler
	// only wires those two resources, but the underlying validators
	// themselves also allow anything else -- see validateMachine).
	allowedRaw, _ := json.Marshal(model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "vm-2"}})
	post(t, "/validate-machinemigration", admission.Request{
		Resource:  admission.GroupVersionResource{Group: "kairon.zyvor.dev", Version: "v1alpha1", Resource: "machinemigrations"},
		Namespace: "prod",
		Operation: "DELETE",
		Object:    allowedRaw,
	})

	rr := httptest.NewRecorder()
	rec.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `kairon_webhook_decisions_total{decision="deny",operation="CREATE",resource="machines"} 1`) {
		t.Errorf("missing expected deny counter in:\n%s", body)
	}
	if !strings.Contains(body, `kairon_webhook_decisions_total{decision="allow",operation="DELETE",resource="machinemigrations"} 1`) {
		t.Errorf("missing expected allow counter in:\n%s", body)
	}
}

// TestWebhookHandlerConvertMachineQuotaEndToEnd exercises the real
// ConversionReview HTTP envelope (internal/conversion) through
// WebhookHandler's /convert/machinequotas route -- confirms the route is
// actually wired to conversion.ConvertMachineQuota, not just that the
// converter works in isolation (internal/conversion's own tests already
// cover that). No live CRD calls this route (see
// docs/guides/crd-versioning.md); this is what would call it if one did.
func TestWebhookHandlerConvertMachineQuotaEndToEnd(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	h := ctl.WebhookHandler()

	obj, _ := json.Marshal(map[string]any{
		"apiVersion": "kairon.zyvor.dev/v1alpha1",
		"kind":       "MachineQuota",
		"metadata":   map[string]any{"name": "q1", "namespace": "prod"},
		"spec":       map[string]any{"maxTotalCpu": "16"},
	})
	review := conversion.Review{
		APIVersion: conversion.APIVersion,
		Kind:       "ConversionReview",
		Request: &conversion.Request{
			UID:               "conv-1",
			DesiredAPIVersion: "kairon.zyvor.dev/v1",
			Objects:           []json.RawMessage{obj},
		},
	}
	body, _ := json.Marshal(review)
	httpReq := httptest.NewRequest(http.MethodPost, "/convert/machinequotas", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (ConversionReview responses are always 200, the result is in the body), got %d", rr.Code)
	}
	var out conversion.Review
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Response == nil || out.Response.UID != "conv-1" {
		t.Fatalf("expected a response echoing UID conv-1, got %+v", out.Response)
	}
	if out.Response.Result.Status != conversion.StatusSuccess {
		t.Fatalf("expected Success, got %+v", out.Response.Result)
	}
	if len(out.Response.ConvertedObjects) != 1 {
		t.Fatalf("expected 1 converted object, got %d", len(out.Response.ConvertedObjects))
	}
	var converted map[string]any
	if err := json.Unmarshal(out.Response.ConvertedObjects[0], &converted); err != nil {
		t.Fatalf("decode converted object: %v", err)
	}
	if converted["apiVersion"] != "kairon.zyvor.dev/v1" {
		t.Fatalf("expected the converted object's apiVersion to be v1, got %v", converted["apiVersion"])
	}
	spec := converted["spec"].(map[string]any)
	if spec["maxCpu"] != "16" {
		t.Fatalf("expected spec.maxTotalCpu to be renamed to spec.maxCpu, got %+v", spec)
	}
}

// TestWebhookHandlerNetworkPolicyRoutesEndToEnd exercises
// /validate-machinenetworkpolicy and /validate-networksecuritygroup
// through the real AdmissionReview HTTP envelope -- confirms
// WebhookHandler actually wires both routes to
// validateMachineNetworkPolicy/validateNetworkSecurityGroup, not just
// that those two functions work in isolation (already covered above).
func TestWebhookHandlerNetworkPolicyRoutesEndToEnd(t *testing.T) {
	ctl := &Controller{}
	h := ctl.WebhookHandler()

	post := func(t *testing.T, path, resource string, obj any) *admission.Review {
		t.Helper()
		raw, _ := json.Marshal(obj)
		review := admission.Review{
			APIVersion: admission.APIVersion,
			Kind:       "AdmissionReview",
			Request: &admission.Request{
				UID:       "net-1",
				Resource:  admission.GroupVersionResource{Group: "kairon.zyvor.dev", Version: "v1alpha1", Resource: resource},
				Namespace: "prod",
				Operation: admission.OperationCreate,
				Object:    raw,
			},
		}
		body, _ := json.Marshal(review)
		httpReq := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httpReq)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s: got %d", path, rr.Code)
		}
		var out admission.Review
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return &out
	}

	deniedPolicy := model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "bad"},
		Spec:     model.MachineNetworkPolicySpec{Policy: model.VmNetworkPolicy{AllowCidrs: []string{"10.0.0.0"}}},
	}
	if out := post(t, "/validate-machinenetworkpolicy", "machinenetworkpolicies", deniedPolicy); out.Response.Allowed {
		t.Fatal("expected /validate-machinenetworkpolicy to deny a CIDR missing /prefix")
	}

	okGroup := model.NetworkSecurityGroup{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "frontend"},
		Spec:     model.NetworkSecurityGroupSpec{Policy: model.VmNetworkPolicy{AllowCidrs: []string{"10.0.0.0/8"}}},
	}
	if out := post(t, "/validate-networksecuritygroup", "networksecuritygroups", okGroup); !out.Response.Allowed {
		t.Fatalf("expected /validate-networksecuritygroup to allow a well-formed group, got denied: %+v", out.Response.Status)
	}
}

// TestWebhookHandlerMachineQuotaRouteEndToEnd exercises
// /validate-machinequota through the real AdmissionReview HTTP envelope --
// confirms WebhookHandler actually wires this route to
// validateMachineQuotaObject (already covered in isolation above), and
// that it's reachable independently of the pre-existing /validate-machine
// route also named after MachineQuota enforcement.
func TestWebhookHandlerMachineQuotaRouteEndToEnd(t *testing.T) {
	ctl := &Controller{}
	h := ctl.WebhookHandler()

	post := func(t *testing.T, obj any) *admission.Review {
		t.Helper()
		raw, _ := json.Marshal(obj)
		review := admission.Review{
			APIVersion: admission.APIVersion,
			Kind:       "AdmissionReview",
			Request: &admission.Request{
				UID:       "quota-1",
				Resource:  admission.GroupVersionResource{Group: "kairon.zyvor.dev", Version: "v1alpha1", Resource: "machinequotas"},
				Namespace: "prod",
				Operation: admission.OperationCreate,
				Object:    raw,
			},
		}
		body, _ := json.Marshal(review)
		httpReq := httptest.NewRequest(http.MethodPost, "/validate-machinequota", bytes.NewReader(body))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httpReq)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST /validate-machinequota: got %d", rr.Code)
		}
		var out admission.Review
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return &out
	}

	bad := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "not-a-number"}}
	if out := post(t, bad); out.Response.Allowed {
		t.Fatal("expected /validate-machinequota to deny a malformed maxTotalCpu")
	}

	good := model.MachineQuota{Metadata: model.ObjectMeta{Namespace: "prod", Name: "q"}, Spec: model.MachineQuotaSpec{MaxTotalCPU: "16"}}
	if out := post(t, good); !out.Response.Allowed {
		t.Fatalf("expected /validate-machinequota to allow a well-formed maxTotalCpu, got denied: %+v", out.Response.Status)
	}
}
