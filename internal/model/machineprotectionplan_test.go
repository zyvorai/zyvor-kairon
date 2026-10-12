// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"reflect"
	"strings"
	"testing"
)

func protectionSpec() MachineProtectionPlanSpec {
	return MachineProtectionPlanSpec{
		Role: ProtectionRolePrimary,
		Machines: []ProtectedMachine{
			{Name: "web", DependsOn: []string{"app"}},
			{Name: "app", DependsOn: []string{"db", "cache"}},
			{Name: "db"},
			{Name: "cache"},
			{Name: "batch", DependsOn: []string{"db"}},
		},
		Replication: ProtectionReplication{Interval: "15m", Target: BackupTarget{S3: &S3BackupTarget{Endpoint: "http://minio:9000", Bucket: "dr", CredentialsSecretRef: "s3"}}},
		Peer:        ProtectionPeer{Server: "https://dr.example:6443", CredentialsSecretRef: "peer"},
	}
}

func TestProtectionOrderWaves(t *testing.T) {
	waves, err := protectionSpec().Order()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"cache", "db"}, {"app", "batch"}, {"web"}}
	if !reflect.DeepEqual(waves, want) {
		t.Fatalf("waves = %v, want %v", waves, want)
	}
}

func TestProtectionOrderRejects(t *testing.T) {
	for name, mutate := range map[string]func(*MachineProtectionPlanSpec){
		"cycle":   func(s *MachineProtectionPlanSpec) { s.Machines[2].DependsOn = []string{"web"} },
		"unknown": func(s *MachineProtectionPlanSpec) { s.Machines[2].DependsOn = []string{"ldap"} },
		"twice":   func(s *MachineProtectionPlanSpec) { s.Machines = append(s.Machines, ProtectedMachine{Name: "db"}) },
		"badname": func(s *MachineProtectionPlanSpec) { s.Machines[0].Name = "Web_1" },
		"empty":   func(s *MachineProtectionPlanSpec) { s.Machines = nil },
	} {
		s := protectionSpec()
		mutate(&s)
		if _, err := s.Order(); err == nil {
			t.Errorf("%s: Order accepted %+v", name, s.Machines)
		}
	}
}

func TestProtectionValidate(t *testing.T) {
	if err := protectionSpec().Validate("shop"); err != nil {
		t.Fatal(err)
	}
	for want, mutate := range map[string]func(*MachineProtectionPlanSpec){
		"role":           func(s *MachineProtectionPlanSpec) { s.Role = "Leader" },
		"replication":    func(s *MachineProtectionPlanSpec) { s.Replication.Interval = "1m" },
		"peer.server":    func(s *MachineProtectionPlanSpec) { s.Peer.Server = "" },
		"objectives.rpo": func(s *MachineProtectionPlanSpec) { s.Objectives.RPO = "soon" },
		"test.ttl":       func(s *MachineProtectionPlanSpec) { s.Test.TTL = "forever" },
		"action.type":    func(s *MachineProtectionPlanSpec) { s.Action = &ProtectionAction{Type: "Promote", ID: "1"} },
		"action.id":      func(s *MachineProtectionPlanSpec) { s.Action = &ProtectionAction{Type: ProtectionActionTest} },
		"applies to Failover": func(s *MachineProtectionPlanSpec) {
			s.Action = &ProtectionAction{Type: ProtectionActionTest, ID: "1", Planned: true}
		},
	} {
		s := protectionSpec()
		mutate(&s)
		if err := s.Validate("shop"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestProtectionBackupPolicySpecDefaults(t *testing.T) {
	s := protectionSpec()
	got := s.BackupPolicySpec("shop")
	if got.Selector[ProtectionPlanLabel] != "shop" || got.IncrementalEvery != "15m" || got.FullEvery != "24h0m0s" ||
		got.Retention != (BackupRetention{Hourly: 24, Daily: 7}) || got.Target.S3.Bucket != "dr" {
		t.Fatalf("policy = %+v", got)
	}
	s.Replication.Retention = &BackupRetention{Daily: 30}
	s.Replication.FullEvery = "7d"
	if got := s.BackupPolicySpec("shop"); got.Retention.Daily != 30 || got.Retention.Hourly != 0 || got.FullEvery != "7d" {
		t.Fatalf("overrides = %+v", got)
	}
}

func TestMirrorSpecClearsPlacement(t *testing.T) {
	var spec MachineSpec
	spec.NodeName = "a1"
	spec.Resources.AllocatedCPUSet = []uint32{4}
	spec.Resources.CPU = "2"
	got := MirrorSpec(spec)
	if got.NodeName != "" || got.Resources.AllocatedCPUSet != nil || got.Resources.CPU != "2" {
		t.Fatalf("mirrored = %+v", got)
	}
}

func TestProtectionValidateNetworkAndSchedule(t *testing.T) {
	for name, mutate := range map[string]func(*MachineProtectionPlanSpec){
		"every too short":   func(s *MachineProtectionPlanSpec) { s.Test.Every = "1m" },
		"every unparseable": func(s *MachineProtectionPlanSpec) { s.Test.Every = "monthly" },
		"dns unknown": func(s *MachineProtectionPlanSpec) {
			s.Network.DNS = []ProtectionDNSRecord{{Machine: "nope", Hostname: "a.example.com"}}
		},
		"dns bad name": func(s *MachineProtectionPlanSpec) {
			s.Network.DNS = []ProtectionDNSRecord{{Machine: "db", Hostname: "Bad_Name"}}
		},
		"dns twice": func(s *MachineProtectionPlanSpec) {
			s.Network.DNS = []ProtectionDNSRecord{{Machine: "db", Hostname: "a.example.com"}, {Machine: "web", Hostname: "a.example.com"}}
		},
		"dns address": func(s *MachineProtectionPlanSpec) {
			s.Network.DNS = []ProtectionDNSRecord{{Machine: "db", Hostname: "a.example.com", Address: "mac"}}
		},
	} {
		s := protectionSpec()
		mutate(&s)
		if err := s.Validate("shop"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s := protectionSpec()
	s.Test.Every = "720h"
	s.Network = ProtectionNetwork{PreserveIP: true, DNS: []ProtectionDNSRecord{{Machine: "db", Hostname: "db.shop.example.com.", Address: DNSAddressIP, TTL: 30}}}
	if err := s.Validate("shop"); err != nil {
		t.Fatal(err)
	}
}

func TestPreservedSpecPinsTheAllocatedAddress(t *testing.T) {
	m := Machine{Spec: MachineSpec{NodeName: "n1", Network: NetworkSpec{Mode: "tap", VirtualNetwork: &MachineVirtualNetwork{Name: "app"}}}}
	m.Status.Network = &MachineNetworkStatus{VirtualNetwork: &MachineVirtualNetworkStatus{Name: "app", IP: "10.50.0.7"}}
	if got := PreservedSpec(m, true); got.Network.VirtualNetwork.IP != "10.50.0.7" || got.NodeName != "" || m.Spec.Network.VirtualNetwork.IP != "" {
		t.Fatalf("preserved = %+v, source changed: %+v", got.Network.VirtualNetwork, m.Spec.Network.VirtualNetwork)
	}
	if got := PreservedSpec(m, false); got.Network.VirtualNetwork.IP != "" {
		t.Fatal("pinned without preserveIP")
	}
	m.Spec.Network.VirtualNetwork.IP = "10.50.0.3"
	if got := PreservedSpec(m, true); got.Network.VirtualNetwork.IP != "10.50.0.3" {
		t.Fatal("a requested address must win")
	}
}

func TestDNSAddressPicksTheFirstAvailable(t *testing.T) {
	m := Machine{}
	m.Status.GuestIP = "192.168.1.5"
	auto := ProtectionDNSRecord{}
	if got := auto.DNSAddress(m); got != "192.168.1.5" {
		t.Fatalf("guest only = %q", got)
	}
	m.Status.Network = &MachineNetworkStatus{VirtualNetwork: &MachineVirtualNetworkStatus{IP: "10.50.0.7", FloatingIP: "203.0.113.4"}}
	if got := auto.DNSAddress(m); got != "203.0.113.4" {
		t.Fatalf("auto = %q", got)
	}
	if got := (ProtectionDNSRecord{Address: DNSAddressIP}).DNSAddress(m); got != "10.50.0.7" {
		t.Fatalf("ip = %q", got)
	}
	if got := (ProtectionDNSRecord{Address: DNSAddressFloatingIP}).DNSAddress(Machine{}); got != "" {
		t.Fatalf("missing floating = %q", got)
	}
}
