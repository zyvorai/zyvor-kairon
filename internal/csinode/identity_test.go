// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package csinode

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestGetPluginInfoReportsDriverNameAndVersion(t *testing.T) {
	resp, err := IdentityServer{}.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if resp.GetName() != DriverName {
		t.Fatalf("expected name %q, got %q", DriverName, resp.GetName())
	}
	if resp.GetVendorVersion() == "" {
		t.Fatal("expected a non-empty vendor_version")
	}
}

func TestGetPluginCapabilitiesReportsNoControllerService(t *testing.T) {
	resp, err := IdentityServer{}.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	if len(resp.GetCapabilities()) != 0 {
		t.Fatalf("expected no plugin capabilities (no Controller service in this first cut), got %+v", resp.GetCapabilities())
	}
}

func TestProbeReportsReady(t *testing.T) {
	resp, err := IdentityServer{}.Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !resp.GetReady().GetValue() {
		t.Fatal("expected Probe to report ready")
	}
}
