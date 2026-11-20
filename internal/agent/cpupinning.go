// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/cpumanager"
	"github.com/zyvorai/kairon/internal/model"
)

// pinRecheck bounds how long an unchanged discovery skips the Node read,
// so an operator edit to the label is still noticed.
const pinRecheck = 5 * time.Minute

type pinState struct {
	mu        sync.Mutex
	last      cpumanager.Result
	checkedAt time.Time
	seen      bool
}

// publishPinnableCPUs projects kairon.zyvor.dev/pinnable-cpus from sysfs
// and kubelet cpu_manager_state. An operator-set label is not replaced.
// A failed discovery clears only a label this agent published, so a node
// that can no longer prove its set fails closed for cpuPinning.
func (a *Agent) publishPinnableCPUs(ctx context.Context) {
	if a.Kube == nil || a.NodeName == "" {
		return
	}
	root := a.SysRoot
	if root == "" {
		root = "/sys"
	}
	state := a.CPUManagerState
	if state == "" {
		state = "/var/lib/kubelet/cpu_manager_state"
	}
	got, _ := cpumanager.Discover(cpumanager.Input{OnlinePath: root + "/devices/system/cpu/online", StatePath: state, Reserved: a.ReservedCPUs})

	a.pin.mu.Lock()
	changed := !a.pin.seen || got.List != a.pin.last.List || got.Refused != a.pin.last.Refused
	fresh := !changed && time.Since(a.pin.checkedAt) < pinRecheck
	a.pin.mu.Unlock()
	switch {
	case !changed || got.Refused == "":
	case len(a.ReservedCPUs) == 0:
		a.log().Info("pinnable cpu discovery off: --reserved-cpus not set", "node", a.NodeName)
	default:
		a.log().Warn("pinnable cpu discovery refused", "node", a.NodeName, "reason", got.Refused)
	}
	if fresh {
		return
	}

	node, err := a.Kube.GetNode(ctx, a.NodeName)
	if err != nil {
		a.log().Debug("node read for pinnable cpus failed", "error", err)
		return
	}
	current := node.Metadata.Labels[model.PinnableCPUsLabel]
	source := node.Metadata.Annotations[cpumanager.AnnSource]
	owned := source == cpumanager.SourceDiscovered
	if patch := cpumanager.LabelPatch(current, source, got.List, owned, got.Refused != ""); patch != nil {
		if err := a.Kube.PatchNode(ctx, a.NodeName, patch); err != nil {
			a.log().Warn("pinnable cpu label patch failed", "node", a.NodeName, "error", err)
			return
		}
		a.log().Info("pinnable cpu label updated", "node", a.NodeName, "cpus", got.List, "refused", got.Refused)
	}
	a.pin.mu.Lock()
	a.pin.last, a.pin.checkedAt, a.pin.seen = got, time.Now(), true
	a.pin.mu.Unlock()
}
