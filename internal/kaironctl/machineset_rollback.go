// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
)

// cmdMachineSetRollback prints the previous template hash and the
// kubectl command an operator would use to restore it. Full template
// restore is intentionally out of scope for this stub.
func cmdMachineSetRollback(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl machineset rollback NAME"))
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
	prev := annotation(ms.Annotations, "kairon.zyvor.dev/previous-template-hash")
	if prev == "" {
		fmt.Println("no previous template hash recorded; nothing to roll back")
		return
	}
	fmt.Printf("previous template hash: %s\n", prev)
	fmt.Println("restore the previous MachineTemplate with kubectl edit or a YAML apply, then re-apply")
}

const machinesetRollbackExamples = `  $ kaironctl machineset rollback web`
