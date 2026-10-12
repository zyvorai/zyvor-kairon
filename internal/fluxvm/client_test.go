// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestBuildCreateRequestSharesMemoryForVhostUserBlk(t *testing.T) {
	m := model.Machine{Spec: model.MachineSpec{
		Image:     model.ImageSpec{Path: "/images/base.img"},
		Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"},
		Disks:     []model.MachineDisk{{Name: "a", IO: &model.DiskIO{Bus: "virtio-blk"}}},
	}}
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil || req.SharedMemory {
		t.Fatalf("virtio-blk only: shared_memory = %v, err = %v", req.SharedMemory, err)
	}
	m.Spec.Disks = append(m.Spec.Disks, model.MachineDisk{Name: "b", IO: &model.DiskIO{Bus: "vhost-user-blk"}})
	req, err = buildCreateRequest(m, "qemu", nil, nil)
	if err != nil || !req.SharedMemory {
		t.Fatalf("with vhost-user-blk: shared_memory = %v, err = %v", req.SharedMemory, err)
	}
}

func TestBuildCreateRequestInPlaceRBD(t *testing.T) {
	m := model.Machine{Spec: model.MachineSpec{
		Image:     model.ImageSpec{Path: "rbd/img", Storage: StorageCephRBDInPlace},
		Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"},
	}}
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil || req.Storage != StorageCephRBDInPlace || req.Image != "rbd/img" {
		t.Fatalf("req=%+v err=%v", req, err)
	}
	if _, err := buildCreateRequest(m, "cloud-hypervisor", nil, nil); err == nil || !strings.Contains(err.Error(), "qemu") {
		t.Fatalf("non-qemu backend: err=%v", err)
	}
	m.Spec.GuestAgent.Console = true
	if _, err := buildCreateRequest(m, "qemu", nil, nil); err == nil || !strings.Contains(err.Error(), "console") {
		t.Fatalf("console agent: err=%v", err)
	}
	m.Spec.Image.Storage = ""
	m.Spec.GuestAgent.Console = false
	if req, _ := buildCreateRequest(m, "qemu", nil, nil); req.Storage != "" {
		t.Fatalf("storage leaked: %+v", req)
	}
}

func TestBuildCreateRequestVirtualNetwork(t *testing.T) {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "img"},
			Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"},
			Network:   model.NetworkSpec{VirtualNetwork: &model.MachineVirtualNetwork{Name: "app"}},
		},
	}
	if _, err := buildCreateRequest(m, "qemu", nil, nil); err == nil || !strings.Contains(err.Error(), "no address allocated") {
		t.Fatalf("unallocated: err=%v", err)
	}
	m.Status.Network = &model.MachineNetworkStatus{VirtualNetwork: &model.MachineVirtualNetworkStatus{Name: "app", VNI: 4096, IP: "10.50.0.2", PrefixLength: 24, Gateway: "10.50.0.1"}}
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req.Network)
	got := string(b)
	for _, want := range []string{`"mode":"tap"`, `"mac":"` + model.DefaultMAC("prod", "web") + `"`, `"vnet":{"vni":4096,"ip":"10.50.0.2","prefix_len":24,"gateway":"10.50.0.1"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("network %s lacks %s", got, want)
		}
	}
	if strings.Contains(got, "bridge") {
		t.Fatalf("vnet NIC got a bridge: %s", got)
	}
}

func TestEjectCdrom(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/u1/cdroms/install/eject" {
			http.Error(w, "bad route "+r.URL.Path, 404)
			return
		}
		_, _ = w.Write([]byte(`{"uuid":"u1","status":"Running","request":{"cdroms":[{"name":"install","path":""},{"name":"virtio","path":"/c/v.iso"}]}}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	rec, err := c.EjectCdrom(context.Background(), "u1", "install")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Request.Cdroms) != 2 || rec.Request.Cdroms[0].Path != "" || rec.Request.Cdroms[1].Path != "/c/v.iso" {
		t.Fatalf("cdroms=%+v", rec.Request.Cdroms)
	}
}

func TestCreateMapping(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			http.Error(w, "bad route", 404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Name: got.Name, Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{Image: model.ImageSpec{Path: "/images/db.qcow2"}, Resources: model.ResourceSpec{CPU: "1500m", Memory: "2Gi"}, Runtime: model.RuntimeSpec{Backend: "qemu"}, Network: model.NetworkSpec{Mode: "tap", NetNS: true, TapName: "tap-db", StaticNetwork: true, PodUID: "pod-1"}}}
	r, err := c.Create(context.Background(), m, "qemu")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID() != "u1" || got.Name != "kairon-prod-db" || got.Tenant != "prod" || got.VCPUs != 2 || got.MemoryMiB != 2048 {
		t.Fatalf("record=%+v request=%+v", r, got)
	}
	if got.Network["mode"] != "tap" || got.Network["netns"] != true || got.Network["tap_name"] != "tap-db" {
		t.Fatalf("network=%v", got.Network)
	}
	if got.PodUID != "pod-1" || got.CloudInit == nil || !got.CloudInit.StaticNetwork {
		t.Fatalf("pod/cloud_init=%+v", got)
	}
}

func TestCreatePassesNUMACPUSetHugepagesForQEMU(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Name: got.Name, Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	numaNode := 1
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/db.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi", NUMANode: &numaNode, CPUSet: "0-1", Hugepages: true},
			Runtime:   model.RuntimeSpec{Backend: "qemu"},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if got.NUMANode == nil || *got.NUMANode != 1 || got.CPUSet != "0-1" || !got.Hugepages {
		t.Fatalf("expected NUMA/cpuset/hugepages to pass through, got %+v", got)
	}
}

func TestCreatePassesMaxCPUMaxMemoryHotplugHeadroom(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Name: got.Name, Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/db.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi", MaxCPU: "16", MaxMemory: "32Gi"},
			Runtime:   model.RuntimeSpec{Backend: "qemu"},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if got.MaxVCPUs == nil || *got.MaxVCPUs != 16 {
		t.Fatalf("expected max_vcpus=16, got %+v", got.MaxVCPUs)
	}
	if got.MaxMemoryMiB == nil || *got.MaxMemoryMiB != 32*1024 {
		t.Fatalf("expected max_memory_mib=32Gi, got %+v", got.MaxMemoryMiB)
	}
}

func TestCreateOmitsMaxCPUMaxMemoryWhenUnset(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/db.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:   model.RuntimeSpec{Backend: "qemu"},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if got.MaxVCPUs != nil || got.MaxMemoryMiB != nil {
		t.Fatalf("expected max_vcpus/max_memory_mib to stay unset (nil) so FluxVM picks its own default, got %+v/%+v", got.MaxVCPUs, got.MaxMemoryMiB)
	}
}

func TestCreateOmitsMaxMemoryAtOrBelowBootMemory(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	for _, maxMem := range []string{"2Gi", "1Gi"} {
		got = CreateRequest{}
		m := model.Machine{
			Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
			Spec: model.MachineSpec{
				Image:     model.ImageSpec{Path: "/images/db.qcow2"},
				Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi", MaxCPU: "2", MaxMemory: maxMem},
				Runtime:   model.RuntimeSpec{Backend: "qemu"},
			},
		}
		if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
			t.Fatal(err)
		}
		if got.MaxMemoryMiB != nil {
			t.Fatalf("maxMemory=%s: expected max_memory_mib unset, got %d", maxMem, *got.MaxMemoryMiB)
		}
		if got.MaxVCPUs == nil || *got.MaxVCPUs != 2 {
			t.Fatalf("maxMemory=%s: expected max_vcpus=2, got %+v", maxMem, got.MaxVCPUs)
		}
	}
}

func TestCreateRejectsInvalidMaxCPUMaxMemory(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("expected the request to be rejected before it ever reached FluxVM")
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	base := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/db.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:   model.RuntimeSpec{Backend: "qemu"},
		},
	}
	badMaxCPU := base
	badMaxCPU.Spec.Resources.MaxCPU = "not-a-number"
	if _, err := c.Create(context.Background(), badMaxCPU, "qemu"); err == nil {
		t.Fatal("expected an error for an invalid spec.resources.maxCpu")
	}
	badMaxMemory := base
	badMaxMemory.Spec.Resources.MaxMemory = "not-a-quantity"
	if _, err := c.Create(context.Background(), badMaxMemory, "qemu"); err == nil {
		t.Fatal("expected an error for an invalid spec.resources.maxMemory")
	}
}

func TestCreateRejectsNUMAForNonQEMUBackend(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("expected the request to be rejected before it ever reached FluxVM")
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	numaNode := 0
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/db.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi", NUMANode: &numaNode},
			Runtime:   model.RuntimeSpec{Backend: "cloud-hypervisor"},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err == nil {
		t.Fatal("expected an error for spec.resources.numaNode on a non-qemu backend")
	}
}

func TestCreateAllowsNoNUMAFieldsForNonQEMUBackend(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/db.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:   model.RuntimeSpec{Backend: "cloud-hypervisor"},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatalf("expected no error when no NUMA/cpuset/hugepages fields are set, got %v", err)
	}
}

func TestCreatePassesSecureBootAndTPMOnQEMU(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Name: got.Name, Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "win", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/win.qcow2"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"},
			Runtime:   model.RuntimeSpec{Backend: "qemu"},
			Security:  model.SecuritySpec{SecureBoot: true, TPM: true},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if !got.SecureBoot || !got.TPM {
		t.Fatalf("expected secure_boot/tpm to pass through, got %+v", got)
	}
}

func TestCreatePassesTPMOnlyOnCloudHypervisor(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Name: got.Name, Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "ch", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/ch.raw"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"},
			Runtime:   model.RuntimeSpec{Backend: "cloud-hypervisor"},
			Security:  model.SecuritySpec{TPM: true},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if !got.TPM {
		t.Fatalf("expected tpm to pass through on cloud-hypervisor, got %+v", got)
	}
}

func TestCreateRejectsSecureBootAndTPMOnUnsupportedBackends(t *testing.T) {
	cases := []struct {
		backend string
		sec     model.SecuritySpec
	}{
		{"cloud-hypervisor", model.SecuritySpec{SecureBoot: true}},
		{"firecracker", model.SecuritySpec{SecureBoot: true}},
		{"firecracker", model.SecuritySpec{TPM: true}},
		{"flux-vm", model.SecuritySpec{TPM: true}},
	}
	for _, tc := range cases {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("expected the request to be rejected before it ever reached FluxVM")
		}))
		c := New(s.URL, "")
		c.HTTP = s.Client()
		m := model.Machine{
			Metadata: model.ObjectMeta{Name: "win", Namespace: "prod"},
			Spec: model.MachineSpec{
				Image:     model.ImageSpec{Path: "/images/win.qcow2"},
				Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"},
				Runtime:   model.RuntimeSpec{Backend: tc.backend},
				Security:  tc.sec,
			},
		}
		_, err := c.Create(context.Background(), m, "qemu")
		s.Close()
		if err == nil {
			t.Fatalf("expected an error for backend=%q security=%+v", tc.backend, tc.sec)
		}
	}
}

func TestCreateUserForwards(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u2", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{Metadata: model.ObjectMeta{Name: "nat", Namespace: "default"}, Spec: model.MachineSpec{
		Image: model.ImageSpec{Path: "/images/a.qcow2"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"},
		Network: model.NetworkSpec{Mode: "user", Forwards: []model.PortForward{{HostPort: 8080, GuestPort: 80}}},
	}}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	fw, ok := got.Network["forwards"].([]any)
	if !ok || len(fw) != 1 {
		t.Fatalf("forwards=%v", got.Network["forwards"])
	}
}

func TestCreateWiresCloudInitWriteFiles(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			Image: model.ImageSpec{Path: "/images/db.qcow2"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"},
			CloudInit: model.CloudInitSpec{WriteFiles: []model.CloudInitFile{
				{Path: "/etc/app/config.yaml", Content: "key: value\n", Permissions: "0600"},
			}},
		},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if got.CloudInit == nil || len(got.CloudInit.WriteFiles) != 1 {
		t.Fatalf("expected write_files to be forwarded, got %+v", got.CloudInit)
	}
	f := got.CloudInit.WriteFiles[0]
	if f.Path != "/etc/app/config.yaml" || f.Content != "key: value\n" || f.Permissions != "0600" {
		t.Fatalf("unexpected write_files entry: %+v", f)
	}
}

func TestCreateOmitsCloudInitWhenNothingSet(t *testing.T) {
	var got CreateRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:     model.MachineSpec{Image: model.ImageSpec{Path: "/images/db.qcow2"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
		t.Fatal(err)
	}
	if got.CloudInit != nil {
		t.Fatalf("expected no cloud_init payload when nothing is set, got %+v", got.CloudInit)
	}
}

func TestSetVMNetworkPolicy(t *testing.T) {
	var path string
	var body WireVmNetworkPolicy
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	mbps := uint32(100)
	if err := c.SetVMNetworkPolicy(context.Background(), "vm-1", model.VmNetworkPolicy{DefaultAllow: false, AllowPorts: []string{"tcp/443"}, MaxEgressMbps: &mbps}); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/vms/vm-1/network/policy" || body.DefaultAllow || body.MaxEgressMbps == nil || *body.MaxEgressMbps != 100 {
		t.Fatalf("path=%s body=%+v", path, body)
	}
}

func TestLookupArray(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Record{{IDValue: "id1", Name: "vm"}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	r, err := c.LookupByName(context.Background(), "vm")
	if err != nil || r == nil || r.ID() != "id1" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

func TestGetStats(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cpu_usage_percent": 42.5, "memory_usage_bytes": 123456, "disk_read_bytes": 111, "disk_write_bytes": 222,
		})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	stats, err := c.GetStats(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/stats" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if stats.CPUUsagePercent != 42.5 || stats.MemoryUsageBytes != 123456 || stats.DiskReadBytes != 111 || stats.DiskWriteBytes != 222 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestGetStatsPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM not found", http.StatusNotFound)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.GetStats(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestPauseAndResume(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		status := "Paused"
		if strings.HasSuffix(r.URL.Path, "/resume") {
			status = "Running"
		}
		_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: status})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	rec, err := c.Pause(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/pause" || rec.Status != "Paused" {
		t.Fatalf("unexpected pause result: path=%q rec=%+v", gotPath, rec)
	}

	rec, err = c.Resume(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/resume" || rec.Status != "Running" {
		t.Fatalf("unexpected resume result: path=%q rec=%+v", gotPath, rec)
	}
}

func TestPauseAndResumePropagateErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM not found", http.StatusNotFound)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.Pause(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
	if _, err := c.Resume(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestDeleteNotFoundIsIdempotent(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if err := c.Delete(context.Background(), "missing"); err != nil {
		t.Fatalf("delete should be idempotent: %v", err)
	}
}

func TestDeleteBadRequestVMNotFoundIsIdempotent(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"VM not found"}`))
			return
		}
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if err := c.Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("HTTP 400 VM not found should count as deleted: %v", err)
	}
}

func TestDeleteOtherBadRequestStillFails(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"VM is busy"}`, http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if err := c.Delete(context.Background(), "x"); err == nil {
		t.Fatal("unrelated 400 must still be an error")
	}
}

func TestCreateWiresQgaAndAgentIndependently(t *testing.T) {
	cases := []struct {
		name       string
		guestAgent model.GuestAgentSpec
		wantQga    bool
		wantAgent  bool
	}{
		{"neither set", model.GuestAgentSpec{}, false, false},
		{"only qemu-guest-agent", model.GuestAgentSpec{Enabled: true}, true, false},
		{"only the fluxvm-native console agent", model.GuestAgentSpec{Console: true}, false, true},
		{"both", model.GuestAgentSpec{Enabled: true, Console: true}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got CreateRequest
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				_ = json.NewEncoder(w).Encode(Record{UUID: "u1", Status: "Running"})
			}))
			defer s.Close()
			c := New(s.URL, "")
			c.HTTP = s.Client()
			m := model.Machine{
				Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
				Spec: model.MachineSpec{
					Image: model.ImageSpec{Path: "/images/db.qcow2"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"},
					Runtime: model.RuntimeSpec{Backend: "qemu"}, GuestAgent: tc.guestAgent,
				},
			}
			if _, err := c.Create(context.Background(), m, "qemu"); err != nil {
				t.Fatal(err)
			}
			if (got.Qga != nil && got.Qga.Enabled) != tc.wantQga {
				t.Fatalf("qga=%+v, want enabled=%v", got.Qga, tc.wantQga)
			}
			if (got.Agent != nil && got.Agent.Enabled) != tc.wantAgent {
				t.Fatalf("agent=%+v, want enabled=%v", got.Agent, tc.wantAgent)
			}
		})
	}
}

func TestSetResourceLimitsOnlySendsSetFields(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	cpu := uint32(200)
	mem := uint64(1 << 30)
	if err := c.SetResourceLimits(context.Background(), "vm-1", model.ResourceLimits{CPUQuotaPercent: &cpu, MemoryMaxBytes: &mem}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/resources" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["cpu_quota_percent"] != float64(200) || gotBody["memory_max_bytes"] != float64(1<<30) {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
	if _, hasIOWeight := gotBody["io_weight"]; hasIOWeight {
		t.Fatalf("did not expect io_weight to be sent when unset, got %+v", gotBody)
	}
	if _, hasPidsMax := gotBody["pids_max"]; hasPidsMax {
		t.Fatalf("did not expect pids_max to be sent when unset, got %+v", gotBody)
	}
}

func TestSetResourceLimitsSendsCPUSetCPUsWhenSet(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.SetResourceLimits(context.Background(), "vm-1", model.ResourceLimits{CPUSetCPUs: []uint32{2, 3, 4}}); err != nil {
		t.Fatal(err)
	}
	cpus, ok := gotBody["cpuset_cpus"].([]any)
	if !ok || len(cpus) != 3 || cpus[0] != float64(2) || cpus[1] != float64(3) || cpus[2] != float64(4) {
		t.Fatalf("unexpected cpuset_cpus: %+v", gotBody["cpuset_cpus"])
	}
}

func TestSetResourceLimitsOmitsCPUSetCPUsWhenEmpty(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	cpu := uint32(200)
	if err := c.SetResourceLimits(context.Background(), "vm-1", model.ResourceLimits{CPUQuotaPercent: &cpu}); err != nil {
		t.Fatal(err)
	}
	if _, hasCPUSet := gotBody["cpuset_cpus"]; hasCPUSet {
		t.Fatalf("did not expect cpuset_cpus to be sent when unset, got %+v", gotBody)
	}
}

func TestSetResourceLimitsPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM has no cgroup (not running)", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	cpu := uint32(50)
	if err := c.SetResourceLimits(context.Background(), "vm-1", model.ResourceLimits{CPUQuotaPercent: &cpu}); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestCreateSendsDataDisks(t *testing.T) {
	var body map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"vm-1","status":"Running"}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:     model.MachineSpec{Image: model.ImageSpec{Path: "/i/disk0.raw"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	if _, err := c.CreateWithVFIO(context.Background(), m, "qemu", nil, nil, []DataDisk{{Name: "import-disk1", Backing: "/i/disk1.raw"}}); err != nil {
		t.Fatal(err)
	}
	disks, _ := body["data_disks"].([]any)
	if len(disks) != 1 || disks[0].(map[string]any)["backing"] != "/i/disk1.raw" {
		t.Fatalf("data_disks = %v", body["data_disks"])
	}
}

func TestBuildCreateRequestInstallMedia(t *testing.T) {
	m := model.Machine{Spec: model.MachineSpec{
		Image:     model.ImageSpec{Path: "/cache/blank/empty.raw", Blank: true, DiskSize: "60Gi"},
		Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"},
		Cdroms:    []model.MachineCdrom{{Name: "install", Path: "/cache/sha256/aa"}, {Name: "virtio", Path: "/cache/sha256/bb"}},
	}}
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.DiskSizeGiB == nil || *req.DiskSizeGiB != 60 {
		t.Fatalf("disk size: %v", req.DiskSizeGiB)
	}
	if len(req.Cdroms) != 2 || req.Cdroms[0] != (Cdrom{Name: "install", Path: "/cache/sha256/aa"}) {
		t.Fatalf("cdroms: %+v", req.Cdroms)
	}
	b, _ := json.Marshal(req)
	if !strings.Contains(string(b), `"disk_size_gib":60`) || !strings.Contains(string(b), `"cdroms":[{"name":"install","path":"/cache/sha256/aa"}`) {
		t.Fatalf("wire format: %s", b)
	}
	if _, err := buildCreateRequest(m, "cloud-hypervisor", nil, nil); err == nil || !strings.Contains(err.Error(), "spec.cdroms requires the qemu backend") {
		t.Fatalf("non-qemu cdroms: %v", err)
	}
	m.Spec.Cdroms[1].Path = ""
	if _, err := buildCreateRequest(m, "qemu", nil, nil); err == nil {
		t.Fatal("unresolved cdrom path must fail")
	}
	plain := model.Machine{Spec: model.MachineSpec{Image: model.ImageSpec{Path: "/x"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}}}
	if b, _ := json.Marshal(mustBuild(t, plain)); strings.Contains(string(b), "cdroms") || strings.Contains(string(b), "disk_size_gib") {
		t.Fatalf("plain request must not carry new fields: %s", b)
	}
}

func mustBuild(t *testing.T, m model.Machine) CreateRequest {
	t.Helper()
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
