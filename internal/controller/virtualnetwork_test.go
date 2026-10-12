// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func vnetFixture(ns, name string, age time.Duration, spec model.VirtualNetworkSpec) model.VirtualNetwork {
	return model.VirtualNetwork{
		Metadata: model.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: time.Unix(1_700_000_000, 0).Add(-age)},
		Spec:     spec,
	}
}

func vnetMachine(ns, name, vnet, node string, ref model.MachineVirtualNetwork) model.Machine {
	ref.Name = vnet
	m := model.Machine{Metadata: model.ObjectMeta{Name: name, Namespace: ns}}
	m.Spec.Network.VirtualNetwork = &ref
	m.Status.NodeName = node
	return m
}

func planByName(plans []VirtualNetworkPlan) map[string]VirtualNetworkPlan {
	out := map[string]VirtualNetworkPlan{}
	for _, p := range plans {
		out[p.Namespace+"/"+p.Name] = p
	}
	return out
}

func TestPlanVirtualNetworks(t *testing.T) {
	n1 := model.Node{Metadata: model.ObjectMeta{Name: "n1"}, Status: model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: "192.0.2.1"}}}}
	vnets := []model.VirtualNetwork{
		vnetFixture("prod", "app", 3*time.Hour, model.VirtualNetworkSpec{CIDR: "10.50.0.0/24", EgressSNAT: true}),
		vnetFixture("prod", "db", 2*time.Hour, model.VirtualNetworkSpec{CIDR: "10.60.0.0/24", VNI: 100}),
		vnetFixture("dev", "clash", time.Hour, model.VirtualNetworkSpec{CIDR: "10.70.0.0/24", VNI: 100}),
		vnetFixture("dev", "bad", time.Hour, model.VirtualNetworkSpec{CIDR: "10.80.0.1/24"}),
	}
	machines := []model.Machine{
		vnetMachine("prod", "web", "app", "n1", model.MachineVirtualNetwork{FloatingIP: "203.0.113.10"}),
		vnetMachine("prod", "api", "app", "", model.MachineVirtualNetwork{IP: "10.50.0.50", FloatingIP: "203.0.113.10"}),
		vnetMachine("dev", "web", "app", "n1", model.MachineVirtualNetwork{}), // other namespace: not a member
		{Metadata: model.ObjectMeta{Name: "plain", Namespace: "prod"}},
	}
	plans := planByName(PlanVirtualNetworks(vnets, machines, []model.Node{n1}))

	app := plans["prod/app"].Status
	if app.Phase != "Ready" || app.VNI != model.VNIAutoMin || app.Gateway != "10.50.0.1" {
		t.Fatalf("app status = %+v", app)
	}
	if len(app.Allocations) != 2 {
		t.Fatalf("app allocations = %+v", app.Allocations)
	}
	api, _ := app.AllocationFor("api")
	web, _ := app.AllocationFor("web")
	if api.IP != "10.50.0.50" || api.FloatingIP != "" || api.Node != "" {
		t.Fatalf("api allocation = %+v", api)
	}
	if web.IP != "10.50.0.2" || web.FloatingIP != "203.0.113.10" || web.Node != "n1" || web.NodeIP != "192.0.2.1" || web.MAC != model.DefaultMAC("prod", "web") {
		t.Fatalf("web allocation = %+v", web)
	}
	if !strings.Contains(app.Message, "Machine api: floating IP 203.0.113.10 is used by prod/app/web") {
		t.Fatalf("app message = %q", app.Message)
	}

	if db := plans["prod/db"].Status; db.Phase != "Ready" || db.VNI != 100 {
		t.Fatalf("db status = %+v", db)
	}
	if clash := plans["dev/clash"].Status; clash.Phase != "Failed" || !strings.Contains(clash.Message, "used by VirtualNetwork prod/db") {
		t.Fatalf("clash status = %+v", clash)
	}
	if bad := plans["dev/bad"].Status; bad.Phase != "Failed" || !strings.Contains(bad.Message, "host bits") {
		t.Fatalf("bad status = %+v", bad)
	}

	// VNIs stick: re-planning from the written status changes nothing, and
	// spec.vni cannot move away from the allocated one.
	for i := range vnets {
		vnets[i].Status = plans[vnets[i].Namespace()+"/"+vnets[i].Metadata.Name].Status
	}
	again := planByName(PlanVirtualNetworks(vnets, machines, []model.Node{n1}))
	if again["prod/app"].Status.VNI != model.VNIAutoMin {
		t.Fatalf("VNI moved: %+v", again["prod/app"].Status)
	}
	vnets[0].Spec.VNI = 7
	moved := planByName(PlanVirtualNetworks(vnets, machines, []model.Node{n1}))
	if st := moved["prod/app"].Status; st.Phase != "Failed" || !strings.Contains(st.Message, "cannot change") {
		t.Fatalf("spec.vni change: %+v", st)
	}
}
