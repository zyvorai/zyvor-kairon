// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const machinesetRollbackExamples = `  $ kaironctl machineset rollback web --dry-run
  $ kaironctl machineset rollback web -n prod`

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
//   - there is no recorded previous template (never changed), or only its
//     hash (too large for an annotation, or recorded by an older controller
//     that kept hashes only);
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
		return "", "", fmt.Errorf("machineset/%s: previous template %s was recorded by hash only (recorded before template copies were kept, or larger than %d bytes of JSON), so it cannot be restored", name, prevHash, model.MaxMachineSetTemplateAnnotationBytes)
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
