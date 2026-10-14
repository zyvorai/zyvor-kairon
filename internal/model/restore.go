// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "fmt"

const KindMachineSnapshotRestore = "MachineSnapshotRestore"

// LabelRestoredBy names the MachineSnapshotRestore that created a Machine.
const LabelRestoredBy = "kairon.zyvor.dev/restored-by"

// MachineSnapshotRestore restores one volume of a Succeeded MachineSnapshot
// into a brand-new PersistentVolumeClaim via the standard CSI
// spec.dataSource restore flow. There is no such thing as an in-place PVC
// restore in Kubernetes -- a bound PVC's dataSource can't be swapped after
// the fact -- so "restore" and "clone-from-snapshot" are the same operation
// here: point a new Machine's spec.volumes[0].claimName at
// status.restoredClaimName once this reaches Succeeded (see
// docs/guides/machine-snapshot-restore.md). With spec.machine set, the
// controller also creates that Machine from the snapshot's recorded
// source spec, booting from the restored claim -- KubeVirt's
// VirtualMachineRestore.
type MachineSnapshotRestore struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                   `json:"metadata"`
	Spec     MachineSnapshotRestoreSpec   `json:"spec"`
	Status   MachineSnapshotRestoreStatus `json:"status,omitempty"`
}

func (r MachineSnapshotRestore) Namespace() string {
	return r.Metadata.Namespace
}

type MachineSnapshotRestoreList struct {
	TypeMeta `json:",inline"`
	Items    []MachineSnapshotRestore `json:"items"`
}

type MachineSnapshotRestoreSpec struct {
	SnapshotName string `json:"snapshotName"`
	// VolumeName selects which of the MachineSnapshot's volumes to restore
	// (matches Machine.spec.volumes[].name at snapshot time) -- optional
	// when the snapshot covers exactly one volume.
	VolumeName string `json:"volumeName,omitempty"`
	// TargetClaimName is the name for the new PersistentVolumeClaim.
	TargetClaimName string `json:"targetClaimName"`
	// StorageClassName and StorageSize are optional overrides. StorageSize
	// defaults to the VolumeSnapshot's own reported restoreSize when unset.
	StorageClassName string `json:"storageClassName,omitempty"`
	StorageSize      string `json:"storageSize,omitempty"`
	// Machine, when set, also creates a Machine that boots from the
	// restored claim.
	Machine *RestoreMachineSpec `json:"machine,omitempty"`
}

// RestoreMachineSpec names the Machine a restore creates. The new Machine
// copies the snapshot's source spec with the restored volume as its boot
// volume; see RestoredMachineSpec for what is dropped.
type RestoreMachineSpec struct {
	Name string `json:"name"`
	// PowerState is Running or Stopped (default Stopped), so the restored
	// guest does not come up next to the original by surprise.
	PowerState string `json:"powerState,omitempty"`
}

// RestoredMachineSpec builds the Machine spec a restore creates from
// source: the restored claim replaces the snapshotted volume, which must
// be the boot volume (volumes[0]). Everything that would collide with the
// original Machine or pin the copy to its node is dropped: other volumes
// and data disks (they still belong to the original), the node, the
// topology allocation, NIC MACs, the tap name and fixed virtual-network
// or floating addresses.
func RestoredMachineSpec(source MachineSpec, volumeName, claimName, powerState string) (MachineSpec, error) {
	if len(source.Volumes) == 0 {
		return MachineSpec{}, fmt.Errorf("the snapshot's Machine had no spec.volumes")
	}
	if source.Volumes[0].Name != volumeName {
		return MachineSpec{}, fmt.Errorf("spec.machine needs the boot volume (%q, volumes[0]) restored, not %q", source.Volumes[0].Name, volumeName)
	}
	switch powerState {
	case "":
		powerState = "Stopped"
	case "Running", "Stopped":
	default:
		return MachineSpec{}, fmt.Errorf("spec.machine.powerState %q: must be Running or Stopped", powerState)
	}
	out := source
	boot := source.Volumes[0]
	boot.ClaimName = claimName
	boot.Atlas = nil
	out.Volumes = []MachineVolume{boot}
	out.Disks = nil
	out.NodeName = ""
	out.TopologyAllocation = nil
	out.Network.MAC = ""
	out.Network.TapName = ""
	if len(source.Network.ExtraInterfaces) > 0 {
		out.Network.ExtraInterfaces = append([]ExtraInterface(nil), source.Network.ExtraInterfaces...)
		for i := range out.Network.ExtraInterfaces {
			out.Network.ExtraInterfaces[i].MAC = ""
		}
	}
	if vn := source.Network.VirtualNetwork; vn != nil {
		cp := *vn
		cp.IP, cp.FloatingIP = "", ""
		out.Network.VirtualNetwork = &cp
	}
	out.PowerState = powerState
	return out, nil
}

type MachineSnapshotRestoreStatus struct {
	Phase string `json:"phase,omitempty"`
	// No omitempty -- see model.MachineStatus.Message's comment: this
	// status is patched as one whole object, so an omitted key never
	// clears a previously-set value under JSON merge-patch semantics.
	Message           string `json:"message"`
	RestoredClaimName string `json:"restoredClaimName,omitempty"`
	// RestoredMachineName is the Machine created for spec.machine.
	RestoredMachineName string `json:"restoredMachineName,omitempty"`
	// AtlasJobID tracks an Atlas snapshot restore job.
	AtlasJobID string `json:"atlasJobID,omitempty"`
}
