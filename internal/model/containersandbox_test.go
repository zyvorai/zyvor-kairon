// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func ociSpec(mut func(*MachineSpec)) MachineSpec {
	s := MachineSpec{Sandbox: &SandboxSpec{OCI: &OCISpec{Image: "alpine:3.22"}}}
	if mut != nil {
		mut(&s)
	}
	return s
}

func TestValidateSandboxOCI(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec MachineSpec
		ok   bool
	}{
		{"not a container sandbox", MachineSpec{}, true},
		{"minimal", ociSpec(nil), true},
		{"full", ociSpec(func(s *MachineSpec) {
			o := s.Sandbox.OCI
			o.Platform, o.Restart, o.ExitPolicy = "linux/amd64", "on-failure", "poweroff"
			o.Env = []string{"A=1", "B="}
			o.Ports = []string{"8080:80", "9000", "8443:443/tcp"}
			o.HealthCheck = &ContainerHealthCheck{Command: []string{"wget", "-q", "http://127.0.0.1"}}
			s.Resources.Memory = "1Gi"
		}), true},
		{"missing image", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Image = " " }), false},
		{"image with space", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Image = "alpine 3" }), false},
		{"with template", ociSpec(func(s *MachineSpec) { s.Sandbox.TemplateName = "t" }), false},
		{"with image path", ociSpec(func(s *MachineSpec) { s.Image.Path = "/images/x.qcow2" }), false},
		{"with volumes", ociSpec(func(s *MachineSpec) { s.Volumes = []MachineVolume{{ClaimName: "c"}} }), false},
		{"bad platform", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Platform = "windows/amd64" }), false},
		{"bad restart", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Restart = "sometimes" }), false},
		{"bad exit policy", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.ExitPolicy = "explode" }), false},
		{"bad env", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Env = []string{"NOEQUALS"} }), false},
		{"privileged host port", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Ports = []string{"80:80"} }), false},
		{"udp port", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Ports = []string{"5000:53/udp"} }), false},
		{"non numeric port", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Ports = []string{"http"} }), false},
		{"ports with offline", ociSpec(func(s *MachineSpec) {
			s.Sandbox.OCI.Ports, s.Sandbox.OCI.Offline = []string{"8080:80"}, true
		}), false},
		{"ports with allowHosts", ociSpec(func(s *MachineSpec) {
			s.Sandbox.OCI.Ports, s.Sandbox.OCI.AllowHosts = []string{"8080:80"}, []string{"example.com"}
		}), false},
		{"empty health command", ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.HealthCheck = &ContainerHealthCheck{} }), false},
		{"memory below 512Mi", ociSpec(func(s *MachineSpec) { s.Resources.Memory = "256Mi" }), false},
	} {
		if err := ValidateSandboxOCI(tc.spec); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestValidateSandboxOCIUpdateIsImmutable(t *testing.T) {
	old := ociSpec(nil)
	same := ociSpec(nil)
	if err := ValidateSandboxOCIUpdate(old, same); err != nil {
		t.Fatalf("unchanged spec rejected: %v", err)
	}
	changed := ociSpec(func(s *MachineSpec) { s.Sandbox.OCI.Image = "alpine:3.21" })
	if err := ValidateSandboxOCIUpdate(old, changed); err == nil {
		t.Fatal("changing the image must be rejected")
	}
	if err := ValidateSandboxOCIUpdate(MachineSpec{}, old); err == nil {
		t.Fatal("adding spec.sandbox.oci to an existing Machine must be rejected")
	}
	if err := ValidateSandboxOCIUpdate(old, MachineSpec{}); err == nil {
		t.Fatal("removing spec.sandbox.oci must be rejected")
	}
}
