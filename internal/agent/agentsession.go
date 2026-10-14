// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// agentSessionState is kairon-node's own bookkeeping for AgentSessions:
// the capability probe's last answer and the creates still running.
type agentSessionState struct {
	mu        sync.Mutex
	probedAt  time.Time
	probed    bool
	supported bool
	// jobs holds a *sessionCreate per namespace/name while FluxVM is
	// creating it; a create boots a VM and clones a repository, far longer
	// than one reconcile tick.
	jobs sync.Map
}

type sessionCreate struct {
	done    chan struct{}
	session *fluxvm.AgentSession
	err     error
}

// sessionAdoptGrace is how long after a create was requested a restarted
// kairon-node waits for that session to show up in FluxVM before asking
// again.
const sessionAdoptGrace = 2 * time.Minute

// agentSessionHostOS is the OS FluxVM runs on (always this kairon-node's
// host). FluxVM through 0.4.8 lists sessions on Linux but every create there
// fails with "sandbox create requires template or spec", so only macOS
// hosts advertise the capability.
var agentSessionHostOS = runtime.GOOS

// reconcileAgentSessions drives every AgentSession assigned to this node:
// create it in FluxVM (or adopt a fork child FluxVM already made), mirror
// FluxVM's record into status every tick, and delete it from FluxVM before
// releasing the finalizer.
func (a *Agent) reconcileAgentSessions(ctx context.Context) {
	if a.Kube == nil || a.Flux == nil || a.NodeName == "" {
		return
	}
	supported := a.publishAgentSessionCapability(ctx)
	sessions, err := a.Kube.ListAgentSessionsWithSelector(ctx, model.AssignedNodeLabelSelector(a.NodeName))
	if err != nil {
		if !kube.IsNotFound(err) {
			a.log().Error("agent session list failed", "error", err)
		}
		return
	}
	claimed := map[string]bool{}
	for _, s := range sessions {
		if s.Status.SessionID != "" {
			claimed[s.Status.SessionID] = true
		}
		if id := s.Adopted(); id != "" {
			claimed[id] = true
		}
	}
	for _, s := range sessions {
		if s.Spec.NodeName != a.NodeName {
			continue
		}
		if err := a.reconcileAgentSession(ctx, s, supported, claimed); err != nil {
			a.log().Error("agent session reconcile failed", "namespace", s.Namespace(), "session", s.Metadata.Name, "error", err)
			if a.Metrics != nil {
				a.Metrics.ObserveReconcileItemError("agentsession")
			}
		}
	}
}

func (a *Agent) reconcileAgentSession(ctx context.Context, s model.AgentSession, supported bool, claimed map[string]bool) error {
	key := s.Namespace() + "/" + s.Metadata.Name
	if s.Metadata.DeletionTimestamp != nil {
		return a.deleteAgentSession(ctx, s, key)
	}
	if !model.HasFinalizerList(s.Metadata.Finalizers, model.FinalizerAgentSession) {
		finalizers := append(append([]string{}, s.Metadata.Finalizers...), model.FinalizerAgentSession)
		if err := a.Kube.PatchAgentSession(ctx, s.Namespace(), s.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finalizers}}); err != nil {
			return err
		}
	}
	status := s.Status
	status.NodeName = a.NodeName
	if status.Phase == model.AgentSessionEnded || status.Phase == model.AgentSessionFailed {
		return a.patchAgentSessionStatus(ctx, s, status)
	}
	id := status.SessionID
	if id == "" {
		id = s.Adopted()
	}
	if id == "" {
		return a.createAgentSession(ctx, s, status, key, supported, claimed)
	}
	fs, err := a.Flux.GetFluxSession(ctx, id)
	switch {
	case errors.Is(err, fluxvm.ErrSessionNotFound):
		status.SessionID = id
		status.Phase = model.AgentSessionEnded
		status.State = "gone"
		status.Message = "FluxVM has no record of session " + id
	case err != nil:
		return err
	default:
		applyAgentSession(&status, fs, time.Now())
	}
	return a.patchAgentSessionStatus(ctx, s, status)
}

// createAgentSession starts (or collects) the FluxVM create for s. The
// create runs in its own goroutine; status says Creating meanwhile, with
// the time it was asked for, so a kairon-node restarted mid-create adopts
// the session it made rather than making a second one.
func (a *Agent) createAgentSession(ctx context.Context, s model.AgentSession, status model.AgentSessionStatus, key string, supported bool, claimed map[string]bool) error {
	if v, ok := a.sessions.jobs.Load(key); ok {
		job := v.(*sessionCreate)
		select {
		case <-job.done:
		default:
			return nil
		}
		a.sessions.jobs.Delete(key)
		if job.err != nil {
			status.Phase = model.AgentSessionFailed
			status.Message = "FluxVM create failed: " + job.err.Error()
		} else {
			claimed[job.session.ID] = true
			applyAgentSession(&status, job.session, time.Now())
		}
		return a.patchAgentSessionStatus(ctx, s, status)
	}
	if status.Phase == model.AgentSessionCreating && status.CreateRequestedAt != nil {
		orphan, err := a.findOrphanAgentSession(ctx, s, *status.CreateRequestedAt, claimed)
		if err != nil {
			return err
		}
		if orphan != nil {
			claimed[orphan.ID] = true
			applyAgentSession(&status, orphan, time.Now())
			return a.patchAgentSessionStatus(ctx, s, status)
		}
		if time.Since(*status.CreateRequestedAt) < sessionAdoptGrace {
			return nil
		}
	}
	if !supported {
		status.Phase = model.AgentSessionPending
		status.Message = "this node's FluxVM does not serve /v1/agents/sessions"
		return a.patchAgentSessionStatus(ctx, s, status)
	}
	if err := model.ValidateAgentSessionSpec(s.Spec); err != nil {
		status.Phase = model.AgentSessionFailed
		status.Message = "spec." + err.Error()
		return a.patchAgentSessionStatus(ctx, s, status)
	}
	now := time.Now().UTC()
	status.Phase = model.AgentSessionCreating
	status.Message = "creating the session in FluxVM"
	status.CreateRequestedAt = &now
	if err := a.patchAgentSessionStatus(ctx, s, status); err != nil {
		return err
	}
	job := &sessionCreate{done: make(chan struct{})}
	a.sessions.jobs.Store(key, job)
	spec := s.Spec
	go func() {
		defer close(job.done)
		job.session, job.err = a.Flux.CreateFluxSession(context.Background(), spec)
	}()
	a.log().Info("agent session create started", "namespace", s.Namespace(), "session", s.Metadata.Name)
	return nil
}

// findOrphanAgentSession is the running FluxVM session no AgentSession
// references, with s's agent, created since requestedAt: the one a
// previous kairon-node process asked for.
func (a *Agent) findOrphanAgentSession(ctx context.Context, s model.AgentSession, requestedAt time.Time, claimed map[string]bool) (*fluxvm.AgentSession, error) {
	all, err := a.Flux.ListFluxSessions(ctx)
	if err != nil {
		return nil, err
	}
	agent := s.Spec.Agent
	if agent == "" {
		agent = "custom"
	}
	var found []fluxvm.AgentSession
	for _, fs := range all {
		if fs.State == "running" && fs.Parent == "" && fs.Agent == agent && !claimed[fs.ID] && !fs.CreatedAt.Before(requestedAt.Add(-5*time.Second)) {
			found = append(found, fs)
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.Before(found[j].CreatedAt) })
	return &found[0], nil
}

func (a *Agent) deleteAgentSession(ctx context.Context, s model.AgentSession, key string) error {
	id := s.Status.SessionID
	if id == "" {
		id = s.Adopted()
	}
	if v, ok := a.sessions.jobs.Load(key); ok {
		job := v.(*sessionCreate)
		select {
		case <-job.done:
		default:
			return nil
		}
		if job.err == nil && id == "" {
			id = job.session.ID
		}
	}
	if id != "" {
		if err := a.Flux.DeleteFluxSession(ctx, id); err != nil {
			return fmt.Errorf("delete FluxVM session %s: %w", id, err)
		}
	}
	a.sessions.jobs.Delete(key)
	if !model.HasFinalizerList(s.Metadata.Finalizers, model.FinalizerAgentSession) {
		return nil
	}
	err := a.Kube.PatchAgentSession(ctx, s.Namespace(), s.Metadata.Name, map[string]any{"metadata": map[string]any{
		"finalizers": model.RemoveFinalizer(s.Metadata.Finalizers, model.FinalizerAgentSession),
	}})
	if kube.IsNotFound(err) {
		return nil
	}
	if err == nil {
		a.log().Info("agent session deleted", "namespace", s.Namespace(), "session", s.Metadata.Name, "id", id)
	}
	return err
}

// applyAgentSession copies FluxVM's record into status. A session FluxVM
// reports expired, gone or deleted (or whose expiry has passed) is Ended.
func applyAgentSession(status *model.AgentSessionStatus, fs *fluxvm.AgentSession, now time.Time) {
	status.SessionID = fs.ID
	status.SandboxID = fs.SandboxID
	status.Backend = fs.Backend
	status.State = fs.State
	status.Parent = fs.Parent
	status.Children = append([]string(nil), fs.Children...)
	created := fs.CreatedAt
	status.CreatedAt = &created
	status.ExpiresAt = fs.ExpiresAt
	caps := model.AgentSessionCapabilities(fs.Capabilities)
	status.Capabilities = &caps
	policy := &model.AgentSessionPolicyStatus{
		Preset:             fs.Policy.Preset,
		Offline:            fs.Policy.Offline,
		AllowHosts:         fs.Policy.AllowHosts,
		Unenforced:         fs.Policy.Unenforced,
		HTTPRules:          fs.Policy.HTTPRules,
		Enforcement:        fs.Policy.Enforcement,
		NetworkEnforcement: fs.Policy.NetworkEnforcement,
	}
	if p := fs.Policy.Process; p != nil {
		policy.Process = &model.AgentProcessPolicy{Allow: p.Allow, Deny: p.Deny, Ask: p.Ask}
	}
	status.Policy = policy
	status.Workspace = nil
	if w := fs.Workspace; w != nil {
		status.Workspace = &model.AgentWorkspaceStatus{
			Path: w.Path, Git: model.RedactGitURL(w.Git), Mode: w.Mode,
			Cloned: w.Cloned, Baseline: w.Baseline, Note: w.Note,
		}
	}
	status.Checkpoints = nil
	for _, cp := range fs.Checkpoints {
		created := cp.CreatedAt
		status.Checkpoints = append(status.Checkpoints, model.AgentCheckpoint{Tag: cp.Tag, Memory: cp.Memory, CreatedAt: &created})
	}
	status.Usage = nil
	if u := fs.Usage; u != nil {
		status.Usage = &model.AgentSessionUsage{Execs: u.Execs, ExecMillis: u.ExecMillis, Jobs: u.Jobs, Children: u.Children, WorkspaceBytes: u.WorkspaceBytes}
	}
	expired := fs.ExpiresAt != nil && !fs.ExpiresAt.After(now)
	if fs.State == "running" && !expired {
		status.Phase = model.AgentSessionRunning
		status.Message = ""
		return
	}
	status.Phase = model.AgentSessionEnded
	state := fs.State
	if state == "running" {
		state = "expired"
	}
	status.Message = "FluxVM reports the session " + state
}

func (a *Agent) patchAgentSessionStatus(ctx context.Context, s model.AgentSession, status model.AgentSessionStatus) error {
	if reflect.DeepEqual(s.Status, status) {
		return nil
	}
	fields, err := statusMergePatch(s.Status, status)
	if err != nil {
		return err
	}
	return a.Kube.PatchAgentSessionStatusFields(ctx, s.Namespace(), s.Metadata.Name, fields)
}

// publishAgentSessionCapability keeps model.LabelAgentSessionsCapable on
// this node in step with whether its FluxVM serves agent sessions,
// probing once per capabilityEvery, and returns the last answer. A probe
// that fails outright (FluxVM down) keeps the previous answer.
func (a *Agent) publishAgentSessionCapability(ctx context.Context) bool {
	st := &a.sessions
	st.mu.Lock()
	due := !st.probed || time.Since(st.probedAt) >= capabilityEvery
	supported := st.supported
	if due {
		st.probedAt, st.probed = time.Now(), true
	}
	st.mu.Unlock()
	if !due {
		return supported
	}
	want, err := a.Flux.AgentSessionsSupported(ctx)
	if err != nil {
		a.log().Debug("FluxVM agent session probe failed", "error", err)
		return supported
	}
	want = want && agentSessionHostOS == "darwin"
	st.mu.Lock()
	st.supported = want
	st.mu.Unlock()
	node, err := a.Kube.GetNode(ctx, a.NodeName)
	if err != nil {
		a.log().Debug("node read for agent-sessions label failed", "error", err)
		return want
	}
	if (node.Metadata.Labels[model.LabelAgentSessionsCapable] == "true") == want {
		return want
	}
	var value any
	if want {
		value = "true"
	}
	patch := map[string]any{"metadata": map[string]any{"labels": map[string]any{model.LabelAgentSessionsCapable: value}}}
	if err := a.Kube.PatchNode(ctx, a.NodeName, patch); err != nil {
		a.log().Warn("agent-sessions label patch failed", "node", a.NodeName, "error", err)
		return want
	}
	a.log().Info("agent-sessions label updated", "node", a.NodeName, "supported", want)
	return want
}
