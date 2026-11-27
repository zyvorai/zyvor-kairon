// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const KindMachineSet = "MachineSet"

// LabelMachineSet/LabelMachineSetTemplateHash are the labels
// kairon-controller stamps onto every Machine it creates for a
// MachineSet (internal/controller/machineset.go) -- the mechanism this
// project uses in place of a real Kubernetes ownerReference/garbage-
// collection (which this project has none of anywhere, having no
// client-go dependency to drive it): a plain label the reconcile loop
// filters on, the same "name/label reference, not an owner ref" pattern
// MachineMigrationSpec.MachineName already establishes for a single
// parent-child link. LabelMachineSetTemplateHash additionally marks
// which revision of spec.template a given Machine was created from, so
// a rolling update can tell "current" replicas from ones still pending
// replacement.
const (
	// AnnotationMachineSetPrevHash stores the previous template hash
	// so operators can see the last revision without inspecting every Machine.
	// Its value is the key kairon-controller (#301) and `kaironctl machineset
	// history` (#296) actually write and read; it was briefly
	// "kairon.zyvor.dev/machineset-prev-hash", which nothing ever wrote.
	AnnotationMachineSetPrevHash = "kairon.zyvor.dev/previous-template-hash"

	LabelMachineSet             = "kairon.zyvor.dev/machineset"
	LabelMachineSetTemplateHash = "kairon.zyvor.dev/machineset-template-hash"
)

// Template revision annotations, written on the MachineSet itself by
// kairon-controller (internal/controller/machineset.go's
// recordMachineSetTemplateRevision) and swapped by `kaironctl machineset
// rollback`. The controller only ever sees the live spec.template, so to
// be able to restore the one before it, it has to remember the template it
// last observed: when the live template's hash stops matching
// AnnotationMachineSetCurrentTemplateHash, the recorded "current" pair is
// moved to the "previous" pair and the live template is recorded as the
// new "current". The template bodies are JSON-encoded MachineTemplates.
//
// Annotations rather than a ControllerRevision-style object: this project
// has no revision CRD (and no client-go to borrow one from), a single step
// of history is all rollback needs, and keeping it on the object means it
// is deleted with the MachineSet and readable with plain kubectl. The cost
// is the Kubernetes 256 KiB total-annotation limit, so a template whose
// JSON exceeds MaxMachineSetTemplateAnnotationBytes is recorded by hash
// only -- rollout still works, but that revision cannot be rolled back to.
const (
	AnnotationMachineSetCurrentTemplateHash  = "kairon.zyvor.dev/current-template-hash"
	AnnotationMachineSetCurrentTemplate      = "kairon.zyvor.dev/current-template"
	AnnotationMachineSetPreviousTemplateHash = AnnotationMachineSetPrevHash
	AnnotationMachineSetPreviousTemplate     = "kairon.zyvor.dev/previous-template"
)

// MaxMachineSetTemplateAnnotationBytes caps one recorded template body.
// Two of them (current + previous) stay well under the 256 KiB limit Kubernetes
// enforces on all of an object's annotations combined, leaving room for
// everyone else's annotations (kubectl's last-applied-configuration
// included).
const MaxMachineSetTemplateAnnotationBytes = 64 * 1024

// MachineSetTemplateHash fingerprints a MachineTemplate: the first 12 hex
// characters of the SHA-256 of its JSON encoding. It is the value of
// LabelMachineSetTemplateHash on every replica and of the template-hash
// annotations above; kaironctl uses it to check a recorded template before
// restoring it.
func MachineSetTemplateHash(t MachineTemplate) string {
	b, _ := json.Marshal(t) // Marshal on a plain struct of strings/maps never errors
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// FinalizerMachineSet guards a MachineSet's own deletion until every
// Machine it owns (LabelMachineSet match) is actually gone -- see
// internal/controller/machineset.go's reconcileMachineSetDeletion. Needed
// for exactly the reason LabelMachineSet's own doc comment gives: this
// project has no ownerReference/garbage-collection to cascade a delete
// through on its own, so without an explicit finalizer a deleted
// MachineSet would simply vanish from Kubernetes while every replica it
// created (and each one's own FluxVM VM) kept running underneath it,
// orphaned.
const FinalizerMachineSet = "kairon.zyvor.dev/machineset-cleanup"

// MachineSet is a Deployment/ReplicaSet-shaped abstraction over the
// Machine CRD: kairon-controller creates/deletes plain Machine objects
// (internal/controller/machineset.go) to match Spec.Replicas, each built
// from Spec.Template. Every Machine a MachineSet owns is a real,
// independently-visible Machine object -- kubectl get machines, kaironctl
// evacuate, MachineDisruptionBudget selectors, and everything else that
// already operates on Machines keeps working unchanged; MachineSet only
// adds a way to stop hand-creating each one and hand-managing template
// drift yourself.
type MachineSet struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta       `json:"metadata"`
	Spec     MachineSetSpec   `json:"spec"`
	Status   MachineSetStatus `json:"status,omitempty"`
}

func (s MachineSet) Namespace() string {
	return s.Metadata.Namespace
}

type MachineSetList struct {
	TypeMeta `json:",inline"`
	Items    []MachineSet `json:"items"`
}

type MachineSetSpec struct {
	// Replicas is the desired Machine count. Negative is treated as 0 by
	// the reconciler rather than rejected -- the same permissive
	// tolerance ResourceSpec parsing already extends elsewhere in this
	// project, since the CRD schema itself only enforces a non-negative
	// minimum on well-formed input.
	Replicas int `json:"replicas"`
	// Template is the Machine spec every replica is created from. Just a
	// MachineSpec plus per-replica Labels -- deliberately not a full
	// ObjectMeta: name/namespace are always generated, never templated.
	Template MachineTemplate `json:"template"`
	// Strategy is RollingUpdate (default, when empty) or Recreate -- see
	// internal/controller/machineset.go's reconcileMachineSet for exactly
	// what each does.
	Strategy string `json:"strategy,omitempty"`
	// MaxUnavailable bounds how many of Replicas may be simultaneously
	// missing/outdated while RollingUpdate replaces old replicas with new
	// ones -- an integer or a percentage string, parsed the same way
	// MachineDisruptionBudget's MinAvailable/MaxUnavailable already are
	// (model.ParseIntOrPercent). Ignored for Recreate. Defaults to 1 when
	// empty.
	MaxUnavailable string `json:"maxUnavailable,omitempty"`
	// MaxSurge bounds how many extra Machines (above Replicas) may be
	// created temporarily during a RollingUpdate so new replicas can come
	// up before old ones are deleted. Integer or percentage. Defaults to 0
	// (no surge). The scheduler still has the final say on placement; a
	// surge that cannot be placed simply stays Pending.
	MaxSurge string `json:"maxSurge,omitempty"`
	// Paused, when true, stops the MachineSet from creating or deleting
	// replicas for rollout or scale purposes. Existing Machines are left
	// untouched, spec.replicas/spec.template edits (including HPA/KEDA
	// writes through /scale) are recorded but not acted on until it is
	// set back to false, and status is still refreshed every tick.
	// Deleting the MachineSet itself still cascades to its replicas.
	// Defaults to false.
	Paused bool `json:"paused,omitempty"`
}

// MachineTemplate is deliberately not embedded MachineSpec directly (a
// plain type alias) -- Labels needs its own field, and a future field
// specific to templating (e.g. name-generation options) has somewhere to
// go without reshaping MachineSpec itself.
type MachineTemplate struct {
	// Labels are applied to every Machine this MachineSet creates, in
	// addition to (never replacing) LabelMachineSet/
	// LabelMachineSetTemplateHash -- e.g. so a MachineDisruptionBudget's
	// selector can match every replica.
	Labels map[string]string `json:"labels,omitempty"`
	Spec   MachineSpec       `json:"spec"`
}

// MachineSetStatus mirrors a real Kubernetes ReplicaSet's status shape
// (replicas/readyReplicas/updatedReplicas) -- written by
// reconcileMachineSet every tick from the same Machine list Reconcile
// already fetched, purely observational like MachineDisruptionBudget's
// own status reconciliation.
type MachineSetStatus struct {
	Replicas        int `json:"replicas,omitempty"`
	ReadyReplicas   int `json:"readyReplicas,omitempty"`
	UpdatedReplicas int `json:"updatedReplicas,omitempty"`
	// Selector is the label selector of owned Machines, for the scale
	// subresource.
	Selector string `json:"selector,omitempty"`
	// Paused mirrors spec.paused. No omitempty, for the same merge-patch
	// reason as Message below: resuming must be able to write false over
	// a previously-patched true.
	Paused bool `json:"paused"`
	// Conditions carries Progressing, Available, ReplicaFailure,
	// ScaleDownBlocked (True, reason DisruptionBudget, while a
	// MachineDisruptionBudget holds back a scale-down) and Paused (True
	// while spec.paused freezes the MachineSet). No omitempty -- see
	// Message.
	Conditions []Condition `json:"conditions"`
	// No omitempty -- this whole status is patched as one object
	// (internal/kube.Client.PatchMachineSetStatus); under JSON merge-patch
	// semantics an absent key never clears a previously-set value, so a
	// recovered MachineSet would keep showing a stale error forever.
	Message string `json:"message"`
	// ObservedGeneration is the metadata.generation the controller last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
