// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/csinode"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeDeviceFlux struct {
	mu    sync.Mutex
	disks map[string]string
	nics  map[string]string
	calls []string
}

func (f *fakeDeviceFlux) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1/disks":
		items := make([]map[string]string, 0, 1+len(f.disks))
		items = append(items, map[string]string{"name": "root", "path": "/w/root.qcow2"})
		for n, p := range f.disks {
			items = append(items, map[string]string{"name": n, "path": p})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/disks":
		f.disks[body["name"]] = body["path"]
		f.calls = append(f.calls, "attach "+body["name"]+" "+body["path"])
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/vms/vm-1/disks/"):
		name := strings.TrimPrefix(r.URL.Path, "/v1/vms/vm-1/disks/")
		delete(f.disks, name)
		f.calls = append(f.calls, "detach "+name)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/nic":
		f.nics[body["mac"]] = body["bridge"]
		f.calls = append(f.calls, "nic+ "+body["bridge"]+" "+body["mac"])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/nic/unplug":
		delete(f.nics, body["mac"])
		f.calls = append(f.calls, "nic- "+body["mac"])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeDeviceFlux) record() *fluxvm.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := &fluxvm.Record{IDValue: "vm-1", Status: "running"}
	for mac, br := range f.nics {
		rec.Request.Network.Extra = append(rec.Request.Network.Extra, struct {
			Bridge  string `json:"bridge,omitempty"`
			MAC     string `json:"mac,omitempty"`
			TapName string `json:"tap_name,omitempty"`
		}{Bridge: br, MAC: mac, TapName: "hn" + mac[len(mac)-2:]})
	}
	return rec
}

func newDeviceAgent(t *testing.T, f *fakeDeviceFlux, pvs map[string]model.PersistentVolume) *Agent {
	t.Helper()
	fs := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(fs.Close)
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const pvcPrefix = "/api/v1/namespaces/prod/persistentvolumeclaims/"
		const pvPrefix = "/api/v1/persistentvolumes/"
		switch {
		case strings.HasPrefix(r.URL.Path, pvcPrefix):
			claim := strings.TrimPrefix(r.URL.Path, pvcPrefix)
			if _, ok := pvs["pv-"+claim]; !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(model.PersistentVolumeClaim{
				Spec:   model.PersistentVolumeClaimSpec{VolumeName: "pv-" + claim},
				Status: model.PersistentVolumeClaimStatus{Phase: "Bound"},
			})
		case strings.HasPrefix(r.URL.Path, pvPrefix):
			_ = json.NewEncoder(w).Encode(pvs[strings.TrimPrefix(r.URL.Path, pvPrefix)])
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	return &Agent{Kube: kc, Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestReconcileDisksAttachesAndDetachesOnlyOwnedDisks(t *testing.T) {
	f := &fakeDeviceFlux{disks: map[string]string{"manual": "/x"}, nics: map[string]string{}}
	a := newDeviceAgent(t, f, map[string]model.PersistentVolume{
		"pv-blk": {Spec: model.PersistentVolumeSpec{VolumeMode: "Block", HostPath: &model.HostPathVolumeSource{Path: "/dev/vg0/data"}}},
		"pv-fs":  {Spec: model.PersistentVolumeSpec{Local: &model.LocalVolumeSource{Path: "/srv/pv/fs"}}},
	})
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", UID: "uid-1"},
		Spec: model.MachineSpec{Disks: []model.MachineDisk{
			{Name: "data", ClaimName: "blk"},
			{Name: "logs", ClaimName: "fs"},
		}},
	}
	disks, _, _, err := a.reconcileDevices(context.Background(), m, f.record())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(disks, []string{"data", "logs"}) {
		t.Fatalf("attached = %v", disks)
	}
	if f.disks["data"] != "/dev/vg0/data" || f.disks["logs"] != "/srv/pv/fs/disk.img" {
		t.Fatalf("paths = %v", f.disks)
	}

	// Steady state: nothing to do.
	m.Status.AttachedDisks = disks
	f.calls = nil
	if _, _, _, err := a.reconcileDevices(context.Background(), m, f.record()); err != nil || len(f.calls) != 0 {
		t.Fatalf("steady state made calls %v (err %v)", f.calls, err)
	}

	// Dropping "logs" detaches it; "manual" was never ours and stays.
	m.Spec.Disks = m.Spec.Disks[:1]
	disks, _, _, err = a.reconcileDevices(context.Background(), m, f.record())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(disks, []string{"data"}) || !slices.Equal(f.calls, []string{"detach logs"}) {
		t.Fatalf("attached = %v, calls = %v", disks, f.calls)
	}
	if _, ok := f.disks["manual"]; !ok {
		t.Fatal("a disk Kairon did not attach was detached")
	}
}

func TestReconcileDisksReportsUnresolvableClaimsWithoutBlockingOthers(t *testing.T) {
	f := &fakeDeviceFlux{disks: map[string]string{}, nics: map[string]string{}}
	a := newDeviceAgent(t, f, map[string]model.PersistentVolume{
		"pv-ok":  {Spec: model.PersistentVolumeSpec{HostPath: &model.HostPathVolumeSource{Path: "/srv/ok"}}},
		"pv-csi": {Metadata: model.ObjectMeta{Name: "pv-csi"}, Spec: model.PersistentVolumeSpec{CSI: &model.CSIPersistentVolumeSource{Driver: "x"}}},
	})
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{Disks: []model.MachineDisk{
			{Name: "csi", ClaimName: "csi"},
			{Name: "ok", ClaimName: "ok"},
		}},
	}
	disks, _, _, err := a.reconcileDevices(context.Background(), m, f.record())
	if err == nil || !strings.Contains(err.Error(), "staging/publish directory") {
		t.Fatalf("want a CSI error, got %v", err)
	}
	if !slices.Equal(disks, []string{"ok"}) {
		t.Fatalf("attached = %v", disks)
	}
}

func TestReconcileDisksPublishesAndUnpublishesCSIVolumes(t *testing.T) {
	f := &fakeDeviceFlux{disks: map[string]string{}, nics: map[string]string{}}
	a := newDeviceAgent(t, f, map[string]model.PersistentVolume{"pv-vol": testCSIPV("pv-vol", "h1")})
	node := &fakeCSINodeServer{}
	a.CSISocketPath = startFakeCSINode(t, node)
	a.CSIStagingDir, a.CSIPublishDir, a.NodeName = t.TempDir(), t.TempDir(), "n1"
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:     model.MachineSpec{Disks: []model.MachineDisk{{Name: "vol", ClaimName: "vol"}}},
	}
	disks, vols, _, err := a.reconcileDevices(context.Background(), m, f.record())
	if err != nil {
		t.Fatal(err)
	}
	publish := filepath.Join(a.CSIPublishDir, "disks", csinode.DriverName, m.RuntimeName(), "vol")
	if !slices.Equal(disks, []string{"vol"}) || len(vols) != 1 || vols[0].Node != "n1" || vols[0].VolumeHandle != "h1" ||
		f.disks["vol"] != filepath.Join(publish, bootDiskFileName) || len(node.stageCalls) != 1 || len(node.publishCalls) != 1 {
		t.Fatalf("disks=%v vols=%+v flux=%v stage=%d publish=%d", disks, vols, f.disks, len(node.stageCalls), len(node.publishCalls))
	}

	m.Status.AttachedDisks, m.Status.DiskVolumes = disks, vols
	f.calls = nil
	if _, again, _, err := a.reconcileDevices(context.Background(), m, f.record()); err != nil || len(f.calls) != 0 || len(node.stageCalls) != 1 || len(again) != 1 {
		t.Fatalf("steady state: calls=%v stage=%d vols=%v err=%v", f.calls, len(node.stageCalls), again, err)
	}

	foreign := model.DiskVolume{Name: "old", Node: "n2", Driver: csinode.DriverName, VolumeHandle: "h0"}
	m.Status.DiskVolumes = append(m.Status.DiskVolumes, foreign)
	m.Spec.Disks = nil
	disks, vols, _, err = a.reconcileDevices(context.Background(), m, f.record())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 0 || !slices.Equal(f.calls, []string{"detach vol"}) || len(node.unpublish) != 1 || len(node.unstage) != 1 ||
		len(vols) != 1 || vols[0] != foreign {
		t.Fatalf("disks=%v calls=%v unpublish=%d unstage=%d vols=%+v", disks, f.calls, len(node.unpublish), len(node.unstage), vols)
	}
	m.Status.DiskVolumes = vols
	if err := a.teardownDiskVolumes(context.Background(), m); err != nil || len(node.unpublish) != 1 {
		t.Fatalf("another node's volume must be left to that node: err=%v unpublish=%d", err, len(node.unpublish))
	}
}

func TestReconcileInterfacesHotplugsAndUnplugsByMAC(t *testing.T) {
	f := &fakeDeviceFlux{disks: map[string]string{}, nics: map[string]string{}}
	a := newDeviceAgent(t, f, nil)
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "fw", Namespace: "prod", UID: "uid-7"},
		Spec: model.MachineSpec{Network: model.NetworkSpec{Mode: "tap", Bridge: "br0", ExtraInterfaces: []model.ExtraInterface{
			{Name: "lan", Bridge: "br-lan"},
			{Name: "dmz", Bridge: "br-dmz", MAC: "02:AA:BB:CC:DD:EE"},
		}}},
	}
	_, _, ifaces, err := a.reconcileDevices(context.Background(), m, f.record())
	if err != nil {
		t.Fatal(err)
	}
	lanMAC := model.ExtraInterfaceMAC("uid-7", model.ExtraInterface{Name: "lan"})
	if len(ifaces) != 2 || ifaces[0].MAC != lanMAC || ifaces[1].MAC != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("ifaces = %+v", ifaces)
	}
	if f.nics[lanMAC] != "br-lan" || f.nics["02:aa:bb:cc:dd:ee"] != "br-dmz" {
		t.Fatalf("nics = %v", f.nics)
	}

	m.Status.AttachedInterfaces = ifaces
	m.Spec.Network.ExtraInterfaces = m.Spec.Network.ExtraInterfaces[1:]
	f.calls = nil
	_, _, ifaces, err = a.reconcileDevices(context.Background(), m, f.record())
	if err != nil {
		t.Fatal(err)
	}
	if len(ifaces) != 1 || ifaces[0].Name != "dmz" || !slices.Equal(f.calls, []string{"nic- " + lanMAC}) {
		t.Fatalf("ifaces = %+v calls = %v", ifaces, f.calls)
	}
}

func TestReconcileDevicesWaitsForARunningVM(t *testing.T) {
	a := &Agent{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := model.Machine{
		Spec:   model.MachineSpec{Disks: []model.MachineDisk{{Name: "d", ClaimName: "c"}}},
		Status: model.MachineStatus{AttachedDisks: []string{"d"}},
	}
	disks, _, _, err := a.reconcileDevices(context.Background(), m, &fluxvm.Record{Status: "paused"})
	if err != nil || !slices.Equal(disks, []string{"d"}) {
		t.Fatalf("disks = %v err = %v", disks, err)
	}
}

func TestExtraInterfaceValidationAndMAC(t *testing.T) {
	mac := model.ExtraInterfaceMAC("uid", model.ExtraInterface{Name: "a"})
	if mac != model.ExtraInterfaceMAC("uid", model.ExtraInterface{Name: "a"}) || !strings.HasPrefix(mac, "02:") {
		t.Fatalf("mac %q is not stable/locally administered", mac)
	}
	if mac == model.ExtraInterfaceMAC("uid", model.ExtraInterface{Name: "b"}) {
		t.Fatal("different interfaces got the same MAC")
	}
	bad := []model.NetworkSpec{
		{Mode: "user", ExtraInterfaces: []model.ExtraInterface{{Name: "a", Bridge: "b"}}},
		{Mode: "tap", ExtraInterfaces: []model.ExtraInterface{{Name: "a", Bridge: "b"}, {Name: "a", Bridge: "c"}}},
		{Mode: "tap", ExtraInterfaces: []model.ExtraInterface{{Name: "a"}}},
		{Mode: "tap", ExtraInterfaces: []model.ExtraInterface{{Name: "a", Bridge: "b", MAC: "zz"}}},
		{Mode: "tap", ExtraInterfaces: make([]model.ExtraInterface, 4)},
	}
	for i, ns := range bad {
		if model.ValidateExtraInterfaces(ns) == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := model.ValidateDisks([]model.MachineDisk{{Name: "root", ClaimName: "c"}}); err == nil {
		t.Error("disk named root accepted")
	}
	if err := model.ValidateDisks([]model.MachineDisk{{Name: "a", ClaimName: "c"}, {Name: "a", ClaimName: "d"}}); err == nil {
		t.Error("duplicate disk accepted")
	}
	if err := model.ValidateDisks([]model.MachineDisk{{Name: "import-disk1", ClaimName: "c"}}); err == nil {
		t.Error("disk using the reserved import-disk prefix accepted")
	}
}

type fakeEgressFlux struct {
	mu      sync.Mutex
	calls   []string
	policy  string // JSON of the VM policy, "" = none (node default)
	putBody string
}

func (f *fakeEgressFlux) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, r.Method)
	switch r.Method {
	case http.MethodGet:
		if f.policy == "" {
			_, _ = w.Write([]byte(`{"policy":{"mode":"loopback","rules":[]},"source":"builtin"}`))
			return
		}
		_, _ = w.Write([]byte(`{"policy":` + f.policy + `,"source":"vm"}`))
	case http.MethodPut:
		f.putBody, f.policy = string(b), string(b)
		_, _ = w.Write([]byte(`{}`))
	case http.MethodDelete:
		f.policy = ""
		_, _ = w.Write([]byte(`{}`))
	}
}

func TestReconcileEgressIsIdempotentAndOnlyClearsWhatItOwns(t *testing.T) {
	f := &fakeEgressFlux{}
	fs := httptest.NewServer(http.HandlerFunc(f.handler))
	defer fs.Close()
	var patches []string
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		patches = append(patches, string(b))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	a := &Agent{Kube: kc, Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := &fluxvm.Record{IDValue: "vm-1", Status: "running"}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", UID: "uid-1"},
		Spec: model.MachineSpec{Network: model.NetworkSpec{Egress: &model.EgressSpec{
			Rules: []model.EgressRule{{CIDR: "10.0.0.0/8", Proto: "tcp", Port: 5151}},
		}}},
	}

	// 1. FluxVM has no VM policy: write it, and mark the Machine as owning it.
	if err := a.reconcileEgress(context.Background(), m, rec); err != nil {
		t.Fatal(err)
	}
	if f.putBody != `{"mode":"allowlist","rules":[{"cidr":"10.0.0.0/8","proto":"tcp","port":5151}]}` {
		t.Fatalf("put body %s", f.putBody)
	}
	if len(patches) != 1 || !strings.Contains(patches[0], model.AnnotationEgressManaged) {
		t.Fatalf("expected one annotation patch, got %v", patches)
	}

	// 2. Same policy again: no write (the reconcile loop runs every tick).
	m.Metadata.Annotations = map[string]string{model.AnnotationEgressManaged: "true"}
	f.calls = nil
	if err := a.reconcileEgress(context.Background(), m, rec); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != http.MethodGet {
		t.Fatalf("expected a single read, got %v", f.calls)
	}

	// 3. Field removed from a Machine that owns the policy: cleared.
	m.Spec.Network.Egress = nil
	f.calls = nil
	if err := a.reconcileEgress(context.Background(), m, rec); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != http.MethodDelete || f.policy != "" {
		t.Fatalf("expected a delete, got %v policy=%q", f.calls, f.policy)
	}

	// 4. A Machine that never set one leaves an operator's FluxVM policy alone.
	f.policy = `{"mode":"open","rules":[]}`
	m.Metadata.Annotations = nil
	f.calls = nil
	if err := a.reconcileEgress(context.Background(), m, rec); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || f.policy == "" {
		t.Fatalf("touched a policy it does not own: calls=%v policy=%q", f.calls, f.policy)
	}

	// 5. A stopped VM is skipped.
	m.Spec.Network.Egress = &model.EgressSpec{Mode: "open"}
	if err := a.reconcileEgress(context.Background(), m, &fluxvm.Record{IDValue: "vm-1", Status: "stopped"}); err != nil || len(f.calls) != 0 {
		t.Fatalf("stopped VM: err=%v calls=%v", err, f.calls)
	}
}
