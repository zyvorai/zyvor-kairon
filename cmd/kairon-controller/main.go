// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	atlas "github.com/zyvorai/atlas/clients/go"

	"github.com/zyvorai/kairon/internal/agentplane/cosign"
	"github.com/zyvorai/kairon/internal/attest"
	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/health"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/leaderelection"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/oteltrace"
	"github.com/zyvorai/kairon/internal/scheduler"
	"github.com/zyvorai/kairon/internal/tlsreload"
)

var version = "dev"

// tlsReloadInterval bounds how often the webhook's TLS certificate/key
// files are polled for a change -- see internal/tlsreload.
const tlsReloadInterval = 30 * time.Second

func main() {
	os.Exit(run())
}

// run returns the process exit code rather than calling os.Exit directly,
// so every deferred cleanup (e.g. cancel()) actually runs before exit.
func run() int {
	fleetEnabled := flag.Bool("fleet-enabled", envBool("KAIRON_FLEET_ENABLED", false), "enable declarative enterprise fleet automation")
	fleetNamespace := flag.String("fleet-control-namespace", envDefault("KAIRON_FLEET_CONTROL_NAMESPACE", "kairon-system"), "namespace for trusted HA profiles and BMC credentials")
	interval := flag.Duration("interval", 5*time.Second, "reconciliation interval")
	healthAddr := flag.String("health-addr", ":32301", "health server address")
	requireLabel := flag.Bool("require-capable-label", true, "only schedule onto nodes labeled kairon.zyvor.dev/capable=true")
	maxPerNode := flag.Int("migration-max-concurrent-per-node", 0, "max concurrent non-terminal migrations touching a single node (0 = unlimited)")
	maxCluster := flag.Int("migration-max-concurrent-cluster", 0, "max concurrent non-terminal migrations cluster-wide (0 = unlimited)")
	cordonEvacuate := flag.Bool("cordon-evacuate", false, "automatically create a MachineMigration for every Machine on a Node whose spec.unschedulable transitions to true (see internal/controller/cordon.go) -- off by default, since unlike -leader-elect this is never a no-op: enabling it means a cordoned node's Machines start migrating on their own")
	cordonEvacuateStrategy := flag.String("cordon-evacuate-strategy", "cold", "migration strategy used by -cordon-evacuate: cold|auto (never live -- see CordonEvacuation's own doc comment)")
	cordonEvacuateMinRetryInterval := flag.Duration("cordon-evacuate-min-retry-interval", controller.DefaultCordonEvacuateMinRetryInterval, "minimum time between -cordon-evacuate retry attempts for a Machine currently blocked by a MachineDisruptionBudget")
	memoryEvacuate := flag.Bool("memory-pressure-evacuate", envBool("KAIRON_MEMORY_PRESSURE_EVACUATE", false), "migrate Machines off a Node whose KaironMemoryPressure condition (set by kairon-node when ballooning cannot relieve host memory) stays True for -memory-pressure-evacuate-grace, one Machine per node at a time, respecting MachineDisruptionBudgets (see internal/controller/memoryevac.go); off by default")
	memoryEvacuateStrategy := flag.String("memory-pressure-evacuate-strategy", "live", "migration strategy used by -memory-pressure-evacuate: live|auto|cold; with live, Machines with passthrough devices are skipped")
	memoryEvacuateGrace := flag.Duration("memory-pressure-evacuate-grace", controller.DefaultMemoryEvacuateGrace, "how long KaironMemoryPressure must stay True before -memory-pressure-evacuate starts a migration")
	memoryEvacuateMinRetryInterval := flag.Duration("memory-pressure-evacuate-min-retry-interval", controller.DefaultMemoryEvacuateMinRetryInterval, "minimum time between -memory-pressure-evacuate attempts for the same Machine")
	nodeMaintenance := flag.Bool("node-maintenance", envBool("KAIRON_NODE_MAINTENANCE", true), "act on Nodes annotated kairon.zyvor.dev/maintenance=true: cordon, migrate their Machines away, park the ones with no target per kairon.zyvor.dev/maintenance-fallback, and undo it when the annotation is removed (see internal/controller/maintenance.go)")
	nodeMaintenanceMaxParallel := flag.Int("node-maintenance-max-parallel", controller.DefaultMaintenanceMaxParallel, "most in-flight maintenance migrations per node")
	nodeMaintenanceMinRetryInterval := flag.Duration("node-maintenance-min-retry-interval", controller.DefaultMaintenanceMinRetryInterval, "minimum time between maintenance migration attempts for the same Machine")
	drsMode := flag.String("drs-mode", envDefault("KAIRON_DRS_MODE", controller.DRSModeOff), "rebalance Machines across nodes by reserved CPU/memory: off|recommend|automatic (see internal/controller/drs.go); recommend only records Events and metrics")
	drsInterval := flag.Duration("drs-interval", controller.DefaultDRSInterval, "how often DRS plans a rebalancing round")
	drsThreshold := flag.Int("drs-threshold-percent", scheduler.DefaultRebalanceThresholdPercent, "imbalance, in percentage points between the most and least utilized node, at which DRS plans moves")
	drsMinImprovement := flag.Int("drs-min-improvement-percent", scheduler.DefaultRebalanceMinImprovementPercent, "how many points a single move must lower the imbalance by to be planned")
	drsMaxMoves := flag.Int("drs-max-moves", scheduler.DefaultRebalanceMaxMoves, "most moves DRS plans per round")
	drsStrategy := flag.String("drs-strategy", "live", "migration strategy for DRS moves: live|auto|cold; with live only Machines live migration can move are considered")
	drsCooldown := flag.Duration("drs-cooldown", controller.DefaultDRSCooldown, "how long DRS leaves a Machine it moved in place")
	staleEvacuation := flag.Bool("stale-evacuation", envBool("KAIRON_STALE_EVACUATION", false), "fence Machines annotated kairon.zyvor.dev/evacuate=true off NotReady nodes annotated kairon.zyvor.dev/node-fenced, so the scheduler places them again (see internal/controller/staleevac.go); off by default")
	staleEvacuationMaxPerTick := flag.Int("stale-evacuation-max-per-tick", controller.DefaultStaleEvacuationMaxPerTick, "most Machines -stale-evacuation fences per reconcile tick")
	ciliumAttach := flag.Bool("cilium-attach", envBool("KAIRON_CILIUM_ATTACH", false), "reconcile CiliumExternalWorkload for Machines with spec.network.ciliumAttach (requires Cilium CNI; off by default)")
	ciliumPolicySync := flag.Bool("cilium-policy-sync", envBool("KAIRON_CILIUM_POLICY_SYNC", false), "sync MachineNetworkPolicy with spec.cilium.sync onto CiliumNetworkPolicy CRs (off by default)")
	nodeLivenessLeaseNamespace := flag.String("node-liveness-lease-namespace", os.Getenv("KAIRON_NODE_LIVENESS_LEASE_NAMESPACE"), "when set, feed kairon-node liveness Leases in this namespace into NodeUnreachable detection (Ready + stale lease → unreachable); empty keeps Node-Ready-only detection. Pair with node.livenessLease.enabled on kairon-node")
	webhookAddr := flag.String("webhook-addr", ":8443", "validating admission webhook listen address (see -webhook-tls-cert/-key)")
	webhookTLSCert := flag.String("webhook-tls-cert", "", "TLS certificate PEM for the admission webhook; must be set together with -webhook-tls-key. Empty (the default) disables the webhook -- MachineQuota/MachineDisruptionBudget enforcement stays reconcile-loop/kaironctl-only, same as before this flag existed")
	webhookTLSKey := flag.String("webhook-tls-key", "", "TLS private key PEM for the admission webhook; must be set together with -webhook-tls-cert")
	leaderElect := flag.Bool("leader-elect", false, "coordinate multiple kairon-controller replicas via a coordination.k8s.io/v1 Lease (see internal/leaderelection) so only the elected leader reconciles -- the admission webhook and health/metrics server are unaffected and always serve from every replica. Off by default for backward compatibility: turning it on requires the ServiceAccount to be granted the small, namespaced leases RBAC this needs, and -leader-elect-namespace (or KAIRON_CONTROLLER_NAMESPACE) to be set -- the Helm chart's controller.leaderElection.enabled turns both on together. Safe to enable even with a single replica; required once controller.replicaCount > 1")
	leaderElectNamespace := flag.String("leader-elect-namespace", os.Getenv("KAIRON_CONTROLLER_NAMESPACE"), "namespace holding the leader-election Lease object; required when -leader-elect is set (defaults to KAIRON_CONTROLLER_NAMESPACE)")
	leaderElectLeaseName := flag.String("leader-elect-lease-name", "kairon-controller", "name of the leader-election Lease object")
	atlasURL := flag.String("atlas-url", os.Getenv("KAIRON_ATLAS_URL"), "Atlas storage gateway base URL (e.g. http://atlas.atlas-system:5110); enables spec.volumes[].atlas provisioning. Empty disables it")
	atlasTokenFile := flag.String("atlas-token-file", os.Getenv("KAIRON_ATLAS_TOKEN_FILE"), "file holding the Atlas bearer token (falls back to KAIRON_ATLAS_TOKEN)")
	atlasTenant := flag.String("atlas-tenant", envDefault("KAIRON_ATLAS_TENANT", "kairon"), "Atlas tenant_id for volumes Kairon creates")
	atlasPolicy := flag.String("atlas-default-policy", os.Getenv("KAIRON_ATLAS_DEFAULT_POLICY"), "Atlas policy intent used when spec.volumes[].atlas.policy is empty")
	atlasBackupBucket := flag.String("atlas-backup-bucket", os.Getenv("KAIRON_ATLAS_BACKUP_BUCKET"), "Atlas S3 bucket id a MachineBackup uses when spec.atlas.bucketID is empty")
	cosignKeyFile := flag.String("cosign-public-key", os.Getenv("KAIRON_COSIGN_PUBLIC_KEY_FILE"), "PEM public key (cosign.pub); when set, the webhook verifies the cosign signature of every kairon.zyvor.dev/agent-pool Machine's OCI image against it (falls back to the PEM in KAIRON_COSIGN_PUBLIC_KEY)")
	attestVerify := flag.Bool("attestation-verify", envDefault("KAIRON_ATTESTATION_VERIFY", "true") == "true", "verify SEV-SNP reports (AMD KDS chain) and TDX quotes for Machines requesting confidential compute; false leaves them unsealed")
	attestAllowDebug := flag.Bool("attestation-allow-debug", false, "accept debug-enabled confidential guests (lab only: the host can read their memory)")
	attestWriters := flag.String("attestation-writers", os.Getenv("KAIRON_ATTESTATION_WRITERS"), "comma-separated usernames the webhook lets set attestation-verified/attestation-nonce; empty allows any kairon-controller service account")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if (*webhookTLSCert == "") != (*webhookTLSKey == "") {
		log.Error("secure startup refused", "reason", "-webhook-tls-cert and -webhook-tls-key must be set together")
		return 1
	}
	if err := controller.ValidateDRSMode(*drsMode); err != nil {
		log.Error("secure startup refused", "reason", err.Error())
		return 1
	}
	switch *drsStrategy {
	case "live", "auto", "cold":
	default:
		log.Error("secure startup refused", "reason", fmt.Sprintf("-drs-strategy %q: want live|auto|cold", *drsStrategy))
		return 1
	}
	if *leaderElect && *leaderElectNamespace == "" {
		log.Error("secure startup refused", "reason", "-leader-elect requires -leader-elect-namespace (or KAIRON_CONTROLLER_NAMESPACE) to be set")
		return 1
	}
	var webhookTLSConfig *tls.Config
	var webhookCertWatcher *tlsreload.Watcher
	if *webhookTLSCert != "" {
		var err error
		webhookTLSConfig, webhookCertWatcher, err = controller.WebhookTLSConfig(log, *webhookTLSCert, *webhookTLSKey)
		if err != nil {
			log.Error("webhook TLS", "error", err)
			return 1
		}
	}
	if *fleetEnabled && webhookTLSConfig == nil {
		log.Error("fleet automation requires admission webhook TLS configuration")
		return 1
	}
	atlasCfg, err := atlasConfig(*atlasURL, *atlasTokenFile, *atlasTenant, *atlasPolicy)
	if err != nil {
		log.Error("atlas client", "error", err)
		return 1
	}
	atlasCfg.BackupBucketID = *atlasBackupBucket
	cosignVerifier, err := cosignConfig(*cosignKeyFile)
	if err != nil {
		log.Error("cosign public key", "error", err)
		return 1
	}
	kc, err := kube.FromEnvironment()
	if err != nil {
		log.Error("kubernetes client", "error", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	rec := metrics.NewRecorder()
	kc.Observe = rec.ObserveAPIRequest
	hs := &health.Server{Metrics: rec.Handler()}
	go func() {
		if err := hs.Run(ctx, *healthAddr); err != nil && err != http.ErrServerClosed {
			log.Error("health server", "error", err)
		}
	}()
	ctl := &controller.Controller{
		Kube:                 kc,
		Scheduler:            scheduler.Scheduler{RequireCapableLabel: *requireLabel},
		Log:                  log,
		Metrics:              rec,
		MaxConcurrentPerNode: *maxPerNode,
		MaxConcurrentCluster: *maxCluster,
		CordonEvacuation: controller.CordonEvacuation{
			Enabled:          *cordonEvacuate,
			Strategy:         *cordonEvacuateStrategy,
			MinRetryInterval: *cordonEvacuateMinRetryInterval,
		},
		MemoryEvacuation: controller.MemoryPressureEvacuation{
			Enabled:          *memoryEvacuate,
			Strategy:         *memoryEvacuateStrategy,
			Grace:            *memoryEvacuateGrace,
			MinRetryInterval: *memoryEvacuateMinRetryInterval,
		},
		StaleEvacuation: controller.StaleEvacuation{
			Enabled:    *staleEvacuation,
			MaxPerTick: *staleEvacuationMaxPerTick,
		},
		Maintenance: controller.NodeMaintenance{
			Enabled:          *nodeMaintenance,
			MaxParallel:      *nodeMaintenanceMaxParallel,
			MinRetryInterval: *nodeMaintenanceMinRetryInterval,
		},
		DRS: controller.DRS{
			Mode:                  *drsMode,
			Interval:              *drsInterval,
			ThresholdPercent:      *drsThreshold,
			MinImprovementPercent: *drsMinImprovement,
			MaxMoves:              *drsMaxMoves,
			Strategy:              *drsStrategy,
			Cooldown:              *drsCooldown,
		},
		CiliumAttach:               *ciliumAttach,
		CiliumPolicySync:           *ciliumPolicySync,
		NodeLivenessLeaseNamespace: *nodeLivenessLeaseNamespace,
		Tracer:                     oteltrace.FromEnv(),
		Atlas:                      atlasCfg,
	}
	if *fleetEnabled {
		ctl.Fleet = &fleet.Engine{Kube: kc, Scheduler: ctl.Scheduler, Log: log, ControlNamespace: *fleetNamespace, RedfishOrigins: fleet.RedfishOrigins(os.Getenv("KAIRON_FLEET_REDFISH_ORIGINS")), IsolatedBridges: fleet.RedfishOrigins(os.Getenv("KAIRON_FLEET_TEST_BRIDGES")), Metrics: &fleet.Prometheus{URL: os.Getenv("KAIRON_FLEET_PROMETHEUS_URL"), Token: os.Getenv("KAIRON_FLEET_PROMETHEUS_TOKEN")}}
	}
	if cosignVerifier != nil {
		ctl.Cosign = cosignVerifier
	} else {
		log.Warn("no cosign public key; agent-pool image signatures are shape-checked only")
	}
	if *attestVerify {
		ctl.Attest = &attest.Verifier{AllowDebug: *attestAllowDebug}
	}
	for _, w := range strings.Split(*attestWriters, ",") {
		if w = strings.TrimSpace(w); w != "" {
			ctl.AttestationWriters = append(ctl.AttestationWriters, w)
		}
	}
	if webhookTLSConfig != nil {
		go func() {
			if err := ctl.RunWebhook(ctx, *webhookAddr, webhookTLSConfig, webhookCertWatcher, tlsReloadInterval); err != nil && ctx.Err() == nil {
				log.Error("webhook server", "error", err)
			}
		}()
		log.Info("admission webhook listening", "address", *webhookAddr)
	}
	hs.SetReady(true)
	runReconcile := func(runCtx context.Context) {
		if err := ctl.Run(runCtx, *interval); err != nil && runCtx.Err() == nil {
			log.Error("controller stopped", "error", err)
		}
	}
	if !*leaderElect {
		runReconcile(ctx)
		return 0
	}
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = "unknown"
	}
	elector := &leaderelection.Elector{
		Kube:      kc,
		Namespace: *leaderElectNamespace,
		Name:      *leaderElectLeaseName,
		Identity:  fmt.Sprintf("%s_%d", identity, os.Getpid()),
		Log:       log,
	}
	elector.Run(ctx, runReconcile)
	return 0
}

// cosignConfig returns nil when no key is configured, which keeps the
// webhook's annotation shape check as the only image rule.
func cosignConfig(keyFile string) (*cosign.Verifier, error) {
	var pemData []byte
	switch {
	case keyFile != "":
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		pemData = b
	case strings.TrimSpace(os.Getenv("KAIRON_COSIGN_PUBLIC_KEY")) != "":
		pemData = []byte(os.Getenv("KAIRON_COSIGN_PUBLIC_KEY"))
	default:
		return nil, nil
	}
	key, err := cosign.LoadPublicKey(pemData)
	if err != nil {
		return nil, err
	}
	return &cosign.Verifier{Key: key}, nil
}

func atlasConfig(baseURL, tokenFile, tenant, policy string) (controller.AtlasConfig, error) {
	cfg := controller.AtlasConfig{TenantID: tenant, DefaultPolicy: policy}
	if strings.TrimSpace(baseURL) == "" {
		return cfg, nil
	}
	token := os.Getenv("KAIRON_ATLAS_TOKEN")
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return cfg, fmt.Errorf("read -atlas-token-file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	opts := []atlas.Option{atlas.WithUserAgent("kairon-controller/" + version)}
	if token != "" {
		opts = append(opts, atlas.WithToken(token))
	}
	client, err := atlas.New(baseURL, opts...)
	if err != nil {
		return cfg, err
	}
	cfg.Client = client
	return cfg, nil
}

func envDefault(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func envBool(k string, d bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return d
	default:
		return d
	}
}
