// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	mcpSessionExecTimeout = 10 * time.Minute
	mcpFileTextLimit      = 256 << 10
)

type sessionRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r sessionRef) ns(def string) string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return nsOrDefault(def)
}

// sessionTools are the AgentSession tools: lifecycle through the cluster,
// verbs through kairon-ui (KAIRON_UI_URL), which applies its own gates.
func sessionTools(nsDefault string, withKube func(context.Context, time.Duration, func(context.Context, *kube.Client) (string, error)) (string, error)) []mcp.Tool {
	nsProp := mcp.String("Kubernetes namespace; defaults to " + nsOrDefault(nsDefault))
	stringList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	ref := func(extra map[string]any, required ...string) map[string]any {
		props := map[string]any{"namespace": nsProp, "name": mcp.String("AgentSession name")}
		for k, v := range extra {
			props[k] = v
		}
		return mcp.Object(props, append([]string{"name"}, required...)...)
	}
	// verb relays one session verb through kairon-ui and returns its JSON.
	// body decodes the full arguments strictly (embedding sessionRef); with
	// no body the arguments are just the reference.
	verb := func(timeout time.Duration, method, suffix string, body func(json.RawMessage) (any, url.Values, error)) func(context.Context, json.RawMessage) (string, error) {
		return func(ctx context.Context, raw json.RawMessage) (string, error) {
			var r sessionRef
			var b any
			var q url.Values
			if body == nil {
				if err := decodeArgs(raw, &r); err != nil {
					return "", err
				}
			} else {
				var err error
				if b, q, err = body(raw); err != nil {
					return "", err
				}
				_ = json.Unmarshal(raw, &r)
			}
			p := suffix
			if len(q) > 0 {
				p += "?" + q.Encode()
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out, err := sessionSend(ctx, r.ns(nsDefault), r.Name, method, p, b)
			var ar *approvalRequired
			if errors.As(err, &ar) {
				return mcp.JSON(map[string]any{
					"approval_required": true, "approval_id": ar.ID, "kind": ar.Kind, "command": ar.Summary,
					"matched_rule": ar.Matched, "expires_at": ar.ExpiresAt,
					"next": fmt.Sprintf("ask a human Kairon admin to approve it (kaironctl -n %s session approve %s %s, or the dashboard), then call this tool again with approvalId %q; or retry with approvalWaitSeconds to wait for the decision",
						r.ns(nsDefault), r.Name, ar.ID, ar.ID),
				})
			}
			if err != nil {
				return "", err
			}
			var v any
			if json.Unmarshal(out, &v) == nil {
				return mcp.JSON(v)
			}
			return string(out), nil
		}
	}
	return []mcp.Tool{
		{
			Name:        "session_list",
			Description: "List AgentSessions (FluxVM agent sessions: a sandbox VM with workspace, policy, checkpoints and ledger) with phase, node and FluxVM session id.",
			Schema:      mcp.Object(map[string]any{"namespace": nsProp}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var r sessionRef
				if err := decodeArgs(raw, &r); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					items, err := kc.ListAgentSessionsNamespace(ctx, r.ns(nsDefault))
					if err != nil {
						return "", err
					}
					out := make([]map[string]any, 0, len(items))
					for _, s := range items {
						out = append(out, map[string]any{"name": s.Metadata.Name, "phase": s.Status.Phase, "node": s.Spec.NodeName,
							"agent": s.Spec.Agent, "session": s.Status.SessionID, "message": s.Status.Message, "expiresAt": s.Status.ExpiresAt})
					}
					return mcp.JSON(map[string]any{"sessions": out})
				})
			},
		},
		{
			Name:        "session_get",
			Description: "Get one AgentSession: spec, compiled policy (including what FluxVM does not enforce), checkpoints, usage and expiry.",
			Schema:      ref(nil),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var r sessionRef
				if err := decodeArgs(raw, &r); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					s, err := kc.GetAgentSession(ctx, r.ns(nsDefault), r.Name)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"metadata": s.Metadata, "spec": s.Spec, "status": s.Status})
				})
			},
		},
		{
			Name:        "session_create",
			Description: "Create an AgentSession. kairon-controller places it on a node that serves sessions; poll session_get until phase is Running.",
			Write:       true,
			Schema: ref(map[string]any{
				"agent":      mcp.String("agent label, e.g. claude or codex"),
				"git":        mcp.String("https:// repository cloned into the workspace"),
				"image":      mcp.String("FluxVM image for the session VM"),
				"cpu":        mcp.String("vCPUs, default 2"),
				"memory":     mcp.String("memory, e.g. 4Gi; default 2Gi"),
				"preset":     mcp.String("policy preset", "offline", "developer-safe", "open"),
				"allowHosts": stringList,
				"ttlSeconds": mcp.Integer("end the session after this many seconds", 1, 30*24*3600),
				"maxExecs":   mcp.Integer("budget: exec calls", 1, 10_000_000),
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					sessionRef
					Agent, Git, Image, CPU, Memory, Preset string
					AllowHosts                             []string `json:"allowHosts"`
					TTLSeconds                             int64    `json:"ttlSeconds"`
					MaxExecs                               int64    `json:"maxExecs"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				spec, err := agentSessionSpec(sessionRunFlags{agent: a.Agent, git: a.Git, image: a.Image, cpu: a.CPU, memory: a.Memory,
					preset: a.Preset, allowHosts: a.AllowHosts, ttl: time.Duration(a.TTLSeconds) * time.Second, budget: model.AgentBudget{MaxExecs: a.MaxExecs}})
				if err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					s := model.AgentSession{
						TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindAgentSession},
						Metadata: model.ObjectMeta{Name: a.Name, Namespace: ns},
						Spec:     spec,
					}
					if _, err := kc.CreateAgentSession(ctx, ns, s); err != nil {
						return "", err
					}
					return fmt.Sprintf("agentsession %s/%s created", ns, a.Name), nil
				})
			},
		},
		{
			Name:        "session_delete",
			Description: "Delete an AgentSession and its FluxVM session (the VM and workspace are gone).",
			Write:       true,
			Schema:      ref(nil),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var r sessionRef
				if err := decodeArgs(raw, &r); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					if err := kc.DeleteAgentSession(ctx, r.ns(nsDefault), r.Name); err != nil {
						return "", err
					}
					return fmt.Sprintf("agentsession %s/%s deleted", r.ns(nsDefault), r.Name), nil
				})
			},
		},
		{
			Name:        "session_exec",
			Description: "Run a shell command line in the session workspace under its policy; returns exit_code, stdout and stderr. A command matching a policy.process.ask rule returns approval_required with an approval_id instead: only a human admin can approve it, then call again with approvalId.",
			Write:       true,
			Schema: ref(map[string]any{
				"command": mcp.String("shell command line"), "timeoutSeconds": mcp.Integer("kill after this many seconds", 1, 3600),
				"approvalId": approvalIDProp, "approvalWaitSeconds": approvalWaitProp,
			}, "command"),
			Call: verb(mcpSessionExecTimeout, http.MethodPost, "exec", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					sessionApprovalArgs
					Command        string `json:"command"`
					TimeoutSeconds int    `json:"timeoutSeconds"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				body := map[string]any{"command": a.Command}
				if a.TimeoutSeconds > 0 {
					body["timeout_seconds"] = a.TimeoutSeconds
				}
				a.apply(body)
				return body, nil, nil
			}),
		},
		{
			Name:        "session_checkpoint",
			Description: "Checkpoint the session (memory and disk) under a tag, to restore to later.",
			Write:       true,
			Schema:      ref(map[string]any{"tag": mcp.String("checkpoint tag (optional)")}),
			Call: verb(mcpSessionExecTimeout, http.MethodPost, "checkpoint", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					Tag string `json:"tag"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				body := map[string]any{}
				if a.Tag != "" {
					body["tag"] = a.Tag
				}
				return body, nil, nil
			}),
		},
		{
			Name:        "session_restore",
			Description: "Roll the session back to a checkpoint tag.",
			Write:       true,
			Schema:      ref(map[string]any{"tag": mcp.String("checkpoint tag")}, "tag"),
			Call: verb(mcpSessionExecTimeout, http.MethodPost, "restore", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					Tag string `json:"tag"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				return map[string]any{"tag": a.Tag}, nil, nil
			}),
		},
		{
			Name:        "session_fork",
			Description: "Fork live copies of the session; each child becomes its own AgentSession (names in agentSessions).",
			Write:       true,
			Schema:      ref(map[string]any{"count": mcp.Integer("number of children", 1, 64)}),
			Call: verb(mcpSessionExecTimeout, http.MethodPost, "fork", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					Count int `json:"count"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				return map[string]any{"count": max(a.Count, 1)}, nil, nil
			}),
		},
		{
			Name:        "session_job_start",
			Description: "Start a background job (long build, dev server); returns its job id for session_job_read. Approval rules apply as for session_exec.",
			Write:       true,
			Schema:      ref(map[string]any{"command": mcp.String("shell command line"), "approvalId": approvalIDProp, "approvalWaitSeconds": approvalWaitProp}, "command"),
			Call: verb(mcpSessionExecTimeout, http.MethodPost, "jobs", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					sessionApprovalArgs
					Command string `json:"command"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				body := map[string]any{"command": a.Command}
				a.apply(body)
				return body, nil, nil
			}),
		},
		{
			Name:        "session_job_read",
			Description: "Read a background job's state and output from offset (use next_offset to continue).",
			Schema:      ref(map[string]any{"job": mcp.String("job id"), "offset": mcp.Integer("stdout byte offset", 0, 1<<30)}, "job"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					sessionRef
					Job    string `json:"job"`
					Offset int    `json:"offset"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				q := url.Values{"offset": {strconv.Itoa(a.Offset)}}
				return verb(mcpCallTimeout, http.MethodGet, "jobs/"+url.PathEscape(a.Job), func(json.RawMessage) (any, url.Values, error) {
					return nil, q, nil
				})(ctx, raw)
			},
		},
		{
			Name:        "session_read_file",
			Description: "Read a text file from the session (up to 256 KiB), or list a directory with list=true.",
			Schema:      ref(map[string]any{"path": mcp.String("absolute guest path"), "list": map[string]any{"type": "boolean", "description": "list a directory"}}, "path"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					sessionRef
					Path string `json:"path"`
					List bool   `json:"list"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
				defer cancel()
				suffix := "files/content"
				if a.List {
					suffix = "files"
				}
				out, err := uiRaw(ctx, http.MethodGet, filesQuery(a.ns(nsDefault), a.Name, suffix, a.Path, nil), nil)
				if err != nil || a.List {
					return string(out), err
				}
				var f struct {
					ContentBase64 string `json:"content_base64"`
				}
				if err := json.Unmarshal(out, &f); err != nil {
					return "", err
				}
				data, err := base64.StdEncoding.DecodeString(f.ContentBase64)
				if err != nil {
					return "", err
				}
				if len(data) > mcpFileTextLimit {
					return string(data[:mcpFileTextLimit]) + "\n[truncated]", nil
				}
				return string(data), nil
			},
		},
		{
			Name:        "session_write_file",
			Description: "Write a text file into the session (parent directories are created).",
			Write:       true,
			Schema:      ref(map[string]any{"path": mcp.String("absolute guest path"), "content": mcp.String("file content")}, "path", "content"),
			Call: verb(mcpCallTimeout, http.MethodPut, "files/content", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				return map[string]any{"path": a.Path, "content_base64": base64.StdEncoding.EncodeToString([]byte(a.Content)), "parents": true}, nil, nil
			}),
		},
		{
			Name:        "session_git",
			Description: "Workspace git status, diff or log.",
			Schema:      ref(map[string]any{"view": mcp.String("which view", "status", "diff", "log")}, "view"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					sessionRef
					View string `json:"view"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.View != "status" && a.View != "diff" && a.View != "log" {
					return "", fmt.Errorf("view must be status, diff or log")
				}
				return verb(mcpCallTimeout, http.MethodGet, "git/"+a.View, func(json.RawMessage) (any, url.Values, error) {
					return nil, nil, nil
				})(ctx, raw)
			},
		},
		{
			Name:        "session_git_commit",
			Description: "Commit all workspace changes with a message.",
			Write:       true,
			Schema:      ref(map[string]any{"message": mcp.String("commit message")}, "message"),
			Call: verb(mcpCallTimeout, http.MethodPost, "git/commit", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					Message string `json:"message"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				return map[string]any{"message": a.Message}, nil, nil
			}),
		},
		{
			Name:        "session_trajectory",
			Description: "The session's numbered steps (execs, checkpoints, forks, policy refusals, VM events) for review or replay.",
			Schema:      ref(nil),
			Call:        verb(mcpCallTimeout, http.MethodGet, "trajectory", nil),
		},
		{
			Name:        "session_preview",
			Description: "Mint an expiring preview URL for a guest port. The URL is a credential; share it carefully.",
			Write:       true,
			Schema:      ref(map[string]any{"port": mcp.Integer("guest port", 1, 65535), "ttlSeconds": mcp.Integer("lifetime, default 3600", 1, 86400)}, "port"),
			Call: verb(mcpCallTimeout, http.MethodPost, "preview", func(raw json.RawMessage) (any, url.Values, error) {
				var a struct {
					sessionRef
					Port       int `json:"port"`
					TTLSeconds int `json:"ttlSeconds"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, nil, err
				}
				body := map[string]any{"port": a.Port}
				if a.TTLSeconds > 0 {
					body["ttl_seconds"] = a.TTLSeconds
				}
				return body, nil, nil
			}),
		},
		{
			Name:        "session_approvals",
			Description: "The session's approval requests (commands matching policy.process.ask) with state pending, approved, denied, consumed or expired. Deciding is for human admins only and has no tool.",
			Schema:      ref(nil),
			Call:        verb(mcpCallTimeout, http.MethodGet, "approvals", nil),
		},
		{
			Name:        "session_probe",
			Description: "Which tools the guest image has (git, python3, node, go, cargo, make, chromium...) and which session features are ready, so you know before building on them.",
			Schema:      ref(nil),
			Call:        verb(mcpCallTimeout, http.MethodGet, "probe", nil),
		},
	}
}

var (
	approvalIDProp   = mcp.String("approval id from an earlier approval_required answer, once a human admin approved it")
	approvalWaitProp = mcp.Integer("wait up to this many seconds for an admin's decision instead of returning approval_required", 1, 120)
)

type sessionApprovalArgs struct {
	ApprovalID          string `json:"approvalId"`
	ApprovalWaitSeconds int    `json:"approvalWaitSeconds"`
}

func (a sessionApprovalArgs) apply(body map[string]any) {
	if a.ApprovalID != "" {
		body["approval_id"] = a.ApprovalID
	}
	if a.ApprovalWaitSeconds > 0 {
		body["approval_wait_seconds"] = min(a.ApprovalWaitSeconds, 120)
	}
}
