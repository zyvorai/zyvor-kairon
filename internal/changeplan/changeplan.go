// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package changeplan implements durable, compensating configuration changes.
// It is not a distributed database transaction and never undoes guest writes.
package changeplan

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/rehearsal"
)

const Annotation = "kairon.zyvor.dev/change-plan"

type Value struct {
	Exists bool            `json:"exists"`
	Data   json.RawMessage `json:"data,omitempty"`
}
type Stage struct {
	Name            string                     `json:"name"`
	Kind            string                     `json:"kind"`
	Target          string                     `json:"target"`
	Changes         map[string]json.RawMessage `json:"changes"`
	Checks          []rehearsal.Step           `json:"checks"`
	RollbackChecks  []rehearsal.Step           `json:"rollbackChecks"`
	Checkpoint      string                     `json:"checkpoint,omitempty"`
	UID             string                     `json:"uid,omitempty"`
	ResourceVersion string                     `json:"resourceVersion,omitempty"`
	Before          map[string]Value           `json:"before,omitempty"`
}
type Plan struct {
	APIVersion     string  `json:"apiVersion"`
	Name           string  `json:"name"`
	Namespace      string  `json:"namespace"`
	TimeoutSeconds int64   `json:"timeoutSeconds"`
	Stages         []Stage `json:"stages"`
	PreparedSHA256 string  `json:"preparedSHA256,omitempty"`
}
type Entry struct {
	Stage     Stage     `json:"stage"`
	State     string    `json:"state"`
	AppliedAt time.Time `json:"appliedAt"`
}
type Report struct {
	Version    int                `json:"version"`
	ID         string             `json:"id"`
	Plan       Plan               `json:"plan"`
	Phase      string             `json:"phase"`
	Decision   string             `json:"decision,omitempty"`
	Error      string             `json:"error,omitempty"`
	StartedAt  time.Time          `json:"startedAt"`
	FinishedAt *time.Time         `json:"finishedAt,omitempty"`
	Entries    []Entry            `json:"entries"`
	Probes     []rehearsal.Result `json:"probes"`
}
type Engine struct {
	Kube            *kube.Client
	Exec            func(context.Context, string, string, rehearsal.Step) (rehearsal.ExecResult, error)
	Save            func(Report) error
	PollInterval    time.Duration
	RecoveryTimeout time.Duration
}

type object struct {
	Metadata model.ObjectMeta           `json:"metadata"`
	Spec     map[string]json.RawMessage `json:"spec"`
	Status   map[string]json.RawMessage `json:"status"`
}

func (p Plan) Validate(prepared bool) error {
	if p.APIVersion != "changeplan.kairon.zyvor.dev/v1alpha1" || p.Name == "" || p.Namespace == "" || strings.ContainsAny(p.Namespace, "/ ") || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 3600 || len(p.Stages) == 0 || len(p.Stages) > 32 {
		return fmt.Errorf("invalid change-plan header or stages")
	}
	seen := map[string]bool{}
	for _, s := range p.Stages {
		key := s.Kind + "/" + s.Target
		if s.Name == "" || s.Target == "" || strings.ContainsAny(s.Target, "/ ") || seen[key] || len(s.Changes) == 0 || len(s.Checks) == 0 || len(s.RollbackChecks) == 0 {
			return fmt.Errorf("stage needs a unique resource, changes, checks and rollbackChecks")
		}
		seen[key] = true
		for path, value := range s.Changes {
			allowed := s.Kind == "Machine" && (path == "/spec/powerState" || path == "/spec/priority" || path == "/spec/disks") || s.Kind == "MachineNetworkPolicy" && path == "/spec/policy"
			if !allowed || !json.Valid(value) {
				return fmt.Errorf("unsupported change %s %s", s.Kind, path)
			}
			switch path {
			case "/spec/powerState":
				var v string
				if json.Unmarshal(value, &v) != nil || (v != "Running" && v != "Paused" && v != "Halted" && v != "Stopped") {
					return fmt.Errorf("invalid power state")
				}
			case "/spec/priority":
				var v int32
				if json.Unmarshal(value, &v) != nil {
					return fmt.Errorf("invalid priority")
				}
			case "/spec/disks":
				var v []model.MachineDisk
				if decodeChange(value, &v) != nil {
					return fmt.Errorf("invalid disks")
				}
				if err := model.ValidateDisks(v); err != nil {
					return err
				}
				if s.Checkpoint == "" {
					return fmt.Errorf("disk changes require a ready MachineSnapshot checkpoint")
				}
			case "/spec/policy":
				var v model.VmNetworkPolicy
				if decodeChange(value, &v) != nil {
					return fmt.Errorf("invalid policy")
				}
				if err := model.ValidateVmNetworkPolicy(v); err != nil {
					return err
				}
			}
		}
		for _, group := range [][]rehearsal.Step{s.Checks, s.RollbackChecks} {
			for _, probe := range group {
				if probe.Name == "" || probe.Machine == "" || strings.ContainsAny(probe.Machine, "/ ") || !strings.HasPrefix(probe.Path, "/") || probe.TimeoutSeconds < 1 || probe.TimeoutSeconds > 240 || probe.ExpectedExitCode != 0 {
					return fmt.Errorf("invalid health probe")
				}
			}
		}
		if prepared && (s.UID == "" || s.ResourceVersion == "" || len(s.Before) != len(s.Changes)) {
			return fmt.Errorf("run preview first: preconditions and before values are required")
		}
	}
	if prepared && digest(p) != p.PreparedSHA256 {
		return fmt.Errorf("prepared plan digest mismatch; preview again")
	}
	return nil
}
func decodeChange(value json.RawMessage, out any) error {
	d := json.NewDecoder(bytes.NewReader(value))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func digest(p Plan) string {
	p.PreparedSHA256 = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func rawEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
func field(o object, path string) Value {
	b, ok := o.Spec[strings.TrimPrefix(path, "/spec/")]
	return Value{Exists: ok, Data: b}
}
func (e *Engine) get(ctx context.Context, ns, kind, name string) (object, error) {
	b, err := e.Kube.OperationObject(ctx, ns, kind, name)
	if err != nil {
		return object{}, err
	}
	var o object
	err = json.Unmarshal(b, &o)
	if err == nil && (o.Metadata.UID == "" || o.Metadata.ResourceVersion == "" || o.Metadata.DeletionTimestamp != nil) {
		err = fmt.Errorf("target is deleting or lacks identity")
	}
	return o, err
}

// Prepare reads the cluster and binds the reviewed change to object identities,
// resource versions and original field values. No mutation occurs here.
func (e *Engine) Prepare(ctx context.Context, p Plan) (Plan, error) {
	if e.Kube == nil {
		return p, fmt.Errorf("kubernetes client is required")
	}
	if err := p.Validate(false); err != nil {
		return p, err
	}
	for i := range p.Stages {
		s := &p.Stages[i]
		o, err := e.get(ctx, p.Namespace, s.Kind, s.Target)
		if err != nil {
			return p, err
		}
		if o.Metadata.Annotations[Annotation] != "" {
			return p, fmt.Errorf("target %s already reserved", s.Target)
		}
		s.UID, s.ResourceVersion = o.Metadata.UID, o.Metadata.ResourceVersion
		s.Before = map[string]Value{}
		for path, value := range s.Changes {
			s.Before[path] = field(o, path)
			// Canonicalize typed nested fields before sending them to Kubernetes.
			// encoding/json accepts case variants which CRD schemas would prune.
			switch path {
			case "/spec/policy":
				var policy model.VmNetworkPolicy
				_ = decodeChange(value, &policy)
				s.Changes[path], _ = json.Marshal(policy)
			case "/spec/disks":
				var disks []model.MachineDisk
				_ = decodeChange(value, &disks)
				s.Changes[path], _ = json.Marshal(disks)
			}
		}
		if s.Kind == "MachineNetworkPolicy" {
			var policy model.MachineNetworkPolicy
			b, _ := json.Marshal(o)
			_ = json.Unmarshal(b, &policy)
			if policy.Spec.MachineName == "" || len(policy.Spec.CNP) > 0 || policy.Spec.Cilium != nil {
				return p, fmt.Errorf("transactions require a single-Machine native edge policy without CNP/Cilium side effects")
			}
			checked := false
			for _, check := range s.Checks {
				checked = checked || check.Machine == policy.Spec.MachineName
			}
			if !checked {
				return p, fmt.Errorf("policy stage must probe its target Machine")
			}
		}
		if s.Kind == "Machine" {
			var m model.Machine
			b, _ := json.Marshal(o)
			_ = json.Unmarshal(b, &m)
			var power string
			_ = json.Unmarshal(s.Changes["/spec/powerState"], &power)
			if power == "Stopped" && len(m.Spec.Volumes) == 0 {
				return p, fmt.Errorf("stopped would destroy an image-backed ephemeral disk; halt it instead")
			}
			if s.Checkpoint != "" {
				cp, err := e.Kube.GetMachineSnapshot(ctx, p.Namespace, s.Checkpoint)
				if err != nil {
					return p, err
				}
				if !cp.Status.ReadyToUse || cp.Status.Phase != "Succeeded" || cp.Spec.MachineName != s.Target || cp.Status.SourceUID != s.UID {
					return p, fmt.Errorf("checkpoint is not ready or belongs to another Machine UID")
				}
			}
			if disks, ok := s.Changes["/spec/disks"]; ok {
				var ds []model.MachineDisk
				_ = json.Unmarshal(disks, &ds)
				if m.Spec.Runtime.Backend != "qemu" {
					return p, fmt.Errorf("disk transactions require explicit qemu backend")
				}
				for _, d := range ds {
					for _, old := range m.Spec.Disks {
						if old.Name == d.Name && !reflect.DeepEqual(old, d) {
							return p, fmt.Errorf("disk stages support attach/detach, not replacement or modification under an existing disk name")
						}
					}
					pvc, err := e.Kube.GetPersistentVolumeClaim(ctx, p.Namespace, d.ClaimName)
					if err != nil {
						return p, err
					}
					if pvc.Status.Phase != "Bound" {
						return p, fmt.Errorf("disk claim %s is not Bound", d.ClaimName)
					}
				}
			}
		}
	}
	p.PreparedSHA256 = digest(p)
	return p, nil
}
func basePatch(o object) []map[string]any {
	return []map[string]any{{"op": "test", "path": "/metadata/uid", "value": o.Metadata.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": o.Metadata.ResourceVersion}}
}
func annotations(o object) map[string]string {
	m := map[string]string{}
	for k, v := range o.Metadata.Annotations {
		m[k] = v
	}
	return m
}
func paths(s Stage) []string {
	out := make([]string, 0, len(s.Changes))
	for p := range s.Changes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func (e *Engine) save(r Report) error {
	if e.Save == nil {
		return fmt.Errorf("durable journal writer is required")
	}
	return e.Save(r)
}

func (e *Engine) Apply(parent context.Context, p Plan) (r Report, err error) {
	if err = p.Validate(true); err != nil {
		return r, err
	}
	if e.Kube == nil || e.Exec == nil {
		return r, fmt.Errorf("kubernetes client and health executor required")
	}
	token := make([]byte, 12)
	if _, err = rand.Read(token); err != nil {
		return r, err
	}
	r = Report{Version: 1, ID: "cp-" + hex.EncodeToString(token), Plan: p, Phase: "Applying", StartedAt: time.Now().UTC()}
	if err = e.save(r); err != nil {
		return r, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	// Recheck every precondition before the first mutation; subsequent CAS also
	// catches races occurring after this complete preflight.
	for _, s := range p.Stages {
		o, getErr := e.get(ctx, p.Namespace, s.Kind, s.Target)
		if getErr != nil {
			err = getErr
			break
		}
		if o.Metadata.UID != s.UID || o.Metadata.ResourceVersion != s.ResourceVersion || o.Metadata.Annotations[Annotation] != "" {
			err = fmt.Errorf("precondition changed for %s", s.Target)
			break
		}
	}
	if err == nil {
		for _, s := range p.Stages {
			r.Entries = append(r.Entries, Entry{Stage: s, State: "Intent", AppliedAt: time.Now().UTC()})
			if err = e.save(r); err != nil {
				break
			}
			var o object
			o, err = e.get(ctx, p.Namespace, s.Kind, s.Target)
			if err != nil {
				break
			}
			if o.Metadata.UID != s.UID || o.Metadata.ResourceVersion != s.ResourceVersion || o.Metadata.Annotations[Annotation] != "" {
				err = fmt.Errorf("stage precondition changed")
				break
			}
			patch := basePatch(o)
			ann := annotations(o)
			ann[Annotation] = r.ID
			patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": ann})
			for _, path := range paths(s) {
				patch = append(patch, map[string]any{"op": "add", "path": path, "value": s.Changes[path]})
			}
			if err = e.Kube.PatchOperationObject(ctx, p.Namespace, s.Kind, s.Target, patch); err != nil {
				break
			}
			r.Entries[len(r.Entries)-1].State = "Applied"
			if err = e.save(r); err != nil {
				break
			}
			if err = e.await(ctx, s, r.Entries[len(r.Entries)-1].AppliedAt, p.Namespace, false); err != nil {
				break
			}
			if err = e.checks(ctx, &r, s.Checks, "Verify"); err != nil {
				break
			}
			r.Entries[len(r.Entries)-1].State = "Verified"
			if err = e.save(r); err != nil {
				break
			}
		}
	}
	if err == nil {
		r.Decision = "Commit"
	} else {
		r.Decision = "Rollback"
		r.Error = err.Error()
	}
	if saveErr := e.save(r); saveErr != nil {
		r.Phase = "NeedsRecovery"
		return r, errors.Join(err, saveErr)
	}
	timeout := e.RecoveryTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	recoveryCtx, stop := context.WithTimeout(context.Background(), timeout)
	defer stop()
	recoverErr := e.Recover(recoveryCtx, &r)
	return r, errors.Join(err, recoverErr)
}

// Recover follows the journal's durable Commit/Rollback decision. It never
// re-executes an application change or guesses after an ambiguous write.
func (e *Engine) Recover(ctx context.Context, r *Report) error {
	if e.Kube == nil || r == nil {
		return fmt.Errorf("kubernetes client and journal are required")
	}
	if r.Version != 1 || len(r.ID) != 27 || !strings.HasPrefix(r.ID, "cp-") {
		return fmt.Errorf("invalid change journal")
	}
	if err := r.Plan.Validate(true); err != nil {
		return err
	}
	if r.Decision == "" {
		r.Decision = "Rollback"
		if err := e.save(*r); err != nil {
			return err
		}
	}
	if r.Decision != "Commit" && r.Decision != "Rollback" {
		return fmt.Errorf("invalid recovery decision")
	}
	if len(r.Entries) > len(r.Plan.Stages) || (r.Decision == "Commit" && len(r.Entries) != len(r.Plan.Stages)) {
		return fmt.Errorf("incomplete or oversized change journal")
	}
	for i, entry := range r.Entries {
		if !reflect.DeepEqual(entry.Stage, r.Plan.Stages[i]) || entry.AppliedAt.IsZero() {
			return fmt.Errorf("journal stage identity or intent time is invalid")
		}
		switch entry.State {
		case "Intent", "Applied", "Verified":
			if r.Decision == "Commit" && entry.State != "Verified" {
				return fmt.Errorf("commit journal contains an unverified stage")
			}
		case "Released":
			if r.Decision != "Commit" {
				return fmt.Errorf("rollback journal contains a released stage")
			}
		case "Restored":
			if r.Decision != "Rollback" {
				return fmt.Errorf("commit journal contains a compensated stage")
			}
		default:
			return fmt.Errorf("unknown journal entry state")
		}
	}
	fail := func(err error) error {
		r.Phase = "NeedsRecovery"
		r.Error = err.Error()
		return errors.Join(err, e.save(*r))
	}
	for i := len(r.Entries) - 1; i >= 0; i-- {
		entry := &r.Entries[i]
		s := entry.Stage
		// Prevent a modified journal from introducing a different recovery target.
		if i >= len(r.Plan.Stages) || !reflect.DeepEqual(s, r.Plan.Stages[i]) {
			return fail(fmt.Errorf("journal stages do not match prepared plan"))
		}
		if entry.State == "Released" || entry.State == "Restored" {
			continue
		}
		o, err := e.get(ctx, r.Plan.Namespace, s.Kind, s.Target)
		if err != nil {
			return fail(err)
		}
		if o.Metadata.UID != s.UID {
			return fail(fmt.Errorf("target UID changed"))
		}
		owner := o.Metadata.Annotations[Annotation]
		after := true
		before := true
		for _, path := range paths(s) {
			current := field(o, path)
			after = after && current.Exists && rawEqual(current.Data, s.Changes[path])
			old := s.Before[path]
			before = before && current.Exists == old.Exists && (!current.Exists || rawEqual(current.Data, old.Data))
		}
		switch {
		case r.Decision == "Commit":
			if !after || (owner != r.ID && owner != "") {
				return fail(fmt.Errorf("committed target changed or reserved by another run"))
			}
		case owner != r.ID:
			if owner == "" && before {
				if err := e.await(ctx, s, time.Time{}, r.Plan.Namespace, true); err != nil {
					return fail(err)
				}
				if err := e.checks(ctx, r, s.RollbackChecks, "RollbackVerify"); err != nil {
					return fail(err)
				}
				entry.State = "Restored"
				if err := e.save(*r); err != nil {
					return fail(err)
				}
				continue
			}
			return fail(fmt.Errorf("ambiguous or foreign target; operator recovery required"))
		case !after && !before:
			return fail(fmt.Errorf("changed fields were modified externally; refusing compensation"))
		}
		patchAt := time.Now().UTC()
		patch := basePatch(o)
		if r.Decision == "Rollback" && after {
			for _, path := range paths(s) {
				v := s.Before[path]
				op := map[string]any{"op": "remove", "path": path}
				if v.Exists {
					op = map[string]any{"op": "add", "path": path, "value": v.Data}
				}
				patch = append(patch, op)
			}
		}
		ann := annotations(o)
		if r.Decision == "Rollback" {
			ann[Annotation] = r.ID
		} else {
			delete(ann, Annotation)
		}
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": ann})
		if err := e.Kube.PatchOperationObject(ctx, r.Plan.Namespace, s.Kind, s.Target, patch); err != nil {
			return fail(err)
		}
		if r.Decision == "Rollback" {
			if before {
				patchAt = time.Time{}
			}
			if err := e.await(ctx, s, patchAt, r.Plan.Namespace, true); err != nil {
				return fail(err)
			}
			if err := e.checks(ctx, r, s.RollbackChecks, "RollbackVerify"); err != nil {
				return fail(err)
			}
			fresh, err := e.get(ctx, r.Plan.Namespace, s.Kind, s.Target)
			if err != nil {
				return fail(err)
			}
			if fresh.Metadata.UID != s.UID || fresh.Metadata.Annotations[Annotation] != r.ID {
				return fail(fmt.Errorf("rollback reservation changed"))
			}
			for _, path := range paths(s) {
				v := field(fresh, path)
				old := s.Before[path]
				if v.Exists != old.Exists || (v.Exists && !rawEqual(v.Data, old.Data)) {
					return fail(fmt.Errorf("rollback fields changed during verification"))
				}
			}
			release := basePatch(fresh)
			a := annotations(fresh)
			delete(a, Annotation)
			release = append(release, map[string]any{"op": "add", "path": "/metadata/annotations", "value": a})
			if err := e.Kube.PatchOperationObject(ctx, r.Plan.Namespace, s.Kind, s.Target, release); err != nil {
				return fail(err)
			}
			entry.State = "Restored"
		} else {
			entry.State = "Released"
		}
		if err := e.save(*r); err != nil {
			return fail(err)
		}
	}
	r.Phase = "Committed"
	if r.Decision == "Rollback" {
		r.Phase = "RolledBack"
	}
	now := time.Now().UTC()
	r.FinishedAt = &now
	return e.save(*r)
}

func (e *Engine) checks(ctx context.Context, r *Report, steps []rehearsal.Step, stage string) error {
	if e.Exec == nil {
		return fmt.Errorf("health executor required")
	}
	for _, s := range steps {
		stepCtx, cancel := context.WithTimeout(ctx, time.Duration(s.TimeoutSeconds)*time.Second)
		start := time.Now()
		out, err := e.Exec(stepCtx, r.Plan.Namespace, s.Machine, s)
		cancel()
		h1 := sha256.Sum256([]byte(out.Stdout))
		h2 := sha256.Sum256([]byte(out.Stderr))
		r.Probes = append(r.Probes, rehearsal.Result{Stage: stage, Name: s.Name, Machine: s.Machine, ExitCode: out.ExitCode, DurationMillis: time.Since(start).Milliseconds(), StdoutSHA256: hex.EncodeToString(h1[:]), StderrSHA256: hex.EncodeToString(h2[:])})
		if saveErr := e.save(*r); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		if err != nil {
			return err
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("health check %s failed with exit %d", s.Name, out.ExitCode)
		}
	}
	return nil
}
func (e *Engine) await(ctx context.Context, s Stage, at time.Time, ns string, before bool) error {
	interval := e.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		o, err := e.get(ctx, ns, s.Kind, s.Target)
		if err != nil {
			return err
		}
		if o.Metadata.UID != s.UID {
			return fmt.Errorf("target UID changed while waiting")
		}
		ready := true
		for path, desired := range s.Changes {
			if before {
				desired = s.Before[path].Data
			}
			switch path {
			case "/spec/powerState":
				var state, phase string
				_ = json.Unmarshal(desired, &state)
				_ = json.Unmarshal(o.Status["phase"], &phase)
				if state == "" {
					state = "Running"
				}
				ready = ready && state == phase
			case "/spec/disks":
				var disks []model.MachineDisk
				_ = json.Unmarshal(desired, &disks)
				var attached []string
				if b := o.Status["attachedDisks"]; len(b) > 0 {
					if err := json.Unmarshal(b, &attached); err != nil {
						return fmt.Errorf("invalid attachedDisks status: %w", err)
					}
				}
				names := map[string]bool{}
				for _, d := range attached {
					names[d] = true
				}
				ready = ready && len(disks) == len(attached)
				for _, d := range disks {
					ready = ready && names[d.Name]
				}
			case "/spec/policy":
				var synced bool
				var applied time.Time
				_ = json.Unmarshal(o.Status["effectiveSynced"], &synced)
				_ = json.Unmarshal(o.Status["lastAppliedTime"], &applied)
				ready = ready && synced && !applied.Before(at)
			}
		}
		if ready {
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
