// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

// PersistentVolumeClaim mirrors just the fields Kairon reads from a
// core/v1 PersistentVolumeClaim to resolve a Machine's spec.volumes[]
// entry to a real host path -- not a general-purpose PVC client.
type PersistentVolumeClaim struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                  `json:"metadata"`
	Spec     PersistentVolumeClaimSpec   `json:"spec"`
	Status   PersistentVolumeClaimStatus `json:"status,omitempty"`
}

type PersistentVolumeClaimSpec struct {
	// VolumeName is set by the Kubernetes binder once the claim is Bound --
	// the name of the PersistentVolume backing it.
	VolumeName string `json:"volumeName,omitempty"`
	// The remaining fields are only meaningful when Kairon is the one
	// creating the PVC (restoring a MachineSnapshot into a new PVC) rather
	// than just reading one to resolve a Machine's boot disk.
	AccessModes      []string                        `json:"accessModes,omitempty"`
	Resources        *PersistentVolumeClaimResources `json:"resources,omitempty"`
	DataSource       *TypedLocalObjectReference      `json:"dataSource,omitempty"`
	StorageClassName *string                         `json:"storageClassName,omitempty"`
}

type PersistentVolumeClaimResources struct {
	Requests map[string]string `json:"requests,omitempty"`
}

// TypedLocalObjectReference points a new PVC's spec.dataSource at the
// VolumeSnapshot to restore from -- same shape Kubernetes itself uses.
type TypedLocalObjectReference struct {
	APIGroup string `json:"apiGroup,omitempty"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

type PersistentVolumeClaimStatus struct {
	Phase string `json:"phase,omitempty"`
}

// PersistentVolume mirrors just the fields Kairon reads to turn a bound PV
// into a real host directory a Machine's disk image can live in.
// hostPath- and local-backed volumes resolve to an already-attach-ready
// host directory directly, no mounting involved. csi-backed volumes
// (Kairon's own driver only -- see internal/csinode) resolve through a
// real NodeStageVolume/NodePublishVolume call instead, since a
// network-block volume has to actually be attached and mounted first;
// see internal/agent/storage.go and docs/guides/machine-storage-csi.md.
type PersistentVolume struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta           `json:"metadata"`
	Spec     PersistentVolumeSpec `json:"spec"`
}

// AnnotationSharedStorage set to "true" on a hostPath PersistentVolume says
// its path is one shared filesystem mounted at the same place on every node
// (NFS, for example). A Machine booting from it opens its raw disk.img in
// place instead of through a node-local overlay, so it can live-migrate.
// Kairon cannot check the claim; a path that is not really shared corrupts
// the disk on the first migration.
const AnnotationSharedStorage = "kairon.zyvor.dev/shared-storage"

// SharedHostPath reports whether pv is a hostPath volume marked with
// AnnotationSharedStorage.
func SharedHostPath(pv PersistentVolume) bool {
	return pv.Spec.HostPath != nil && pv.Metadata.Annotations[AnnotationSharedStorage] == "true"
}

type PersistentVolumeSpec struct {
	// VolumeMode is "Filesystem" (default, empty means Filesystem per the
	// Kubernetes API) or "Block". Only Filesystem-mode volumes are
	// supported: the Machine's disk image is a file inside the volume's
	// directory, not the raw block device itself.
	VolumeMode string                     `json:"volumeMode,omitempty"`
	HostPath   *HostPathVolumeSource      `json:"hostPath,omitempty"`
	Local      *LocalVolumeSource         `json:"local,omitempty"`
	CSI        *CSIPersistentVolumeSource `json:"csi,omitempty"`
}

type HostPathVolumeSource struct {
	Path string `json:"path,omitempty"`
}

type LocalVolumeSource struct {
	Path string `json:"path,omitempty"`
}

// CSIPersistentVolumeSource mirrors core/v1's CSIPersistentVolumeSource,
// scoped to what internal/agent/storage.go needs to drive
// internal/csinode's Node service directly as its own CSI client (see
// that package's own doc comment for why there's no Controller service,
// and why kairon-node dials it directly rather than going through
// kubelet's Pod volume machinery). Only Driver == csinode.DriverName is
// ever resolvable -- a PV naming any other CSI driver is rejected the
// same way an unrecognized volume source always was.
type CSIPersistentVolumeSource struct {
	Driver           string            `json:"driver"`
	VolumeHandle     string            `json:"volumeHandle"`
	FSType           string            `json:"fsType,omitempty"`
	ReadOnly         bool              `json:"readOnly,omitempty"`
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`
	// NodeStageSecretRef names a Secret holding CHAP username/password
	// for Kairon's iSCSI driver. Resolved only when kairon-node's
	// CSIChapSecretNamespace is set and the ref's namespace matches that
	// allowlist -- see docs/guides/machine-storage-csi.md.
	NodeStageSecretRef *SecretReference `json:"nodeStageSecretRef,omitempty"`
	// NodePublishSecretRef is sent to third-party drivers only, from the
	// same namespace allowlist as NodeStageSecretRef (see
	// docs/guides/machine-storage-thirdparty-csi.md).
	NodePublishSecretRef *SecretReference `json:"nodePublishSecretRef,omitempty"`
}

// CSIDriver mirrors the storage.k8s.io/v1 fields kairon-node reads to
// decide whether a third-party driver needs ControllerPublish before
// NodeStage.
type CSIDriver struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta    `json:"metadata"`
	Spec     CSIDriverSpec `json:"spec"`
}

type CSIDriverSpec struct {
	AttachRequired *bool `json:"attachRequired,omitempty"`
}

// VolumeAttachment mirrors storage.k8s.io/v1 VolumeAttachment. The
// driver's external-attacher sidecar watches these, calls
// ControllerPublishVolume, and reports the result in status.
type VolumeAttachment struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta             `json:"metadata"`
	Spec     VolumeAttachmentSpec   `json:"spec"`
	Status   VolumeAttachmentStatus `json:"status,omitempty"`
}

type VolumeAttachmentSpec struct {
	Attacher string                 `json:"attacher"`
	NodeName string                 `json:"nodeName"`
	Source   VolumeAttachmentSource `json:"source"`
}

type VolumeAttachmentSource struct {
	PersistentVolumeName *string `json:"persistentVolumeName,omitempty"`
}

type VolumeAttachmentStatus struct {
	Attached           bool              `json:"attached"`
	AttachmentMetadata map[string]string `json:"attachmentMetadata,omitempty"`
	AttachError        *VolumeError      `json:"attachError,omitempty"`
	DetachError        *VolumeError      `json:"detachError,omitempty"`
}

type VolumeError struct {
	Message string `json:"message,omitempty"`
}

// SecretReference mirrors core/v1's SecretReference -- name plus optional
// namespace (empty means "use the agent's CSIChapSecretNamespace").
type SecretReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}
