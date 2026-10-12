// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileDevices hot-attaches spec.disks and spec.network.extraInterfaces
// to a running VM and removes the ones dropped from spec. FluxVM's disk list
// and VM record are the source of truth for what is plugged in, so a VM
// recreated from scratch gets everything re-attached; status only records
// which devices Kairon owns, so removal never touches anything else.
// Errors are joined so one bad device doesn't block the others.
func (a *Agent) reconcileDevices(ctx context.Context, m model.Machine, rec *fluxvm.Record) ([]string, []model.DiskVolume, []model.AttachedInterface, error) {
	if normalizePhase(rec.Status) != "Running" {
		return m.Status.AttachedDisks, m.Status.DiskVolumes, m.Status.AttachedInterfaces, nil
	}
	disks, volumes, diskErr := a.reconcileDisks(ctx, m, rec)
	ifaces, nicErr := a.reconcileInterfaces(ctx, m, rec)
	return disks, volumes, ifaces, errors.Join(diskErr, nicErr)
}

// reconcileDisks also returns the CSI volumes published for CSI-backed
// disks. A volume is unpublished once its disk is out of spec and detached.
func (a *Agent) reconcileDisks(ctx context.Context, m model.Machine, rec *fluxvm.Record) ([]string, []model.DiskVolume, error) {
	if len(m.Spec.Disks) == 0 && len(m.Status.AttachedDisks) == 0 && len(m.Status.DiskVolumes) == 0 {
		return nil, nil, nil
	}
	if err := model.ValidateDisks(m.Spec.Disks); err != nil {
		return m.Status.AttachedDisks, m.Status.DiskVolumes, err
	}
	list, err := a.Flux.ListDisks(ctx, rec.ID())
	if err != nil {
		return m.Status.AttachedDisks, m.Status.DiskVolumes, fmt.Errorf("list disks: %w", err)
	}
	volumes := map[string]model.DiskVolume{}
	var foreign []model.DiskVolume
	for _, v := range m.Status.DiskVolumes {
		if v.Node == a.NodeName {
			volumes[v.Name] = v
		} else {
			foreign = append(foreign, v)
		}
	}
	present := map[string]bool{}
	for _, d := range list {
		present[d.Name] = true
	}
	want := map[string]bool{}
	var errs []error
	for _, d := range m.Spec.Disks {
		want[d.Name] = true
		if present[d.Name] {
			continue
		}
		path, vol, err := a.resolveDiskPath(ctx, m, d, volumes[d.Name])
		if vol != nil {
			volumes[d.Name] = *vol
		}
		if err == nil {
			err = a.Flux.AttachDisk(ctx, rec.ID(), d.Name, path, d.IO)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("attach disk %q: %w", d.Name, err))
			continue
		}
		present[d.Name] = true
		a.Log.Info("attached disk", "machine", m.Metadata.Name, "disk", d.Name, "path", path)
	}
	for _, name := range m.Status.AttachedDisks {
		if want[name] || !present[name] {
			continue
		}
		if err := a.Flux.DetachDisk(ctx, rec.ID(), name); err != nil {
			errs = append(errs, fmt.Errorf("detach disk %q: %w", name, err))
			continue
		}
		present[name] = false
		a.Log.Info("detached disk", "machine", m.Metadata.Name, "disk", name)
	}
	for name, v := range volumes {
		if want[name] || present[name] {
			continue
		}
		if err := a.unpublishDiskCSI(ctx, v); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(volumes, name)
		a.Log.Info("unpublished disk volume", "machine", m.Metadata.Name, "disk", name, "driver", v.Driver)
	}
	out := foreign
	for _, v := range volumes {
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y model.DiskVolume) int { return strings.Compare(x.Node+"/"+x.Name, y.Node+"/"+y.Name) })
	var attached []string
	for _, d := range m.Spec.Disks {
		if present[d.Name] {
			attached = append(attached, d.Name)
		}
	}
	for _, name := range m.Status.AttachedDisks {
		if !want[name] && present[name] {
			attached = append(attached, name)
		}
	}
	return attached, out, errors.Join(errs...)
}

// resolveDiskPath maps a spec.disks claim to the host path FluxVM attaches:
// the device of a Block-mode hostPath/local PV, disk.img inside a
// Filesystem-mode one, or the published path of a CSI-backed one. For CSI
// it also returns the volume record; known is the one already published
// for this disk on this node, if any.
func (a *Agent) resolveDiskPath(ctx context.Context, m model.Machine, d model.MachineDisk, known model.DiskVolume) (string, *model.DiskVolume, error) {
	pvc, err := a.Kube.GetPersistentVolumeClaim(ctx, m.Namespace(), d.ClaimName)
	if err != nil {
		return "", nil, fmt.Errorf("get PersistentVolumeClaim %s: %w", d.ClaimName, err)
	}
	if pvc.Status.Phase != "Bound" || strings.TrimSpace(pvc.Spec.VolumeName) == "" {
		return "", nil, fmt.Errorf("PersistentVolumeClaim %s is not Bound yet (phase=%q)", d.ClaimName, pvc.Status.Phase)
	}
	pv, err := a.Kube.GetPersistentVolume(ctx, pvc.Spec.VolumeName)
	if err != nil {
		return "", nil, fmt.Errorf("get PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.VolumeMode != "" && pv.Spec.VolumeMode != "Filesystem" && pv.Spec.VolumeMode != "Block" {
		return "", nil, fmt.Errorf("PersistentVolume %s: volumeMode %q is not supported", pv.Metadata.Name, pv.Spec.VolumeMode)
	}
	if src := pv.Spec.CSI; src != nil {
		if known.VolumeHandle == src.VolumeHandle && known.Driver == src.Driver {
			path, err := diskVolumePath(known)
			return path, &known, err
		}
		if known.VolumeHandle != "" {
			return "", nil, fmt.Errorf("disk %q already has CSI volume %s published; remove the disk from spec.disks before pointing it at another claim", d.Name, known.VolumeHandle)
		}
		v, err := a.publishDiskCSI(ctx, m, d.Name, pv)
		if err != nil {
			return "", nil, err
		}
		path, err := diskVolumePath(v)
		return path, &v, err
	}
	dir, err := hostDirForPV(pv)
	if err != nil {
		return "", nil, err
	}
	if pv.Spec.VolumeMode == "Block" {
		return dir, nil, nil
	}
	return filepath.Join(dir, bootDiskFileName), nil, nil
}

func (a *Agent) reconcileInterfaces(ctx context.Context, m model.Machine, rec *fluxvm.Record) ([]model.AttachedInterface, error) {
	ns := m.Spec.Network
	if len(ns.ExtraInterfaces) == 0 && len(m.Status.AttachedInterfaces) == 0 {
		return nil, nil
	}
	if err := model.ValidateExtraInterfaces(ns); err != nil {
		return m.Status.AttachedInterfaces, err
	}
	present := map[string]bool{}
	for _, n := range rec.Request.Network.Extra {
		if n.TapName != "" {
			present[strings.ToLower(n.MAC)] = true
		}
	}
	wantMAC := map[string]bool{}
	var errs []error
	var attached []model.AttachedInterface
	for _, iface := range ns.ExtraInterfaces {
		mac := model.ExtraInterfaceMAC(m.Metadata.UID, iface)
		wantMAC[mac] = true
		if !present[mac] {
			if err := a.Flux.HotplugNIC(ctx, rec.ID(), iface.Bridge, mac); err != nil {
				errs = append(errs, fmt.Errorf("hotplug interface %q: %w", iface.Name, err))
				continue
			}
			present[mac] = true
			a.Log.Info("hot-added interface", "machine", m.Metadata.Name, "interface", iface.Name, "mac", mac)
		}
		attached = append(attached, model.AttachedInterface{Name: iface.Name, Bridge: iface.Bridge, MAC: mac})
	}
	for _, old := range m.Status.AttachedInterfaces {
		mac := strings.ToLower(old.MAC)
		if wantMAC[mac] || !present[mac] {
			continue
		}
		if err := a.Flux.UnplugNIC(ctx, rec.ID(), mac); err != nil {
			errs = append(errs, fmt.Errorf("unplug interface %q: %w", old.Name, err))
			attached = append(attached, old)
			continue
		}
		a.Log.Info("unplugged interface", "machine", m.Metadata.Name, "interface", old.Name, "mac", mac)
	}
	return attached, errors.Join(errs...)
}
