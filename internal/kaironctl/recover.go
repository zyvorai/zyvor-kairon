// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdRecover(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl recover MIGRATION --action ACTION --diagnosis DIAGNOSIS --reason REASON\n" +
			"  ACTION: ConfirmDestinationCommitted | ConfirmDestinationNotCommitted | ForceAbort\n" +
			"  DIAGNOSIS: DestinationCommitted | DestinationNotCommitted | Unknown"))
	}
	name := args[0]
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	action := fs.String("action", "", "recovery action (required)")
	diagnosis := fs.String("diagnosis", "", "what you observed, attested (required)")
	reason := fs.String("reason", "", "evidence for this decision (required)")
	_ = fs.Parse(args[1:])

	current, err := kc.GetMachineMigration(ctx, *ns, name)
	if err != nil {
		fatal(fmt.Errorf("get machinemigration %s/%s: %w", *ns, name, err))
	}
	if current.Status.Phase != "NeedsRecovery" {
		fatal(fmt.Errorf("machinemigration %s/%s is in phase %q, not NeedsRecovery", *ns, name, current.Status.Phase))
	}
	fmt.Println("current diagnosis:")
	if r := current.Status.Recovery; r != nil {
		fmt.Printf("  source runtime status:       %s\n", dash(r.SourceRuntimeStatus))
		fmt.Printf("  destination session phase:   %s\n", dash(r.DestinationSessionPhase))
		fmt.Printf("  destination runtime found:   %v\n", r.DestinationRuntimeFound)
		fmt.Printf("  destination runtime status:  %s\n", dash(r.DestinationRuntimeStatus))
		if r.DiagnosedAt != nil {
			fmt.Printf("  diagnosed at:                %s\n", r.DiagnosedAt.Format(time.RFC3339))
		}
	} else {
		fmt.Println("  (none yet -- wait for the source agent's next reconcile tick)")
	}

	if *action == "" || *diagnosis == "" || *reason == "" {
		fatal(fmt.Errorf("--action, --diagnosis and --reason are all required"))
	}
	patch := map[string]any{
		"spec": map[string]any{
			"recovery": model.MachineMigrationRecoverySpec{
				Action:                *action,
				AcknowledgedDiagnosis: *diagnosis,
				Reason:                *reason,
			},
		},
	}
	if err := kc.PatchMachineMigration(ctx, *ns, name, patch); err != nil {
		fatal(fmt.Errorf("patch machinemigration %s/%s: %w", *ns, name, err))
	}
	okf("machinemigration/%s: recovery %s requested; the source node's agent will validate and apply it on its next reconcile", name, *action)
}

// cmdCancelMigration is a thin convenience layer over spec.cancel, matching
// cmdRecover's own "print current state, then patch spec, then let the
// source node's agent actually apply it" shape. Only meaningful while the
// migration is still Starting or Running (before the destination has
// committed) -- refuses locally rather than silently patching a migration
// that's already past the point where cancelling is safe, so an operator
// gets an immediate, clear answer instead of a spec.cancel that the agent
// will just ignore as a documented no-op.
func cmdCancelMigration(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl cancel-migration MIGRATION [-n NAMESPACE]"))
	}
	name := args[0]
	ns, _ := nsFlag(args[1:])

	current, err := kc.GetMachineMigration(ctx, ns, name)
	if err != nil {
		fatal(fmt.Errorf("get machinemigration %s/%s: %w", ns, name, err))
	}
	if current.Status.Phase != "Starting" && current.Status.Phase != "Running" {
		fatal(fmt.Errorf("machinemigration %s/%s is in phase %q; cancel only applies to a live migration still in Starting or Running, before the destination has committed", ns, name, current.Status.Phase))
	}
	if current.Status.EffectiveStrategy != "live" {
		fatal(fmt.Errorf("machinemigration %s/%s is a %q-strategy migration; cancel only applies to live migrations", ns, name, current.Status.EffectiveStrategy))
	}
	if a := current.Status.Adaptive; a != nil && a.PostCopyStarted {
		fatal(fmt.Errorf("machinemigration %s/%s has started post-copy; the source can no longer take the guest back, so it can't be cancelled", ns, name))
	}
	if current.Spec.Cancel {
		okf("machinemigration/%s: cancel already requested; waiting for the source node's agent to apply it", name)
		return
	}
	patch := map[string]any{"spec": map[string]any{"cancel": true}}
	if err := kc.PatchMachineMigration(ctx, ns, name, patch); err != nil {
		fatal(fmt.Errorf("patch machinemigration %s/%s: %w", ns, name, err))
	}
	okf("machinemigration/%s: cancel requested; the source node's agent will abort the in-flight transfer and mark it Cancelled on its next reconcile", name)
}
