// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/diskexport"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type exportJob struct {
	done chan struct{}
	dst  string
	err  error
}

// reconcileExports prepares MachineExports whose Machine lives on this node
// and drops registry entries for exports that are gone.
func (a *Agent) reconcileExports(ctx context.Context, machines map[string]model.Machine) error {
	exports, err := a.Kube.ListMachineExports(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	seen := map[string]bool{}
	for _, e := range exports {
		key := diskexport.Key(e.Namespace(), e.Metadata.Name)
		m, ok := machines[e.Namespace()+"/"+e.Spec.MachineName]
		if !ok {
			if e.Status.NodeName == a.NodeName && !model.ExportTerminal(e.Status.Phase) {
				seen[key] = true
				a.dropExport(key)
				_ = a.patchExport(ctx, e, model.MachineExportStatus{Phase: "Failed", NodeName: a.NodeName, Message: "Machine " + e.Spec.MachineName + " is no longer on this node"})
			}
			continue
		}
		seen[key] = true
		if err := a.reconcileExport(ctx, e, m); err != nil {
			a.Log.Error("machine export failed", "namespace", e.Namespace(), "export", e.Metadata.Name, "error", err)
		}
	}
	for _, key := range a.Exports.Keys() {
		if !seen[key] {
			a.dropExport(key)
		}
	}
	a.exportJobs.Range(func(k, _ any) bool {
		if !seen[k.(string)] {
			a.dropExport(k.(string))
		}
		return true
	})
	return nil
}

func (a *Agent) reconcileExport(ctx context.Context, e model.MachineExport, m model.Machine) error {
	key := diskexport.Key(e.Namespace(), e.Metadata.Name)
	if model.ExportTerminal(e.Status.Phase) {
		a.dropExport(key)
		return nil
	}
	status := model.MachineExportStatus{NodeName: a.NodeName}
	fail := func(msg string) error {
		a.dropExport(key)
		status.Phase, status.Message = "Failed", msg
		return a.patchExport(ctx, e, status)
	}
	if err := model.ValidateMachineExportSpec(e.Spec); err != nil {
		return fail(err.Error())
	}
	ttl, _ := model.ExportTTL(e.Spec)
	expires := e.Metadata.CreationTimestamp.Add(ttl).UTC().Truncate(time.Second)
	status.ExpiresAt = &expires
	if time.Now().After(expires) {
		a.dropExport(key)
		status.Phase, status.Message = "Expired", "TTL elapsed"
		return a.patchExport(ctx, e, status)
	}
	if m.Status.Phase != "Stopped" && m.Status.Phase != "Halted" {
		a.dropExport(key)
		status.Phase = "Pending"
		status.Message = fmt.Sprintf("waiting for Machine %s to be Stopped or Halted (phase %q)", m.Metadata.Name, m.Status.Phase)
		return a.patchExport(ctx, e, status)
	}
	src, err := a.exportSourceDisk(ctx, m)
	if err != nil {
		return fail(err.Error())
	}
	srcFormat, backed, err := diskFormat(src)
	if err != nil {
		return fail(err.Error())
	}
	want := e.Spec.Format
	if want == "" {
		want = srcFormat
	}
	status.Format = want
	path, temp := src, false
	// A qcow2 overlay is useless off this node without its backing chain;
	// qemu-img convert flattens it into a standalone image.
	if want != srcFormat || backed {
		job, started := a.exportJob(key, src, want)
		select {
		case <-job.done:
		default:
			status.Phase = "Preparing"
			status.Message = fmt.Sprintf("converting %s to %s", srcFormat, want)
			if want == srcFormat {
				status.Message = "flattening the qcow2 overlay into a standalone image"
			}
			if started {
				a.Log.Info("machine export converting", "namespace", e.Namespace(), "export", e.Metadata.Name, "from", srcFormat, "to", want)
			}
			return a.patchExport(ctx, e, status)
		}
		if job.err != nil {
			return fail(job.err.Error())
		}
		path, temp = job.dst, true
	}
	size, err := diskSize(path)
	if err != nil {
		return fail(err.Error())
	}
	a.Exports.Put(key, diskexport.Entry{Path: path, Format: want, Expires: expires, Temp: temp})
	status.Phase = "Ready"
	status.SizeBytes = size
	status.DownloadPath = model.ExportDownloadPath(e.Namespace(), e.Metadata.Name)
	status.Message = ""
	return a.patchExport(ctx, e, status)
}

// exportSourceDisk returns the host path of m's boot disk while it is not
// running. A Halted Machine keeps its FluxVM record; a Stopped one only
// keeps disks that live on a PersistentVolume.
func (a *Agent) exportSourceDisk(ctx context.Context, m model.Machine) (string, error) {
	if m.Status.Phase == "Halted" {
		rec, err := a.current(ctx, m)
		if err != nil {
			return "", err
		}
		if rec != nil && rec.Disk != "" {
			return rec.Disk, nil
		}
	}
	if len(m.Spec.Volumes) == 0 {
		return "", fmt.Errorf("a Stopped Machine without spec.volumes has no disk left (FluxVM removes it with the VM); halt the Machine instead of stopping it")
	}
	vol := m.Spec.Volumes[0]
	if vol.Atlas.EffectiveMode() == model.AtlasModeRBD {
		return "", fmt.Errorf("atlas rbd boot volumes can't be exported yet")
	}
	pvc, err := a.Kube.GetPersistentVolumeClaim(ctx, m.Namespace(), vol.ClaimName)
	if err != nil {
		return "", fmt.Errorf("get PersistentVolumeClaim %s: %w", vol.ClaimName, err)
	}
	if pvc.Spec.VolumeName != "" {
		pv, err := a.Kube.GetPersistentVolume(ctx, pvc.Spec.VolumeName)
		if err != nil {
			return "", fmt.Errorf("get PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
		}
		if pv.Spec.CSI != nil {
			return "", fmt.Errorf("CSI-backed boot volumes can't be exported yet (PV %s); halt the Machine to export from its FluxVM disk", pv.Metadata.Name)
		}
	}
	path, _, err := a.resolveBootDiskPath(ctx, m)
	return path, err
}

// exportJob returns the conversion job for key, starting it if needed.
func (a *Agent) exportJob(key, src, format string) (*exportJob, bool) {
	dst := filepath.Join(a.exportDir(), strings.ReplaceAll(key, "/", "_")+"."+format)
	job := &exportJob{done: make(chan struct{}), dst: dst}
	if v, loaded := a.exportJobs.LoadOrStore(key, job); loaded {
		return v.(*exportJob), false
	}
	go func() {
		defer close(job.done)
		job.err = convertDisk(src, dst, format)
	}()
	return job, true
}

func (a *Agent) exportDir() string {
	if a.ExportDir != "" {
		return a.ExportDir
	}
	return "/var/lib/kairon-node/exports"
}

// dropExport stops serving key and removes any converted copy once its
// conversion has finished.
func (a *Agent) dropExport(key string) {
	if e, ok := a.Exports.Delete(key); ok && e.Temp {
		_ = os.Remove(e.Path)
	}
	v, ok := a.exportJobs.Load(key)
	if !ok {
		return
	}
	job := v.(*exportJob)
	select {
	case <-job.done:
		a.exportJobs.Delete(key)
		_ = os.Remove(job.dst)
	default:
	}
}

func (a *Agent) patchExport(ctx context.Context, e model.MachineExport, status model.MachineExportStatus) error {
	if exportStatusEqual(e.Status, status) {
		return nil
	}
	return a.Kube.PatchMachineExportStatus(ctx, e.Namespace(), e.Metadata.Name, status)
}

func exportStatusEqual(a, b model.MachineExportStatus) bool {
	sameTime := (a.ExpiresAt == nil) == (b.ExpiresAt == nil) && (a.ExpiresAt == nil || a.ExpiresAt.Equal(*b.ExpiresAt))
	a.ExpiresAt, b.ExpiresAt = nil, nil
	return sameTime && a == b
}

// diskFormat sniffs path's format and, for qcow2, whether the header names a
// backing file (bytes 8-15, big-endian backing_file_offset).
func diskFormat(path string) (format string, backed bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("open disk: %w", err)
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, 16)
	n, err := io.ReadFull(f, hdr)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", false, fmt.Errorf("read disk %s: %w", path, err)
	}
	if n < len(qcow2Magic) || !bytes.Equal(hdr[:len(qcow2Magic)], qcow2Magic) {
		return "raw", false, nil
	}
	return "qcow2", n == len(hdr) && binary.BigEndian.Uint64(hdr[8:16]) != 0, nil
}

func diskSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open disk: %w", err)
	}
	defer func() { _ = f.Close() }()
	return f.Seek(0, io.SeekEnd)
}

func convertDisk(src, dst, format string) error {
	if _, err := exec.LookPath(qemuImg); err != nil {
		return fmt.Errorf("converting to %s needs qemu-img on kairon-node's PATH", format)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("export dir: %w", err)
	}
	tmp := dst + ".part"
	_ = os.Remove(tmp)
	args := []string{"convert", "-O", format}
	if format == "qcow2" {
		args = append(args, "-c")
	}
	args = append(args, src, tmp)
	if out, err := exec.Command(qemuImg, args...).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("qemu-img convert to %s: %w: %s", format, err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, dst)
}
