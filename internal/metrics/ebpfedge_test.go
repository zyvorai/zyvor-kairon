// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEdgeRecorderDropAndRestore(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewEdgeRecorder(reg)
	r.ObserveDrop("demo", "web", "sni_deny", "egress-443")
	r.ObserveDrop("demo", "web", "spoof_mac", "")
	r.ObserveConntrackRestore(4, 40)
	if got := testutil.ToFloat64(r.Drops.WithLabelValues("demo", "web", "sni_deny", "egress-443")); got != 1 {
		t.Fatalf("sni drops = %v", got)
	}
	if got := testutil.ToFloat64(r.Drops.WithLabelValues("demo", "web", "spoof_mac", "-")); got != 1 {
		t.Fatalf("spoof drops = %v", got)
	}
	if got := testutil.ToFloat64(r.ConntrackRestored); got != 4 {
		t.Fatalf("restored = %v", got)
	}
}
