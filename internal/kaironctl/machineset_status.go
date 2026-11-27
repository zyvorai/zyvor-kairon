// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const machinesetStatusExamples = `  $ kaironctl machineset status web
  $ kaironctl machineset status web -n prod`

// machineSetStatusKnownConditions is the order `kaironctl machineset status`
// always prints a row for, even when the MachineSet has not recorded that
// type yet (STATUS=Unknown). These are the conditions kairon-controller
// actually writes today (internal/controller/machineset.go and
// machineset_mdb.go). Any other condition on the object is appended after.
var machineSetStatusKnownConditions = []string{
	"Progressing",
	"Available",
	"ReplicaFailure",
	"Paused",
	"ScaleDownBlocked",
}

// machineSetStatus prints a MachineSet's replica counts, generation, pause
// flag and status.conditions in one place. The conditions table always
// lists Progressing, Available, ReplicaFailure, Paused and
// ScaleDownBlocked (Unknown/- when absent) so an operator can see the
// same set regardless of how far the controller has got.
func machineSetStatus(ctx context.Context, kc *kube.Client, ns, name string, w io.Writer, now time.Time) error {
	ms, err := kc.GetMachineSet(ctx, ns, name)
	if err != nil {
		return err
	}
	writeMachineSetStatus(w, ms, now)
	return nil
}

func writeMachineSetStatus(w io.Writer, ms model.MachineSet, now time.Time) {
	fmt.Fprintf(w, "NAME\tNAMESPACE\tDESIRED\tREADY\tUPDATED\tGENERATION\tOBSERVED\tPAUSED\n")
	fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%t\n",
		ms.Metadata.Name, ms.Namespace(),
		ms.Spec.Replicas, ms.Status.ReadyReplicas, ms.Status.UpdatedReplicas,
		ms.Metadata.Generation, ms.Status.ObservedGeneration, ms.Status.Paused)
	if ms.Status.Message != "" {
		fmt.Fprintf(w, "MESSAGE\t%s\n", ms.Status.Message)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tSTATUS\tREASON\tMESSAGE\tAGE")
	seen := map[string]bool{}
	for _, typ := range machineSetStatusKnownConditions {
		writeMachineSetConditionRow(tw, typ, ms.Status.Conditions, now)
		seen[typ] = true
	}
	for _, c := range ms.Status.Conditions {
		if c.Type == "" || seen[c.Type] {
			continue
		}
		writeMachineSetConditionRow(tw, c.Type, ms.Status.Conditions, now)
		seen[c.Type] = true
	}
	_ = tw.Flush()
}

func writeMachineSetConditionRow(w io.Writer, typ string, conds []model.Condition, now time.Time) {
	c, ok := model.FindCondition(conds, typ)
	if !ok {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", typ, "Unknown", "-", "-", "-")
		return
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", typ, dash(c.Status), dash(c.Reason), dash(c.Message), auditAge(now, c.LastTransitionTime))
}
