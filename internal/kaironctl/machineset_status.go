// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
)

// cmdMachineSetStatus prints the key MachineSet conditions in one table.
func cmdMachineSetStatus(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl machineset status NAME"))
	}
	name := args[0]
	ns := "default"
	for i := 1; i < len(args)-1; i++ {
		if args[i] == "--namespace" || args[i] == "-n" {
			ns = args[i+1]
		}
	}
	ms, err := kc.GetMachineSet(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("NAME\tNAMESPACE\tREPLICAS\tREADY\tUPDATED\tOBSERVED\n")
	fmt.Printf("%s\t%s\t%d\t%d\t%d\t%d\n",
		ms.Metadata.Name, ms.Metadata.Namespace,
		ms.Status.Replicas, ms.Status.ReadyReplicas, ms.Status.UpdatedReplicas,
		ms.Status.ObservedGeneration)
	fmt.Println("\nCONDITION\tSTATUS\tREASON")
	want := []string{"Progressing", "Available", "ReplicaFailure", "Paused", "ScaleDownBlocked"}
	for _, t := range want {
		st, reason := "-", "-"
		for _, c := range ms.Status.Conditions {
			if c.Type == t {
				st, reason = c.Status, c.Reason
				break
			}
		}
		fmt.Printf("%s\t%s\t%s\n", t, st, reason)
	}
}

const machinesetStatusExamples = `  $ kaironctl machineset status web`
