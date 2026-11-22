// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/zyvorai/kairon/internal/fencing"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/nodeliveness"
)

func cmdFence(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl fence MACHINE --reason REASON"))
	}
	name := args[0]
	fs := flag.NewFlagSet("fence", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	reason := fs.String("reason", "", "your out-of-band evidence the node is truly gone, not just unreachable (required)")
	livenessLeaseNamespace := fs.String("liveness-lease-namespace", "", "namespace holding kairon-node's own liveness Leases (see node.livenessLease.enabled); when set, fence cross-checks the fenced node's own Lease against ConditionNodeUnreachable and refuses if kairon-node itself still looks alive there -- empty (the default) skips this extra check entirely, exactly kaironctl fence's behavior before it existed")
	forceIgnoreLiveness := fs.Bool("force-ignore-liveness", false, "proceed even if the liveness lease cross-check above would refuse")
	_ = fs.Parse(args[1:])
	if *reason == "" {
		fatal(fmt.Errorf("--reason is required: state the out-of-band evidence you have that the node is truly gone"))
	}

	m, err := kc.GetMachine(ctx, *ns, name)
	if err != nil {
		fatal(fmt.Errorf("get machine %s/%s: %w", *ns, name, err))
	}
	cond, found := model.FindCondition(m.Status.Conditions, model.ConditionNodeUnreachable)
	if !found || cond.Status != "True" {
		fatal(fmt.Errorf("machine %s/%s does not currently have %s=True -- nothing to fence (its node looks Ready to kairon-controller)", *ns, name, model.ConditionNodeUnreachable))
	}
	fencedNode := m.Spec.NodeName
	// ConditionNodeUnreachable is purely a kubelet Node-Ready-derived
	// signal (internal/controller/fencing.go) -- kubelet can flap
	// NotReady (a brief network blip, an apiserver hiccup) while
	// kairon-node's own reconcile loop keeps running fine on that node.
	// When an operator has opted a node into node.livenessLease.enabled,
	// cross-check kairon-node's own independent liveness signal before
	// trusting Node Ready alone -- see internal/nodeliveness's own doc
	// comment for the full reasoning. Skipped entirely (falls back to
	// today's exact behavior) when --liveness-lease-namespace isn't set,
	// or when no Lease is found for this node (liveness leases disabled
	// on it, or it never came up) -- this is an additional safety gate on
	// top of the existing check, never a replacement for it.
	if *livenessLeaseNamespace != "" {
		lease, err := kc.GetLease(ctx, *livenessLeaseNamespace, nodeliveness.LeaseName(fencedNode))
		switch {
		case kube.IsNotFound(err):
			// No liveness signal recorded for this node -- proceed exactly
			// as before this check existed.
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: could not read kairon-node's liveness lease for node %q (%v) -- proceeding without this extra check\n", fencedNode, err)
		default:
			if nodeliveness.IsFresh(lease, 0) && !*forceIgnoreLiveness {
				fatal(fmt.Errorf("refusing to fence: kairon-node on node %q renewed its own liveness lease recently, despite %s=True -- kairon-node's reconcile loop may still be alive and actively managing this Machine, and fencing now risks abandoning a still-live VM instead of a truly dead one; pass --force-ignore-liveness if you are certain this is safe", fencedNode, model.ConditionNodeUnreachable))
			}
		}
	}
	okf("fencing machine/%s off node %q (kairon-controller's last-observed reason: %s)", name, fencedNode, cond.Message)

	if err := fencing.Fence(ctx, kc, m, "OperatorAttested", fmt.Sprintf("fenced off node %q by an operator: %s", fencedNode, *reason)); err != nil {
		fatal(err)
	}
	okf("machine/%s: spec.nodeName cleared; will be rescheduled onto a different node on kairon-controller's next reconcile tick", name)
}
