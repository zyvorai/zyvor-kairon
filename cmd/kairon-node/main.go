// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zyvorai/kairon/internal/agent"
	"github.com/zyvorai/kairon/internal/bgp"
	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/diskexport"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/health"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/macnode"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/oteltrace"
	"github.com/zyvorai/kairon/internal/tlsreload"
)

var version = "dev"

// tlsReloadInterval bounds how often every TLS hop this binary owns
// (migration mTLS, the VNC console relay) polls its certificate/key files
// for a change -- see internal/tlsreload. Frequent enough to notice a
// rotation well within a typical short-lived certificate's remaining
// lifetime, cheap enough (a stat() call) to not matter at this cadence.
const tlsReloadInterval = 30 * time.Second

func main() {
	os.Exit(run())
}

// osDefault picks a flag default per OS: kairon-node's paths assume a Linux host unless told otherwise.
func osDefault(linux, darwin string) string {
	if runtime.GOOS == "darwin" {
		return darwin
	}
	return linux
}

// macSupport is a path under the user's Application Support folder (used for macOS defaults).
func macSupport(rel string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "kairon", rel)
	}
	return filepath.Join(home, "Library", "Application Support", rel)
}

// macHome is a path under the user's home directory.
func macHome(rel string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "kairon", rel)
	}
	return filepath.Join(home, rel)
}

// run returns the process exit code rather than calling os.Exit directly,
// so every deferred cleanup (e.g. cancel()) actually runs before exit.
func run() int {
	interval := flag.Duration("interval", 5*time.Minute, "safety resync interval (hot-path reconcile is watch-driven)")
	healthAddr := flag.String("health-addr", ":32302", "health server address")
	fluxURL := flag.String("fluxvm-url", env("FLUXVM_URL", "http://127.0.0.1:7788"), "node-local FluxVM URL")
	backend := flag.String("default-backend", env("KAIRON_DEFAULT_BACKEND", osDefault("qemu", "vz")), "backend used when Machine runtime.backend is auto")
	imageRoot := flag.String("image-root", env("KAIRON_IMAGE_ROOT", osDefault("/var/lib/fluxvm/images", macSupport("FluxVM/images"))), "allowed root for Machine image paths")
	exportDir := flag.String("export-dir", env("KAIRON_EXPORT_DIR", osDefault("/var/lib/kairon-node/exports", macSupport("Kairon/exports"))), "directory for MachineExport copies converted to another disk format")
	imageCacheDir := flag.String("image-cache-dir", env("KAIRON_IMAGE_CACHE_DIR", ""), "directory kairon-node downloads/caches spec.image.source images into, keyed by digest -- empty (the default) refuses any Machine that sets spec.image.source; see docs/guides/machine-image-import.md")
	vfioAllowlistRaw := flag.String("vfio-allowlist", env("KAIRON_VFIO_ALLOWLIST", ""), "comma-separated PCI BDFs this node permits for DRA-backed VFIO passthrough")
	mdevParentsRaw := flag.String("mdev-parents", env("KAIRON_MDEV_PARENTS", ""), "comma-separated mediated device parents (PCI BDFs or names such as mbochs) the scheduler may create vGPUs on; empty allows every parent")
	migrationAddr := flag.String("migration-addr", env("KAIRON_MIGRATION_ADDR", ":9443"), "mTLS migration peer listen address")
	migrationCA := flag.String("migration-ca", env("KAIRON_MIGRATION_CA", ""), "CA PEM used to verify Kairon node peers")
	migrationCert := flag.String("migration-cert", env("KAIRON_MIGRATION_CERT", ""), "node migration TLS certificate PEM")
	migrationKey := flag.String("migration-key", env("KAIRON_MIGRATION_KEY", ""), "node migration TLS private key PEM")
	migrationServerName := flag.String("migration-server-name", env("KAIRON_MIGRATION_SERVER_NAME", "kairon-node"), "TLS server name expected from migration peers; empty verifies the peer IP from the URL")
	migrationStateDir := flag.String("migration-state-dir", env("KAIRON_MIGRATION_STATE_DIR", osDefault("/var/run/kairon/migrations", macSupport("Kairon/migrations"))), "destination migration session journal")
	migrationAdapterSocket := flag.String("migration-adapter-socket", env("KAIRON_MIGRATION_ADAPTER_SOCKET", ""), "optional Kairon migration adapter Unix socket")
	migrationPort := flag.Int("migration-port", int(envUint("KAIRON_MIGRATION_PORT", 9443)), "peer migration TCP port this node dials on a target's InternalIP; every node in the cluster must listen on it (--migration-addr)")
	migrationHeartbeatTTL := flag.Duration("migration-heartbeat-ttl", 0, "how long a destination-side Prepared migration session may go without a heartbeat from its source before this node self-aborts it, releasing whatever it reserved -- closes the gap where a source kairon-node crashes or is fenced between a successful Prepare and ever calling Commit/Abort. Zero (the default) disables this entirely: no reaper goroutine, no behavior change from before this existed. A non-zero value should be set comfortably larger than several multiples of --interval (the source heartbeats once per reconcile tick) to tolerate transient blips; the reaper itself checks every TTL/4 (floored at 10s).")
	consoleAddr := flag.String("console-addr", env("KAIRON_NODE_CONSOLE_ADDR", ":8090"), "VNC console relay listen address")
	consoleToken := flag.String("console-token", os.Getenv("KAIRON_NODE_CONSOLE_TOKEN"), "shared bearer token kairon-ui must present for VNC console relay (default: $KAIRON_NODE_CONSOLE_TOKEN); empty disables the console listener")
	consoleTLSCert := flag.String("console-tls-cert", env("KAIRON_NODE_CONSOLE_TLS_CERT", ""), "optional TLS certificate PEM for the console relay listener (server-only TLS -- the shared token already authenticates the caller, so no client cert is needed); must be set together with --console-tls-key")
	consoleTLSKey := flag.String("console-tls-key", env("KAIRON_NODE_CONSOLE_TLS_KEY", ""), "optional TLS private key PEM for the console relay listener; must be set together with --console-tls-cert")
	csiSocket := flag.String("csi-socket", env("KAIRON_CSI_SOCKET", ""), "kairon-csi-node's local Unix socket path (default: $KAIRON_CSI_SOCKET); empty refuses any CSI-backed (network-block) Machine volume with a clear error rather than silently failing -- see docs/guides/machine-storage-csi.md")
	csiStagingDir := flag.String("csi-staging-dir", env("KAIRON_CSI_STAGING_DIR", osDefault("/var/lib/kairon/csi/staging", macSupport("Kairon/csi/staging"))), "per-node directory kairon-node asks kairon-csi-node to stage CSI volumes under")
	edgeBaselinePath := flag.String("edge-baseline-file", env("KAIRON_EDGE_BASELINE_FILE", ""), "node-local snapshot of per-Machine traffic baselines so a restart keeps the warm-up, e.g. /var/lib/kairon/edge-baseline.json; empty keeps them in memory")
	attestCommand := flag.String("attest-guest-command", env("KAIRON_ATTEST_GUEST_COMMAND", ""), "in-guest command that prints a base64 SEV-SNP report or TDX quote for {kind} with hex REPORT_DATA {data}; empty uses the go-sev-guest/go-tdx-guest attest tool")
	csiPublishDir := flag.String("csi-publish-dir", env("KAIRON_CSI_PUBLISH_DIR", osDefault("/var/lib/kairon/csi/publish", macSupport("Kairon/csi/publish"))), "per-node directory kairon-node asks kairon-csi-node to publish (bind-mount) CSI volumes under")
	csiChapSecretNamespace := flag.String("csi-chap-secret-namespace", env("KAIRON_CSI_CHAP_SECRET_NAMESPACE", ""), "when set, allow resolveCSIVolume to read a PV's nodeStageSecretRef for iSCSI CHAP -- but only Secrets in this exact namespace (never cluster-wide); empty (the default) refuses any secret ref. Pair with node.csi.chap.enabled in Helm")
	thirdPartyCSIDriversRaw := flag.String("third-party-csi-drivers", env("KAIRON_THIRD_PARTY_CSI_DRIVERS", ""), "comma-separated driverName=/socket/path list of third-party CSI drivers kairon-node may drive directly for a Machine boot disk (attachRequired drivers go through a VolumeAttachment -- see docs/guides/machine-storage-thirdparty-csi.md); empty (the default) means only Kairon's own driver can be used, exactly as before this existed")
	thirdPartyCSISecretNamespace := flag.String("third-party-csi-secret-namespace", env("KAIRON_THIRD_PARTY_CSI_SECRET_NAMESPACE", ""), "the only namespace a third-party CSI PV's nodeStageSecretRef/nodePublishSecretRef may name; empty (the default) refuses any secret ref")
	livenessLeaseNamespace := flag.String("liveness-lease-namespace", env("KAIRON_NODE_NAMESPACE", ""), "namespace to hold this node's own coordination.k8s.io/v1 liveness Lease in (internal/nodeliveness), renewed once per reconcile tick; empty (the default) disables this entirely -- no Lease writes, no extra RBAC needed, exactly kairon-node's behavior before this existed. Requires the ServiceAccount to be granted 'leases' get/create/update in this namespace; the Helm chart's node.livenessLease.enabled turns both on together. kaironctl fence cross-checks this Lease against Node Ready before proceeding.")
	livenessLeaseDuration := flag.Duration("liveness-lease-duration", 0, "override nodeliveness.DefaultLeaseDuration (60s) when non-zero")
	networkDefaultDeny := flag.Bool("network-default-deny", env("KAIRON_NETWORK_DEFAULT_DENY", "false") == "true", "push DefaultAllow: false onto every Machine on this node not currently matched by any MachineNetworkPolicy/NetworkSecurityGroup-derived policy, instead of silently leaving it on FluxVM's native defaultAllow: true. False (the default) is today's unchanged behavior. Global, not per-namespace: enabling this with no policies written yet cuts all VM-to-VM connectivity on this node outright -- see docs/guides/network-policy.md.")
	atlasRBDPools := flag.String("atlas-rbd-pools", env("KAIRON_ATLAS_RBD_POOLS", ""), "comma-separated Ceph pools this node may boot Atlas rbd-mode volumes from (FluxVM storage=ceph-rbd-in-place, credentials from FluxVM's own config); empty (the default) refuses atlas.mode=rbd -- see docs/guides/machine-storage-atlas.md")
	reservedCPUs := flag.String("reserved-cpus", env("KAIRON_RESERVED_CPUS", ""), "cpuset kept off the discovered kairon.zyvor.dev/pinnable-cpus label (OS, kubelet, kairon-node), e.g. 0-1; empty (the default) publishes no label -- see docs/guides/machine-cpu-pinning.md")
	cpuManagerState := flag.String("cpu-manager-state", env("KAIRON_CPU_MANAGER_STATE", "/var/lib/kubelet/cpu_manager_state"), "kubelet cpu_manager_state file; exclusive pod CPUs listed there are kept off the pinnable set")
	registerNode := flag.Bool("register-node", env("KAIRON_REGISTER_NODE", fmt.Sprint(runtime.GOOS == "darwin")) == "true", "create and heartbeat this host's Kubernetes Node object (a Mac, or a Linux host with no kubelet, has nothing else to do it); on by default on macOS")
	nodeIP := flag.String("node-ip", env("KAIRON_NODE_IP", ""), "InternalIP the registered Node publishes (--register-node), which migration peers dial; empty (the default) is the host's first private IPv4")
	mlxURL := flag.String("mlx-url", env("KAIRON_MLX_URL", ""), "loopback OpenAI-compatible endpoint of this Mac's MLX runtime (Velora); when set the Node gets the kairon.zyvor.dev/mlx=true label and an inference-url annotation")
	tbFabric := flag.String("thunderbolt-fabric", env("KAIRON_THUNDERBOLT_FABRIC", ""), "name of the group of Macs cabled together over Thunderbolt; with a Thunderbolt Bridge address the Node gets kairon.zyvor.dev/thunderbolt-fabric=<name> and MacNetworks with spec.fabric run over it")
	fabricSecretFile := flag.String("mac-fabric-secret-file", env("KAIRON_MAC_FABRIC_SECRET_FILE", ""), "file with the secret every Mac shares, from which MacNetwork keys are derived; empty leaves MacNetworks off on this Mac (docs/guides/mac-fabric.md)")
	overlayConfig := flag.String("mac-overlay-config", env("FLUXVM_VZ_OVERLAY_CONFIG", macHome(".config/fluxvm/overlay.json")), "FluxVM's overlay.json, which kairon-node writes for the MacNetworks this Mac belongs to")
	memoryReclaim := flag.Bool("memory-reclaim", env("KAIRON_MEMORY_RECLAIM", "true") == "true", "balloon spec.resources.elasticMemory Machines down toward their minimum under host memory pressure and set the KaironMemoryPressure Node condition when nothing is left to reclaim (needs a FluxVM with host.balloon) -- see docs/guides/machine-elastic-memory.md")
	memoryLowMiB := flag.Uint64("memory-low-watermark-mib", envUint("KAIRON_MEMORY_LOW_WATERMARK_MIB", 0), "host MemAvailable below this is memory pressure; 0 (the default) is 10% of MemTotal")
	memoryHighMiB := flag.Uint64("memory-high-watermark-mib", envUint("KAIRON_MEMORY_HIGH_WATERMARK_MIB", 0), "reclaim aims for this much MemAvailable and balloons deflate only above it; 0 (the default) is 20% of MemTotal, or twice --memory-low-watermark-mib when that is set")
	memoryPSI := flag.Float64("memory-psi-some-avg10", envFloat("KAIRON_MEMORY_PSI_SOME_AVG10", 0), "host memory PSI some avg10 (percent) above this is memory pressure; 0 (the default) is 10")
	memoryReclaimInterval := flag.Duration("memory-reclaim-interval", 0, "time between reclaim passes; 0 (the default) is 10s")
	pressureReport := flag.Bool("pressure-report", env("KAIRON_PRESSURE_REPORT", "true") == "true", "publish host PSI and each local Machine's own PSI (some avg60, from FluxVM) on this Node's kairon.zyvor.dev/pressure annotation for kairon-controller -drs-signal=pressure; needs a FluxVM with GET /v1/host/pressure -- see docs/guides/node-maintenance-drs.md")
	pressureReportInterval := flag.Duration("pressure-report-interval", agent.DefaultPressureReportInterval, "time between Node pressure annotations")
	vnetSync := flag.Bool("virtual-networks", env("KAIRON_VIRTUAL_NETWORKS", "true") == "true", "apply VirtualNetworks with a Machine on this node to the local FluxVM (needs a FluxVM with host.vnet) -- see docs/guides/virtual-networks.md")
	vnetUplink := flag.String("vnet-uplink", env("KAIRON_VNET_UPLINK", ""), "host interface carrying virtual network egress SNAT and floating IPs; empty keeps virtual networks closed on this node")
	vnetNodeIP := flag.String("vnet-node-ip", env("KAIRON_VNET_NODE_IP", ""), "underlay address other nodes tunnel to and the SNAT address; empty takes the Node's InternalIP")
	bgpPeers := flag.String("bgp-peers", env("KAIRON_BGP_PEERS", ""), "BGP peers to announce floating IPs to, addr=asn[,addr:port=asn]; empty (the default) disables the speaker")
	bgpASN := flag.Uint("bgp-local-asn", uint(envUint("KAIRON_BGP_LOCAL_ASN", 0)), "this node's BGP AS number (required with --bgp-peers)")
	bgpRouterID := flag.String("bgp-router-id", env("KAIRON_BGP_ROUTER_ID", ""), "BGP router ID; empty takes --vnet-node-ip or the Node's InternalIP")
	bgpNextHop := flag.String("bgp-next-hop", env("KAIRON_BGP_NEXT_HOP", ""), "next hop announced for every prefix; empty is the router ID")
	bgpServiceVIPs := flag.Bool("bgp-advertise-service-vips", env("KAIRON_BGP_ADVERTISE_SERVICE_VIPS", "false") == "true", "also announce the Service Fabric VIPs FluxVM marks for advertisement")
	bgpPrefixes := flag.String("bgp-advertise-prefixes", env("KAIRON_BGP_ADVERTISE_PREFIXES", ""), "extra IPv4 prefixes to announce, comma-separated")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	node := os.Getenv("NODE_NAME")
	if node == "" {
		fmt.Fprintln(os.Stderr, "NODE_NAME is required")
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	kc, err := kube.FromEnvironment()
	if err != nil {
		log.Error("kubernetes client", "error", err)
		return 1
	}
	rec := metrics.NewNodeRecorder()
	kc.Observe = rec.ObserveAPIRequest
	fc := fluxvm.New(*fluxURL, os.Getenv("FLUXVM_TOKEN"))
	vfioAllowlist, err := agent.ParseVFIOAllowlist(*vfioAllowlistRaw)
	if err != nil {
		log.Error("invalid VFIO allowlist", "error", err)
		return 2
	}
	thirdPartyCSIDrivers, err := agent.ParseThirdPartyCSIDrivers(*thirdPartyCSIDriversRaw)
	if err != nil {
		log.Error("invalid third-party CSI driver list", "error", err)
		return 2
	}
	if *nodeIP != "" && net.ParseIP(*nodeIP) == nil {
		log.Error("invalid --node-ip", "value", *nodeIP)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if *registerNode {
		discover := func(ctx context.Context) macnode.Info {
			info := macnode.Discover()
			if *nodeIP != "" {
				info.InternalIP = *nodeIP
			}
			info.MLXURL = *mlxURL
			info.ThunderboltFabric = *tbFabric
			if h, err := fc.AppleHost(ctx); err == nil {
				info.Rosetta = h.Rosetta == "installed"
				info.NestedVirtualization = h.NestedVirtualization
				if h.OSVersion != "" {
					info.MacOSVersion = h.OSVersion
				}
			}
			return info
		}
		var sync func(context.Context) error
		if *fabricSecretFile != "" {
			secret, err := os.ReadFile(*fabricSecretFile)
			if err != nil || len(strings.TrimSpace(string(secret))) < 32 {
				log.Error("mac fabric secret: need at least 32 characters", "file", *fabricSecretFile, "error", err)
				return 2
			}
			sync = macnode.OverlaySync{API: kc, Node: node, Path: *overlayConfig, Secret: []byte(strings.TrimSpace(string(secret)))}.Sync
		}
		info := discover(ctx)
		log.Info("registering this host as a Kubernetes Node", "node", node, "arch", info.Arch, "cpus", info.CPUs, "memoryMiB", info.MemoryMiB, "address", info.InternalIP, "chip", info.Chip, "thunderbolt", info.ThunderboltAddresses)
		go macnode.Run(ctx, kc, node, discover, 10*time.Second, sync, func(err error) { log.Error("node registration", "error", err) })
	}

	hs := &health.Server{Metrics: rec.Handler()}
	go func() {
		if err := hs.Run(ctx, *healthAddr); err != nil && err != http.ErrServerClosed {
			log.Error("health server", "error", err)
		}
	}()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		// FluxVM's own /readyz already fail-closes when sandbox.dataplane.required
		// is set, so the node agent's readiness only needs to track it directly.
		for {
			err := fc.Ready(ctx)
			hs.SetReady(err == nil)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	restores := migration.NewRestoreStore()
	restores.Observe = rec.Edge().ObserveConntrackRestore
	peer, source, err := configureMigration(ctx, log, cancel, node, fc, *migrationAddr, *migrationCA, *migrationCert, *migrationKey, *migrationServerName, *migrationStateDir, *migrationAdapterSocket, *migrationHeartbeatTTL, restores)
	if err != nil {
		log.Error("migration control plane", "error", err)
		return 2
	}

	exports := &diskexport.Registry{}
	configureConsole(ctx, log, fc, exports, *consoleAddr, *consoleToken, *consoleTLSCert, *consoleTLSKey)

	vnetCfg, err := configureVirtualNetworks(ctx, log, kc, node, *vnetSync, *vnetUplink, *vnetNodeIP, *bgpPeers, uint32(*bgpASN), *bgpRouterID, *bgpNextHop, *bgpServiceVIPs, *bgpPrefixes)
	if err != nil {
		log.Error("virtual networks", "error", err)
		return 2
	}

	a := &agent.Agent{
		NodeName:                     node,
		Kube:                         kc,
		Flux:                         fc,
		DefaultBackend:               *backend,
		ImageRoot:                    *imageRoot,
		ImageCacheDir:                *imageCacheDir,
		Exports:                      exports,
		ExportDir:                    *exportDir,
		VFIOAllowlist:                vfioAllowlist,
		MdevParents:                  agent.ParseMdevParents(*mdevParentsRaw),
		MigrationPeer:                peer,
		Restores:                     restores,
		SourceMigrator:               source,
		MigrationPort:                *migrationPort,
		CSISocketPath:                *csiSocket,
		CSIStagingDir:                *csiStagingDir,
		CSIPublishDir:                *csiPublishDir,
		EdgeBaselinePath:             *edgeBaselinePath,
		AttestCommand:                *attestCommand,
		CSIChapSecretNamespace:       *csiChapSecretNamespace,
		ThirdPartyCSISecretNamespace: *thirdPartyCSISecretNamespace,
		ThirdPartyCSIDrivers:         thirdPartyCSIDrivers,
		LivenessLeaseNamespace:       *livenessLeaseNamespace,
		LivenessLeaseDuration:        *livenessLeaseDuration,
		NetworkDefaultDeny:           *networkDefaultDeny,
		AtlasRBDPools:                splitList(*atlasRBDPools),
		ReservedCPUs:                 *reservedCPUs,
		CPUManagerState:              *cpuManagerState,
		MemoryReclaim: agent.MemoryReclaimConfig{
			Disabled:         !*memoryReclaim,
			LowWatermarkMiB:  *memoryLowMiB,
			HighWatermarkMiB: *memoryHighMiB,
			PSISomeAvg10:     *memoryPSI,
			Interval:         *memoryReclaimInterval,
		},
		PressureReport: agent.PressureReportConfig{
			Disabled: !*pressureReport,
			Interval: *pressureReportInterval,
		},
		VirtualNetwork: vnetCfg,
		Log:            log,
		Metrics:        rec,
		Tracer:         oteltrace.FromEnv(),
	}
	if err := a.Run(ctx, *interval); err != nil && ctx.Err() == nil {
		log.Error("agent stopped", "error", err)
		return 1
	}
	return 0
}

// configureVirtualNetworks builds the VirtualNetwork sync settings and,
// when peers are configured, the BGP speaker. The router ID falls back to
// the node's underlay address.
func configureVirtualNetworks(ctx context.Context, log *slog.Logger, kc *kube.Client, nodeName string, enabled bool, uplink, nodeIP, peersRaw string, asn uint32, routerID, nextHop string, serviceVIPs bool, prefixesRaw string) (agent.VirtualNetworkConfig, error) {
	cfg := agent.VirtualNetworkConfig{Disabled: !enabled, Uplink: strings.TrimSpace(uplink), NodeIP: strings.TrimSpace(nodeIP), AdvertiseServiceVIPs: serviceVIPs}
	for _, p := range splitList(prefixesRaw) {
		pfx, err := netip.ParsePrefix(p)
		if err != nil || !pfx.Addr().Is4() {
			return cfg, fmt.Errorf("--bgp-advertise-prefixes: %q is not an IPv4 prefix", p)
		}
		cfg.AdvertisePrefixes = append(cfg.AdvertisePrefixes, pfx.Masked())
	}
	peers, err := bgp.ParsePeers(peersRaw)
	if err != nil {
		return cfg, err
	}
	if len(peers) == 0 {
		return cfg, nil
	}
	id := strings.TrimSpace(routerID)
	if id == "" {
		id = cfg.NodeIP
	}
	if id == "" {
		if n, err := kc.GetNode(ctx, nodeName); err == nil {
			id = model.NodeInternalIP(n)
		}
	}
	rid, err := netip.ParseAddr(id)
	if err != nil {
		return cfg, fmt.Errorf("BGP needs an IPv4 router ID: set --bgp-router-id or --vnet-node-ip (got %q)", id)
	}
	var nh netip.Addr
	if strings.TrimSpace(nextHop) != "" {
		if nh, err = netip.ParseAddr(strings.TrimSpace(nextHop)); err != nil {
			return cfg, fmt.Errorf("--bgp-next-hop: %w", err)
		}
	}
	sp, err := bgp.New(bgp.Config{LocalASN: asn, RouterID: rid, NextHop: nh, Peers: peers, Log: log.With("component", "bgp")})
	if err != nil {
		return cfg, err
	}
	cfg.BGP = sp
	log.Info("BGP speaker configured", "asn", asn, "routerID", rid, "peers", len(peers))
	return cfg, nil
}

func configureMigration(ctx context.Context, log *slog.Logger, cancel context.CancelFunc, nodeName string, fc *fluxvm.Client, addr, caPath, certPath, keyPath, serverName, stateDir, adapterSocket string, heartbeatTTL time.Duration, restores *migration.RestoreStore) (*migration.Client, migration.SourceDriver, error) {
	configured := 0
	for _, v := range []string{caPath, certPath, keyPath} {
		if strings.TrimSpace(v) != "" {
			configured++
		}
	}
	if configured == 0 {
		log.Warn("live migration peer control plane disabled; cold migration remains available")
		return nil, nil, nil
	}
	if configured != 3 {
		return nil, nil, fmt.Errorf("migration mTLS is fail-closed: --migration-ca, --migration-cert and --migration-key must be configured together")
	}
	certWatcher, err := migration.NewCertWatcher(log, caPath, certPath, keyPath)
	if err != nil {
		return nil, nil, err
	}
	go certWatcher.Run(ctx, tlsReloadInterval)
	serverTLS, err := migration.ServerTLSConfig(caPath, certWatcher)
	if err != nil {
		return nil, nil, err
	}
	clientTLS, err := migration.ClientTLSConfig(caPath, certWatcher, serverName)
	if err != nil {
		return nil, nil, err
	}

	var destination migration.DestinationDriver = migration.UnsupportedDestinationDriver{Reason: "no Kairon migration adapter is configured; current FluxVM does not expose a verified live-migration API"}
	var source migration.SourceDriver = migration.UnsupportedSourceDriver{Reason: "no Kairon migration adapter is configured; current FluxVM does not expose a verified live-migration API"}
	if restores == nil {
		restores = migration.NewRestoreStore()
	}
	if strings.TrimSpace(adapterSocket) != "" {
		adapter := migration.NewAdapter(adapterSocket)
		destination = migration.NetworkAwareDestination{Inner: adapter, Flux: fc, Restores: restores}
		source = migration.SourceAdapter{Adapter: adapter}
	} else {
		destination = migration.NetworkAwareDestination{Inner: destination, Flux: fc, Restores: restores}
	}

	migServer := &migration.Server{Store: migration.NewFileStore(stateDir), Driver: destination, NodeName: nodeName, HeartbeatTTL: heartbeatTTL}
	server := &http.Server{
		Addr:              addr,
		Handler:           migServer.Handler(),
		TLSConfig:         serverTLS,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for migration peers: %w", err)
	}
	tlsListener := tls.NewListener(listener, serverTLS)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if heartbeatTTL > 0 {
		reapInterval := heartbeatTTL / 4
		if reapInterval < 10*time.Second {
			reapInterval = 10 * time.Second
		}
		go migServer.RunReaper(ctx, reapInterval)
		log.Info("destination migration session reaper enabled", "heartbeatTTL", heartbeatTTL, "reapInterval", reapInterval)
	}
	go func() {
		log.Info("migration mTLS peer listening", "address", addr, "adapterConfigured", strings.TrimSpace(adapterSocket) != "")
		if err := server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
			log.Error("migration peer server stopped", "error", err)
			cancel()
		}
	}()

	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: 20 * time.Second}
	return migration.NewClient(httpClient), source, nil
}

// configureConsole starts the VNC console relay listener (see
// internal/consoleproxy) unless consoleToken is empty, in which case the
// feature is simply off -- same opt-in-via-configuration posture as
// migration, no separate --console-enabled flag needed.
//
// Unlike the migration listener (a startup-time fail-closed check, since
// misconfigured mTLS material should stop the node before it reconciles
// anything), a console bind failure must NOT take the whole node agent
// down with it -- console is a purely optional, add-on capability, and
// e.g. a port already in use on a shared host should just mean "no
// console today," not "no Machine reconciliation either."
func configureConsole(ctx context.Context, log *slog.Logger, fc *fluxvm.Client, exports *diskexport.Registry, addr, token, tlsCertPath, tlsKeyPath string) {
	if strings.TrimSpace(token) == "" {
		log.Warn("VNC console relay disabled; set --console-token/$KAIRON_NODE_CONSOLE_TOKEN to enable")
		return
	}
	var tlsConfig *tls.Config
	certSet, keySet := strings.TrimSpace(tlsCertPath) != "", strings.TrimSpace(tlsKeyPath) != ""
	switch {
	case certSet != keySet:
		log.Error("VNC console relay disabled: --console-tls-cert and --console-tls-key must be set together")
		return
	case certSet && keySet:
		watcher, err := tlsreload.New(log, tlsCertPath, tlsKeyPath)
		if err != nil {
			log.Error("VNC console relay disabled: failed to load TLS keypair", "error", err)
			return
		}
		go watcher.Run(ctx, tlsReloadInterval)
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: watcher.GetCertificate}
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("VNC console relay disabled: failed to bind", "address", addr, "error", err)
		return
	}
	server := &http.Server{
		Handler:           (&consoleproxy.Server{Flux: fc, Token: token, Exports: exports}).Handler(),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout/IdleTimeout: a VNC session is a long-lived
		// streaming connection, not a short request/response cycle.
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		log.Info("VNC console relay listening", "address", addr, "tls", tlsConfig != nil)
		var serveErr error
		if tlsConfig != nil {
			// certFile/keyFile are intentionally empty: the certificate is
			// already loaded into server.TLSConfig above.
			serveErr = server.ServeTLS(listener, "", "")
		} else {
			serveErr = server.Serve(listener)
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Error("VNC console relay stopped", "error", serveErr)
		}
	}()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envUint(k string, d uint64) uint64 {
	if v, err := strconv.ParseUint(os.Getenv(k), 10, 64); err == nil {
		return v
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}
