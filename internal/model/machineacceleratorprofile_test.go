// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"reflect"
	"strings"
	"testing"
)

func TestAcceleratorProfileValidate(t *testing.T) {
	cases := []struct {
		name string
		spec MachineAcceleratorProfileSpec
		want string
	}{
		{"defaults", MachineAcceleratorProfileSpec{}, ""},
		{"full", MachineAcceleratorProfileSpec{
			Vendor: "nvidia", Model: "20b5", Profile: "GRID A100-3-20C", Count: 2,
			Topology: &AcceleratorTopology{P2PRequired: true, SameNUMA: true},
			Memory:   &AcceleratorMemory{MinimumPerDevice: "10Gi"},
			NIC:      &AcceleratorNIC{Vendor: "mellanox", VirtualFunction: true},
		}, ""},
		{"bad class", MachineAcceleratorProfileSpec{Class: "nic"}, "spec.class"},
		{"bad count", MachineAcceleratorProfileSpec{Count: 17}, "spec.count"},
		{"bad vendor", MachineAcceleratorProfileSpec{Vendor: "acme"}, "spec.vendor"},
		{"bad model", MachineAcceleratorProfileSpec{Model: "20B5x"}, "spec.model"},
		{"bad profile", MachineAcceleratorProfileSpec{Profile: "/x"}, "spec.profile"},
		{"memory on accelerator", MachineAcceleratorProfileSpec{Class: "accelerator", Memory: &AcceleratorMemory{MinimumPerDevice: "1Gi"}}, "only applies to class gpu"},
		{"bad memory", MachineAcceleratorProfileSpec{Memory: &AcceleratorMemory{MinimumPerDevice: "lots"}}, "minimumPerDevice"},
		{"bad nic vendor", MachineAcceleratorProfileSpec{NIC: &AcceleratorNIC{Vendor: "acme"}}, "spec.nic.vendor"},
	}
	for _, tc := range cases {
		err := tc.spec.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestAcceleratorProfileUsable(t *testing.T) {
	if r, _ := (MachineAcceleratorProfileSpec{}).Usable(); r != "" {
		t.Fatalf("plain profile: %s", r)
	}
	if r, _ := (MachineAcceleratorProfileSpec{Class: "x"}).Usable(); r != AcceleratorProfileReasonInvalid {
		t.Fatalf("invalid profile: %q", r)
	}
	if r, _ := (MachineAcceleratorProfileSpec{Migration: &AcceleratorMigration{Required: true}}).Usable(); r != AcceleratorProfileReasonLiveMigrate {
		t.Fatalf("live migration: %q", r)
	}
}

func TestAcceleratorProfileDevices(t *testing.T) {
	spec := MachineAcceleratorProfileSpec{
		Vendor: "nvidia", Count: 2,
		Topology: &AcceleratorTopology{SameNUMA: true},
		Memory:   &AcceleratorMemory{MinimumPerDevice: "40Gi"},
		NIC:      &AcceleratorNIC{Vendor: "mellanox", VirtualFunction: true},
	}
	got := spec.Devices("train")
	want := []MachineDevice{
		{Name: "train", Class: "gpu", Vendor: "nvidia", Count: 2, MinMemory: "40Gi", Alignment: []string{AlignSameNUMA, AlignSamePCIeRoot}},
		{Name: "train-nic", Class: "nic", Vendor: "mellanox", VirtualFunction: true, Alignment: []string{AlignSameNUMA, AlignSamePCIeRoot}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if err := ValidateTopology(MachineSpec{Devices: got}); err != nil {
		t.Fatalf("expanded devices do not validate: %v", err)
	}

	plainPair := MachineAcceleratorProfileSpec{Profile: "mbochs-small", Count: 2}.Devices("g")
	if len(plainPair) != 1 || plainPair[0].Alignment != nil || plainPair[0].Profile != "mbochs-small" {
		t.Fatalf("no topology: %+v", plainPair)
	}
	p2p := MachineAcceleratorProfileSpec{Topology: &AcceleratorTopology{P2PRequired: true}}.Devices("g")
	if !p2p[0].Aligned(AlignSamePCIeRoot) || p2p[0].Aligned(AlignSameNUMA) {
		t.Fatalf("p2pRequired: %+v", p2p)
	}
}

func TestValidateAccelerators(t *testing.T) {
	acc := func(name, profile string) MachineAccelerator { return MachineAccelerator{Name: name, Profile: profile} }
	cases := []struct {
		name string
		spec MachineSpec
		ann  map[string]string
		want string
	}{
		{"none", MachineSpec{}, nil, ""},
		{"two", MachineSpec{Accelerators: []MachineAccelerator{acc("a", "pair"), acc("b", "a100.single")}}, nil, ""},
		{"annotation preset", MachineSpec{Accelerators: []MachineAccelerator{acc("a", "pair")}}, map[string]string{AnnotationAccelerators: "a=pair@1"}, "set by kairon-controller"},
		{"node preset", MachineSpec{NodeName: "n1", Accelerators: []MachineAccelerator{acc("a", "pair")}}, nil, "unset spec.nodeName"},
		{"backend", MachineSpec{Runtime: RuntimeSpec{Backend: "cloud-hypervisor"}, Accelerators: []MachineAccelerator{acc("a", "pair")}}, nil, "qemu backend"},
		{"too many", MachineSpec{Accelerators: []MachineAccelerator{acc("a", "p"), acc("b", "p"), acc("c", "p"), acc("d", "p"), acc("e", "p")}}, nil, "at most 4"},
		{"bad name", MachineSpec{Accelerators: []MachineAccelerator{acc("A", "p")}}, nil, ".name"},
		{"long name", MachineSpec{Accelerators: []MachineAccelerator{acc(strings.Repeat("a", 29), "p")}}, nil, ".name"},
		{"bad profile", MachineSpec{Accelerators: []MachineAccelerator{acc("a", "Pair")}}, nil, ".profile"},
		{"clashes with a device", MachineSpec{Devices: []MachineDevice{{Name: "a-nic", Class: "nic"}}, Accelerators: []MachineAccelerator{acc("a", "p")}}, nil, `already uses "a-nic"`},
		{"duplicate", MachineSpec{Accelerators: []MachineAccelerator{acc("a", "p"), acc("a", "q")}}, nil, "already uses"},
	}
	for _, tc := range cases {
		err := ValidateAccelerators(tc.spec, tc.ann)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestAcceleratorRecord(t *testing.T) {
	v := FormatAcceleratorRecord(map[string]string{"b": "single@2", "a": "pair@7"})
	if v != "a=pair@7,b=single@2" {
		t.Fatalf("record %q", v)
	}
	m := Machine{Metadata: ObjectMeta{Annotations: map[string]string{AnnotationAccelerators: v}}}
	m.Spec.Accelerators = []MachineAccelerator{{Name: "a", Profile: "pair"}}
	if !m.AcceleratorsResolved() {
		t.Fatal("annotated Machine is not resolved")
	}
	if got := AcceleratorProfilesOf(m); !reflect.DeepEqual(got, []string{"pair", "single"}) {
		t.Fatalf("profiles %v", got)
	}
	m.Metadata.Annotations = nil
	if m.AcceleratorsResolved() {
		t.Fatal("Machine without the record is resolved")
	}
}

func TestTopologyUpdateRefusesAcceleratorChangeAfterScheduling(t *testing.T) {
	old := MachineSpec{NodeName: "n1", Accelerators: []MachineAccelerator{{Name: "a", Profile: "p"}}}
	cur := old
	cur.Accelerators = []MachineAccelerator{{Name: "a", Profile: "q"}}
	if err := ValidateTopologyUpdate(old, cur); err == nil || !strings.Contains(err.Error(), "spec.accelerators") {
		t.Fatalf("err %v", err)
	}
}
