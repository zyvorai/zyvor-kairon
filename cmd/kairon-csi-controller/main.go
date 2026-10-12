// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// kairon-csi-controller serves Kairon's first-cut CSI Controller service
// (see internal/csinode) over gRPC on a Unix socket -- dynamic
// provisioning for the same iSCSI backend kairon-csi-node already
// consumes. Discovered by the standard upstream external-provisioner
// sidecar this binary is always deployed alongside (see
// charts/kairon/templates/all.yaml's kairon-csi-controller Deployment),
// which is what actually watches PersistentVolumeClaims naming this
// driver's StorageClass and turns them into CreateVolume/DeleteVolume
// calls -- this binary itself never talks to the Kubernetes API server
// directly. Runs as a single replica on whichever node is configured as
// this cluster's storage node (see -portal): unlike kairon-csi-node (a
// DaemonSet, one per node, no single-writer concern), more than one
// Controller replica racing to create/delete the same LIO target would be
// a real problem, and this first cut has no leader election of its own to
// prevent that -- see docs/guides/machine-storage-csi.md's real limits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	grpc_csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"github.com/zyvorai/kairon/internal/csinode"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	endpoint := flag.String("endpoint", env("CSI_ENDPOINT", "unix:///csi/csi.sock"), "CSI gRPC endpoint (unix:// only)")
	portal := flag.String("portal", env("KAIRON_CSI_PORTAL", ""), "host:port initiators dial to reach volumes this Controller provisions -- this Controller's own node's real, cluster-reachable IP (or a stable VIP in front of it), never 0.0.0.0 (default: $KAIRON_CSI_PORTAL)")
	volumeDir := flag.String("volume-dir", env("KAIRON_CSI_VOLUME_DIR", "/var/lib/kairon/csi-volumes"), "local directory each dynamically provisioned volume's backing sparse file is created under (default: $KAIRON_CSI_VOLUME_DIR)")
	nvmePortals := flag.String("nvme-portals", env("KAIRON_CSI_NVME_PORTALS", ""), "comma-separated tcp:IP:PORT / rdma:IP:PORT addresses NVMe-oF volumes (StorageClass transport: nvme-tcp or nvme-rdma) are exported on; several of one transport are several paths to every volume. Empty disables NVMe-oF (default: $KAIRON_CSI_NVME_PORTALS)")
	nvmetRoot := flag.String("nvmet-root", env("KAIRON_CSI_NVMET_ROOT", "/sys/kernel/config/nvmet"), "the kernel NVMe target's configfs directory (default: $KAIRON_CSI_NVMET_ROOT)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	socketPath, err := unixSocketPath(*endpoint)
	if err != nil {
		log.Error("invalid -endpoint", "endpoint", *endpoint, "error", err)
		return 1
	}
	controllerServer, err := csinode.NewControllerServer(csinode.NewCommandRunner(), *portal, *volumeDir)
	if err != nil {
		log.Error("startup refused", "reason", err.Error())
		return 1
	}
	if *nvmePortals != "" {
		portals, err := csinode.ParseNVMePortals(*nvmePortals)
		if err == nil {
			err = controllerServer.EnableNVMe(portals, *nvmetRoot)
		}
		if err != nil {
			log.Error("startup refused", "reason", err.Error())
			return 1
		}
	}
	// Idempotent restart: a prior instance's socket file surviving a
	// crash-restart would otherwise make the new listener fail with
	// "address already in use" even though nothing is actually listening
	// anymore -- same reasoning as kairon-csi-node's own main.go.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("removing stale socket", "path", socketPath, "error", err)
		return 1
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Error("listen", "path", socketPath, "error", err)
		return 1
	}

	// No TLS: this socket is local-only by CSI convention, reachable only
	// by whatever shares the hostPath-mounted plugin directory it lives
	// in (the external-provisioner sidecar, in this Controller's own
	// Pod) -- same posture as kairon-csi-node's own socket.
	grpcServer := grpc.NewServer()
	grpc_csi.RegisterIdentityServer(grpcServer, csinode.IdentityServer{ControllerServiceSupported: true})
	grpc_csi.RegisterControllerServer(grpcServer, controllerServer)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	log.Info("kairon-csi-controller listening", "endpoint", *endpoint, "portal", *portal, "nvmePortals", *nvmePortals, "volumeDir", *volumeDir, "driver", csinode.DriverName, "version", version)
	if err := grpcServer.Serve(listener); err != nil {
		log.Error("grpc serve", "error", err)
		return 1
	}
	return 0
}

// unixSocketPath validates that endpoint is a unix:// URL -- kubernetes'
// external-provisioner sidecar, like kubelet dialing a Node plugin, only
// ever uses a local Unix socket. Duplicated from cmd/kairon-csi-node's
// own copy rather than shared: these are different binaries' main
// packages, the same small-helper-duplication convention this project
// already uses elsewhere (e.g. kairon-ui/kairon-node's own relay/
// nodeInternalIP helpers).
func unixSocketPath(endpoint string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(endpoint, prefix) {
		return "", fmt.Errorf("endpoint %q must start with %q", endpoint, prefix)
	}
	return strings.TrimPrefix(endpoint, prefix), nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
