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
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func readyVnet(name string, vni uint32, snat bool, allocs ...model.VirtualNetworkAllocation) model.VirtualNetwork {
	return model.VirtualNetwork{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.VirtualNetworkSpec{CIDR: "10.50.0.0/24", EgressSNAT: snat},
		Status:   model.VirtualNetworkStatus{Phase: "Ready", VNI: vni, Gateway: "10.50.0.1", Allocations: allocs},
	}
}

func TestDesiredVnetConfigs(t *testing.T) {
	vnets := []model.VirtualNetwork{
		readyVnet("app", 4096, true,
			model.VirtualNetworkAllocation{Machine: "a", IP: "10.50.0.2", MAC: "52:54:00:00:00:02", FloatingIP: "203.0.113.10", Node: "n1", NodeIP: "192.0.2.1"},
			model.VirtualNetworkAllocation{Machine: "b", IP: "10.50.0.3", MAC: "52:54:00:00:00:03", FloatingIP: "203.0.113.11", Node: "n2", NodeIP: "192.0.2.2"},
			model.VirtualNetworkAllocation{Machine: "c", IP: "10.50.0.4", MAC: "52:54:00:00:00:04"}, // unscheduled
		),
		readyVnet("elsewhere", 4097, false,
			model.VirtualNetworkAllocation{Machine: "d", IP: "10.50.0.2", MAC: "52:54:00:00:00:05", Node: "n2", NodeIP: "192.0.2.2"},
		),
		{Metadata: model.ObjectMeta{Name: "pending"}, Spec: model.VirtualNetworkSpec{CIDR: "10.9.0.0/24"}},
	}
	got, floating := DesiredVnetConfigs(vnets, "n1", "198.51.100.1", "eno1")
	if len(got) != 1 {
		t.Fatalf("configs = %+v", got)
	}
	cfg := got[4096]
	if cfg.CIDR != "10.50.0.0/24" || cfg.Gateway != "10.50.0.1" || cfg.Uplink != "eno1" || cfg.SNATAddress != "198.51.100.1" {
		t.Fatalf("config = %+v", cfg)
	}
	if len(cfg.Endpoints) != 2 || cfg.Endpoints[0].NodeIP != "198.51.100.1" || cfg.Endpoints[1].NodeIP != "192.0.2.2" {
		t.Fatalf("endpoints = %+v", cfg.Endpoints)
	}
	if len(cfg.FloatingIPs) != 1 || cfg.FloatingIPs[0].Address != "203.0.113.10" || cfg.FloatingIPs[0].IP != "10.50.0.2" {
		t.Fatalf("floating IPs = %+v", cfg.FloatingIPs)
	}
	if len(floating) != 1 || floating[0].String() != "203.0.113.10/32" {
		t.Fatalf("announced = %v", floating)
	}

	// No uplink: the network stays closed, nothing to announce.
	got, floating = DesiredVnetConfigs(vnets, "n1", "198.51.100.1", "")
	if c := got[4096]; c.Uplink != "" || c.SNATAddress != "" || len(c.FloatingIPs) != 0 || len(floating) != 0 {
		t.Fatalf("closed config = %+v floating=%v", c, floating)
	}
}

func TestMachineVirtualNetworkStatus(t *testing.T) {
	vn := readyVnet("app", 4096, false, model.VirtualNetworkAllocation{Machine: "a", IP: "10.50.0.2", MAC: "52:54:00:00:00:02", FloatingIP: "203.0.113.10"})
	st, err := machineVirtualNetworkStatus(vn, "a")
	if err != nil {
		t.Fatal(err)
	}
	if st.VNI != 4096 || st.IP != "10.50.0.2" || st.PrefixLength != 24 || st.Gateway != "10.50.0.1" || st.FloatingIP != "203.0.113.10" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := machineVirtualNetworkStatus(vn, "zz"); err == nil || !strings.Contains(err.Error(), "waiting for an address") {
		t.Fatalf("unallocated: %v", err)
	}
	vn.Status.Phase = "Failed"
	if _, err := machineVirtualNetworkStatus(vn, "a"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("failed network: %v", err)
	}
}

func TestProjectNetworkStatusRecoversALostVirtualNetworkStatus(t *testing.T) {
	vn := readyVnet("app", 4096, false, model.VirtualNetworkAllocation{Machine: "db", IP: "10.50.0.3", MAC: "52:54:00:00:00:03"})
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kairon.zyvor.dev/v1/namespaces/prod/virtualnetworks/app" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(vn)
	}))
	defer ks.Close()
	fs := httptest.NewServer(http.NotFoundHandler())
	defer fs.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	a := &Agent{Kube: kc, Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// Running, but the status write that recorded the address was lost.
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{VirtualNetwork: &model.MachineVirtualNetwork{Name: "app"}}},
	}
	var status model.MachineStatus
	if err := a.projectNetworkStatus(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Network == nil || status.Network.VirtualNetwork == nil || status.Network.VirtualNetwork.IP != "10.50.0.3" || status.Network.VirtualNetwork.VNI != 4096 {
		t.Fatalf("network = %+v", status.Network)
	}
}
