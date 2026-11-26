// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const machineSetExamples = `  $ kaironctl machineset rollback web
  $ kaironctl machineset rollback web --namespace prod --dry-run`

// cmdMachineSet dispatches `kaironctl machineset SUBCOMMAND`. rollback is
// the only subcommand today; create/edit/scale/get stay under their verbs.
func cmdMachineSet(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 || args[0] != "rollback" {
		fatal(fmt.Errorf("usage: kaironctl machineset rollback NAME [--namespace NS] [--dry-run]"))
	}
	rest := args[1:]
	if len(rest) < 1 || rest[0] == "" || rest[0][0] == '-' {
		fatal(fmt.Errorf("usage: kaironctl machineset rollback NAME [--namespace NS] [--dry-run]"))
	}
	name := rest[0]
	fs := flag.NewFlagSet("machineset rollback", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	dryRun := fs.Bool("dry-run", false, "print what would be restored without changing anything")
	_ = fs.Parse(rest[1:])
	from, to, err := rollbackMachineSet(ctx, kc, *ns, name, *dryRun)
	if err != nil {
		fatal(err)
	}
	if *dryRun {
		okf("machineset/%s would roll back template %s -> %s (dry-run, not changed)", name, from, to)
		return
	}
	okf("machineset/%s rolled back template %s -> %s", name, from, to)
}

// rollbackMachineSet restores the template kairon-controller recorded in
// the MachineSet's previous-template annotations (see
// model.AnnotationMachineSetPreviousTemplate) and swaps the current/previous
// annotation pairs in the same PATCH, so the controller finds them already
// in step and running this again goes forward to the template it replaced.
// The rollout itself is the ordinary template-hash rollout: replicas whose
// LabelMachineSetTemplateHash no longer matches are replaced under the
// MachineSet's own strategy, maxUnavailable and maxSurge.
//
// It refuses rather than guess when:
//   - there is no recorded previous template (never changed, or recorded
//     by hash only because it was too large for an annotation);
//   - the controller has not yet recorded the live template as current
//     (rolling back then would lose the live template for good);
//   - the recorded body does not hash to its recorded hash.
//
// The PATCH carries metadata.resourceVersion, so a concurrent edit makes it
// fail with a conflict instead of being overwritten.
func rollbackMachineSet(ctx context.Context, kc *kube.Client, ns, name string, dryRun bool) (from, to string, err error) {
	ms, err := kc.GetMachineSet(ctx, ns, name)
	if err != nil {
		return "", "", err
	}
	ann := ms.Metadata.Annotations
	liveHash := model.MachineSetTemplateHash(ms.Spec.Template)
	prevHash := ann[model.AnnotationMachineSetPreviousTemplateHash]
	prevBody := ann[model.AnnotationMachineSetPreviousTemplate]
	if prevHash == "" {
		return "", "", fmt.Errorf("machineset/%s has no previous template recorded (its template has not changed since kairon-controller started tracking it)", name)
	}
	if prevBody == "" {
		return "", "", fmt.Errorf("machineset/%s: previous template %s was recorded by hash only (larger than %d bytes), so it cannot be restored", name, prevHash, model.MaxMachineSetTemplateAnnotationBytes)
	}
	if recorded := ann[model.AnnotationMachineSetCurrentTemplateHash]; recorded != liveHash {
		return "", "", fmt.Errorf("machineset/%s: kairon-controller has not recorded the live template %s yet (annotation says %q); wait a reconcile interval and retry", name, liveHash, recorded)
	}
	var prev model.MachineTemplate
	if err := json.Unmarshal([]byte(prevBody), &prev); err != nil {
		return "", "", fmt.Errorf("machineset/%s: decode %s: %w", name, model.AnnotationMachineSetPreviousTemplate, err)
	}
	if got := model.MachineSetTemplateHash(prev); got != prevHash {
		return "", "", fmt.Errorf("machineset/%s: recorded previous template hashes to %s, not %s; refusing to restore it", name, got, prevHash)
	}
	if dryRun {
		return liveHash, prevHash, nil
	}

	templatePatch, err := replaceMergePatch(ms.Spec.Template, prev)
	if err != nil {
		return "", "", err
	}
	// The live template becomes "previous"; same size rule the controller
	// applies (machineSetTemplateRevisionPatch): too large means hash only.
	var liveBody any
	if b, err := json.Marshal(ms.Spec.Template); err == nil && len(b) <= model.MaxMachineSetTemplateAnnotationBytes {
		liveBody = string(b)
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"resourceVersion": ms.Metadata.ResourceVersion,
			"annotations": map[string]any{
				model.AnnotationMachineSetCurrentTemplateHash:  prevHash,
				model.AnnotationMachineSetCurrentTemplate:      prevBody,
				model.AnnotationMachineSetPreviousTemplateHash: liveHash,
				model.AnnotationMachineSetPreviousTemplate:     liveBody,
			},
		},
		"spec": map[string]any{"template": templatePatch},
	}
	if err := kc.PatchMachineSet(ctx, ns, name, patch); err != nil {
		return "", "", fmt.Errorf("rollback machineset/%s: %w", name, err)
	}
	return liveHash, prevHash, nil
}

// replaceMergePatch returns a JSON merge patch (RFC 7386) that turns from
// into to. A merge patch keeps every key it does not mention, so a plain
// {"template": to} would leave behind labels or fields the old template had
// and the restored one does not; each of those is sent as null instead.
// Same approach as internal/agent's statusMergePatch.
func replaceMergePatch(from, to any) (map[string]any, error) {
	fromMap, err := jsonObject(from)
	if err != nil {
		return nil, err
	}
	toMap, err := jsonObject(to)
	if err != nil {
		return nil, err
	}
	nullMissing(fromMap, toMap)
	return toMap, nil
}

func jsonObject(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	return out, json.Unmarshal(raw, &out)
}

func nullMissing(from, to map[string]any) {
	for k, fv := range from {
		tv, ok := to[k]
		if !ok {
			to[k] = nil
			continue
		}
		fm, fIsMap := fv.(map[string]any)
		tm, tIsMap := tv.(map[string]any)
		if fIsMap && tIsMap {
			nullMissing(fm, tm)
		}
	}
}
