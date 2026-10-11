// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"flag"
	"fmt"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/model"
)

// policyTuningFlags are MigrationPolicy's adaptive tuning flags, shared by
// create and edit.
type policyTuningFlags struct {
	strategy, completionTimeoutPerGiB, progressTimeout, postCopyAfter, compression string
	maxDowntime, dirtyRateThreshold, maxBandwidth                                  uint64
	autoConverge, postCopy, adaptiveBandwidth                                      bool
	compressionLevel                                                               uint
}

func (f *policyTuningFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.strategy, "strategy", "", "static|adaptive (adaptive is experimental until the lab migration matrix runs green)")
	fs.Uint64Var(&f.maxDowntime, "max-downtime-ms", 0, "default maximum downtime for matching migrations")
	fs.StringVar(&f.completionTimeoutPerGiB, "completion-timeout-per-gib", "", "adaptive: abort after guest RAM in GiB times this, e.g. 120s")
	fs.StringVar(&f.progressTimeout, "progress-timeout", "", "adaptive: stall window for remaining RAM, e.g. 30s")
	fs.BoolVar(&f.autoConverge, "auto-converge", false, "adaptive: throttle guest vCPUs when a transfer stalls")
	fs.Uint64Var(&f.dirtyRateThreshold, "dirty-rate-threshold-mbps", 0, "adaptive: start auto-converge at this guest dirty rate")
	fs.BoolVar(&f.postCopy, "post-copy", false, "adaptive: allow switching a stalled transfer to post-copy")
	fs.StringVar(&f.postCopyAfter, "post-copy-after", "", "adaptive: switch to post-copy after this long, e.g. 90s")
	fs.StringVar(&f.compression, "compression", "", "multifd compression: none|zlib|zstd")
	fs.UintVar(&f.compressionLevel, "compression-level", 0, "compression level (zstd 1-20, zlib 1-9)")
	fs.Uint64Var(&f.maxBandwidth, "max-bandwidth-mbps", 0, "bandwidth ceiling (supersedes --bandwidth-mbps)")
	fs.BoolVar(&f.adaptiveBandwidth, "adaptive-bandwidth", false, "adaptive: raise bandwidth towards the ceiling while throughput keeps up")
}

// patch returns the spec merge patch for the flags in set.
func (f *policyTuningFlags) patch(set map[string]bool) (map[string]any, error) {
	spec := map[string]any{}
	if set["strategy"] {
		if f.strategy != model.MigrationTuningStatic && f.strategy != model.MigrationTuningAdaptive {
			return nil, fmt.Errorf("--strategy must be static or adaptive")
		}
		spec["strategy"] = f.strategy
	}
	if set["max-downtime-ms"] {
		spec["maxDowntimeMs"] = f.maxDowntime
	}
	if set["completion-timeout-per-gib"] {
		spec["completionTimeoutPerGiB"] = f.completionTimeoutPerGiB
	}
	if set["progress-timeout"] {
		spec["progressTimeout"] = f.progressTimeout
	}
	sub := func(key string, fields map[string]any) {
		if len(fields) > 0 {
			spec[key] = fields
		}
	}
	ac := map[string]any{}
	if set["auto-converge"] {
		ac["enabled"] = f.autoConverge
	}
	if set["dirty-rate-threshold-mbps"] {
		ac["dirtyRateThresholdMbps"] = f.dirtyRateThreshold
	}
	sub("autoConverge", ac)
	pc := map[string]any{}
	if set["post-copy"] {
		pc["enabled"] = f.postCopy
	}
	if set["post-copy-after"] {
		pc["triggerAfter"] = f.postCopyAfter
	}
	sub("postCopy", pc)
	comp := map[string]any{}
	if set["compression"] {
		comp["algorithm"] = f.compression
	}
	if set["compression-level"] {
		if f.compressionLevel > 20 {
			return nil, fmt.Errorf("--compression-level must be <= 20")
		}
		comp["level"] = f.compressionLevel
	}
	sub("compression", comp)
	bw := map[string]any{}
	if set["max-bandwidth-mbps"] {
		bw["maxMbps"] = f.maxBandwidth
	}
	if set["adaptive-bandwidth"] {
		bw["adaptive"] = f.adaptiveBandwidth
	}
	sub("bandwidth", bw)

	check := model.MachineMigrationAdaptiveSpec{CompletionTimeoutPerGiB: f.completionTimeoutPerGiB, ProgressTimeout: f.progressTimeout, PostCopy: &model.MigrationPostCopy{TriggerAfter: f.postCopyAfter}}
	if set["compression"] {
		check.Compression = &model.MigrationCompression{Algorithm: f.compression, Level: uint8(min(f.compressionLevel, 255))}
	}
	if err := controller.ValidateAdaptiveSpec(check); err != nil {
		return nil, err
	}
	return spec, nil
}

func visited(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}
