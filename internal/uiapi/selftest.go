// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/selftest"
)

const (
	selftestKeep    = 5
	selftestTimeout = 25 * time.Minute
)

// selftestLine is one log line of a run, timestamped relative to its start.
type selftestLine struct {
	AtMs  int64  `json:"atMs"`
	Level string `json:"level"`
	Step  string `json:"step,omitempty"`
	Msg   string `json:"msg"`
}

// selftestStep is a scenario's live state.
type selftestStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // running, pass, fail, skip
	Reason string `json:"reason,omitempty"`
	Hint   string `json:"hint,omitempty"`
	DurMs  int64  `json:"durationMs,omitempty"`
}

// selftestJob is one run. It is the Reporter for the engine and is read by
// the polling GET handler, so every access takes mu.
type selftestJob struct {
	mu       sync.Mutex
	ID       string         `json:"id"`
	Profile  string         `json:"profile"`
	State    string         `json:"state"` // running, passed, failed, cancelled
	Started  time.Time      `json:"started"`
	Finished *time.Time     `json:"finished,omitempty"`
	Lines    []selftestLine `json:"-"`
	Steps    []selftestStep `json:"-"`
	Summary  *selftest.Summary
	cancel   context.CancelFunc
	cur      string
}

func (j *selftestJob) Log(l selftest.Level, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Lines = append(j.Lines, selftestLine{AtMs: time.Since(j.Started).Milliseconds(), Level: string(l), Step: j.cur, Msg: msg})
}

func (j *selftestJob) StepStart(id, title string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cur = id
	j.Steps = append(j.Steps, selftestStep{ID: id, Title: title, Status: "running"})
}

func (j *selftestJob) StepDone(r selftest.Result) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := range j.Steps {
		if j.Steps[i].ID == r.ID {
			j.Steps[i].Status = string(r.Status)
			j.Steps[i].Reason = r.Reason
			j.Steps[i].Hint = r.Hint
			j.Steps[i].DurMs = r.Duration.Milliseconds()
		}
	}
	j.cur = ""
}

type selftestView struct {
	ID       string            `json:"id"`
	Profile  string            `json:"profile"`
	State    string            `json:"state"`
	Started  time.Time         `json:"started"`
	Finished *time.Time        `json:"finished,omitempty"`
	Steps    []selftestStep    `json:"steps"`
	Lines    []selftestLine    `json:"lines"`
	Next     int               `json:"next"` // pass as ?after= to fetch only newer lines
	Summary  *selftest.Summary `json:"summary,omitempty"`
}

func (j *selftestJob) view(after int) selftestView {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after < 0 || after > len(j.Lines) {
		after = 0
	}
	return selftestView{
		ID: j.ID, Profile: j.Profile, State: j.State, Started: j.Started, Finished: j.Finished,
		Steps: append([]selftestStep(nil), j.Steps...), Lines: append([]selftestLine(nil), j.Lines[after:]...),
		Next: len(j.Lines), Summary: j.Summary,
	}
}

// selftestStore keeps the last few runs and enforces one at a time.
type selftestStore struct {
	mu   sync.Mutex
	jobs []*selftestJob
}

func (s *Server) selftests() *selftestStore {
	s.selftestOnce.Do(func() { s.selftestJobs = &selftestStore{} })
	return s.selftestJobs
}

func (st *selftestStore) running() *selftestJob {
	for _, j := range st.jobs {
		j.mu.Lock()
		run := j.State == "running"
		j.mu.Unlock()
		if run {
			return j
		}
	}
	return nil
}

func (st *selftestStore) get(id string) *selftestJob {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, j := range st.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

type selftestRequest struct {
	Profile string   `json:"profile"`
	Only    []string `json:"only"`
	Skip    []string `json:"skip"`
	NoVM    bool     `json:"noVm"`
	Keep    bool     `json:"keep"`
}

// handleSelftestScenarios serves GET /api/v1/selftest/scenarios.
func (s *Server) handleSelftestScenarios(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Profile string `json:"profile"`
	}
	var out []row
	for _, sc := range selftest.List() {
		out = append(out, row{ID: sc.ID, Title: sc.Title, Profile: string(sc.Profile)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSelftestStart serves POST /api/v1/selftest. It creates and deletes
// real Machines, so it is administrator-only and one run at a time.
func (s *Server) handleSelftestStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req selftestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	profile, err := selftest.ParseProfile(req.Profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if bad := selftest.UnknownIDs(append(append([]string{}, req.Only...), req.Skip...)); len(bad) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown scenario(s): %v", bad))
		return
	}

	st := s.selftests()
	st.mu.Lock()
	if st.running() != nil {
		st.mu.Unlock()
		writeError(w, http.StatusConflict, "a self-test is already running")
		return
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	ctx, cancel := context.WithTimeout(context.Background(), selftestTimeout)
	job := &selftestJob{ID: hex.EncodeToString(b), Profile: string(profile), State: "running", Started: time.Now(), cancel: cancel}
	st.jobs = append(st.jobs, job)
	if len(st.jobs) > selftestKeep {
		st.jobs = st.jobs[len(st.jobs)-selftestKeep:]
	}
	st.mu.Unlock()

	ns := s.SelftestNamespace
	if ns == "" {
		ns = "default"
	}
	env := selftest.Env{
		Kube: s.Kube, Namespace: ns, NoVM: req.NoVM, Keep: req.Keep,
		ControlPlane: s.selftestControlPlane,
		SerialLog:    s.selftestSerialLog,
		Metrics: func(_ context.Context, ns, name string) (int, error) {
			return len(s.usageHistory().query(ns, name, time.Now().Add(-15*time.Minute))), nil
		},
	}
	user := usernameFromContext(r.Context())
	go func() {
		defer cancel()
		if s.Log != nil {
			s.Log.Info("uiapi selftest started", "id", job.ID, "profile", profile, "user", user)
		}
		results := selftest.Run(ctx, env, job, selftest.Options{Profile: profile, Only: req.Only, Skip: req.Skip})
		sum := selftest.Summarize(results)
		now := time.Now()
		job.mu.Lock()
		job.Finished, job.Summary = &now, &sum
		switch {
		case ctx.Err() != nil && ctx.Err() == context.Canceled:
			job.State = "cancelled"
		case sum.Fail > 0:
			job.State = "failed"
		default:
			job.State = "passed"
		}
		job.mu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID})
}

// handleSelftestList serves GET /api/v1/selftest.
func (s *Server) handleSelftestList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	st := s.selftests()
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]selftestView, 0, len(st.jobs))
	for i := len(st.jobs) - 1; i >= 0; i-- {
		v := st.jobs[i].view(0)
		v.Lines, v.Steps = nil, nil
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSelftestGet serves GET /api/v1/selftest/{id}?after=N.
func (s *Server) handleSelftestGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	job := s.selftests().get(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, "no such self-test run")
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	writeJSON(w, http.StatusOK, job.view(after))
}

// handleSelftestCancel serves DELETE /api/v1/selftest/{id}: cancels a
// running run; the engine still deletes the Machines it created.
func (s *Server) handleSelftestCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	job := s.selftests().get(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, "no such self-test run")
		return
	}
	job.cancel()
	w.WriteHeader(http.StatusNoContent)
}

// selftestControlPlane is the dashboard's version of the doctor checks: what
// kairon-ui can see with its own service account.
func (s *Server) selftestControlPlane(ctx context.Context) []selftest.Check {
	var out []selftest.Check
	nodes, err := s.Kube.ListNodes(ctx)
	if err != nil {
		return []selftest.Check{{Name: "API server", Status: selftest.Fail, Detail: err.Error(), Hint: "check kairon-ui's connection to the Kubernetes API"}}
	}
	out = append(out, selftest.Check{Name: "API server", Status: selftest.Pass, Detail: fmt.Sprintf("%d node(s) listed", len(nodes))})

	capable := 0
	for _, n := range nodes {
		if n.Metadata.Labels[model.CapableLabel] == "true" {
			capable++
		}
	}
	if capable == 0 {
		out = append(out, selftest.Check{Name: "Capable nodes", Status: selftest.Fail,
			Detail: "no node carries " + model.CapableLabel + "=true", Hint: "kubectl label node <name> " + model.CapableLabel + "=true"})
	} else {
		out = append(out, selftest.Check{Name: "Capable nodes", Status: selftest.Pass, Detail: fmt.Sprintf("%d node(s)", capable)})
	}

	pods, err := s.Kube.ListPods(ctx, "")
	if err != nil {
		return append(out, selftest.Check{Name: "Kairon pods", Status: selftest.Skip, Detail: "not checked: " + err.Error()})
	}
	by := map[string][2]int{} // component -> {pods, ready}
	for _, p := range pods {
		c := kaironComponent(p)
		if c == "" {
			continue
		}
		v := by[c]
		v[0]++
		if p.Ready && p.Phase == "Running" {
			v[1]++
		}
		by[c] = v
	}
	if len(by) == 0 {
		return append(out, selftest.Check{Name: "Kairon pods", Status: selftest.Skip, Detail: "no kairon-* pods (components may run as systemd units)"})
	}
	for _, c := range []string{"kairon-controller", "kairon-node", "kairon-ui"} {
		v, ok := by[c]
		switch {
		case !ok:
			out = append(out, selftest.Check{Name: c, Status: selftest.Skip, Detail: "no pods"})
		case v[1] == v[0]:
			out = append(out, selftest.Check{Name: c, Status: selftest.Pass, Detail: fmt.Sprintf("%d/%d pod(s) ready", v[1], v[0])})
		default:
			out = append(out, selftest.Check{Name: c, Status: selftest.Fail, Detail: fmt.Sprintf("%d/%d pod(s) ready", v[1], v[0]), Hint: "kaironctl logs " + c[len("kairon-"):]})
		}
	}
	return out
}

// selftestSerialLog reads a Machine's serial console through the same node
// relay handleLogs uses, without needing an HTTP request of its own.
func (s *Server) selftestSerialLog(ctx context.Context, ns, name string) (string, error) {
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		return "", fmt.Errorf("console relay is not enabled on this deployment")
	}
	m, err := s.Kube.GetMachine(ctx, ns, name)
	if err != nil {
		return "", err
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		return "", nil // not started yet; the caller polls
	}
	addr, err := s.nodeInternalIP(ctx, m.Status.NodeName)
	if err != nil {
		return "", err
	}
	scheme, transport := "http", http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme, transport = "https", &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	u := fmt.Sprintf("%s://%s:%s/logs/%s?lines=400&follow=false", scheme, addr, s.ConsolePort, m.Status.RuntimeID)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("node logs relay returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}
