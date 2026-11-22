// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package csinode

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// DriverVersion is reported via GetPluginInfo -- opaque to the CO
// (Container Orchestrator), bumped by hand when this driver's behavior
// changes in a way worth being able to tell apart in logs/support.
const DriverVersion = "0.1.0"

// IdentityServer implements the CSI Identity service -- required by every
// CSI plugin regardless of which optional services (Controller, Node) it
// also implements. kairon-csi-node and kairon-csi-controller each run
// their own IdentityServer on their own gRPC endpoint (a Node DaemonSet
// pod and a Controller Deployment pod are different processes, different
// sockets) -- ControllerServiceSupported is how each one accurately
// reports which services live behind *its* socket specifically.
type IdentityServer struct {
	csi.UnimplementedIdentityServer

	// ControllerServiceSupported is true only for kairon-csi-controller's
	// IdentityServer instance (see cmd/kairon-csi-controller/main.go) --
	// the external-provisioner sidecar checks this via
	// GetPluginCapabilities before ever calling CreateVolume/DeleteVolume
	// on that same endpoint. kairon-csi-node's IdentityServer{} zero
	// value leaves this false, unchanged from before the Controller
	// service existed -- it has never implemented Controller and still
	// doesn't.
	ControllerServiceSupported bool
}

func (s IdentityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: DriverVersion}, nil
}

// GetPluginCapabilities reports the Controller service capability only
// when ControllerServiceSupported -- see its doc comment.
func (s IdentityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	if !s.ControllerServiceSupported {
		return &csi.GetPluginCapabilitiesResponse{}, nil
	}
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
			}}},
		},
	}, nil
}

func (IdentityServer) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
