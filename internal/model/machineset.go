// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

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
	LabelMachineSet             = "kairon.zyvor.dev/machineset"
	LabelMachineSetTemplateHash = "kairon.zyvor.dev/machineset-template-hash"
)

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
	// what each does. Never MaxSurge-capable: only MaxUnavailable is
	// offered, since Kairon has no capacity/overcommit model
	// (internal/scheduler) to safely reason about a temporary surge
	// above Replicas against yet.
	Strategy string `json:"strategy,omitempty"`
	// MaxUnavailable bounds how many of Replicas may be simultaneously
	// missing/outdated while RollingUpdate replaces old replicas with new
	// ones -- an integer or a percentage string, parsed the same way
	// MachineDisruptionBudget's MinAvailable/MaxUnavailable already are
	// (model.ParseIntOrPercent). Ignored for Recreate. Defaults to 1 when
	// empty.
	MaxUnavailable string `json:"maxUnavailable,omitempty"`
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
	// No omitempty -- this whole status is patched as one object
	// (internal/kube.Client.PatchMachineSetStatus); under JSON merge-patch
	// semantics an absent key never clears a previously-set value, so a
	// recovered MachineSet would keep showing a stale error forever.
	Message string `json:"message"`
}
