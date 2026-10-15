// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdMigrate(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl migrate MACHINE [--strategy auto|live|cold|adaptive] [flags]"))
	}
	machine := args[0]
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	name := fs.String("name", "", "MachineMigration name")
	f := migrateFlags{}
	fs.StringVar(&f.strategy, "strategy", "auto", "auto|live|cold|adaptive (adaptive is a live migration steered by kairon-node's adaptive engine; experimental)")
	fs.StringVar(&f.target, "target-node", "", "target Kubernetes node; empty lets the scheduler choose")
	fs.StringVar(&f.mode, "mode", "pre-copy", "pre-copy|post-copy")
	fs.Uint64Var(&f.bandwidth, "bandwidth-mbps", 0, "migration adapter bandwidth limit (with --strategy adaptive and --adaptive-bandwidth, the starting value)")
	fs.Uint64Var(&f.downtime, "max-downtime-ms", 0, "maximum requested downtime")
	fs.UintVar(&f.multifd, "multifd-channels", 0, "QEMU multifd channels (0 disables explicit setting)")
	fs.StringVar(&f.migrationNetwork, "migration-network", "", "migration network name configured on the target node's adapter (-migration-network name=ip); empty uses the adapter's default advertise address")
	fs.StringVar(&f.progressTimeout, "progress-timeout", "", "adaptive: how long remaining RAM may fail to shrink before the transfer counts as stalled, e.g. 30s")
	fs.StringVar(&f.completionTimeoutPerGiB, "completion-timeout-per-gib", "", "adaptive: abort after guest RAM in GiB times this, e.g. 120s")
	fs.BoolVar(&f.autoConverge, "auto-converge", false, "adaptive: throttle guest vCPUs when the transfer stalls")
	fs.Uint64Var(&f.dirtyRateThreshold, "dirty-rate-threshold-mbps", 0, "adaptive: start auto-converge once the guest dirties memory this fast")
	fs.BoolVar(&f.postCopy, "post-copy", false, "adaptive: allow switching a stalled transfer to post-copy (a failure after the switch needs operator recovery)")
	fs.StringVar(&f.postCopyAfter, "post-copy-after", "", "adaptive: switch to post-copy once the transfer has run this long, e.g. 90s")
	fs.StringVar(&f.compression, "compression", "", "adaptive: multifd compression, none|zlib|zstd")
	fs.UintVar(&f.compressionLevel, "compression-level", 0, "adaptive: compression level (zstd 1-20, zlib 1-9)")
	fs.Uint64Var(&f.maxBandwidth, "max-bandwidth-mbps", 0, "adaptive: bandwidth ceiling")
	fs.BoolVar(&f.adaptiveBandwidth, "adaptive-bandwidth", false, "adaptive: raise bandwidth towards --max-bandwidth-mbps while throughput keeps up")
	fs.BoolVar(&f.fallbackToCold, "fallback-to-cold", false, "continue as a cold migration (Machine is stopped for the move) if a live migration is blocked before the source was touched")
	dryRun := fs.Bool("dry-run", false, "create the MachineMigration with dryRun=All: admission (budgets, policies) runs, nothing is stored")
	_ = fs.Parse(args[1:])
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	spec, err := f.spec(machine)
	if err != nil {
		fatal(err)
	}
	if *name == "" {
		*name = resourceName("migration-" + machine + "-" + time.Now().UTC().Format("20060102-150405"))
	}
	migration := model.MachineMigration{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
		Metadata: model.ObjectMeta{Name: *name, Namespace: *ns},
		Spec:     spec,
	}
	if *dryRun {
		if _, err := kc.DryRunCreateMachineMigration(ctx, *ns, migration); err != nil {
			fatal(err)
		}
		okf("machinemigration/%s would be created (server dry run: admission accepted it, nothing changed)", *name)
		return
	}
	out, err := kc.CreateMachineMigration(ctx, *ns, migration)
	if err != nil {
		fatal(err)
	}
	okf("machinemigration/%s created", out.Metadata.Name)
}

type migrateFlags struct {
	strategy, target, mode, migrationNetwork string
	bandwidth, downtime                      uint64
	multifd                                  uint

	progressTimeout, completionTimeoutPerGiB, postCopyAfter, compression string
	autoConverge, postCopy, adaptiveBandwidth                            bool
	dirtyRateThreshold, maxBandwidth                                     uint64
	compressionLevel                                                     uint

	// set holds the flags given on the command line, so adaptive overrides
	// only replace policy values the user actually chose.
	set            map[string]bool
	fallbackToCold bool
}

var adaptiveFlagNames = []string{"progress-timeout", "completion-timeout-per-gib", "auto-converge", "dirty-rate-threshold-mbps", "post-copy", "post-copy-after", "compression", "compression-level", "max-bandwidth-mbps", "adaptive-bandwidth"}

func (f migrateFlags) spec(machine string) (model.MachineMigrationSpec, error) {
	if f.multifd > 255 {
		return model.MachineMigrationSpec{}, fmt.Errorf("--multifd-channels must be <= 255")
	}
	spec := model.MachineMigrationSpec{MachineName: machine, Strategy: f.strategy, TargetNode: f.target, Mode: f.mode, BandwidthMbps: f.bandwidth, MaxDowntimeMs: f.downtime, MultifdChannels: uint8(f.multifd), MigrationNetwork: f.migrationNetwork, FallbackToCold: f.fallbackToCold}
	if f.strategy != model.MigrationTuningAdaptive {
		for _, n := range adaptiveFlagNames {
			if f.set[n] {
				return spec, fmt.Errorf("--%s needs --strategy adaptive", n)
			}
		}
		return spec, nil
	}
	spec.Strategy = "live"
	a := &model.MachineMigrationAdaptiveSpec{ProgressTimeout: f.progressTimeout, CompletionTimeoutPerGiB: f.completionTimeoutPerGiB}
	if f.set["auto-converge"] || f.set["dirty-rate-threshold-mbps"] {
		a.AutoConverge = &model.MigrationAutoConverge{Enabled: f.autoConverge || f.dirtyRateThreshold > 0, DirtyRateThresholdMbps: f.dirtyRateThreshold}
	}
	if f.set["post-copy"] || f.set["post-copy-after"] {
		a.PostCopy = &model.MigrationPostCopy{Enabled: f.postCopy || f.postCopyAfter != "", TriggerAfter: f.postCopyAfter}
	}
	if f.compression != "" || f.set["compression-level"] {
		if f.compressionLevel > 20 {
			return spec, fmt.Errorf("--compression-level must be <= 20")
		}
		algorithm := f.compression
		if algorithm == "" {
			algorithm = model.MigrationCompressionZstd
		}
		a.Compression = &model.MigrationCompression{Algorithm: algorithm, Level: uint8(f.compressionLevel)}
		if algorithm != model.MigrationCompressionNone && spec.MultifdChannels == 0 {
			spec.MultifdChannels = 2
		}
	}
	if f.set["max-bandwidth-mbps"] || f.set["adaptive-bandwidth"] {
		a.Bandwidth = &model.MigrationBandwidth{MaxMbps: f.maxBandwidth, Adaptive: f.adaptiveBandwidth}
	}
	if err := controller.ValidateAdaptiveSpec(*a); err != nil {
		return spec, err
	}
	spec.Adaptive = a
	return spec, nil
}

// migrationStillPending reports whether a MachineMigration has NOT yet
// reached one of its real terminal phases (Succeeded/Failed/Blocked/
// Cancelled) -- deliberately different from internal/controller/disruption.go's
// own isTerminalMigrationPhase, which treats "" as terminal too, for that
// function's narrower "does this count toward a budget's currentHealthy"
// purpose. Here "" means "just created, not yet reconciled by
// kairon-controller" -- exactly the case evacuatePass must NOT treat as
// safe to recreate. Found the hard way against a real cluster: running
// evacuate twice in quick succession (within one reconcile interval, before
// kairon-controller had advanced the first migration's phase past "")
// created a real duplicate MachineMigration for the same Machine.
func migrationStillPending(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "Blocked", "Cancelled":
		return false
	}
	return true
}
