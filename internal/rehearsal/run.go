// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package rehearsal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type Resource struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	UID     string `json:"uid,omitempty"`
	Deleted bool   `json:"deleted"`
}

type Result struct {
	ExpectedExitCode int64  `json:"expectedExitCode"`
	Stage            string `json:"stage"`
	Name             string `json:"name"`
	Machine          string `json:"machine"`
	ExitCode         int64  `json:"exitCode"`
	DurationMillis   int64  `json:"durationMillis"`
	StdoutSHA256     string `json:"stdoutSHA256"`
	StderrSHA256     string `json:"stderrSHA256"`
}

type Source struct {
	RecoveryPointAt *time.Time        `json:"recoveryPointAt,omitempty"`
	SourceSnapshot  string            `json:"sourceSnapshot,omitempty"`
	GuestReady      bool              `json:"guestReady"`
	Name            string            `json:"name"`
	UID             string            `json:"uid"`
	ResourceVersion string            `json:"resourceVersion"`
	Copy            string            `json:"copy"`
	Snapshots       map[string]string `json:"snapshots"`
}

type Report struct {
	ApplicationReadyAt      *time.Time        `json:"applicationReadyAt,omitempty"`
	ApplicationRTOSeconds   *float64          `json:"applicationRTOSeconds,omitempty"`
	RecoveryPointAgeSeconds *float64          `json:"recoveryPointAgeSeconds,omitempty"`
	Investigation           []json.RawMessage `json:"investigation,omitempty"`
	ReservationPending      bool              `json:"reservationPending"`
	Version                 int               `json:"version"`
	RunID                   string            `json:"runID"`
	Plan                    Plan              `json:"plan"`
	StartedAt               time.Time         `json:"startedAt"`
	FinishedAt              *time.Time        `json:"finishedAt,omitempty"`
	Phase                   string            `json:"phase"`
	Error                   string            `json:"error,omitempty"`
	Cleaned                 bool              `json:"cleaned"`
	Sources                 []Source          `json:"sources"`
	Resources               []Resource        `json:"resources"`
	Results                 []Result          `json:"results"`
	Warnings                []string          `json:"warnings"`
}

type ExecResult struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type Runner struct {
	Investigate    func(context.Context, string, string) (json.RawMessage, error)
	Kube           *kube.Client
	Exec           func(context.Context, string, string, Step) (ExecResult, error)
	Save           func(Report) error
	PollInterval   time.Duration
	CleanupTimeout time.Duration
}

type memberState struct {
	source             model.Machine
	claims             []model.PersistentVolumeClaim
	restored           map[string]string
	snapshots          map[string]string
	copy               string
	recoveryPointAt    *time.Time
	sourceSnapshot     string
	recoveryPointKnown bool
}

func (r *Runner) poll(ctx context.Context, f func() (bool, error)) error {
	interval := r.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := f()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runner) save(out *Report) error {
	if r.Save == nil {
		return fmt.Errorf("a durable report writer is required")
	}
	return r.Save(*out)
}

// Run records each create intent before sending it, including ambiguous
// transport failures, so cleanup can discover resources created by a lost POST.
// CSI snapshots in crash mode are not an atomic distributed recovery point.
func (r *Runner) Run(parent context.Context, p Plan) (out Report, retErr error) {
	if err := p.Validate(); err != nil {
		return out, err
	}
	if r.Kube == nil || r.Exec == nil {
		return out, fmt.Errorf("kubernetes client and guest executor are required")
	}
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		return out, err
	}
	id := "rh-" + hex.EncodeToString(token)
	out = Report{Version: 1, RunID: id, Plan: p, StartedAt: time.Now().UTC(), Phase: "Preflight",
		Warnings: []string{"Experimental: requires hardware qualification on your CSI/FluxVM stack.", "Existing snapshots retain their externally established consistency; crash mode snapshots are independently timed, not application-atomic.", "The snapshot contains guest credentials and services; the isolated bridge must remain reserved for this run."}}
	if err := r.save(&out); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	defer func() {
		timeout := r.CleanupTimeout
		if timeout <= 0 {
			timeout = 2 * time.Minute
		}
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), timeout)
		defer cleanCancel()
		cleanupErr := r.Cleanup(cleanCtx, &out)
		retErr = errors.Join(retErr, cleanupErr)
		now := time.Now().UTC()
		out.FinishedAt = &now
		if retErr != nil {
			out.Phase = "Failed"
			out.Error = retErr.Error()
		} else {
			out.Phase = "Succeeded"
		}
		retErr = errors.Join(retErr, r.save(&out))
	}()
	states, err := r.preflight(ctx, p, id)
	if err != nil {
		return out, err
	}
	out.ReservationPending = true
	if err := r.save(&out); err != nil {
		return out, err
	}
	if err := r.reserve(ctx, &out); err != nil {
		return out, err
	}
	for _, s := range states {
		out.Sources = append(out.Sources, Source{RecoveryPointAt: s.recoveryPointAt, SourceSnapshot: s.sourceSnapshot, Name: s.source.Metadata.Name, UID: s.source.Metadata.UID, ResourceVersion: s.source.Metadata.ResourceVersion, Copy: s.copy, Snapshots: s.snapshots})
	}
	if err := r.save(&out); err != nil {
		return out, err
	}
	// Capture every volume before creating any clone. Sources are never exec'd,
	// powered off, upgraded, or patched by this runner.
	out.Phase = "Snapshotting"
	for i := range states {
		s := &states[i]
		for j, v := range s.source.Spec.Volumes {
			name := s.snapshots[v.Name]
			if p.Consistency == "crash" {
				name = fmt.Sprintf("%s-m%d-v%d-s", id, i, j)
				s.snapshots[v.Name] = name
				out.Sources[i].Snapshots = s.snapshots
				claim := v.ClaimName
				snap := model.VolumeSnapshot{TypeMeta: model.TypeMeta{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot"}, Metadata: ownedMeta(p.Namespace, name, id), Spec: model.VolumeSnapshotSpec{Source: model.VolumeSnapshotSource{PersistentVolumeClaimName: &claim}}}
				if p.SnapshotClass != "" {
					snap.Spec.VolumeSnapshotClassName = &p.SnapshotClass
				}
				if err := r.intent(&out, "volumesnapshots", name); err != nil {
					return out, err
				}
				made, err := r.Kube.CreateVolumeSnapshot(ctx, p.Namespace, snap)
				if err != nil {
					return out, err
				}
				out.Resources[len(out.Resources)-1].UID = made.Metadata.UID
				if err := r.save(&out); err != nil {
					return out, err
				}
			}
			if err := r.poll(ctx, func() (bool, error) {
				snap, err := r.Kube.GetVolumeSnapshot(ctx, p.Namespace, name)
				if err != nil {
					return false, err
				}
				if snap.Status.Error != nil {
					return false, fmt.Errorf("snapshot %s failed", name)
				}
				return snap.Status.ReadyToUse != nil && *snap.Status.ReadyToUse, nil
			}); err != nil {
				return out, err
			}
		}
	}
	out.Phase = "Restoring"
	for i := range states {
		s := &states[i]
		for j, v := range s.source.Spec.Volumes {
			snap, err := r.Kube.GetVolumeSnapshot(ctx, p.Namespace, s.snapshots[v.Name])
			if err != nil {
				return out, err
			}
			if snap.Status.RestoreSize == nil || *snap.Status.RestoreSize == "" {
				return out, fmt.Errorf("snapshot %s has no restore size", snap.Metadata.Name)
			}
			name := s.restored[v.Name]
			original := s.claims[j]
			pvc := model.PersistentVolumeClaim{TypeMeta: model.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, Metadata: ownedMeta(p.Namespace, name, id), Spec: model.PersistentVolumeClaimSpec{
				StorageClassName: original.Spec.StorageClassName, VolumeMode: original.Spec.VolumeMode, AccessModes: []string{"ReadWriteOnce"},
				Resources:  &model.PersistentVolumeClaimResources{Requests: map[string]string{"storage": *snap.Status.RestoreSize}},
				DataSource: &model.TypedLocalObjectReference{APIGroup: "snapshot.storage.k8s.io", Kind: "VolumeSnapshot", Name: snap.Metadata.Name},
			}}
			if err := r.intent(&out, "persistentvolumeclaims", name); err != nil {
				return out, err
			}
			made, err := r.Kube.CreatePersistentVolumeClaim(ctx, p.Namespace, pvc)
			if err != nil {
				return out, err
			}
			out.Resources[len(out.Resources)-1].UID = made.Metadata.UID
			if err := r.save(&out); err != nil {
				return out, err
			}
		}
	}
	// Create consumers before waiting on PVC binding (WaitForFirstConsumer).
	out.Phase = "Booting"
	for _, s := range states {
		spec, err := CopySpec(s.source, s.restored, p)
		if err != nil {
			return out, err
		}
		m := model.Machine{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: "Machine"}, Metadata: ownedMeta(p.Namespace, s.copy, id), Spec: spec}
		m.Metadata.Annotations = map[string]string{GuardAnnotation: "v1"}
		if err := r.intent(&out, "machines", s.copy); err != nil {
			return out, err
		}
		made, err := r.Kube.CreateMachine(ctx, p.Namespace, m)
		if err != nil {
			return out, err
		}
		out.Resources[len(out.Resources)-1].UID = made.Metadata.UID
		if err := r.save(&out); err != nil {
			return out, err
		}
	}
	for _, s := range states {
		if err := r.poll(ctx, func() (bool, error) {
			m, err := r.Kube.GetMachine(ctx, p.Namespace, s.copy)
			if err != nil {
				return false, err
			}
			if err := ownedCopy(m, out, s.copy); err != nil {
				return false, err
			}
			if m.Status.Phase == "Failed" || m.Status.Phase == "Blocked" {
				return false, fmt.Errorf("copy %s failed to boot", s.copy)
			}
			return m.Status.Phase == "Running" && m.Status.RuntimeID != "", nil
		}); err != nil {
			return out, err
		}
	}
	out.Phase = "GuestReady"
	for i, state := range states {
		for _, claim := range state.restored {
			if err := r.poll(ctx, func() (bool, error) {
				pvc, err := r.Kube.GetPersistentVolumeClaim(ctx, p.Namespace, claim)
				if err != nil {
					return false, err
				}
				if pvc.Status.Phase == "Lost" {
					return false, fmt.Errorf("restored claim %s is Lost", claim)
				}
				return pvc.Status.Phase == "Bound", nil
			}); err != nil {
				return out, err
			}
		}
		if err := r.poll(ctx, func() (bool, error) {
			probeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			result, err := r.Exec(probeCtx, p.Namespace, state.copy, Step{Name: "guest-ready", Path: "/bin/true", TimeoutSeconds: 5})
			// Boot readiness is retried; upgrade and verification commands are
			// never retried, since they may have performed side effects.
			return err == nil && result.ExitCode == 0, nil
		}); err != nil {
			return out, err
		}
		out.Sources[i].GuestReady = true
		if err := r.save(&out); err != nil {
			return out, err
		}
	}
	copies := map[string]string{}
	for _, s := range states {
		copies[s.source.Metadata.Name] = s.copy
	}
	for _, stage := range []struct {
		name  string
		steps []Step
	}{{"Setup", p.Setup}, {"Baseline", p.Baseline}, {"Changes", p.Changes}, {"Verify", p.Verify}} {
		out.Phase = stage.name
		for _, step := range stage.steps {
			if err := r.save(&out); err != nil {
				return out, err
			}
			m, err := r.Kube.GetMachine(ctx, p.Namespace, copies[step.Machine])
			if err != nil {
				return out, err
			}
			if err := ownedCopy(m, out, copies[step.Machine]); err != nil {
				return out, err
			}
			stepCtx, stepCancel := context.WithTimeout(ctx, time.Duration(step.TimeoutSeconds)*time.Second)
			start := time.Now()
			res, err := r.Exec(stepCtx, p.Namespace, copies[step.Machine], step)
			stepCancel()
			stdoutHash := sha256.Sum256([]byte(res.Stdout))
			stderrHash := sha256.Sum256([]byte(res.Stderr))
			out.Results = append(out.Results, Result{Stage: stage.name, ExpectedExitCode: step.ExpectedExitCode, Name: step.Name, Machine: copies[step.Machine], ExitCode: res.ExitCode, DurationMillis: time.Since(start).Milliseconds(), StdoutSHA256: hex.EncodeToString(stdoutHash[:]), StderrSHA256: hex.EncodeToString(stderrHash[:])})
			if saveErr := r.save(&out); saveErr != nil {
				return out, errors.Join(err, saveErr)
			}
			if err != nil {
				return out, fmt.Errorf("%s step %s: %w", stage.name, step.Name, err)
			}
			if res.ExitCode != step.ExpectedExitCode {
				return out, fmt.Errorf("%s step %s exited %d, expected %d", stage.name, step.Name, res.ExitCode, step.ExpectedExitCode)
			}
		}
		if p.Purpose == "incident" && stage.name == "Baseline" {
			if r.Investigate == nil {
				return out, fmt.Errorf("incident workflow requires a diagnostic adapter")
			}
			for _, s := range states {
				finding, err := r.Investigate(ctx, p.Namespace, s.copy)
				if err != nil {
					return out, err
				}
				if len(finding) > 1<<20 || !json.Valid(finding) {
					return out, fmt.Errorf("invalid or oversized investigation result")
				}
				out.Investigation = append(out.Investigation, finding)
				if err := r.save(&out); err != nil {
					return out, err
				}
			}
		}
	}
	readyAt := time.Now().UTC()
	out.ApplicationReadyAt = &readyAt
	seconds := readyAt.Sub(out.StartedAt).Seconds()
	out.ApplicationRTOSeconds = &seconds
	if p.Purpose == "recovery" {
		known := true
		var oldest *time.Time
		for _, s := range states {
			if !s.recoveryPointKnown || s.recoveryPointAt == nil {
				known = false
				continue
			}
			if oldest == nil || s.recoveryPointAt.Before(*oldest) {
				oldest = s.recoveryPointAt
			}
		}
		if known && oldest != nil {
			age := out.StartedAt.Sub(*oldest).Seconds()
			if age < 0 {
				return out, fmt.Errorf("recovery point timestamp is in the future")
			}
			out.RecoveryPointAgeSeconds = &age
		}
		if p.Objectives != nil {
			if p.Objectives.MaxRTOSeconds > 0 && seconds > p.Objectives.MaxRTOSeconds {
				return out, fmt.Errorf("application RTO objective exceeded")
			}
			if p.Objectives.MaxRPOSeconds > 0 && (out.RecoveryPointAgeSeconds == nil || *out.RecoveryPointAgeSeconds > p.Objectives.MaxRPOSeconds) {
				return out, fmt.Errorf("recovery point age unknown or RPO objective exceeded")
			}
		}
	}
	return out, nil
}

func ownedMeta(ns, name, id string) model.ObjectMeta {
	return model.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{RunLabel: id}}
}

func (r *Runner) intent(out *Report, kind, name string) error {
	out.Resources = append(out.Resources, Resource{Kind: kind, Name: name})
	return r.save(out)
}

func (r *Runner) preflight(ctx context.Context, p Plan, id string) ([]memberState, error) {
	node, err := r.Kube.GetNode(ctx, p.Node)
	if err != nil {
		return nil, err
	}
	if node.Metadata.Annotations[BridgeAnnotation] != p.Bridge || node.Metadata.Annotations[GuardAnnotation] != "v1" {
		return nil, fmt.Errorf("node needs the reserved bridge and v1 guard annotations; deploy the updated node agent and prepare the bridge first")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			ready = true
		}
	}
	if !ready {
		return nil, fmt.Errorf("rehearsal node is not Ready")
	}
	if node.Spec.Unschedulable {
		return nil, fmt.Errorf("rehearsal node is cordoned")
	}
	states := make([]memberState, 0, len(p.Members))
	claimed := map[string]bool{}
	for i, member := range p.Members {
		src, points, err := r.readSource(ctx, p, member)
		if err != nil {
			return nil, err
		}
		if src.Metadata.Labels[RunLabel] != "" {
			return nil, fmt.Errorf("cannot rehearse another rehearsal")
		}
		s := memberState{source: src, sourceSnapshot: member.SourceSnapshot, recoveryPointKnown: true, restored: map[string]string{}, snapshots: map[string]string{}, copy: fmt.Sprintf("%s-m%d", id, i)}
		for j, v := range src.Spec.Volumes {
			if claimed[v.ClaimName] {
				return nil, fmt.Errorf("shared source claim %s is unsupported; each rehearsal volume must have independent storage", v.ClaimName)
			}
			claimed[v.ClaimName] = true
			s.restored[v.Name] = fmt.Sprintf("%s-m%d-v%d", id, i, j)
			pvc, err := r.sourceClaim(ctx, p, member, v, points[v.Name])
			if err != nil {
				return nil, err
			}
			if pvc.Status.Phase != "Bound" {
				return nil, fmt.Errorf("source claim %s is not Bound", v.ClaimName)
			}
			if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
				return nil, fmt.Errorf("source claim %s needs an explicit storage class", v.ClaimName)
			}
			if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != "Filesystem" {
				return nil, fmt.Errorf("raw Block claims are not supported by rehearsal v1")
			}
			s.claims = append(s.claims, pvc)
			if p.Consistency == "existing" {
				name := points[v.Name]
				if name == "" {
					return nil, fmt.Errorf("member %s needs a snapshot for volume %s", member.Name, v.Name)
				}
				snap, err := r.Kube.GetVolumeSnapshot(ctx, p.Namespace, name)
				if err != nil {
					return nil, err
				}
				if snap.Spec.Source.PersistentVolumeClaimName == nil || *snap.Spec.Source.PersistentVolumeClaimName != v.ClaimName || snap.Status.ReadyToUse == nil || !*snap.Status.ReadyToUse {
					return nil, fmt.Errorf("snapshot %s must be ready and reference source claim %s", name, v.ClaimName)
				}
				s.snapshots[v.Name] = name
				if p.Purpose == "recovery" && snap.Status.CreationTime != nil && snap.Status.CreationTime.After(time.Now().UTC()) {
					return nil, fmt.Errorf("snapshot %s recovery point timestamp is in the future", name)
				}
				if snap.Status.CreationTime == nil {
					s.recoveryPointKnown = false
				} else if s.recoveryPointAt == nil || snap.Status.CreationTime.Before(*s.recoveryPointAt) {
					s.recoveryPointAt = snap.Status.CreationTime
				}
			}
		}
		if p.Consistency == "existing" && len(points) != len(src.Spec.Volumes) {
			return nil, fmt.Errorf("member %s snapshot map must exactly cover its volumes", member.Name)
		}
		if _, err := CopySpec(src, s.restored, p); err != nil {
			return nil, err
		}
		states = append(states, s)
	}
	return states, nil
}

// Cleanup waits for Machine finalizers before deleting claims, then waits for
// claim protection before deleting snapshots. It fails closed on ownership/UID
// mismatches. The journal can be passed again after cancellation or process loss.
func (r *Runner) Cleanup(ctx context.Context, out *Report) error {
	if out.Version != 1 || len(out.RunID) != 27 || !dnsName.MatchString(out.RunID) {
		return fmt.Errorf("invalid rehearsal report identity")
	}
	if err := out.Plan.Validate(); err != nil {
		return err
	}
	out.Cleaned = false
	for _, kind := range []string{"machines", "persistentvolumeclaims", "volumesnapshots"} {
		for i := range out.Resources {
			item := &out.Resources[i]
			if item.Deleted || item.Kind != kind {
				continue
			}
			if len(item.Name) < len(out.RunID)+1 || item.Name[:len(out.RunID)+1] != out.RunID+"-" {
				return fmt.Errorf("resource name is outside this run")
			}
			err := r.poll(ctx, func() (bool, error) {
				meta, err := r.metadata(ctx, out.Plan.Namespace, *item)
				if kube.IsNotFound(err) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				if meta.Labels[RunLabel] != out.RunID || (item.UID != "" && item.UID != meta.UID) {
					return false, fmt.Errorf("refusing to delete foreign or replaced %s/%s", kind, item.Name)
				}
				// Persist the discovered UID before retrying a possibly ambiguous delete.
				item.UID = meta.UID
				if err := r.save(out); err != nil {
					return false, err
				}
				if meta.DeletionTimestamp != nil {
					return false, nil
				}
				err = r.Kube.DeleteRehearsalResource(ctx, out.Plan.Namespace, kind, item.Name, item.UID)
				if kube.IsNotFound(err) {
					return true, nil
				}
				return false, err
			})
			if err != nil {
				return fmt.Errorf("cleanup %s/%s: %w", kind, item.Name, err)
			}
			item.Deleted = true
			if err := r.save(out); err != nil {
				return err
			}
		}
	}
	// Reject unknown kinds instead of declaring a partially processed journal clean.
	for _, item := range out.Resources {
		if !item.Deleted {
			return fmt.Errorf("unprocessed rehearsal resource %s", item.Kind)
		}
	}
	if out.ReservationPending {
		if err := r.release(ctx, out); err != nil {
			return err
		}
		out.ReservationPending = false
	}
	out.Cleaned = true
	return r.save(out)
}

func (r *Runner) metadata(ctx context.Context, ns string, item Resource) (model.ObjectMeta, error) {
	switch item.Kind {
	case "machines":
		m, e := r.Kube.GetMachine(ctx, ns, item.Name)
		return m.Metadata, e
	case "persistentvolumeclaims":
		v, e := r.Kube.GetPersistentVolumeClaim(ctx, ns, item.Name)
		return v.Metadata, e
	case "volumesnapshots":
		s, e := r.Kube.GetVolumeSnapshot(ctx, ns, item.Name)
		return s.Metadata, e
	default:
		return model.ObjectMeta{}, fmt.Errorf("unknown rehearsal resource")
	}
}

// WriteReport atomically replaces a private JSON journal; commands and guest
// output are not included, except commands already present in the input plan.
func WriteReport(path string, out any) error {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rehearsal-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err = f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func ownedCopy(m model.Machine, out Report, name string) error {
	if m.Spec.Network.Mode != "tap" || m.Spec.Network.Bridge != out.Plan.Bridge || m.Spec.Network.NetNS || m.Spec.Network.VirtualNetwork != nil || len(m.Spec.Network.ExtraInterfaces) != 0 || len(m.Spec.Network.Forwards) != 0 || m.Metadata.Annotations[GuardAnnotation] != "v1" {
		return fmt.Errorf("copy %s isolation configuration changed", name)
	}

	for _, resource := range out.Resources {
		if resource.Kind == "machines" && resource.Name == name && resource.UID != "" && resource.UID == m.Metadata.UID && m.Metadata.Labels[RunLabel] == out.RunID && m.Metadata.DeletionTimestamp == nil {
			return nil
		}
	}
	return fmt.Errorf("copy %s has changed ownership or UID", name)
}

func (r *Runner) reserve(ctx context.Context, out *Report) error {
	node, err := r.Kube.GetNode(ctx, out.Plan.Node)
	if err != nil {
		return err
	}
	if node.Metadata.Annotations[BridgeAnnotation] != out.Plan.Bridge || node.Metadata.Annotations[GuardAnnotation] != "v1" {
		return fmt.Errorf("node isolation configuration changed during preflight")
	}
	if node.Metadata.Annotations[ReservationAnnotation] != "" {
		return fmt.Errorf("rehearsal node is reserved; finish cleanup of its prior run first")
	}
	node.Metadata.Annotations[ReservationAnnotation] = out.RunID
	return r.Kube.SetRehearsalReservation(ctx, out.Plan.Node, node.Metadata.ResourceVersion, node.Metadata.Annotations)
}

func (r *Runner) release(ctx context.Context, out *Report) error {
	node, err := r.Kube.GetNode(ctx, out.Plan.Node)
	if err != nil {
		return err
	}
	if node.Metadata.Annotations[ReservationAnnotation] != out.RunID {
		return nil
	}
	delete(node.Metadata.Annotations, ReservationAnnotation)
	return r.Kube.SetRehearsalReservation(ctx, out.Plan.Node, node.Metadata.ResourceVersion, node.Metadata.Annotations)
}
