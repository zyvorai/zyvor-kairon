// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// machineSetHistory prints the template revisions kairon-controller has
// recorded on a MachineSet (see model.AnnotationMachineSetCurrentTemplateHash
// and friends) so an operator can see what `kaironctl machineset rollback`
// would restore. STORED says whether a usable copy of the previous template
// is kept: "yes" (present and hash-verified), "hash-only" (larger than
// model.MaxMachineSetTemplateAnnotationBytes, or recorded before template
// copies existed), "invalid" (does not decode or hash to PREVIOUS) or "-"
// (no previous revision yet). CURRENT falls back to the live template's hash,
// marked "(unrecorded)", until the controller has caught up.
func machineSetHistory(ctx context.Context, kc *kube.Client, ns, name string, w io.Writer) error {
	ms, err := kc.GetMachineSet(ctx, ns, name)
	if err != nil {
		return err
	}
	ann := ms.Metadata.Annotations
	current := annotation(ann, model.AnnotationMachineSetCurrentTemplateHash)
	live := model.MachineSetTemplateHash(ms.Spec.Template)
	if current != live {
		current = live + " (unrecorded)"
	}
	prev := annotation(ann, model.AnnotationMachineSetPreviousTemplateHash)
	stored := previousTemplateState(prev, annotation(ann, model.AnnotationMachineSetPreviousTemplate))
	fmt.Fprintf(w, "NAME\tNAMESPACE\tCURRENT\tPREVIOUS\tSTORED\n")
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ms.Metadata.Name, ms.Namespace(), current, shortHash(prev), stored)
	if prev == "" {
		fmt.Fprintln(w, "\nno previous template hash recorded yet")
	}
	return nil
}

func previousTemplateState(hash, body string) string {
	switch {
	case hash == "":
		return "-"
	case body == "":
		return "hash-only"
	}
	var t model.MachineTemplate
	if err := json.Unmarshal([]byte(body), &t); err != nil || model.MachineSetTemplateHash(t) != hash {
		return "invalid"
	}
	return "yes"
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
