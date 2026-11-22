// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdEvacuate(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl evacuate NODE [--strategy cold|auto] [--dry-run] [--wait] [--timeout 15m] [--poll-interval 10s]"))
	}
	node := args[0]
	fs := flag.NewFlagSet("evacuate", flag.ExitOnError)
	strategy := fs.String("strategy", "cold", "cold|auto; use migrate --strategy live for the secure peer handshake")
	wait := fs.Bool("wait", false, "keep retrying -- respecting MachineDisruptionBudget and skipping any Machine already mid-migration -- until every Machine has left NODE or --timeout elapses, instead of the default single pass that leaves budget-blocked Machines behind for good. Matches kubectl drain's own retry model; this is the closest thing to an automatic node-drain Kairon has, deliberately still operator-invoked rather than triggered silently by e.g. node.spec.unschedulable, the same 'operator attests, Kairon then follows through' shape as kaironctl fence/recover.")
	timeout := fs.Duration("timeout", 15*time.Minute, "with --wait, how long to keep retrying before giving up")
	pollInterval := fs.Duration("poll-interval", 10*time.Second, "with --wait, how often to re-check and retry")
	dryRun := fs.Bool("dry-run", false, "print which Machines would be migrated and which would be skipped (and why) without creating any MachineMigration; reads the cluster only")
	_ = fs.Parse(args[1:])
	if *dryRun && *wait {
		fatal(fmt.Errorf("evacuate --dry-run is a single read-only preview; it cannot be combined with --wait"))
	}
	if *strategy != "cold" && *strategy != "auto" {
		fatal(fmt.Errorf("evacuate supports --strategy cold|auto; use migrate for explicit live migration"))
	}

	if *dryRun {
		would, skipped, remaining, err := evacuatePassMode(ctx, kc, node, *strategy, true)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("dry-run: %d of %d machine(s) on %s would be queued for evacuation, %d skipped; nothing was created\n", would, remaining, node, skipped)
		return
	}

	created, skipped, remaining, err := evacuatePass(ctx, kc, node, *strategy)
	if err != nil {
		fatal(err)
	}
	if !*wait {
		fmt.Printf("evacuation queued: %d machine(s) from %s", created, node)
		if skipped > 0 {
			fmt.Printf(", %d skipped (MachineDisruptionBudget or already mid-migration)\n", skipped)
			os.Exit(1)
		}
		fmt.Println()
		return
	}

	deadline := time.Now().Add(*timeout)
	for remaining > 0 {
		if time.Now().After(deadline) {
			fatal(fmt.Errorf("evacuate --wait timed out after %s: %d machine(s) still on %s", *timeout, remaining, node))
		}
		select {
		case <-ctx.Done():
			fatal(ctx.Err())
		case <-time.After(*pollInterval):
		}
		createdThisPass, _, r, err := evacuatePass(ctx, kc, node, *strategy)
		if err != nil {
			fatal(err)
		}
		created += createdThisPass
		remaining = r
		fmt.Printf("%s: %d machine(s) still on %s (%d newly queued this pass, %d total queued)\n", time.Now().UTC().Format(time.RFC3339), remaining, node, createdThisPass, created)
	}
	fmt.Printf("evacuation of %s complete: %d machine(s) migrated\n", node, created)
}

// evacuatePass makes one pass over every Machine currently on node: creates
// a MachineMigration for each one MachineDisruptionBudget allows and that
// isn't already mid-migration (a Machine can only sensibly have one active
// MachineMigration at a time -- skip rather than create a conflicting
// second one; nothing else in this codebase already guarded against that,
// so this closes a real latent gap, not just something --wait needed).
// remaining is how many Machines are still assigned to node afterward
// (including ones just queued this pass, since they haven't actually left
// yet) -- the number cmdEvacuate's --wait loop polls until it reaches 0.
func evacuatePass(ctx context.Context, kc *kube.Client, node, strategy string) (created, skipped, remaining int, err error) {
	return evacuatePassMode(ctx, kc, node, strategy, false)
}

// evacuatePassMode is evacuatePass with an optional dryRun: it makes the
// same decisions (budget admission, already-migrating skip) but creates no
// MachineMigration and only prints what it would do. In dry-run mode the
// returned created counts the migrations that would be created.
func evacuatePassMode(ctx context.Context, kc *kube.Client, node, strategy string, dryRun bool) (created, skipped, remaining int, err error) {
	machines, err := kc.ListMachines(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	migrations, err := kc.ListMachineMigrations(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return 0, 0, 0, err
	}
	budgets, err := kc.ListMachineDisruptionBudgets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return 0, 0, 0, err
	}
	states, err := controller.LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		return 0, 0, 0, err
	}
	inFlight := map[string]bool{}
	for _, mig := range migrations {
		if migrationStillPending(mig.Status.Phase) {
			inFlight[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		}
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	for _, machine := range machines {
		if machine.Spec.NodeName != node || machine.Metadata.DeletionTimestamp != nil {
			continue
		}
		remaining++
		key := machine.Namespace() + "/" + machine.Metadata.Name
		if inFlight[key] {
			if dryRun {
				fmt.Printf("machine %s skipped: already has a MachineMigration in flight\n", key)
			}
			continue // already migrating -- wait for it, don't create a second one
		}
		if blocker := controller.AdmitDisruption(states, machine); blocker != "" {
			fmt.Printf("machine %s/%s skipped: %s\n", machine.Namespace(), machine.Metadata.Name, blocker)
			skipped++
			continue
		}
		name := resourceName("evacuate-" + node + "-" + machine.Metadata.Name + "-" + stamp)
		if dryRun {
			fmt.Printf("machine %s would be migrated (strategy %s, machinemigration/%s)\n", key, strategy, name)
			created++
			continue
		}
		migration := model.MachineMigration{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
			Metadata: model.ObjectMeta{Name: name, Namespace: machine.Namespace()},
			Spec:     model.MachineMigrationSpec{MachineName: machine.Metadata.Name, Strategy: strategy},
		}
		if _, err := kc.CreateMachineMigration(ctx, machine.Namespace(), migration); err != nil {
			return created, skipped, remaining, fmt.Errorf("create migration for %s/%s: %w", machine.Namespace(), machine.Metadata.Name, err)
		}
		okf("machinemigration/%s created for %s/%s", name, machine.Namespace(), machine.Metadata.Name)
		created++
	}
	return created, skipped, remaining, nil
}

// cmdRecover is a thin convenience layer over spec.recovery, not a second
// source of truth: it prints the migration's current status.Recovery
// diagnosis (so the operator sees ground truth before acting), then patches
// spec.recovery -- internal/agent's reconcileNeedsRecovery is what actually
