// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func macMachine() model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "mac-vm", Namespace: "dev"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/debian-arm64.raw"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:   model.RuntimeSpec{Backend: "vz"},
			CloudInit: model.CloudInitSpec{Hostname: "mac-vm", User: "velora"},
		},
	}
}

func TestAppleBackendPassesThroughAPlainMachine(t *testing.T) {
	req, err := buildCreateRequest(macMachine(), "qemu", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Backend != "vz" || req.VCPUs != 2 || req.MemoryMiB != 2048 || req.CloudInit == nil || req.CloudInit.Hostname != "mac-vm" {
		t.Fatalf("req = %+v", req)
	}
	if mode := req.Network["mode"]; mode != "user" {
		t.Fatalf("network mode = %v, want the NAT default", mode)
	}
}

func TestAppleBackendIsTheNodeDefaultWhenBackendIsAuto(t *testing.T) {
	m := macMachine()
	m.Spec.Runtime.Backend = "auto"
	req, err := buildCreateRequest(m, "vz", nil, nil)
	if err != nil || req.Backend != "vz" {
		t.Fatalf("req = %+v err = %v", req, err)
	}
}

func TestAppleBackendRejectsLinuxOnlyFeaturesWithAClearMessage(t *testing.T) {
	cases := map[string]struct {
		mutate func(*model.Machine)
		want   string
	}{
		"tap network": {func(m *model.Machine) { m.Spec.Network.Mode = "tap" }, "NAT only"},
		"forwards":    {func(m *model.Machine) { m.Spec.Network.Forwards = []model.PortForward{{HostPort: 2222, GuestPort: 22}} }, "port forwards"},
		"cpu pinning": {func(m *model.Machine) { m.Spec.Resources.CPUPinning = true }, "cpuPinning"},
		"cpu hotplug": {func(m *model.Machine) { m.Spec.Resources.MaxCPU = "4" }, "CPU hotplug"},
		"mem hotplug": {func(m *model.Machine) { m.Spec.Resources.MaxMemory = "8Gi" }, "memory hotplug"},
		"qga":         {func(m *model.Machine) { m.Spec.GuestAgent.Enabled = true }, "QEMU guest-agent"},
		"hugepages":   {func(m *model.Machine) { m.Spec.Resources.Hugepages = true }, "qemu backend"},
		"secure boot": {func(m *model.Machine) { m.Spec.Security.SecureBoot = true }, "qemu backend"},
		"tpm":         {func(m *model.Machine) { m.Spec.Security.TPM = true }, "qemu or cloud-hypervisor"},
	}
	for name, c := range cases {
		m := macMachine()
		c.mutate(&m)
		if _, err := buildCreateRequest(m, "qemu", nil, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
	if _, err := buildCreateRequest(macMachine(), "qemu", []string{"0000:01:00.0"}, nil); err == nil || !strings.Contains(err.Error(), "passthrough") {
		t.Errorf("vfio: err = %v", err)
	}
}

func TestAppleSpecBecomesFluxVMsAppleBlock(t *testing.T) {
	m := macMachine()
	m.Spec.Apple = &model.AppleSpec{Rosetta: true, NestedVirtualization: true, MinChipGeneration: 3, Networks: []string{"shard-net"}}
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := req.Apple; a == nil || !a.Rosetta || !a.NestedVirtualization || len(a.Networks) != 1 || a.Networks[0].Name != model.MacNetworkName("dev", "shard-net") {
		t.Fatalf("apple = %+v", req.Apple)
	}
	m.Spec.Apple = &model.AppleSpec{MinChipGeneration: 3}
	if req, _ := buildCreateRequest(m, "qemu", nil, nil); req.Apple != nil {
		t.Fatalf("placement-only spec.apple sent %+v", req.Apple)
	}
	m.Spec.Runtime.Backend = "qemu"
	if _, err := buildCreateRequest(m, "qemu", nil, nil); err == nil || !strings.Contains(err.Error(), "vz backend") {
		t.Fatalf("spec.apple on qemu: err = %v", err)
	}
}

func TestAppleCapabilityTableMatchesWhatTheCheckEnforces(t *testing.T) {
	var supported, unsupported int
	for _, c := range AppleCapabilities {
		if c.Supported {
			supported++
		} else {
			unsupported++
		}
	}
	if supported == 0 || unsupported == 0 {
		t.Fatal("the table should name both what works and what does not")
	}
}
