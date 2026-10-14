// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func sessionPath(ns, name, suffix string) string {
	p := "/api/v1/agentsessions/" + url.PathEscape(ns) + "/" + url.PathEscape(name)
	if suffix != "" {
		p += "/" + suffix
	}
	return p
}

type sessionRunFlags struct {
	agent, git, mode, path, image, cpu, memory, node string
	preset, networkDefault                           string
	allowHosts, processAllow, processDeny            []string
	processAsk                                       []string
	fsRead, fsWrite                                  []string
	ttl                                              time.Duration
	budget                                           model.AgentBudget
}

// agentSessionSpec turns run flags into a spec and validates it the way
// the admission webhook will.
func agentSessionSpec(f sessionRunFlags) (model.AgentSessionSpec, error) {
	spec := model.AgentSessionSpec{
		Agent:      f.agent,
		Image:      f.image,
		Resources:  model.AgentSessionResources{CPU: f.cpu, Memory: f.memory},
		TTLSeconds: int64(f.ttl / time.Second),
		NodeName:   f.node,
	}
	if f.git != "" || f.path != "" || f.mode != "" {
		spec.Workspace = &model.AgentWorkspace{Git: f.git, Path: f.path, Mode: f.mode}
	}
	if f.preset != "" || f.networkDefault != "" || len(f.allowHosts) > 0 || len(f.processAllow) > 0 || len(f.processDeny) > 0 || len(f.processAsk) > 0 || len(f.fsRead) > 0 || len(f.fsWrite) > 0 {
		p := &model.AgentPolicy{Preset: f.preset}
		if f.networkDefault != "" || len(f.allowHosts) > 0 {
			p.Network = &model.AgentNetworkPolicy{Default: f.networkDefault}
			for _, h := range f.allowHosts {
				p.Network.Allow = append(p.Network.Allow, model.AgentNetworkAllow{Host: h})
			}
		}
		if len(f.processAllow) > 0 || len(f.processDeny) > 0 || len(f.processAsk) > 0 {
			p.Process = &model.AgentProcessPolicy{Allow: f.processAllow, Deny: f.processDeny, Ask: f.processAsk}
		}
		if len(f.fsRead) > 0 || len(f.fsWrite) > 0 {
			p.Filesystem = &model.AgentFilesystemPolicy{Read: f.fsRead, Write: f.fsWrite}
		}
		spec.Policy = p
	}
	if f.budget != (model.AgentBudget{}) {
		b := f.budget
		spec.Budget = &b
	}
	if err := model.ValidateAgentSessionSpec(spec); err != nil {
		return spec, fmt.Errorf("spec.%w", err)
	}
	return spec, nil
}

func newSessionCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions", "asess"},
		Short:   "Run FluxVM agent sessions (AgentSession) and their verbs",
		Long: `An AgentSession is a FluxVM agent session: its own sandbox VM with a workspace,
a compiled policy, checkpoints, an event ledger, background jobs, files, git and
preview URLs. kairon-controller places it on a node that serves sessions (today
a Mac running FluxVM 0.4.8 or later) and counts it against MachineQuota.

run, list, get and rm talk to the cluster. Every other verb goes through
kairon-ui (KAIRON_UI_URL, KAIRON_UI_TOKEN) and needs an admin account, or any
named operator when ui.auth.sandboxOperatorExec is on.`,
		Example: `  $ kaironctl session run demo --agent codex --git https://github.com/org/repo --ttl 2h
  $ kaironctl session exec demo -- make test
  $ kaironctl session checkpoint demo --tag green
  $ kaironctl session rollout demo --count 4 -- ./try-fix.sh
  $ kaironctl session export demo --otlp http://collector:4318`,
	}
	sp := func(suffix string) func([]string) (string, error) {
		return func(args []string) (string, error) { return sessionPath(opts.Namespace, args[0], suffix), nil }
	}
	cmd.AddCommand(
		newSessionRunCmd(opts),
		&cobra.Command{
			Use:   "list",
			Short: "List AgentSessions in the namespace",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				kc, err := newKubeClient()
				if err != nil {
					return err
				}
				items, err := kc.ListAgentSessionsNamespace(cmd.Context(), opts.Namespace)
				if err != nil {
					return err
				}
				printAgentSessions(items)
				return nil
			},
		},
		&cobra.Command{
			Use:   "get NAME",
			Short: "Print an AgentSession as JSON",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				kc, err := newKubeClient()
				if err != nil {
					return err
				}
				s, err := kc.GetAgentSession(cmd.Context(), opts.Namespace, args[0])
				if err != nil {
					return err
				}
				fmt.Println(string(jsonIndent(s)))
				return nil
			},
		},
		&cobra.Command{
			Use:     "rm NAME...",
			Aliases: []string{"delete"},
			Short:   "Delete AgentSessions (and their FluxVM sessions)",
			Args:    cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				kc, err := newKubeClient()
				if err != nil {
					return err
				}
				for _, name := range args {
					if err := kc.DeleteAgentSession(cmd.Context(), opts.Namespace, name); err != nil {
						return err
					}
					okf("agentsession/%s deleted", name)
				}
				return nil
			},
		},
		newSessionExecCmd(opts),
		newSessionCheckpointCmd(opts),
		&cobra.Command{
			Use:   "restore NAME TAG",
			Short: "Roll the session back to a checkpoint",
			Args:  cobra.ExactArgs(2),
			RunE: uiRun(http.MethodPost, sp("restore"), func(args []string) (any, error) {
				return map[string]any{"tag": args[1]}, nil
			}),
		},
		newSessionForkCmd(opts),
		newSessionPromoteCmd(opts),
		newSessionRolloutCmd(opts),
		newSessionBgCmd(opts),
		&cobra.Command{
			Use:   "jobs NAME",
			Short: "List the session's background jobs",
			Args:  cobra.ExactArgs(1),
			RunE:  uiRun(http.MethodGet, sp("jobs"), nil),
		},
		newSessionLogsCmd(opts),
		newSessionKillCmd(opts),
		newSessionLsCmd(opts),
		newSessionCatCmd(opts),
		newSessionPutCmd(opts),
		newSessionRmFileCmd(opts),
		newSessionGitCmd(opts),
		newSessionExposeCmd(opts),
		&cobra.Command{
			Use:   "previews NAME",
			Short: "List the session's live preview URLs",
			Args:  cobra.ExactArgs(1),
			RunE:  uiRun(http.MethodGet, sp("previews"), nil),
		},
		&cobra.Command{
			Use:   "unexpose NAME TOKEN",
			Short: "Revoke a preview URL",
			Args:  cobra.ExactArgs(2),
			RunE: uiRun(http.MethodDelete, func(args []string) (string, error) {
				return sessionPath(opts.Namespace, args[0], "preview/"+url.PathEscape(args[1])), nil
			}, nil),
		},
		newSessionTimelineCmd(opts),
		&cobra.Command{
			Use:   "artifacts NAME",
			Short: "List what the session produced (checkpoints, previews, workspace changes)",
			Args:  cobra.ExactArgs(1),
			RunE:  uiRun(http.MethodGet, sp("artifacts"), nil),
		},
		newSessionExportCmd(opts),
		newSessionShellCmd(opts),
		newSessionProbeCmd(opts),
		newSessionApprovalsCmd(opts),
		newSessionDecideCmd(opts, "approve"),
		newSessionDecideCmd(opts, "deny"),
		newSessionGrantCmd(opts),
		&cobra.Command{
			Use:   "grants NAME",
			Short: "List the session's credential grants, without their values (admin only)",
			Args:  cobra.ExactArgs(1),
			RunE:  uiRun(http.MethodGet, sp("grants"), nil),
		},
		newSessionRevokeGrantCmd(opts),
	)
	return cmd
}

func printAgentSessions(items []model.AgentSession) {
	sort.Slice(items, func(i, j int) bool { return items[i].Metadata.Name < items[j].Metadata.Name })
	fmt.Printf("NAME\tPHASE\tNODE\tAGENT\tBACKEND\tEXPIRES\tSESSION\n")
	for _, s := range items {
		expires := "-"
		if s.Status.ExpiresAt != nil {
			expires = time.Until(*s.Status.ExpiresAt).Round(time.Second).String()
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Metadata.Name, style.Phase(os.Stdout, dash(s.Status.Phase)),
			dash(s.Spec.NodeName), dash(s.Spec.Agent), dash(s.Status.Backend), expires, dash(s.Status.SessionID))
	}
}

func newSessionRunCmd(opts *Options) *cobra.Command {
	var f sessionRunFlags
	var wait time.Duration
	var noWait bool
	c := &cobra.Command{
		Use:   "run NAME",
		Short: "Create an AgentSession and wait for it to run",
		Args:  cobra.ExactArgs(1),
		Example: `  $ kaironctl session run fix-123 --agent claude --git https://github.com/org/repo \
      --preset developer-safe --allow-host api.github.com --ttl 4h --max-execs 500`,
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := agentSessionSpec(f)
			if err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			s := model.AgentSession{
				TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindAgentSession},
				Metadata: model.ObjectMeta{Name: args[0], Namespace: opts.Namespace},
				Spec:     spec,
			}
			if _, err := kc.CreateAgentSession(cmd.Context(), opts.Namespace, s); err != nil {
				return err
			}
			if noWait {
				okf("agentsession/%s created", args[0])
				return nil
			}
			got, err := waitForAgentSession(cmd.Context(), kc, opts.Namespace, args[0], wait)
			if err != nil {
				return err
			}
			okf("agentsession/%s running on %s (session %s)", args[0], got.Status.NodeName, got.Status.SessionID)
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&f.agent, "agent", "", "agent label (claude, codex, ...; FluxVM default custom)")
	fl.StringVar(&f.git, "git", "", "https:// repository cloned into the workspace")
	fl.StringVar(&f.path, "workspace", "", "guest workspace directory (default /workspace)")
	fl.StringVar(&f.mode, "mode", "", "workspace mode: clone or readonly")
	fl.StringVar(&f.image, "image", "", "FluxVM image for the session VM")
	fl.StringVar(&f.cpu, "cpu", "", "vCPUs (default 2)")
	fl.StringVar(&f.memory, "memory", "", "memory, e.g. 4Gi (default 2Gi)")
	fl.StringVar(&f.node, "node", "", "pin to this node instead of letting the controller choose")
	fl.StringVar(&f.preset, "preset", "", "policy preset: offline, developer-safe or open")
	fl.StringVar(&f.networkDefault, "network-default", "", "network default: deny or allow")
	fl.StringArrayVar(&f.allowHosts, "allow-host", nil, "host the session may reach (repeatable; *.example.com allowed)")
	fl.StringArrayVar(&f.processAllow, "process-allow", nil, "command glob allowed (repeatable)")
	fl.StringArrayVar(&f.processDeny, "process-deny", nil, "command glob refused (repeatable)")
	fl.StringArrayVar(&f.processAsk, "process-ask", nil, "command glob that waits for an admin's approval (repeatable)")
	fl.StringArrayVar(&f.fsRead, "fs-read", nil, "guest path exec may read (repeatable; dir/** for a tree)")
	fl.StringArrayVar(&f.fsWrite, "fs-write", nil, "guest path exec may write (repeatable)")
	fl.DurationVar(&f.ttl, "ttl", 0, "end the session after this long")
	fl.Int64Var(&f.budget.MaxExecs, "max-execs", 0, "budget: exec calls")
	fl.Int64Var(&f.budget.MaxExecSeconds, "max-exec-seconds", 0, "budget: total exec seconds")
	fl.Int64Var(&f.budget.MaxJobs, "max-jobs", 0, "budget: background jobs")
	fl.Int64Var(&f.budget.MaxConcurrentJobs, "max-concurrent-jobs", 0, "budget: jobs at once")
	fl.Int64Var(&f.budget.MaxChildren, "max-children", 0, "budget: fork children")
	fl.Int64Var(&f.budget.MaxComputeSeconds, "max-compute-seconds", 0, "budget: total compute seconds")
	fl.Int64Var(&f.budget.MaxOutputBytes, "max-output-bytes", 0, "budget: bytes of output per call")
	fl.Int64Var(&f.budget.MaxWorkspaceBytes, "max-workspace-bytes", 0, "budget: workspace size")
	fl.DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for Running")
	fl.BoolVar(&noWait, "no-wait", false, "create and return immediately")
	return c
}

func waitForAgentSession(ctx context.Context, kc *kube.Client, ns, name string, timeout time.Duration) (model.AgentSession, error) {
	deadline := time.Now().Add(timeout)
	for {
		s, err := kc.GetAgentSession(ctx, ns, name)
		if err == nil {
			switch s.Status.Phase {
			case model.AgentSessionRunning:
				return s, nil
			case model.AgentSessionFailed, model.AgentSessionEnded:
				return s, fmt.Errorf("agentsession/%s is %s: %s", name, s.Status.Phase, s.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			msg := s.Status.Message
			if msg == "" {
				msg = "phase " + dash(s.Status.Phase)
			}
			return s, fmt.Errorf("timed out waiting for agentsession/%s: %s", name, msg)
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

type sessionExecResult struct {
	ExitCode *int64 `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func newSessionExecCmd(opts *Options) *cobra.Command {
	var timeout uint64
	var approval approvalFlags
	c := &cobra.Command{
		Use:   "exec NAME -- COMMAND [ARGS...]",
		Short: "Run a command in the session and exit with its code",
		Long: `The command line runs through the guest's shell under the session policy, in
the workspace. Exits with the command's exit code.

A command matching a policy.process.ask rule waits for an admin: the first try
prints the approval id; repeat with --approval-id once it is approved, or pass
--approval-wait to hold the request open for the decision.`,
		Example: `  $ kaironctl session exec demo -- 'cd /workspace && go test ./...'
  $ kaironctl session exec demo --approval-wait 2m -- git push origin fix`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := sessionExec(cmd.Context(), opts.Namespace, args[0], strings.Join(args[1:], " "), timeout, approval)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, res.Stdout)
			fmt.Fprint(os.Stderr, res.Stderr)
			if res.ExitCode != nil && *res.ExitCode != 0 {
				os.Exit(int(min(max(*res.ExitCode, 1), 255)))
			}
			return nil
		},
	}
	c.Flags().Uint64Var(&timeout, "timeout", 0, "seconds before the command is killed")
	approval.register(c)
	return c
}

func sessionExec(ctx context.Context, ns, name, command string, timeout uint64, approval approvalFlags) (sessionExecResult, error) {
	body := map[string]any{"command": command}
	if timeout > 0 {
		body["timeout_seconds"] = timeout
	}
	var res sessionExecResult
	if err := approval.apply(body); err != nil {
		return res, err
	}
	raw, err := sessionSend(ctx, ns, name, http.MethodPost, "exec", body)
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(raw, &res)
}

func newSessionCheckpointCmd(opts *Options) *cobra.Command {
	var tag string
	var diskOnly bool
	c := &cobra.Command{
		Use:   "checkpoint NAME",
		Short: "Snapshot the session (memory and disk by default)",
		Args:  cobra.ExactArgs(1),
		RunE: uiRun(http.MethodPost, func(args []string) (string, error) {
			return sessionPath(opts.Namespace, args[0], "checkpoint"), nil
		}, func([]string) (any, error) {
			body := map[string]any{}
			if tag != "" {
				body["tag"] = tag
			}
			if diskOnly {
				body["memory"] = false
			}
			return body, nil
		}),
	}
	c.Flags().StringVar(&tag, "tag", "", "checkpoint name (default generated)")
	c.Flags().BoolVar(&diskOnly, "disk-only", false, "skip memory: smaller, but restore cold-boots")
	return c
}

type forkAnswer struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
	TotalMs       any      `json:"total_ms"`
	AgentSessions []string `json:"agentSessions"`
	Errors        []string `json:"agentSessionErrors"`
}

func sessionFork(ctx context.Context, ns, name string, count int, identity string) (forkAnswer, json.RawMessage, error) {
	body := map[string]any{"count": count}
	if identity != "" {
		body["identity"] = identity
	}
	var out forkAnswer
	raw, err := uiRaw(ctx, http.MethodPost, sessionPath(ns, name, "fork"), body)
	if err != nil {
		return out, raw, err
	}
	return out, raw, json.Unmarshal(raw, &out)
}

func newSessionForkCmd(opts *Options) *cobra.Command {
	var count int
	var identity string
	c := &cobra.Command{
		Use:   "fork NAME",
		Short: "Fork live copies of the session; each child is its own AgentSession",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, raw, err := sessionFork(cmd.Context(), opts.Namespace, args[0], count, identity)
			if err != nil {
				return err
			}
			return printRaw(raw)
		},
	}
	c.Flags().IntVar(&count, "count", 1, "number of children")
	c.Flags().StringVar(&identity, "identity", "", "best-effort (default), unique or preserve")
	return c
}

func newSessionPromoteCmd(opts *Options) *cobra.Command {
	var dryRun, skipConflicts bool
	var planID string
	var paths []string
	c := &cobra.Command{
		Use:   "promote CHILD",
		Short: "Apply a fork child's workspace changes back onto its parent",
		Long: `Run with --dry-run first to see the plan, then apply it with --plan-id so the
apply is refused if the plan changed in between.`,
		Args: cobra.ExactArgs(1),
		RunE: uiRun(http.MethodPost, func(args []string) (string, error) {
			return sessionPath(opts.Namespace, args[0], "promote"), nil
		}, func([]string) (any, error) {
			body := map[string]any{"dry_run": dryRun, "skip_conflicts": skipConflicts}
			if planID != "" {
				body["plan_id"] = planID
			}
			if len(paths) > 0 {
				body["paths"] = paths
			}
			return body, nil
		}),
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "compute and print the plan; change nothing")
	c.Flags().StringVar(&planID, "plan-id", "", "the plan_id of a reviewed plan")
	c.Flags().BoolVar(&skipConflicts, "skip-conflicts", false, "apply clean paths and leave conflicting ones")
	c.Flags().StringArrayVar(&paths, "path", nil, "only this file or directory (repeatable)")
	return c
}

func newSessionRolloutCmd(opts *Options) *cobra.Command {
	var count int
	var keep bool
	var timeout uint64
	var identity string
	var wait time.Duration
	c := &cobra.Command{
		Use:   "rollout NAME --count N -- COMMAND",
		Short: "Fork N children, run COMMAND in each in parallel, then delete them",
		Long: `The children start from the parent's live state, so N attempts at a task share
one setup. Prints one result per child; --keep leaves the children for
inspection or promote.`,
		Example: `  $ kaironctl session rollout demo --count 4 -- 'git apply /tmp/p.diff && make test'`,
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			out, err := sessionRollout(cmd.Context(), kc, opts.Namespace, args[0], count, strings.Join(args[1:], " "), timeout, keep, identity, wait)
			if err != nil {
				return err
			}
			fmt.Println(string(jsonIndent(out)))
			return nil
		},
	}
	c.Flags().IntVar(&count, "count", 2, "number of children")
	c.Flags().BoolVar(&keep, "keep", false, "keep the children afterwards")
	c.Flags().Uint64Var(&timeout, "timeout", 0, "seconds before each command is killed")
	c.Flags().StringVar(&identity, "identity", "", "fork identity: best-effort, unique or preserve")
	c.Flags().DurationVar(&wait, "wait", 5*time.Minute, "how long to wait for the children to run")
	return c
}

func sessionRollout(ctx context.Context, kc *kube.Client, ns, parent string, count int, command string, timeout uint64, keep bool, identity string, wait time.Duration) (map[string]any, error) {
	forked, _, err := sessionFork(ctx, ns, parent, count, identity)
	if err != nil {
		return nil, fmt.Errorf("fork: %w", err)
	}
	if len(forked.Errors) > 0 {
		fmt.Fprintln(os.Stderr, "warning: some children have no AgentSession:", strings.Join(forked.Errors, "; "))
	}
	started := time.Now()
	results := make([]map[string]any, len(forked.AgentSessions))
	var wg sync.WaitGroup
	for i, child := range forked.AgentSessions {
		wg.Add(1)
		go func(i int, child string) {
			defer wg.Done()
			t0 := time.Now()
			if _, err := waitForAgentSession(ctx, kc, ns, child, wait); err != nil {
				results[i] = map[string]any{"session": child, "error": err.Error()}
				return
			}
			res, err := sessionExec(ctx, ns, child, command, timeout, approvalFlags{})
			if err != nil {
				results[i] = map[string]any{"session": child, "error": err.Error()}
				return
			}
			results[i] = map[string]any{"session": child, "exit_code": res.ExitCode, "stdout": res.Stdout, "stderr": res.Stderr,
				"duration_ms": time.Since(t0).Milliseconds()}
		}(i, child)
	}
	wg.Wait()
	execMs := time.Since(started).Milliseconds()
	deleted := 0
	if !keep {
		for _, child := range forked.AgentSessions {
			if kc.DeleteAgentSession(ctx, ns, child) == nil {
				deleted++
			}
		}
	}
	return map[string]any{
		"parent":           parent,
		"fork_ms":          forked.TotalMs,
		"exec_wall_ms":     execMs,
		"results":          results,
		"children_kept":    keep,
		"children_deleted": deleted,
	}, nil
}

func newSessionBgCmd(opts *Options) *cobra.Command {
	var timeout, maxOutput uint64
	var stdin bool
	var approval approvalFlags
	c := &cobra.Command{
		Use:   "bg NAME -- COMMAND",
		Short: "Start a background job; prints its id",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"command": strings.Join(args[1:], " "), "stdin": stdin}
			if timeout > 0 {
				body["timeout_seconds"] = timeout
			}
			if maxOutput > 0 {
				body["max_output_bytes"] = maxOutput
			}
			if err := approval.apply(body); err != nil {
				return err
			}
			raw, err := sessionSend(cmd.Context(), opts.Namespace, args[0], http.MethodPost, "jobs", body)
			if err != nil {
				return err
			}
			return printRaw(raw)
		},
	}
	c.Flags().Uint64Var(&timeout, "timeout", 0, "seconds before the job is killed")
	c.Flags().Uint64Var(&maxOutput, "max-output-bytes", 0, "stop recording output after this many bytes")
	c.Flags().BoolVar(&stdin, "stdin", false, "give the job a writable stdin")
	approval.register(c)
	return c
}

type jobChunk struct {
	State            string `json:"state"`
	ExitCode         *int64 `json:"exit_code"`
	Output           string `json:"output"`
	NextOffset       uint64 `json:"next_offset"`
	Stderr           string `json:"stderr"`
	StderrNextOffset uint64 `json:"stderr_next_offset"`
	More             bool   `json:"more"`
}

func newSessionLogsCmd(opts *Options) *cobra.Command {
	var follow bool
	c := &cobra.Command{
		Use:   "logs NAME JOB",
		Short: "Print a background job's output (-f to follow until it exits)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var off, errOff uint64
			for {
				q := url.Values{"offset": {strconv.FormatUint(off, 10)}, "stderr_offset": {strconv.FormatUint(errOff, 10)}}
				if follow {
					q.Set("wait_ms", "20000")
				}
				raw, err := uiRaw(cmd.Context(), http.MethodGet, sessionPath(opts.Namespace, args[0], "jobs/"+url.PathEscape(args[1]))+"?"+q.Encode(), nil)
				if err != nil {
					return err
				}
				var ch jobChunk
				if err := json.Unmarshal(raw, &ch); err != nil {
					return err
				}
				fmt.Fprint(os.Stdout, ch.Output)
				fmt.Fprint(os.Stderr, ch.Stderr)
				if ch.NextOffset > off {
					off = ch.NextOffset
				}
				if ch.StderrNextOffset > errOff {
					errOff = ch.StderrNextOffset
				}
				if ch.More {
					continue
				}
				if !follow || ch.State != "running" {
					if follow && ch.ExitCode != nil && *ch.ExitCode != 0 {
						os.Exit(int(min(max(*ch.ExitCode, 1), 255)))
					}
					return nil
				}
			}
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep reading until the job exits")
	return c
}

func newSessionKillCmd(opts *Options) *cobra.Command {
	var signal string
	c := &cobra.Command{
		Use:   "kill NAME JOB",
		Short: "Stop a background job",
		Args:  cobra.ExactArgs(2),
		RunE: uiRun(http.MethodDelete, func(args []string) (string, error) {
			p := sessionPath(opts.Namespace, args[0], "jobs/"+url.PathEscape(args[1]))
			if signal != "" {
				p += "?signal=" + url.QueryEscape(signal)
			}
			return p, nil
		}, nil),
	}
	c.Flags().StringVar(&signal, "signal", "", "signal to send (default TERM)")
	return c
}

func filesQuery(ns, name, suffix, guestPath string, extra url.Values) string {
	q := url.Values{"path": {guestPath}}
	for k, v := range extra {
		q[k] = v
	}
	return sessionPath(ns, name, suffix) + "?" + q.Encode()
}

func newSessionLsCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "ls NAME [PATH]",
		Short: "List a guest directory (default the workspace)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: uiRun(http.MethodGet, func(args []string) (string, error) {
			p := "/workspace"
			if len(args) > 1 {
				p = args[1]
			}
			return filesQuery(opts.Namespace, args[0], "files", p, nil), nil
		}, nil),
	}
}

func newSessionCatCmd(opts *Options) *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:     "cat NAME PATH",
		Aliases: []string{"pull"},
		Short:   "Print a guest file (or save it with --out)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := uiRaw(cmd.Context(), http.MethodGet, filesQuery(opts.Namespace, args[0], "files/content", args[1], nil), nil)
			if err != nil {
				return err
			}
			var f struct {
				ContentBase64 string `json:"content_base64"`
				Mode          uint32 `json:"mode"`
			}
			if err := json.Unmarshal(raw, &f); err != nil {
				return err
			}
			data, err := base64.StdEncoding.DecodeString(f.ContentBase64)
			if err != nil {
				return fmt.Errorf("decode file content: %w", err)
			}
			if out == "" {
				_, err = os.Stdout.Write(data)
				return err
			}
			perm := os.FileMode(f.Mode & 0o777)
			if perm == 0 {
				perm = 0o644
			}
			return os.WriteFile(out, data, perm)
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "", "write to this local file")
	return c
}

func newSessionPutCmd(opts *Options) *cobra.Command {
	var mode string
	var noParents bool
	c := &cobra.Command{
		Use:   "put NAME LOCAL GUEST-PATH",
		Short: "Write a local file into the guest",
		Long: `Files up to 1 MiB are written in one request. Larger ones (up to 2 GiB) go
in 4 MiB chunks through a resumable upload, and the guest checks the SHA-256
before the file appears at GUEST-PATH.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			var perm *uint64
			if mode != "" {
				m, err := strconv.ParseUint(mode, 8, 32)
				if err != nil {
					return fmt.Errorf("--mode %q: octal permission bits like 0644", mode)
				}
				perm = &m
			}
			st, err := os.Stat(args[1])
			if err != nil {
				return err
			}
			if st.Size() > inlinePutLimit {
				raw, err := sessionUpload(cmd.Context(), opts.Namespace, args[0], args[1], args[2], perm, !noParents, os.Stderr)
				if err != nil {
					return err
				}
				return printRaw(raw)
			}
			data, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			body := map[string]any{"path": args[2], "content_base64": base64.StdEncoding.EncodeToString(data), "parents": !noParents}
			if perm != nil {
				body["mode"] = *perm
			}
			raw, err := uiRaw(cmd.Context(), http.MethodPut, sessionPath(opts.Namespace, args[0], "files/content"), body)
			if err != nil {
				return err
			}
			return printRaw(raw)
		},
	}
	c.Flags().StringVar(&mode, "mode", "", "octal permission bits, e.g. 0755")
	c.Flags().BoolVar(&noParents, "no-parents", false, "fail if the parent directory is missing")
	return c
}

func newSessionRmFileCmd(opts *Options) *cobra.Command {
	var recursive bool
	c := &cobra.Command{
		Use:   "rm-file NAME PATH",
		Short: "Delete a guest file (-r for a directory)",
		Args:  cobra.ExactArgs(2),
		RunE: uiRun(http.MethodDelete, func(args []string) (string, error) {
			return filesQuery(opts.Namespace, args[0], "files", args[1], url.Values{"recursive": {strconv.FormatBool(recursive)}}), nil
		}, nil),
	}
	c.Flags().BoolVarP(&recursive, "recursive", "r", false, "delete a directory tree")
	return c
}

func newSessionGitCmd(opts *Options) *cobra.Command {
	var repo, path string
	var staged bool
	var limit int
	c := &cobra.Command{
		Use:   "git NAME status|diff|log|commit MESSAGE|branch NAME",
		Short: "Read or change the workspace git repository",
		Example: `  $ kaironctl session git demo diff --staged
  $ kaironctl session git demo commit "fix: handle empty input"
  $ kaironctl session git demo branch agent/fix-123`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, verb := args[0], args[1]
			q := url.Values{}
			if repo != "" {
				q.Set("repo", repo)
			}
			var method string
			var body map[string]any
			switch verb {
			case "status", "diff", "log":
				method = http.MethodGet
				if staged {
					q.Set("staged", "true")
				}
				if path != "" {
					q.Set("path", path)
				}
				if limit > 0 {
					q.Set("limit", strconv.Itoa(limit))
				}
			case "commit", "branch":
				if len(args) != 3 {
					return fmt.Errorf("git %s needs a %s", verb, map[string]string{"commit": "message", "branch": "branch name"}[verb])
				}
				method = http.MethodPost
				body = map[string]any{}
				if verb == "commit" {
					body["message"] = args[2]
				} else {
					body["name"] = args[2]
				}
				if repo != "" {
					body["repo"] = repo
				}
				q = url.Values{}
			default:
				return fmt.Errorf("unknown git verb %q (status, diff, log, commit, branch)", verb)
			}
			p := sessionPath(opts.Namespace, name, "git/"+verb)
			if len(q) > 0 {
				p += "?" + q.Encode()
			}
			var b any
			if body != nil {
				b = body
			}
			raw, err := uiRaw(cmd.Context(), method, p, b)
			if err != nil {
				return err
			}
			var diff struct {
				Diff string `json:"diff"`
			}
			if verb == "diff" && json.Unmarshal(raw, &diff) == nil && diff.Diff != "" {
				fmt.Print(diff.Diff)
				return nil
			}
			return printRaw(raw)
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "repository directory (default the workspace)")
	c.Flags().StringVar(&path, "path", "", "limit status/diff/log to this path")
	c.Flags().BoolVar(&staged, "staged", false, "diff the index instead of the worktree")
	c.Flags().IntVar(&limit, "limit", 0, "log: number of commits")
	return c
}

func newSessionExposeCmd(opts *Options) *cobra.Command {
	var ttl time.Duration
	c := &cobra.Command{
		Use:   "expose NAME PORT",
		Short: "Mint an expiring preview URL for a guest port",
		Long: `kairon-ui serves the URL at /preview/<namespace>/<name>/<token>/ when
ui.agentSessions.previews is on. The token is the credential: anyone holding the
URL reaches the port until it expires or is revoked with unexpose.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[1], 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("port %q: must be 1-65535", args[1])
			}
			body := map[string]any{"port": port}
			if ttl > 0 {
				body["ttl_seconds"] = int64(ttl / time.Second)
			}
			raw, err := uiRaw(cmd.Context(), http.MethodPost, sessionPath(opts.Namespace, args[0], "preview"), body)
			if err != nil {
				return err
			}
			var p struct {
				URL    string `json:"url"`
				Served bool   `json:"served"`
			}
			if json.Unmarshal(raw, &p) == nil && p.URL != "" {
				fmt.Println(uiBaseURL() + p.URL)
				if !p.Served {
					fmt.Fprintln(os.Stderr, "note: kairon-ui does not serve previews (ui.agentSessions.previews is off)")
				}
				return nil
			}
			return printRaw(raw)
		},
	}
	c.Flags().DurationVar(&ttl, "ttl", 0, "how long the URL works (default 1h, at most 24h)")
	return c
}

func newSessionTimelineCmd(opts *Options) *cobra.Command {
	var since string
	var trajectory bool
	c := &cobra.Command{
		Use:   "timeline NAME",
		Short: "Print the session's event ledger (--trajectory merges VM events)",
		Args:  cobra.ExactArgs(1),
		RunE: uiRun(http.MethodGet, func(args []string) (string, error) {
			if trajectory {
				return sessionPath(opts.Namespace, args[0], "trajectory"), nil
			}
			p := sessionPath(opts.Namespace, args[0], "events")
			if since != "" {
				p += "?since=" + url.QueryEscape(since)
			}
			return p, nil
		}, nil),
	}
	c.Flags().StringVar(&since, "since", "", "only events after this RFC 3339 time")
	c.Flags().BoolVar(&trajectory, "trajectory", false, "numbered steps from the ledger and the VM")
	return c
}

func newSessionExportCmd(opts *Options) *cobra.Command {
	var out, otlp string
	var headers []string
	c := &cobra.Command{
		Use:   "export NAME",
		Short: "Export the session trajectory as JSONL, or send it as an OTLP trace",
		Long: `JSONL: a first line {"type":"session",...} then one step per line. With --otlp
the trajectory becomes one trace (trace id = the FluxVM session id, one span per
step) POSTed to the collector's /v1/traces as OTLP/JSON.`,
		Example: `  $ kaironctl session export demo --out demo.jsonl
  $ kaironctl session export demo --otlp http://otel-collector:4318 --otlp-header x-tenant=team-a`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := uiRaw(cmd.Context(), http.MethodGet, sessionPath(opts.Namespace, args[0], "trajectory"), nil)
			if err != nil {
				return err
			}
			if otlp != "" {
				doc, err := otlpTraces(raw)
				if err != nil {
					return err
				}
				if err := sendOTLP(cmd.Context(), otlp, headers, doc); err != nil {
					return err
				}
				okf("trace for agentsession/%s sent", args[0])
				return nil
			}
			w := io.Writer(os.Stdout)
			if out != "" {
				f, err := os.Create(filepath.Clean(out))
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				w = f
			}
			return trajectoryJSONL(raw, w)
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "", "write JSONL to this file")
	c.Flags().StringVar(&otlp, "otlp", "", "OTLP/HTTP collector endpoint")
	c.Flags().StringArrayVar(&headers, "otlp-header", nil, "KEY=VALUE header for the collector (repeatable)")
	return c
}

func trajectoryJSONL(raw []byte, w io.Writer) error {
	var traj struct {
		Session json.RawMessage   `json:"session"`
		Steps   []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(raw, &traj); err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	head, _ := json.Marshal(map[string]any{"type": "session", "session": traj.Session})
	_, _ = bw.Write(append(head, '\n'))
	for _, st := range traj.Steps {
		var buf bytes.Buffer
		if err := json.Compact(&buf, st); err != nil {
			return err
		}
		buf.WriteByte('\n')
		_, _ = bw.Write(buf.Bytes())
	}
	return bw.Flush()
}

// otlpSpanID is the first 8 bytes of SHA-256("<session>:<tag>"), never
// zero, matching fluxctl so the same session exports the same span ids.
func otlpSpanID(session, tag string) string {
	h := sha256.Sum256([]byte(session + ":" + tag))
	id := binary.BigEndian.Uint64(h[:8])
	if id == 0 {
		id = 1
	}
	return fmt.Sprintf("%016x", id)
}

func otlpAttr(key string, v any) (map[string]any, bool) {
	var value map[string]any
	switch x := v.(type) {
	case nil:
		return nil, false
	case string:
		value = map[string]any{"stringValue": x}
	case bool:
		value = map[string]any{"boolValue": x}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			value = map[string]any{"intValue": strconv.FormatInt(i, 10)}
		} else {
			f, _ := x.Float64()
			value = map[string]any{"doubleValue": f}
		}
	default:
		b, _ := json.Marshal(x)
		value = map[string]any{"stringValue": string(b)}
	}
	return map[string]any{"key": key, "value": value}, true
}

// otlpTraces turns a trajectory into one OTLP/JSON trace: a root span for
// the session and one child per step. An exec is stamped when it finishes,
// so its span ends at ts and starts duration_ms earlier.
func otlpTraces(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var traj struct {
		Session map[string]any   `json:"session"`
		Steps   []map[string]any `json:"steps"`
	}
	if err := dec.Decode(&traj); err != nil {
		return nil, err
	}
	session, _ := traj.Session["id"].(string)
	if session == "" {
		return nil, fmt.Errorf("trajectory has no session id")
	}
	traceID := strings.ReplaceAll(session, "-", "")
	if len(traceID) != 32 {
		return nil, fmt.Errorf("session id %q is not a UUID", session)
	}
	nanos := func(v any) (int64, error) {
		s, _ := v.(string)
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return 0, fmt.Errorf("bad timestamp %q", s)
		}
		return t.UnixNano(), nil
	}
	rootID := otlpSpanID(session, "root")
	spans := []map[string]any{}
	first, last := int64(1<<63-1), int64(0)
	for _, st := range traj.Steps {
		end, err := nanos(st["ts"])
		if err != nil {
			return nil, err
		}
		detail, _ := st["detail"].(map[string]any)
		var durMs int64
		if n, ok := detail["duration_ms"].(json.Number); ok {
			if d, err := n.Int64(); err == nil && d > 0 {
				durMs = d
			}
		}
		start := end - durMs*int64(time.Millisecond)
		first, last = min(first, start), max(last, end)
		source, _ := st["source"].(string)
		kind, _ := st["kind"].(string)
		source, kind = orDash(source), orDash(kind)
		var seq uint64
		seqTag := "null"
		if n, ok := st["seq"].(json.Number); ok {
			seqTag = n.String()
			seq, _ = strconv.ParseUint(n.String(), 10, 64)
		}
		attrs := []map[string]any{
			{"key": "fluxvm.source", "value": map[string]any{"stringValue": source}},
			{"key": "fluxvm.step", "value": map[string]any{"intValue": strconv.FormatUint(seq, 10)}},
		}
		keys := make([]string, 0, len(detail))
		for k := range detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if a, ok := otlpAttr("fluxvm."+k, detail[k]); ok {
				attrs = append(attrs, a)
			}
		}
		failed := strings.HasSuffix(kind, ".refused")
		if n, ok := detail["exit_code"].(json.Number); ok {
			if c, err := n.Int64(); err == nil && c != 0 {
				failed = true
			}
		}
		code := 0
		if failed {
			code = 2
		}
		spans = append(spans, map[string]any{
			"traceId":           traceID,
			"spanId":            otlpSpanID(session, seqTag),
			"parentSpanId":      rootID,
			"name":              kind,
			"kind":              1,
			"startTimeUnixNano": strconv.FormatInt(start, 10),
			"endTimeUnixNano":   strconv.FormatInt(end, 10),
			"attributes":        attrs,
			"status":            map[string]any{"code": code},
		})
	}
	if len(traj.Steps) == 0 {
		t, err := nanos(traj.Session["created_at"])
		if err != nil {
			return nil, err
		}
		first, last = t, t
	}
	rootAttrs := []map[string]any{}
	for _, k := range []string{"agent", "backend", "state", "sandbox_id"} {
		if a, ok := otlpAttr("fluxvm.session."+k, traj.Session[k]); ok {
			rootAttrs = append(rootAttrs, a)
		}
	}
	root := map[string]any{
		"traceId":           traceID,
		"spanId":            rootID,
		"name":              "agent.session",
		"kind":              1,
		"startTimeUnixNano": strconv.FormatInt(first, 10),
		"endTimeUnixNano":   strconv.FormatInt(last, 10),
		"attributes":        rootAttrs,
		"status":            map[string]any{"code": 0},
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{
			map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "fluxvm"}},
		}},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "fluxctl agent export"},
			"spans": append([]map[string]any{root}, spans...),
		}},
	}}}, nil
}

func sendOTLP(ctx context.Context, endpoint string, headers []string, doc any) error {
	u := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(u, "/v1/traces") {
		u += "/v1/traces"
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range headers {
		k, v, ok := strings.Cut(h, "=")
		if !ok || k == "" {
			return fmt.Errorf("--otlp-header %q must be KEY=VALUE", h)
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector answered %s for %s", resp.Status, u)
	}
	return nil
}
