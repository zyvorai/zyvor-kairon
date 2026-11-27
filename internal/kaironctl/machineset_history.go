// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// cmdMachineSetHistory lists the previous template hash recorded on a
// MachineSet so an operator can inspect what a rollback would restore.
// The controller stores the prior hash in the annotation
// kairon.zyvor.dev/previous-template-hash when a template change is observed.
func cmdMachineSetHistory(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl machineset history NAME"))
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
	current := annotation(ms.Annotations, model.LabelMachineSetTemplateHash)
	fmt.Printf("NAME\tNAMESPACE\tCURRENT\tPREVIOUS\n")
	fmt.Printf("%s\t%s\t%s\t%s\n", ms.Name, ms.Namespace, shortHash(current), shortHash(prev))
	if prev == "" {
		fmt.Println("\nno previous template hash recorded yet")
	}
}

func annotation(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

func shortHash(h string) string {
	if h == "" {
		return "-"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

const machinesetHistoryExamples = `  $ kaironctl machineset history web
  $ kaironctl machineset history web -n prod`
