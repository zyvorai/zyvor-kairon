// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package selftest runs a short, self-cleaning series of checks against a
// live Kairon install: it boots a tiny CirrOS Machine and exercises power,
// pause, network, hotplug and (when the cluster allows) fork and migration.
// It knows nothing about terminals or HTTP: callers (kaironctl, kairon-ui)
// supply a Kube implementation and a Reporter and decide how to render.
package selftest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// Status is the outcome of one scenario.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Level classifies a log line for colouring.
type Level string

const (
	LevelInfo Level = "info"
	LevelOK   Level = "ok"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
	// LevelVM marks a line copied from the guest's serial console.
	LevelVM Level = "vm"
)

// Profile selects how much to run. Each profile includes the ones before it.
type Profile string

const (
	ProfileQuick    Profile = "quick"
	ProfileStandard Profile = "standard"
	ProfileFull     Profile = "full"
)

func (p Profile) rank() int {
	switch p {
	case ProfileQuick:
		return 1
	case ProfileStandard:
		return 2
	case ProfileFull:
		return 3
	}
	return 0
}

// ParseProfile validates a profile name; the empty string is "standard".
func ParseProfile(s string) (Profile, error) {
	switch p := Profile(strings.ToLower(s)); p {
	case "":
		return ProfileStandard, nil
	case ProfileQuick, ProfileStandard, ProfileFull:
		return p, nil
	}
	return "", fmt.Errorf("unknown profile %q (quick, standard or full)", s)
}

// Result is the final state of one scenario.
type Result struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Status   Status        `json:"status"`
	Reason   string        `json:"reason,omitempty"` // why it failed or was skipped
	Hint     string        `json:"hint,omitempty"`   // what to do about it
	Duration time.Duration `json:"durationNs"`
}

// Reporter receives progress. Implementations must be safe to call from the
// runner's goroutine only; the runner never calls it concurrently.
type Reporter interface {
	Log(level Level, msg string)
	StepStart(id, title string)
	StepDone(r Result)
}

// Kube is the subset of *kube.Client the engine uses; a fake satisfies it in
// tests.
type Kube interface {
	CreateMachine(ctx context.Context, ns string, m model.Machine) (model.Machine, error)
	GetMachine(ctx context.Context, ns, name string) (model.Machine, error)
	DeleteMachine(ctx context.Context, ns, name string) error
	PatchMachine(ctx context.Context, ns, name string, patch map[string]any) error
	ListNodes(ctx context.Context) ([]model.Node, error)
	ListEventsFor(ctx context.Context, namespace, kind, name string) ([]model.Event, error)
	CreateMachineMigration(ctx context.Context, ns string, m model.MachineMigration) (model.MachineMigration, error)
	GetMachineMigration(ctx context.Context, ns, name string) (model.MachineMigration, error)
}

// Check is one control-plane probe result supplied by the caller.
type Check struct {
	Name   string
	Status Status // Pass, Fail or Skip; a warning is reported as Pass with Warn=true
	Warn   bool
	Detail string
	Hint   string
}

// Env is everything a run needs from its surroundings.
type Env struct {
	Kube      Kube
	Namespace string // where test Machines are created

	// Image selection. ImagePath (a node-local file) wins; otherwise
	// ImageURL+ImageDigest; otherwise the pinned CirrOS for Arch.
	ImagePath   string
	ImageURL    string
	ImageDigest string
	Arch        string // amd64 or arm64; detected from the nodes when empty

	// ControlPlane runs the doctor-style checks. Nil skips that scenario.
	ControlPlane func(ctx context.Context) []Check
	// SerialLog returns the guest's serial console text. Nil disables the
	// log-based checks (boot is then proven by phase alone).
	SerialLog func(ctx context.Context, ns, name string) (string, error)
	// Metrics returns how many usage samples the dashboard has for a
	// Machine. Nil skips that part of the observability check.
	Metrics func(ctx context.Context, ns, name string) (int, error)
	// Dial connects to addr and returns what the peer sends first. Nil uses
	// a plain TCP dial.
	Dial func(ctx context.Context, addr string) (string, error)

	Bridge string // host bridge for the NIC hotplug scenario
	PVC    string // PersistentVolumeClaim for the disk hotplug scenario

	NoVM         bool
	Keep         bool
	BootTimeout  time.Duration
	StepTimeout  time.Duration
	PollInterval time.Duration
	// RunID is appended to resource names; generated when empty.
	RunID string
	Now   func() time.Time
}

// LabelKey marks every Machine a run creates, so leftovers are findable.
const LabelKey = "kairon.zyvor.dev/selftest"

func (e *Env) defaults() {
	if e.BootTimeout == 0 {
		e.BootTimeout = 4 * time.Minute
	}
	if e.StepTimeout == 0 {
		e.StepTimeout = 90 * time.Second
	}
	if e.PollInterval == 0 {
		e.PollInterval = 2 * time.Second
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Namespace == "" {
		e.Namespace = "default"
	}
}

// Options filter which scenarios run.
type Options struct {
	Profile Profile
	Only    []string
	Skip    []string
}

// Scenario is one check. Needs returns a non-empty reason to skip it.
type Scenario struct {
	ID      string
	Title   string
	Profile Profile
	// DependsOn names a scenario that must have passed first.
	DependsOn string
	Needs     func(s *session) string
	Run       func(ctx context.Context, s *session) error
}

// List returns the scenarios' ids, titles and profiles in run order.
func List() []Scenario { return append([]Scenario(nil), scenarios...) }

// Run executes the selected scenarios and always cleans up the Machines it
// created (unless Env.Keep). It returns one Result per selected scenario.
func Run(ctx context.Context, env Env, rep Reporter, opt Options) []Result {
	env.defaults()
	if opt.Profile == "" {
		opt.Profile = ProfileStandard
	}
	s := newSession(&env, rep)
	defer s.cleanup()

	only := toSet(opt.Only)
	skip := toSet(opt.Skip)
	var results []Result
	passed := map[string]bool{}
	for _, sc := range scenarios {
		if len(only) > 0 && !only[sc.ID] {
			continue
		}
		if skip[sc.ID] || sc.Profile.rank() > opt.Profile.rank() {
			continue
		}
		rep.StepStart(sc.ID, sc.Title)
		started := env.Now()
		res := Result{ID: sc.ID, Title: sc.Title}
		switch {
		case ctx.Err() != nil:
			res.Status, res.Reason = Skip, "interrupted"
		case env.NoVM && sc.ID != "control-plane":
			res.Status, res.Reason = Skip, "not run (--no-vm)"
		case sc.DependsOn != "" && !passed[sc.DependsOn]:
			res.Status, res.Reason = Skip, sc.DependsOn+" did not pass"
		default:
			if reason := needs(sc, s); reason != "" {
				res.Status, res.Reason = Skip, reason
				break
			}
			if err := sc.Run(ctx, s); err != nil {
				res.Status, res.Reason = Fail, err.Error()
				res.Hint = s.hint
			} else {
				res.Status = Pass
				if s.skipReason != "" {
					res.Status, res.Reason = Skip, s.skipReason
				}
			}
		}
		s.hint, s.skipReason = "", ""
		res.Duration = env.Now().Sub(started)
		passed[sc.ID] = res.Status == Pass
		results = append(results, res)
		rep.StepDone(res)
	}
	return results
}

func needs(sc Scenario, s *session) string {
	if sc.Needs == nil {
		return ""
	}
	return sc.Needs(s)
}

func toSet(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			m[id] = true
		}
	}
	return m
}

// Summary counts results by status.
type Summary struct{ Pass, Fail, Skip int }

// Summarize tallies results.
func Summarize(rs []Result) Summary {
	var s Summary
	for _, r := range rs {
		switch r.Status {
		case Pass:
			s.Pass++
		case Fail:
			s.Fail++
		default:
			s.Skip++
		}
	}
	return s
}

// ExitCode is 1 when anything failed (or, with strict, was skipped).
func ExitCode(rs []Result, strict bool) int {
	s := Summarize(rs)
	if s.Fail > 0 || (strict && s.Skip > 0) {
		return 1
	}
	return 0
}

// IDs returns the scenario ids, sorted, for validation of --only/--skip.
func IDs() []string {
	out := make([]string, 0, len(scenarios))
	for _, sc := range scenarios {
		out = append(out, sc.ID)
	}
	sort.Strings(out)
	return out
}

// UnknownIDs returns any of ids that is not a scenario.
func UnknownIDs(ids []string) []string {
	known := toSet(IDs())
	var bad []string
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !known[id] {
			bad = append(bad, id)
		}
	}
	return bad
}
