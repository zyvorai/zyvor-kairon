// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha1" // #nosec G505 -- short object names, not security
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// DisasterRecovery configures MachineProtectionPlans -- see protection.go.
type DisasterRecovery struct {
	// SecretNamespace is where peer credential Secrets are read from:
	// kairon-controller's own namespace.
	SecretNamespace string
	// TestBridges are the isolated bridges DR tests may attach copies to.
	TestBridges []string
	// Peer builds a client for a plan's peer site; nil uses kube.NewWithCA.
	Peer func(server string, secret model.Secret) (*kube.Client, error)
}

const (
	protectionTestLabel  = "kairon.zyvor.dev/protection-test"
	protectionTestSuffix = "-drtest"
)

// reconcileProtectionPlans runs every MachineProtectionPlan. Off unless
// DR.SecretNamespace is set; tolerant of the CRD not being installed.
func (c *Controller) reconcileProtectionPlans(ctx context.Context, machines []model.Machine) error {
	if c.DR.SecretNamespace == "" {
		return nil
	}
	plans, err := c.Kube.ListMachineProtectionPlans(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	if len(plans) == 0 {
		return nil
	}
	backups, err := c.Kube.ListMachineBackups(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	now := time.Now().UTC()
	for _, p := range plans {
		st := c.runProtectionPlan(ctx, p, machines, backups, now)
		if c.Metrics != nil {
			c.Metrics.ObserveProtectionPlan(p.Namespace(), p.Metadata.Name, st)
		}
		if reflect.DeepEqual(st, p.Status) {
			continue
		}
		if err := c.Kube.PatchMachineProtectionPlanStatus(ctx, p.Namespace(), p.Metadata.Name, st); err != nil {
			c.Log.Error("protection plan status patch failed", "namespace", p.Namespace(), "plan", p.Metadata.Name, "error", err)
		}
	}
	return nil
}

// protection is one plan's reconcile pass.
type protection struct {
	c        *Controller
	plan     model.MachineProtectionPlan
	st       *model.MachineProtectionPlanStatus
	machines map[string]model.Machine
	backups  []model.MachineBackup
	peer     *kube.Client
	peerPlan *model.MachineProtectionPlan
	now      time.Time
	// netProblems are, with preserveIP, the Machines whose address does
	// not fit on this site.
	netProblems map[string]string
}

func (c *Controller) runProtectionPlan(ctx context.Context, p model.MachineProtectionPlan, machines []model.Machine, backups []model.MachineBackup, now time.Time) model.MachineProtectionPlanStatus {
	var st model.MachineProtectionPlanStatus
	if raw, err := json.Marshal(p.Status); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if err := p.Spec.Validate(p.Metadata.Name); err != nil {
		st.Phase, st.Message = model.ProtectionPhaseInvalid, err.Error()
		return st
	}
	if st.Phase == "" || st.Phase == model.ProtectionPhaseInvalid {
		if st.Epoch == 0 && p.Spec.Role == model.ProtectionRolePrimary {
			st.Active, st.Epoch = true, 1
		}
	}
	r := &protection{c: c, plan: p, st: &st, machines: map[string]model.Machine{}, now: now}
	for _, m := range machines {
		if m.Namespace() == p.Namespace() {
			r.machines[m.Metadata.Name] = m
		}
	}
	for _, b := range backups {
		if b.Namespace() == p.Namespace() {
			r.backups = append(r.backups, b)
		}
	}
	r.contactPeer(ctx)
	if st.Active && st.Peer.Reachable && st.Peer.Epoch > st.Epoch {
		r.demote(ctx)
	}
	if !st.Active && st.Peer.Epoch > st.Epoch {
		st.Epoch = st.Peer.Epoch
	}
	r.takeAction(ctx)
	if st.Active {
		r.active(ctx)
	} else {
		r.standby(ctx)
	}
	r.stepRuns(ctx)
	r.observeObjectives()
	return st
}

func (r *protection) contactPeer(ctx context.Context) {
	ps := &model.ProtectionPeerStatus{}
	if r.st.Peer != nil {
		*ps = *r.st.Peer
	}
	r.st.Peer = ps
	fail := func(msg string) {
		ps.Reachable, ps.Message = false, msg
	}
	peer, err := r.c.peerClient(ctx, r.plan)
	if err != nil {
		fail(err.Error())
		return
	}
	pp, err := peer.GetMachineProtectionPlan(ctx, r.plan.PeerNamespace(), r.plan.PeerPlan())
	if err != nil {
		fail(fmt.Sprintf("get plan %s/%s on %s: %v", r.plan.PeerNamespace(), r.plan.PeerPlan(), r.plan.Spec.Peer.Server, err))
		return
	}
	now := r.now
	r.peer, r.peerPlan = peer, &pp
	ps.Reachable, ps.LastContact, ps.Message = true, &now, ""
	ps.Active, ps.Epoch = pp.Status.Active, pp.Status.Epoch
}

func (c *Controller) peerClient(ctx context.Context, p model.MachineProtectionPlan) (*kube.Client, error) {
	ns := c.DR.SecretNamespace
	if ns == "" {
		return nil, fmt.Errorf("kairon-controller has no --dr-secret-namespace")
	}
	s, err := c.Kube.GetSecret(ctx, ns, p.Spec.Peer.CredentialsSecretRef)
	if err != nil {
		return nil, fmt.Errorf("get peer credentials %s/%s: %w", ns, p.Spec.Peer.CredentialsSecretRef, err)
	}
	if s.Metadata.Labels[model.DRPeerCredentialsLabel] != "true" {
		return nil, fmt.Errorf("secret %s/%s is not labelled %s=true", ns, s.Metadata.Name, model.DRPeerCredentialsLabel)
	}
	if c.DR.Peer != nil {
		return c.DR.Peer(p.Spec.Peer.Server, s)
	}
	token := strings.TrimSpace(string(s.Data["token"]))
	if token == "" {
		return nil, fmt.Errorf("secret %s/%s has no token", ns, s.Metadata.Name)
	}
	return kube.NewWithCA(p.Spec.Peer.Server, token, s.Data["ca.crt"], false)
}

// demote hands the Machines over to a peer that failed over: halt them
// here (keeping their disks for a later failback) and stop replicating.
func (r *protection) demote(ctx context.Context) {
	for _, pm := range r.plan.Spec.Machines {
		m, ok := r.machines[pm.Name]
		if !ok || m.DesiredPowerState() == "Halted" || m.DesiredPowerState() == "Stopped" {
			continue
		}
		if err := r.c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"spec": map[string]any{"powerState": "Halted"}}); err != nil {
			r.st.Message = fmt.Sprintf("halt %s after the peer failed over: %v", m.Metadata.Name, err)
			return
		}
	}
	if err := r.c.Kube.DeleteMachineBackupPolicy(ctx, r.plan.Namespace(), r.plan.PolicyName()); err != nil && !kube.IsNotFound(err) {
		r.st.Message = fmt.Sprintf("delete backup policy %s: %v", r.plan.PolicyName(), err)
		return
	}
	r.c.Log.Info("protection plan demoted: the peer failed over", "namespace", r.plan.Namespace(), "plan", r.plan.Metadata.Name, "epoch", r.st.Epoch, "peerEpoch", r.st.Peer.Epoch)
	r.st.Active, r.st.Epoch, r.st.Handoff = false, r.st.Peer.Epoch, nil
	r.st.Message = fmt.Sprintf("the peer failed over (epoch %d); Machines here are halted", r.st.Peer.Epoch)
}

func (r *protection) takeAction(ctx context.Context) {
	a := r.plan.Spec.Action
	if a == nil || a.ID == r.st.LastAction {
		return
	}
	r.st.LastAction = a.ID
	now := r.now
	switch a.Type {
	case model.ProtectionActionFailover:
		if r.st.Active {
			r.st.Message = "failover " + a.ID + " ignored: this site is already active"
			return
		}
		if r.running(r.st.Test) {
			r.st.Message = "failover " + a.ID + " ignored: end the DR test first"
			return
		}
		// Raise the epoch now so the old site halts as soon as it sees it;
		// a planned failover raises it once the old site has handed off.
		if !a.Planned {
			r.st.Epoch++
		}
		r.st.Failover = &model.ProtectionRun{ID: a.ID, Planned: a.Planned, Phase: model.ProtectionRunRunning, StartedAt: &now}
		r.c.Log.Info("protection plan failover started", "namespace", r.plan.Namespace(), "plan", r.plan.Metadata.Name, "id", a.ID, "planned", a.Planned)
	case model.ProtectionActionTest:
		switch {
		case r.st.Active:
			r.st.Message = "test " + a.ID + " ignored: DR tests run on the standby site"
			return
		case r.running(r.st.Test):
			r.st.Message = "test " + a.ID + " ignored: test " + r.st.Test.ID + " is still running"
			return
		case r.plan.Spec.Test.Bridge == "" || !slices.Contains(r.c.DR.TestBridges, r.plan.Spec.Test.Bridge):
			r.st.Message = "test " + a.ID + " ignored: test.bridge must be in kairon-controller's --dr-test-bridges"
			return
		}
		ttl, _ := r.plan.Spec.TestTTL()
		expires := now.Add(ttl)
		r.st.Test = &model.ProtectionRun{ID: a.ID, Phase: model.ProtectionRunRunning, StartedAt: &now, ExpiresAt: &expires}
	case model.ProtectionActionEndTest:
		if r.st.Test != nil && r.st.Test.Phase != model.ProtectionRunEnded {
			r.endTest(ctx, "ended by action "+a.ID)
		}
	}
}

func (r *protection) running(run *model.ProtectionRun) bool {
	return run != nil && run.Phase == model.ProtectionRunRunning
}

// active keeps the plan's Machines labelled and replicated, and answers a
// planned failover's handoff.
func (r *protection) active(ctx context.Context) {
	r.st.Phase, r.st.Message = model.ProtectionPhaseActive, ""
	var missing []string
	for _, pm := range r.plan.Spec.Machines {
		m, ok := r.machines[pm.Name]
		if !ok {
			missing = append(missing, pm.Name)
			continue
		}
		if m.Metadata.Labels[model.ProtectionPlanLabel] != r.plan.Metadata.Name {
			patch := map[string]any{"metadata": map[string]any{"labels": map[string]string{model.ProtectionPlanLabel: r.plan.Metadata.Name}}}
			if err := r.c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
				r.st.Message = fmt.Sprintf("label %s: %v", m.Metadata.Name, err)
			}
		}
	}
	if len(missing) > 0 {
		r.st.Message = "not found here: " + strings.Join(missing, ", ")
	}
	if err := r.ensurePolicy(ctx); err != nil {
		r.st.Message = err.Error()
	}
	r.st.Machines = r.machineStatuses(func(b model.MachineBackup) bool {
		return !b.Replicated() && b.Metadata.Labels[model.BackupPolicyLabel] == r.plan.PolicyName()
	}, false)
	r.handoff(ctx)
	r.publishDNS(ctx)
	r.st.Conditions = removeCondition(r.st.Conditions, condNetworkReady)
	r.st.NextTest = nil
}

func (r *protection) ensurePolicy(ctx context.Context) error {
	want := r.plan.Spec.BackupPolicySpec(r.plan.Metadata.Name)
	name := r.plan.PolicyName()
	cur, err := r.c.Kube.GetMachineBackupPolicy(ctx, r.plan.Namespace(), name)
	if kube.IsNotFound(err) {
		_, err = r.c.Kube.CreateMachineBackupPolicy(ctx, r.plan.Namespace(), model.MachineBackupPolicy{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupPolicy},
			Metadata: model.ObjectMeta{Name: name, Namespace: r.plan.Namespace(), Labels: map[string]string{model.ProtectionPlanLabel: r.plan.Metadata.Name}},
			Spec:     want,
		})
		if err != nil {
			return fmt.Errorf("create backup policy %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get backup policy %s: %w", name, err)
	}
	if cur.Metadata.Labels[model.ProtectionPlanLabel] != r.plan.Metadata.Name {
		return fmt.Errorf("backup policy %s exists and is not this plan's", name)
	}
	if !reflect.DeepEqual(cur.Spec, want) {
		if err := r.c.Kube.PatchMachineBackupPolicy(ctx, r.plan.Namespace(), name, map[string]any{"spec": want}); err != nil {
			return fmt.Errorf("update backup policy %s: %w", name, err)
		}
	}
	return nil
}

// handoff halts the Machines and takes a final backup of each when the
// standby site asks for a planned failover.
func (r *protection) handoff(ctx context.Context) {
	id := r.plan.Metadata.Annotations[model.ProtectionHandoffAnnotation]
	if id == "" {
		return
	}
	h := r.st.Handoff
	if h == nil || h.ID != id {
		h = &model.ProtectionHandoff{ID: id, Phase: model.ProtectionHandoffHalting}
		r.st.Handoff = h
	}
	switch h.Phase {
	case model.ProtectionHandoffHalting:
		halted := true
		for _, pm := range r.plan.Spec.Machines {
			m, ok := r.machines[pm.Name]
			if !ok {
				continue
			}
			if m.DesiredPowerState() != "Halted" {
				if err := r.c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"spec": map[string]any{"powerState": "Halted"}}); err != nil {
					h.Message = fmt.Sprintf("halt %s: %v", m.Metadata.Name, err)
					return
				}
				halted = false
				continue
			}
			halted = halted && m.Status.Phase == "Halted"
		}
		if !halted {
			h.Message = "halting the Machines"
			return
		}
		if r.policyBusy() {
			h.Message = "waiting for an in-flight replication backup"
			return
		}
		h.Phase, h.Message = model.ProtectionHandoffBackup, "taking final backups"
		for _, pm := range r.plan.Spec.Machines {
			if _, ok := r.machines[pm.Name]; !ok {
				continue
			}
			name, err := r.finalBackup(ctx, pm.Name, id)
			if err != nil {
				h.Phase, h.Message = model.ProtectionHandoffHalting, err.Error()
				return
			}
			h.Backups = append(h.Backups, name)
		}
	case model.ProtectionHandoffBackup:
		var waiting []string
		for _, pm := range r.plan.Spec.Machines {
			name := r.finalBackupName(pm.Name, id)
			if !slices.Contains(h.Backups, name) {
				continue
			}
			b, ok := r.backup(name)
			switch {
			case !ok:
				// Not listed yet, or deleted (retention drops a failed one):
				// creating it again is a no-op for the former.
				if _, err := r.finalBackup(ctx, pm.Name, id); err != nil {
					waiting = append(waiting, err.Error())
					continue
				}
				waiting = append(waiting, "waiting for "+name)
			case b.Status.Phase == model.BackupFailed:
				waiting = append(waiting, fmt.Sprintf("final backup %s failed: %s", name, b.Status.Message))
			case b.Status.Phase != model.BackupSucceeded:
				waiting = append(waiting, "waiting for final backup "+name)
			}
		}
		if len(waiting) > 0 {
			h.Message = strings.Join(waiting, "; ")
			return
		}
		h.Phase, h.Message = model.ProtectionHandoffReady, "Machines halted and final backups taken; waiting for the peer to fail over"
	}
}

func (r *protection) policyBusy() bool {
	for _, b := range r.backups {
		if b.Metadata.Labels[model.BackupPolicyLabel] == r.plan.PolicyName() && b.Metadata.DeletionTimestamp == nil &&
			b.Status.Phase != model.BackupSucceeded && b.Status.Phase != model.BackupFailed {
			return true
		}
	}
	return false
}

func (r *protection) finalBackupName(machine, id string) string {
	return fmt.Sprintf("%s-%s-final-%s", r.plan.PolicyName(), machine, shortHash(id))
}

func (r *protection) finalBackup(ctx context.Context, machine, id string) (string, error) {
	policy := r.plan.PolicyName()
	mode, parent := model.BackupModeFull, ""
	if newest, ok := r.newestBackup(machine, func(b model.MachineBackup) bool {
		return !b.Replicated() && b.Metadata.Labels[model.BackupPolicyLabel] == policy && b.BackupMode() != model.BackupModeSyntheticFull
	}); ok {
		mode, parent = model.BackupModeIncremental, newest.Metadata.Name
	}
	spec := r.plan.Spec.BackupPolicySpec(r.plan.Metadata.Name)
	target := spec.Target
	name := r.finalBackupName(machine, id)
	b := model.MachineBackup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup},
		Metadata: model.ObjectMeta{Name: name, Namespace: r.plan.Namespace(), Labels: map[string]string{
			model.BackupPolicyLabel: policy, model.BackupMachineLabel: machine, model.ProtectionPlanLabel: r.plan.Metadata.Name,
		}},
		Spec: model.MachineBackupSpec{
			MachineName: machine, Quiesce: model.BackupQuiesceNever, Target: &target, Mode: mode, Parent: parent,
			PolicyName: policy, Encryption: spec.Encryption,
		},
	}
	if _, err := r.c.Kube.CreateMachineBackup(ctx, r.plan.Namespace(), b); err != nil && !isAlreadyExists(err) {
		return "", fmt.Errorf("create final backup of %s: %w", machine, err)
	}
	return name, nil
}

// standby mirrors the active site's Machine specs and restore points.
func (r *protection) standby(ctx context.Context) {
	if !r.running(r.st.Failover) {
		r.st.Phase = model.ProtectionPhaseStandby
	}
	if r.peer != nil {
		if err := r.mirror(ctx); err != nil {
			r.st.Peer.Message = err.Error()
		}
	}
	r.st.Machines = r.machineStatuses(func(b model.MachineBackup) bool {
		return b.Replicated() && b.Metadata.Labels[model.ProtectionPlanLabel] == r.plan.Metadata.Name
	}, true)
	r.withdrawDNS(ctx)
	r.checkNetwork(ctx)
	r.scheduleTest()
}

// mirror copies the peer's Machine specs into the plan's status and
// imports the peer's replication backups as local MachineBackups.
func (r *protection) mirror(ctx context.Context) error {
	ns := r.plan.PeerNamespace()
	specs := map[string]model.ProtectedMachineStatus{}
	for _, pm := range r.plan.Spec.Machines {
		m, err := r.peer.GetMachine(ctx, ns, pm.Name)
		if err != nil {
			if kube.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get peer machine %s: %w", pm.Name, err)
		}
		spec := model.PreservedSpec(m, r.plan.Spec.Network.PreserveIP)
		for _, prev := range r.st.Machines {
			if prev.Name == pm.Name {
				keepPinnedIPs(&spec, prev.Spec)
			}
		}
		labels := map[string]string{}
		for k, v := range m.Metadata.Labels {
			if k != model.ProtectionPlanLabel {
				labels[k] = v
			}
		}
		specs[pm.Name] = model.ProtectedMachineStatus{Spec: &spec, Labels: labels}
	}
	for i := range r.st.Machines {
		if s, ok := specs[r.st.Machines[i].Name]; ok {
			r.st.Machines[i].Spec, r.st.Machines[i].Labels = s.Spec, s.Labels
		}
	}
	for name, s := range specs {
		if !slices.ContainsFunc(r.st.Machines, func(ms model.ProtectedMachineStatus) bool { return ms.Name == name }) {
			r.st.Machines = append(r.st.Machines, model.ProtectedMachineStatus{Name: name, Spec: s.Spec, Labels: s.Labels})
		}
	}

	peerBackups, err := r.peer.ListMachineBackupsNamespace(ctx, ns)
	if err != nil {
		return fmt.Errorf("list peer backups: %w", err)
	}
	peerPolicy := "dr-" + r.plan.PeerPlan()
	protected := map[string]bool{}
	for _, pm := range r.plan.Spec.Machines {
		protected[pm.Name] = true
	}
	source := map[string]bool{}
	for _, pb := range peerBackups {
		if pb.Replicated() || pb.Metadata.Labels[model.BackupPolicyLabel] != peerPolicy || !protected[pb.Spec.MachineName] ||
			pb.Metadata.DeletionTimestamp != nil || pb.Status.Phase != model.BackupSucceeded || pb.Status.ManifestKey == "" {
			continue
		}
		source[ns+"/"+pb.Metadata.Name] = true
		if _, ok := r.backup(pb.Metadata.Name); ok {
			continue
		}
		if err := r.importBackup(ctx, pb); err != nil {
			return err
		}
	}
	for _, b := range r.backups {
		from := b.Metadata.Annotations[model.ReplicatedFromAnnotation]
		if from == "" || b.Metadata.Labels[model.ProtectionPlanLabel] != r.plan.Metadata.Name || source[from] || r.pinned(b.Metadata.Name) {
			continue
		}
		if err := r.c.Kube.DeleteMachineBackup(ctx, b.Namespace(), b.Metadata.Name); err != nil && !kube.IsNotFound(err) {
			return fmt.Errorf("delete replica %s: %w", b.Metadata.Name, err)
		}
	}
	return nil
}

// pinned reports whether a running failover, or a test whose copies are
// still there, restores from name.
func (r *protection) pinned(name string) bool {
	for _, run := range []*model.ProtectionRun{r.st.Failover, r.st.Test} {
		if r.running(run) || run == r.st.Test && run != nil && run.Phase != model.ProtectionRunEnded {
			for _, p := range run.Points {
				if p == name {
					return true
				}
			}
		}
	}
	return false
}

func (r *protection) importBackup(ctx context.Context, pb model.MachineBackup) error {
	target := r.plan.Spec.Replication.Target
	b := model.MachineBackup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup},
		Metadata: model.ObjectMeta{
			Name: pb.Metadata.Name, Namespace: r.plan.Namespace(),
			Labels:      map[string]string{model.ProtectionPlanLabel: r.plan.Metadata.Name, model.BackupMachineLabel: pb.Spec.MachineName},
			Annotations: map[string]string{model.ReplicatedFromAnnotation: pb.Namespace() + "/" + pb.Metadata.Name},
		},
		Spec: model.MachineBackupSpec{
			MachineName: pb.Spec.MachineName, Target: &target, Mode: pb.Spec.Mode, Parent: pb.Spec.Parent,
			Compression: pb.Spec.Compression, Encryption: r.plan.Spec.Replication.Encryption,
		},
	}
	if _, err := r.c.Kube.CreateMachineBackup(ctx, r.plan.Namespace(), b); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("import backup %s: %w", pb.Metadata.Name, err)
	}
	status := map[string]any{
		"phase": model.BackupSucceeded, "message": "replicated from " + r.plan.Spec.Peer.Server,
		"chain": pb.Status.Chain, "kind": pb.Status.Kind, "pointInTime": pb.Status.PointInTime,
		"manifestKey": pb.Status.ManifestKey, "objects": pb.Status.Objects, "storedBytes": pb.Status.StoredBytes,
		"changedBytes": pb.Status.ChangedBytes, "completionTime": pb.Status.CompletionTime,
	}
	if err := r.c.Kube.PatchMachineBackupStatus(ctx, r.plan.Namespace(), pb.Metadata.Name, status); err != nil {
		return fmt.Errorf("import backup %s status: %w", pb.Metadata.Name, err)
	}
	r.c.Log.Info("protection plan imported restore point", "namespace", r.plan.Namespace(), "plan", r.plan.Metadata.Name, "backup", pb.Metadata.Name)
	imported := b
	imported.Status = pb.Status
	imported.Status.Disk = nil
	r.backups = append(r.backups, imported)
	return nil
}

// isAlreadyExists treats a create's 409 as the object being there already.
func isAlreadyExists(err error) bool { return kube.IsConflict(err) }

// transientAPIError reports an API call that may succeed if repeated: no
// response, a timeout, throttling or a server error.
func transientAPIError(err error) bool {
	var api *kube.APIError
	if errors.As(err, &api) {
		return api.StatusCode >= 500 || api.StatusCode == http.StatusTooManyRequests || api.StatusCode == http.StatusRequestTimeout
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

func (r *protection) backup(name string) (model.MachineBackup, bool) {
	for _, b := range r.backups {
		if b.Metadata.Name == name {
			return b, true
		}
	}
	return model.MachineBackup{}, false
}

func (r *protection) newestBackup(machine string, keep func(model.MachineBackup) bool) (model.MachineBackup, bool) {
	var best model.MachineBackup
	found := false
	for _, b := range r.backups {
		if b.Spec.MachineName != machine || b.Status.Phase != model.BackupSucceeded || b.Status.PointInTime == nil ||
			b.Metadata.DeletionTimestamp != nil || !keep(b) {
			continue
		}
		if !found || b.Status.PointInTime.After(*best.Status.PointInTime) {
			best, found = b, true
		}
	}
	return best, found
}

// machineStatuses reports each protected Machine's newest restore point
// among the backups keep accepts.
func (r *protection) machineStatuses(keep func(model.MachineBackup) bool, mirrored bool) []model.ProtectedMachineStatus {
	prev := map[string]model.ProtectedMachineStatus{}
	for _, ms := range r.st.Machines {
		prev[ms.Name] = ms
	}
	out := make([]model.ProtectedMachineStatus, 0, len(r.plan.Spec.Machines))
	for _, pm := range r.plan.Spec.Machines {
		ms := model.ProtectedMachineStatus{Name: pm.Name}
		if mirrored {
			ms.Spec, ms.Labels = prev[pm.Name].Spec, prev[pm.Name].Labels
			if ms.Spec == nil {
				ms.Message = "no spec mirrored from the peer yet"
			}
		}
		if b, ok := r.newestBackup(pm.Name, keep); ok {
			t := *b.Status.PointInTime
			ms.RestorePoint, ms.PointInTime = b.Metadata.Name, &t
		} else if ms.Message == "" {
			ms.Message = "no restore point yet"
		}
		out = append(out, ms)
	}
	return out
}

func (r *protection) observeObjectives() {
	var oldest *time.Time
	complete := len(r.st.Machines) > 0
	for _, ms := range r.st.Machines {
		if ms.PointInTime == nil {
			complete = false
			continue
		}
		if oldest == nil || ms.PointInTime.Before(*oldest) {
			oldest = ms.PointInTime
		}
	}
	r.st.RPOSeconds = nil
	if complete && oldest != nil {
		age := r.now.Sub(*oldest).Seconds()
		r.st.RPOSeconds = &age
	}
	peer := model.Condition{Type: "PeerReachable", Status: "True", Reason: "Reachable"}
	if r.st.Peer == nil || !r.st.Peer.Reachable {
		peer.Status, peer.Reason = "False", "Unreachable"
		if r.st.Peer != nil {
			peer.Message = r.st.Peer.Message
		}
	}
	r.st.Conditions = model.SetCondition(r.st.Conditions, peer)
	rpo, _ := model.Objective(r.plan.Spec.Objectives.RPO)
	if rpo == 0 {
		r.st.Conditions = removeCondition(r.st.Conditions, "RPOMet")
		return
	}
	met := model.Condition{Type: "RPOMet", Status: "True", Reason: "WithinObjective"}
	switch {
	case r.st.RPOSeconds == nil:
		met.Status, met.Reason, met.Message = "False", "NoRestorePoint", "not every Machine has a restore point"
	case *r.st.RPOSeconds > rpo.Seconds():
		met.Status, met.Reason = "False", "Exceeded"
		met.Message = fmt.Sprintf("newest restore points are %s old, objective %s", time.Duration(*r.st.RPOSeconds*float64(time.Second)).Round(time.Second), rpo)
	}
	r.st.Conditions = model.SetCondition(r.st.Conditions, met)
}

func removeCondition(conds []model.Condition, t string) []model.Condition {
	return slices.DeleteFunc(conds, func(c model.Condition) bool { return c.Type == t })
}

// stepRuns advances a running failover or DR test, and removes expired
// test copies.
func (r *protection) stepRuns(ctx context.Context) {
	if run := r.st.Failover; r.running(run) {
		r.st.Phase = model.ProtectionPhaseFailingOver
		if done := r.stepRun(ctx, run, false); done {
			r.finishFailover(run)
		}
	}
	if run := r.st.Test; run != nil {
		if run.ExpiresAt != nil && r.now.After(*run.ExpiresAt) && run.Phase != model.ProtectionRunEnded {
			r.endTest(ctx, "TTL expired")
			return
		}
		if r.running(run) {
			if done := r.stepRun(ctx, run, true); done {
				r.completeRun(run, "every test copy Running on bridge "+r.plan.Spec.Test.Bridge)
			}
		}
		r.recordTest(run)
		if run.Scheduled && (run.Phase == model.ProtectionRunSucceeded || run.Phase == model.ProtectionRunFailed) {
			r.endTest(ctx, "scheduled test finished")
		}
	}
}

func (r *protection) finishFailover(run *model.ProtectionRun) {
	r.completeRun(run, "every Machine Running")
	r.st.Active, r.st.Phase = true, model.ProtectionPhaseActive
	r.c.Log.Info("protection plan failover finished", "namespace", r.plan.Namespace(), "plan", r.plan.Metadata.Name, "id", run.ID, "rtoSeconds", *run.RTOSeconds, "epoch", r.st.Epoch)
}

func (r *protection) completeRun(run *model.ProtectionRun, msg string) {
	now := r.now
	rto := now.Sub(*run.StartedAt).Seconds()
	run.Phase, run.Message, run.CompletedAt, run.RTOSeconds = model.ProtectionRunSucceeded, msg, &now, &rto
	if rtoObj, _ := model.Objective(r.plan.Spec.Objectives.RTO); rtoObj > 0 && rto > rtoObj.Seconds() {
		run.Message += fmt.Sprintf("; took %s, over the %s objective", time.Duration(rto*float64(time.Second)).Round(time.Second), rtoObj)
	}
}

func (r *protection) failRun(run *model.ProtectionRun, msg string) {
	now := r.now
	run.Phase, run.Message, run.CompletedAt = model.ProtectionRunFailed, msg, &now
}

// stepRun moves a failover or test one step forward and reports whether
// every Machine is Running.
func (r *protection) stepRun(ctx context.Context, run *model.ProtectionRun, test bool) bool {
	if run.Points == nil {
		if !r.choosePoints(ctx, run) {
			return false
		}
	}
	waves, _ := r.plan.Spec.Order()
	for _, wave := range waves {
		waiting := []string{}
		for _, name := range wave {
			if slices.Contains(run.Restored, name) {
				continue
			}
			done, msg, err := r.stepMachine(ctx, run, name, test)
			if transientAPIError(err) {
				done, msg, err = false, "retrying: "+err.Error(), nil
			}
			if err != nil {
				r.failRun(run, fmt.Sprintf("%s: %v", name, err))
				return false
			}
			if done {
				run.Restored = append(run.Restored, name)
				continue
			}
			waiting = append(waiting, name+": "+msg)
		}
		if len(waiting) > 0 {
			run.Message = strings.Join(waiting, "; ")
			return false
		}
	}
	return true
}

// choosePoints fixes the backup each Machine restores from: the final
// backups of a planned failover's handoff, else the newest replicas.
func (r *protection) choosePoints(ctx context.Context, run *model.ProtectionRun) bool {
	points := map[string]string{}
	if run.Planned {
		if r.peer == nil {
			run.Message = "planned failover: waiting for the peer to be reachable"
			return false
		}
		if r.peerPlan.Metadata.Annotations[model.ProtectionHandoffAnnotation] != run.ID {
			patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{model.ProtectionHandoffAnnotation: run.ID}}}
			if err := r.peer.PatchMachineProtectionPlan(ctx, r.plan.PeerNamespace(), r.plan.PeerPlan(), patch); err != nil {
				run.Message = fmt.Sprintf("planned failover: ask the peer to hand off: %v", err)
				return false
			}
			run.Message = "planned failover: asked the peer to halt its Machines and take final backups"
			return false
		}
		h := r.peerPlan.Status.Handoff
		if h == nil || h.ID != run.ID || h.Phase != model.ProtectionHandoffReady {
			msg := "waiting for the peer's handoff"
			if h != nil && h.ID == run.ID {
				msg += ": " + h.Phase + " " + h.Message
			}
			run.Message = "planned failover: " + msg
			return false
		}
		for _, name := range h.Backups {
			b, ok := r.backup(name)
			if !ok || !b.Replicated() {
				run.Message = "planned failover: waiting to import final backup " + name
				return false
			}
			points[b.Spec.MachineName] = name
		}
		r.st.Epoch = max(r.st.Epoch, r.st.Peer.Epoch) + 1
	}
	var oldest *time.Time
	for _, pm := range r.plan.Spec.Machines {
		if _, ok := points[pm.Name]; !ok {
			b, found := r.newestBackup(pm.Name, func(b model.MachineBackup) bool {
				return b.Replicated() && b.Metadata.Labels[model.ProtectionPlanLabel] == r.plan.Metadata.Name
			})
			if !found {
				r.failRun(run, "no restore point for "+pm.Name)
				return false
			}
			points[pm.Name] = b.Metadata.Name
		}
		b, _ := r.backup(points[pm.Name])
		if b.Status.PointInTime != nil && (oldest == nil || b.Status.PointInTime.Before(*oldest)) {
			oldest = b.Status.PointInTime
		}
	}
	run.Points = points
	if oldest != nil {
		loss := max(run.StartedAt.Sub(*oldest).Seconds(), 0)
		run.DataLossSeconds = &loss
	}
	return true
}

// stepMachine takes one protected Machine through create, halt, restore
// and start. It returns done once the Machine is Running.
func (r *protection) stepMachine(ctx context.Context, run *model.ProtectionRun, name string, test bool) (bool, string, error) {
	target := name
	if test {
		target = name + protectionTestSuffix
	}
	m, ok := r.machines[target]
	if !ok {
		var mirrored *model.ProtectedMachineStatus
		for i := range r.st.Machines {
			if r.st.Machines[i].Name == name {
				mirrored = &r.st.Machines[i]
			}
		}
		if mirrored == nil || mirrored.Spec == nil {
			return false, "", fmt.Errorf("no spec mirrored from the peer to create it from")
		}
		if p := r.netProblems[name]; p != "" && !test {
			return false, "", fmt.Errorf("network.preserveIP: %s", p)
		}
		nm := model.Machine{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
			Metadata: model.ObjectMeta{Name: target, Namespace: r.plan.Namespace(), Labels: map[string]string{}},
			Spec:     *mirrored.Spec,
		}
		for k, v := range mirrored.Labels {
			nm.Metadata.Labels[k] = v
		}
		nm.Spec.PowerState = "Running"
		if test {
			nm.Metadata.Labels = map[string]string{protectionTestLabel: r.plan.Metadata.Name}
			nm.Spec.Network = model.NetworkSpec{Mode: "tap", Bridge: r.plan.Spec.Test.Bridge}
			nm.Spec.ServiceFabric = model.ServiceFabricSpec{}
			nm.Spec.TTLSeconds = 0
		} else {
			nm.Metadata.Labels[model.ProtectionPlanLabel] = r.plan.Metadata.Name
		}
		if _, err := r.c.Kube.CreateMachine(ctx, r.plan.Namespace(), nm); err != nil && !isAlreadyExists(err) {
			return false, "", fmt.Errorf("create Machine: %w", err)
		}
		if !slices.Contains(run.Machines, target) {
			run.Machines = append(run.Machines, target)
		}
		return false, "creating " + target, nil
	}
	restoreName := protectionRestoreName(r.plan.Metadata.Name, run.ID, target)
	restore, err := r.c.Kube.GetMachineBackupRestore(ctx, r.plan.Namespace(), restoreName)
	if kube.IsNotFound(err) {
		if m.Status.RuntimeID == "" {
			return false, "waiting for its runtime", nil
		}
		if m.DesiredPowerState() != "Halted" {
			if err := r.c.Kube.PatchMachine(ctx, r.plan.Namespace(), target, map[string]any{"spec": map[string]any{"powerState": "Halted"}}); err != nil {
				return false, "", fmt.Errorf("halt: %w", err)
			}
			return false, "halting", nil
		}
		if m.Status.Phase != "Halted" {
			return false, "halting", nil
		}
		_, err = r.c.Kube.CreateMachineBackupRestore(ctx, r.plan.Namespace(), model.MachineBackupRestore{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupRestore},
			Metadata: model.ObjectMeta{Name: restoreName, Namespace: r.plan.Namespace(), Labels: map[string]string{model.ProtectionPlanLabel: r.plan.Metadata.Name}},
			Spec:     model.MachineBackupRestoreSpec{BackupName: run.Points[name], MachineName: target},
		})
		if err != nil && !isAlreadyExists(err) {
			return false, "", fmt.Errorf("create restore: %w", err)
		}
		return false, "restoring " + run.Points[name], nil
	}
	if err != nil {
		return false, "", fmt.Errorf("get restore: %w", err)
	}
	switch restore.Status.Phase {
	case model.BackupFailed:
		return false, "", fmt.Errorf("restore %s failed: %s", restoreName, restore.Status.Message)
	case model.BackupSucceeded:
	default:
		return false, "restoring " + run.Points[name], nil
	}
	if m.DesiredPowerState() != "Running" {
		if err := r.c.Kube.PatchMachine(ctx, r.plan.Namespace(), target, map[string]any{"spec": map[string]any{"powerState": "Running"}}); err != nil {
			return false, "", fmt.Errorf("start: %w", err)
		}
		return false, "starting", nil
	}
	if m.Status.Phase != "Running" {
		return false, "starting", nil
	}
	return true, "", nil
}

func protectionRestoreName(plan, runID, machine string) string {
	name := fmt.Sprintf("dr-%s-%s-%s", plan, shortHash(runID), machine)
	if len(name) > 63 {
		name = name[:54] + "-" + shortHash(name)
	}
	return name
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s)) // #nosec G401 -- naming only
	return hex.EncodeToString(sum[:])[:8]
}

// endTest deletes a DR test's copies and restores.
func (r *protection) endTest(ctx context.Context, why string) {
	run := r.st.Test
	var failed []string
	names := append([]string(nil), run.Machines...)
	sort.Strings(names)
	for _, target := range names {
		if err := r.c.Kube.DeleteMachine(ctx, r.plan.Namespace(), target); err != nil && !kube.IsNotFound(err) {
			failed = append(failed, fmt.Sprintf("%s: %v", target, err))
		}
		if err := r.c.Kube.DeleteMachineBackupRestore(ctx, r.plan.Namespace(), protectionRestoreName(r.plan.Metadata.Name, run.ID, target)); err != nil && !kube.IsNotFound(err) {
			failed = append(failed, fmt.Sprintf("restore of %s: %v", target, err))
		}
	}
	if len(failed) > 0 {
		run.Message = "cleaning up: " + strings.Join(failed, "; ")
		return
	}
	if run.Phase == model.ProtectionRunRunning {
		r.failRun(run, "ended before every copy was Running")
	}
	r.recordTest(run)
	run.Phase = model.ProtectionRunEnded
	run.Message = strings.TrimPrefix(run.Message+"; test copies removed ("+why+")", "; ")
}
