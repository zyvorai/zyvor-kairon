// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleListPoolsFullRelay(t *testing.T) {
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pools" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"warm-agents","size":3,"members":[]}]}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/worker-1/pools", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCreatePoolDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "worker-1"}}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, // not admin
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/pools", token, createPoolRequest{Name: "warm-agents", Size: 3})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCreatePoolFullRelay(t *testing.T) {
	var gotBody string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pools" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		buf := make([]byte, 2048)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"name":"warm-agents","size":3,"members":[]}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/pools", token, createPoolRequest{Name: "warm-agents", Size: 3, Template: map[string]any{"backend": "flux-vm", "image": "alpine-agent", "vcpus": 1, "memory_mib": 512}})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotBody == "" {
		t.Fatal("expected the request to reach FluxVM through both relay hops")
	}
}

func TestHandleClaimPoolFullRelay(t *testing.T) {
	var gotPath string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"uuid":"vm-1","status":"Running"}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/pools/warm-agents/claim", token, claimPoolRequest{Name: "agent-run"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/pools/warm-agents/claim" {
		t.Fatalf("unexpected upstream path: %q", gotPath)
	}
}

func TestHandleDeletePoolFullRelay(t *testing.T) {
	var gotPath, gotMethod string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodDelete, "/api/v1/nodes/worker-1/pools/warm-agents", token, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/pools/warm-agents" || gotMethod != http.MethodDelete {
		t.Fatalf("unexpected upstream request: method=%q path=%q", gotMethod, gotPath)
	}
}

func TestHandleClaimPoolWithCreateMachineForcesRuntimeNameAndCreatesMachine(t *testing.T) {
	var claimBody string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/pools/warm-agents":
			_, _ = w.Write([]byte(`{"name":"warm-agents","size":3,"template":{"backend":"qemu","image":"/images/agent.qcow2","vcpus":2,"memory_mib":1024}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pools/warm-agents/claim":
			buf := make([]byte, 2048)
			n, _ := r.Body.Read(buf)
			claimBody = string(buf[:n])
			_, _ = w.Write([]byte(`{"uuid":"vm-1","status":"Running"}`))
		default:
			http.Error(w, "bad route: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/pools/warm-agents/claim", token, claimPoolRequest{
		CreateMachine: true, Namespace: "default", MachineName: "agent-run",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The claim body sent upstream must carry the forced RuntimeName(),
	// not left empty or whatever the caller might have set on Name --
	// this is what lets kairon-node's existing adoption path pick up
	// this exact runtime instead of creating a second, duplicate VM.
	if !strings.Contains(claimBody, "kairon-default-agent-run") {
		t.Fatalf("expected the upstream claim body to force name=kairon-default-agent-run, got %q", claimBody)
	}

	var resp struct {
		VM      map[string]any `json:"vm"`
		Machine model.Machine  `json:"machine"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.VM["uuid"] != "vm-1" {
		t.Fatalf("expected the claimed VM record in the response, got %+v", resp.VM)
	}
	if resp.Machine.Metadata.Name != "agent-run" || resp.Machine.Metadata.Namespace != "default" {
		t.Fatalf("unexpected machine identity in response: %+v", resp.Machine.Metadata)
	}
	if resp.Machine.Spec.Image.Path != "/images/agent.qcow2" || resp.Machine.Spec.Resources.CPU != "2" || resp.Machine.Spec.Resources.Memory != "1024Mi" {
		t.Fatalf("expected the Machine spec built from the pool template, got %+v", resp.Machine.Spec)
	}

	fk.mu.Lock()
	created, ok := fk.machines["agent-run"]
	fk.mu.Unlock()
	if !ok {
		t.Fatal("expected a real Machine object to have been created via the Kubernetes API server")
	}
	if created.Spec.Image.Path != "/images/agent.qcow2" {
		t.Fatalf("unexpected created machine spec: %+v", created.Spec)
	}
	if created.Spec.NodeName != "worker-1" {
		t.Fatalf("expected the Machine pinned to the claimed VM's node, got nodeName=%q", created.Spec.NodeName)
	}
}

func TestMachineSpecFromPoolTemplateCarriesClaims(t *testing.T) {
	numa := 1
	template := map[string]any{
		"backend": "qemu", "image": "/images/agent.qcow2", "kernel": "/k/vmlinuz",
		"vcpus": 2, "memory_mib": 1024, "max_vcpus": 8, "max_memory_mib": 8192,
		"numa_node": numa, "cpuset": "2-3", "hugepages": true,
		"secure_boot": true, "tpm": true, "ttl_seconds": 600, "storage": "default",
		"network": map[string]any{"mode": "user", "forwards": []map[string]any{{"host_port": 2222, "guest_port": 22, "protocol": "tcp"}}},
		"qga":     map[string]any{"enabled": true},
		"agent":   map[string]any{"enabled": true},
		"cloud_init": map[string]any{
			"hostname": "agent", "user": "ops", "ssh_authorized_keys": []string{"ssh-ed25519 AAA"},
			"runcmd": []string{"echo hi"}, "static_network": true,
			"write_files": []map[string]any{{"path": "/etc/x", "content": "y", "permissions": "0644"}},
		},
	}
	spec, warnings, err := machineSpecFromPoolTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	r := spec.Resources
	if r.CPU != "2" || r.Memory != "1024Mi" || r.MaxCPU != "8" || r.MaxMemory != "8192Mi" {
		t.Fatalf("resources not carried: %+v", r)
	}
	if r.NUMANode == nil || *r.NUMANode != 1 || r.CPUSet != "2-3" || !r.Hugepages {
		t.Fatalf("NUMA claims not carried: %+v", r)
	}
	if f := spec.Network.Forwards; len(f) != 1 || f[0].HostPort != 2222 || f[0].GuestPort != 22 {
		t.Fatalf("user-mode forwards not carried: %+v", f)
	}
	if spec.Runtime.Kernel != "/k/vmlinuz" || spec.TTLSeconds != 600 {
		t.Fatalf("kernel/ttl not carried: %+v", spec)
	}
	if !spec.Security.SecureBoot || !spec.Security.TPM || !spec.GuestAgent.Enabled || !spec.GuestAgent.Console {
		t.Fatalf("security/guest agent not carried: %+v %+v", spec.Security, spec.GuestAgent)
	}
	ci := spec.CloudInit
	if ci.Hostname != "agent" || ci.User != "ops" || len(ci.SSHAuthorizedKeys) != 1 || len(ci.RunCmd) != 1 ||
		len(ci.WriteFiles) != 1 || ci.WriteFiles[0].Permissions != "0644" || !spec.Network.StaticNetwork {
		t.Fatalf("cloud-init not carried: %+v static=%v", ci, spec.Network.StaticNetwork)
	}
}

func TestMachineSpecFromPoolTemplateWarnsOnUncarriedFields(t *testing.T) {
	_, warnings, err := machineSpecFromPoolTemplate(map[string]any{
		"image": "/images/a.qcow2", "vcpus": 1, "memory_mib": 512,
		"vfio_devices": []string{"0000:01:00.0"},
		"network":      map[string]any{"mode": "tap", "mac": "52:54:00:00:00:01"},
		"data_disks":   []map[string]any{{"name": "d", "backing": "/b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"vfio_devices", "network", "data_disks"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected a warning naming %s, got %v", want, warnings)
		}
	}
}

func TestHandleClaimPoolWithCreateMachineRequiresMachineName(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: "10.0.0.1"}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/pools/warm-agents/claim", token, claimPoolRequest{CreateMachine: true})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when createMachine is set without machineName, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleClaimPoolWithoutCreateMachineIsUnchanged(t *testing.T) {
	// A request that omits createMachine entirely must behave exactly as
	// before this feature existed -- the raw claimed VM record, not
	// wrapped in a {"vm":...,"machine":...} envelope, and no Machine
	// object created.
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"uuid":"vm-1","status":"Running"}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/pools/warm-agents/claim", token, claimPoolRequest{Name: "agent-run"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["uuid"] != "vm-1" {
		t.Fatalf("expected the raw claimed VM record unwrapped, got %+v", got)
	}
	if len(fk.machines) != 0 {
		t.Fatalf("expected no Machine to be created, got %+v", fk.machines)
	}
}
