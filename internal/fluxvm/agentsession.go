// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// AgentSession mirrors the session record FluxVM's /v1/agents/sessions
// routes return (fluxvm-api agent_sessions.rs, v0.4.8).
type AgentSession struct {
	ID           string                   `json:"id"`
	Agent        string                   `json:"agent"`
	SandboxID    string                   `json:"sandbox_id"`
	Backend      string                   `json:"backend"`
	Parent       string                   `json:"parent,omitempty"`
	Children     []string                 `json:"children"`
	State        string                   `json:"state"`
	CreatedAt    time.Time                `json:"created_at"`
	ExpiresAt    *time.Time               `json:"expires_at"`
	Capabilities AgentSessionCapabilities `json:"capabilities"`
	Policy       AgentSessionPolicy       `json:"policy"`
	Workspace    *AgentSessionWorkspace   `json:"workspace"`
	Checkpoints  []AgentSessionCheckpoint `json:"checkpoints"`
	Usage        *AgentSessionUsage       `json:"usage,omitempty"`
}

type AgentSessionCapabilities struct {
	Snapshot    bool   `json:"snapshot"`
	Fork        bool   `json:"fork"`
	Pty         bool   `json:"pty"`
	Confinement bool   `json:"confinement"`
	HTTPPolicy  bool   `json:"http_policy"`
	Engine      string `json:"engine"`
	Supervisor  bool   `json:"supervisor"`
}

type AgentSessionPolicy struct {
	Preset     string   `json:"preset"`
	Offline    bool     `json:"offline"`
	AllowHosts []string `json:"allow_hosts"`
	Process    *struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
		Ask   []string `json:"ask"`
	} `json:"process,omitempty"`
	Unenforced         []string `json:"unenforced"`
	Enforcement        string   `json:"enforcement,omitempty"`
	HTTPRules          []string `json:"http_rules,omitempty"`
	NetworkEnforcement string   `json:"network_enforcement,omitempty"`
}

type AgentSessionWorkspace struct {
	Path     string `json:"path"`
	Git      string `json:"git"`
	Mode     string `json:"mode"`
	Cloned   bool   `json:"cloned"`
	Baseline bool   `json:"baseline"`
	Note     string `json:"note"`
}

type AgentSessionCheckpoint struct {
	Tag       string    `json:"tag"`
	Memory    bool      `json:"memory"`
	CreatedAt time.Time `json:"created_at"`
}

type AgentSessionUsage struct {
	Execs          int64 `json:"execs"`
	ExecMillis     int64 `json:"exec_ms"`
	Jobs           int64 `json:"jobs"`
	Children       int64 `json:"children"`
	WorkspaceBytes int64 `json:"workspace_bytes,omitempty"`
}

// ErrSessionNotFound is FluxVM answering 404 for a session id.
var ErrSessionNotFound = errors.New("agent session not found")

// sessionCreateTimeout bounds POST /v1/agents/sessions, which boots the
// sandbox, mounts volumes, clones the workspace and probes the policy
// before it answers.
const sessionCreateTimeout = 10 * time.Minute

// AgentSessionsSupported reports whether this FluxVM serves agent
// sessions: GET /v1/agents/sessions answers, since
// /v1/runtime/capabilities does not list them.
func (c *Client) AgentSessionsSupported(ctx context.Context) (bool, error) {
	_, err := c.do(ctx, http.MethodGet, "/v1/agents/sessions", nil)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "HTTP 404") {
		return false, nil
	}
	return false, err
}

// AgentSessionCreateBody is the POST /v1/agents/sessions body for spec.
func AgentSessionCreateBody(spec model.AgentSessionSpec) (map[string]any, error) {
	body := map[string]any{}
	if spec.Agent != "" {
		body["agent"] = spec.Agent
	}
	if w := spec.Workspace; w != nil {
		ws := map[string]any{}
		if w.Git != "" {
			ws["git"] = w.Git
		}
		if w.Path != "" {
			ws["path"] = w.Path
		}
		if w.Mode != "" {
			ws["mode"] = w.Mode
		}
		if w.RequireReady {
			ws["require_ready"] = true
		}
		body["workspace"] = ws
	}
	runtime := map[string]any{}
	if spec.Image != "" {
		runtime["image"] = spec.Image
	}
	if spec.Engine != "" {
		runtime["engine"] = spec.Engine
	}
	if spec.Resources.CPU != "" {
		n, err := model.ParseVCPUs(spec.Resources.CPU)
		if err != nil {
			return nil, fmt.Errorf("resources.cpu: %w", err)
		}
		runtime["cpus"] = n
	}
	if spec.Resources.Memory != "" {
		mib, err := model.ParseMemoryMiB(spec.Resources.Memory)
		if err != nil {
			return nil, fmt.Errorf("resources.memory: %w", err)
		}
		runtime["memory_mb"] = mib
	}
	if len(runtime) > 0 {
		body["runtime"] = runtime
	}
	if p := spec.Policy; p != nil {
		policy := map[string]any{}
		if p.Preset != "" {
			policy["preset"] = p.Preset
		}
		if n := p.Network; n != nil {
			network := map[string]any{}
			if n.Default != "" {
				network["default"] = n.Default
			}
			if n.Enforcement != "" {
				network["enforcement"] = n.Enforcement
			}
			allow := make([]any, 0, len(n.Allow))
			for _, a := range n.Allow {
				if len(a.Methods) == 0 && len(a.Paths) == 0 {
					allow = append(allow, a.Host)
					continue
				}
				entry := map[string]any{"host": a.Host}
				if len(a.Methods) > 0 {
					entry["methods"] = a.Methods
				}
				if len(a.Paths) > 0 {
					entry["paths"] = a.Paths
				}
				allow = append(allow, entry)
			}
			if len(allow) > 0 {
				network["allow"] = allow
			}
			policy["network"] = network
		}
		if f := p.Filesystem; f != nil {
			fs := map[string]any{}
			if len(f.Read) > 0 {
				fs["read"] = f.Read
			}
			if len(f.Write) > 0 {
				fs["write"] = f.Write
			}
			if f.Enforcement != "" {
				fs["enforcement"] = f.Enforcement
			}
			policy["filesystem"] = fs
		}
		if pr := p.Process; pr != nil {
			proc := map[string]any{}
			if len(pr.Allow) > 0 {
				proc["allow"] = pr.Allow
			}
			if len(pr.Deny) > 0 {
				proc["deny"] = pr.Deny
			}
			if len(pr.Ask) > 0 {
				proc["ask"] = pr.Ask
			}
			policy["process"] = proc
		}
		body["policy"] = policy
	}
	if b := spec.Budget; b != nil {
		budget := map[string]any{}
		for key, v := range map[string]int64{
			"max_execs":           b.MaxExecs,
			"max_exec_seconds":    b.MaxExecSeconds,
			"max_jobs":            b.MaxJobs,
			"max_concurrent_jobs": b.MaxConcurrentJobs,
			"max_children":        b.MaxChildren,
			"max_compute_seconds": b.MaxComputeSeconds,
			"max_output_bytes":    b.MaxOutputBytes,
			"max_workspace_bytes": b.MaxWorkspaceBytes,
		} {
			if v > 0 {
				budget[key] = v
			}
		}
		if len(budget) > 0 {
			body["budget"] = budget
		}
	}
	if spec.TTLSeconds > 0 {
		body["ttl_seconds"] = spec.TTLSeconds
	}
	return body, nil
}

// CreateFluxSession creates a session (and its sandbox VM) from spec.
func (c *Client) CreateFluxSession(ctx context.Context, spec model.AgentSessionSpec) (*AgentSession, error) {
	body, err := AgentSessionCreateBody(spec)
	if err != nil {
		return nil, err
	}
	hc := *c.HTTP
	hc.Timeout = sessionCreateTimeout
	data, err := c.doWith(ctx, &hc, http.MethodPost, "/v1/agents/sessions", body)
	if err != nil {
		return nil, err
	}
	var out AgentSession
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM create agent session: %w", err)
	}
	return &out, nil
}

// GetFluxSession reads one session; ErrSessionNotFound when FluxVM has
// no record of it.
func (c *Client) GetFluxSession(ctx context.Context, id string) (*AgentSession, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/agents/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	var out AgentSession
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM agent session: %w", err)
	}
	return &out, nil
}

func (c *Client) ListFluxSessions(ctx context.Context) ([]AgentSession, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/agents/sessions", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []AgentSession `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM agent sessions: %w", err)
	}
	return out.Items, nil
}

// DeleteFluxSession ends a session and deletes its sandbox. A session
// FluxVM no longer knows counts as deleted.
func (c *Client) DeleteFluxSession(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/agents/sessions/"+url.PathEscape(id), nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return err
}
