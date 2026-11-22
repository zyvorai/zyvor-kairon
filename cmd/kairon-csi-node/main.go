// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// kairon-csi-node serves Kairon's first-cut CSI Node service (see
// internal/csinode) over gRPC on a Unix socket -- the standard CSI
// convention, discovered by kubelet via the upstream
// csi-node-driver-registrar sidecar this binary is always deployed
// alongside (see charts/kairon/templates/all.yaml's kairon-csi-node
// DaemonSet). Runs as a privileged DaemonSet: logging in to iSCSI
// targets and mounting block devices both require it.
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
	nodeID := flag.String("node-id", env("KAIRON_NODE_NAME", ""), "this Kubernetes Node's name, reported via NodeGetInfo (default: $KAIRON_NODE_NAME, set via the downward API)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if *nodeID == "" {
		log.Error("startup refused", "reason", "-node-id (or $KAIRON_NODE_NAME, set via the downward API fieldRef spec.nodeName) is required -- NodeGetInfo has nothing meaningful to report otherwise")
		return 1
	}

	socketPath, err := unixSocketPath(*endpoint)
	if err != nil {
		log.Error("invalid -endpoint", "endpoint", *endpoint, "error", err)
		return 1
	}
	// Idempotent restart: a prior instance's socket file surviving a
	// crash-restart would otherwise make the new listener fail with
	// "address already in use" even though nothing is actually listening
	// anymore.
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
	// in (kubelet, the registrar sidecar, and -- for Kairon's own
	// Machine boot-disk path -- kairon-node itself, see
	// internal/agent/storage.go). Filesystem permissions on that
	// directory are the real access control here, the same posture every
	// other CSI driver in the ecosystem takes.
	grpcServer := grpc.NewServer()
	grpc_csi.RegisterIdentityServer(grpcServer, csinode.IdentityServer{})
	grpc_csi.RegisterNodeServer(grpcServer, csinode.NewNodeServer(*nodeID, csinode.NewCommandRunner()))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	log.Info("kairon-csi-node listening", "endpoint", *endpoint, "nodeID", *nodeID, "driver", csinode.DriverName, "version", version)
	if err := grpcServer.Serve(listener); err != nil {
		log.Error("grpc serve", "error", err)
		return 1
	}
	return 0
}

// unixSocketPath validates that endpoint is a unix:// URL (the only
// scheme this binary -- and every other CSI node plugin -- ever actually
// uses; kubelet only ever dials node plugins over a local Unix socket)
// and returns the plain filesystem path net.Listen("unix", ...) needs.
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
