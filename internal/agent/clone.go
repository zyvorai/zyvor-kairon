// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

func (a *Agent) cloneDir() string {
	return filepath.Join(a.ImageCacheDir, "clones")
}

func (a *Agent) clonePath(m model.Machine) string {
	return filepath.Join(a.cloneDir(), m.Namespace()+"_"+m.Metadata.Name+".qcow2")
}

// cloneBootDisk returns the flattened copy of m's clone-from source disk,
// starting the copy on first call. ready is false while qemu-img runs.
// The copy is kept for as long as m exists: m's runtime boots from it.
func (a *Agent) cloneBootDisk(ctx context.Context, m model.Machine) (path string, ready bool, err error) {
	srcName := m.Metadata.Annotations[model.AnnotationCloneFrom]
	if a.ImageCacheDir == "" {
		return "", false, fmt.Errorf("clone from %s needs kairon-node --image-cache-dir", srcName)
	}
	if len(m.Spec.Volumes) > 0 || len(m.Spec.Disks) > 0 {
		return "", false, fmt.Errorf("clone %s/%s must not have volumes or disks: it boots from a copy of %s's disk", m.Namespace(), m.Metadata.Name, srcName)
	}
	dst := a.clonePath(m)
	key := m.Namespace() + "/" + m.Metadata.Name
	if v, ok := a.cloneJobs.Load(key); ok {
		job := v.(*exportJob)
		select {
		case <-job.done:
		default:
			return "", false, nil
		}
		a.cloneJobs.Delete(key)
		if job.err != nil {
			return "", false, fmt.Errorf("clone from %s: %w", srcName, job.err)
		}
		// The source must not have run while it was copied.
		src, err := a.Kube.GetMachine(ctx, m.Namespace(), srcName)
		if err != nil || src.Status.Phase != "Halted" {
			_ = os.Remove(dst)
			return "", false, fmt.Errorf("clone source %s/%s left Halted while its disk was copied; retrying", m.Namespace(), srcName)
		}
		return dst, true, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return dst, true, nil
	}
	src, err := a.Kube.GetMachine(ctx, m.Namespace(), srcName)
	if err != nil {
		return "", false, fmt.Errorf("clone source %s/%s: %w", m.Namespace(), srcName, err)
	}
	if src.Spec.NodeName != a.NodeName {
		return "", false, fmt.Errorf("clone source %s/%s is on node %q, not %q", m.Namespace(), srcName, src.Spec.NodeName, a.NodeName)
	}
	if src.Status.Phase != "Halted" {
		return "", false, fmt.Errorf("clone source %s/%s is %q; halt it first (kaironctl halt %s)", m.Namespace(), srcName, src.Status.Phase, srcName)
	}
	disk, err := a.exportSourceDisk(ctx, src)
	if err != nil {
		return "", false, err
	}
	job := &exportJob{done: make(chan struct{}), dst: dst}
	if _, loaded := a.cloneJobs.LoadOrStore(key, job); !loaded {
		a.Log.Info("cloning machine disk", "namespace", m.Namespace(), "machine", m.Metadata.Name, "source", srcName)
		go func() {
			defer close(job.done)
			job.err = convertDisk(disk, dst, "qcow2")
		}()
	}
	return "", false, nil
}

// sweepClones removes clone copies whose Machine no longer exists here.
func (a *Agent) sweepClones(local map[string]model.Machine) {
	if a.ImageCacheDir == "" {
		return
	}
	entries, err := os.ReadDir(a.cloneDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".qcow2")
		if !ok {
			continue
		}
		ns, machine, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		key := ns + "/" + machine
		if _, live := local[key]; live {
			continue
		}
		if _, running := a.cloneJobs.Load(key); running {
			continue
		}
		_ = os.Remove(filepath.Join(a.cloneDir(), e.Name()))
	}
}
