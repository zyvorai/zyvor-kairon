// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const KindAgentSession = "AgentSession"

// An AgentSession is one FluxVM agent session (FluxVM's
// /v1/agents/sessions): its own sandbox VM plus a workspace, a compiled
// policy, checkpoints, an event ledger and the exec/jobs/files/git/preview
// verbs on top. kairon-controller picks a node that advertises
// LabelAgentSessionsCapable, kairon-node creates the session in that
// node's FluxVM and mirrors it into status, and kairon-ui relays the
// verbs. The VM belongs to FluxVM, not to a Machine.
const (
	// LabelAgentSessionsCapable is "true" on a node whose FluxVM serves
	// agent sessions; kairon-node probes and publishes it.
	LabelAgentSessionsCapable = "kairon.zyvor.dev/agent-sessions"
	// FinalizerAgentSession holds a deleting session until kairon-node has
	// deleted it in FluxVM.
	FinalizerAgentSession = "kairon.zyvor.dev/agentsession-cleanup"
	// AnnotationAdoptSession names an existing FluxVM session id this
	// AgentSession mirrors instead of creating one: kairon-ui sets it on
	// the child sessions a fork made.
	AnnotationAdoptSession = "kairon.zyvor.dev/adopt-session"

	AgentSessionPending  = "Pending"
	AgentSessionCreating = "Creating"
	AgentSessionRunning  = "Running"
	AgentSessionEnded    = "Ended"
	AgentSessionFailed   = "Failed"
)

type AgentSession struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta         `json:"metadata"`
	Spec     AgentSessionSpec   `json:"spec"`
	Status   AgentSessionStatus `json:"status,omitempty"`
}

func (s AgentSession) Namespace() string {
	return s.Metadata.Namespace
}

// Adopted reports whether this session mirrors an existing FluxVM session
// (a fork child) rather than creating its own.
func (s AgentSession) Adopted() string {
	return s.Metadata.Annotations[AnnotationAdoptSession]
}

// Active reports whether the session holds (or is about to hold) a VM on
// its node: scheduled and not Ended or Failed.
func (s AgentSession) Active() bool {
	return s.Spec.NodeName != "" && s.Status.Phase != AgentSessionEnded && s.Status.Phase != AgentSessionFailed
}

type AgentSessionList struct {
	TypeMeta `json:",inline"`
	Items    []AgentSession `json:"items"`
}

type AgentSessionSpec struct {
	// Agent labels the session (FluxVM default "custom").
	Agent     string                `json:"agent,omitempty"`
	Workspace *AgentWorkspace       `json:"workspace,omitempty"`
	Resources AgentSessionResources `json:"resources,omitempty"`
	// Image is a FluxVM image name or path for the session's VM; empty
	// uses FluxVM's default sandbox image.
	Image  string       `json:"image,omitempty"`
	Policy *AgentPolicy `json:"policy,omitempty"`
	Budget *AgentBudget `json:"budget,omitempty"`
	// TTLSeconds ends the session this long after it is created.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
	// NodeName is set by kairon-controller, or by the author to pin it.
	NodeName  string                `json:"nodeName,omitempty"`
	Placement AgentSessionPlacement `json:"placement,omitempty"`
}

type AgentWorkspace struct {
	// Git is an https:// repository cloned into Path at create.
	Git string `json:"git,omitempty"`
	// Path is the guest directory; FluxVM's default is /workspace.
	Path string `json:"path,omitempty"`
	// Mode is clone (default) or readonly.
	Mode string `json:"mode,omitempty"`
	// RequireReady fails the create unless the directory exists and the
	// clone succeeded.
	RequireReady bool `json:"requireReady,omitempty"`
}

type AgentSessionResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type AgentSessionPlacement struct {
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Tolerations  []Toleration      `json:"tolerations,omitempty"`
}

// AgentPolicy is FluxVM's session policy. FluxVM compiles it at create
// and reports the result in status.policy.
type AgentPolicy struct {
	// Preset is offline, developer-safe or open.
	Preset     string                 `json:"preset,omitempty"`
	Network    *AgentNetworkPolicy    `json:"network,omitempty"`
	Filesystem *AgentFilesystemPolicy `json:"filesystem,omitempty"`
	Process    *AgentProcessPolicy    `json:"process,omitempty"`
}

type AgentNetworkPolicy struct {
	// Default is deny (default) or allow.
	Default string              `json:"default,omitempty"`
	Allow   []AgentNetworkAllow `json:"allow,omitempty"`
	// Enforcement is strict (default) or best-effort; it only matters
	// when an allow entry names methods or paths.
	Enforcement string `json:"enforcement,omitempty"`
}

// AgentNetworkAllow is a host (example.com, *.example.com) the session
// may reach, optionally limited to HTTP methods and path prefixes.
type AgentNetworkAllow struct {
	Host    string   `json:"host"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

// AgentFilesystemPolicy confines exec, jobs, files and git in the guest
// (Landlock); a trailing /** means the directory.
type AgentFilesystemPolicy struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
	// Enforcement is strict (default) or best-effort.
	Enforcement string `json:"enforcement,omitempty"`
}

// AgentProcessPolicy is glob rules over submitted command strings. FluxVM
// documents it as a guardrail, not a security boundary.
type AgentProcessPolicy struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// AgentBudget limits what the session may consume; absent limits do not
// apply. Fork children get the parent's remaining budget.
type AgentBudget struct {
	MaxExecs          int64 `json:"maxExecs,omitempty"`
	MaxExecSeconds    int64 `json:"maxExecSeconds,omitempty"`
	MaxJobs           int64 `json:"maxJobs,omitempty"`
	MaxConcurrentJobs int64 `json:"maxConcurrentJobs,omitempty"`
	MaxChildren       int64 `json:"maxChildren,omitempty"`
	MaxComputeSeconds int64 `json:"maxComputeSeconds,omitempty"`
	MaxOutputBytes    int64 `json:"maxOutputBytes,omitempty"`
	MaxWorkspaceBytes int64 `json:"maxWorkspaceBytes,omitempty"`
}

type AgentSessionStatus struct {
	Phase    string `json:"phase,omitempty"`
	Message  string `json:"message,omitempty"`
	NodeName string `json:"nodeName,omitempty"`
	// SessionID is FluxVM's session id; SandboxID its VM.
	SessionID string `json:"sessionID,omitempty"`
	SandboxID string `json:"sandboxID,omitempty"`
	Backend   string `json:"backend,omitempty"`
	// State is FluxVM's own: running, expired, gone or deleted.
	State        string                    `json:"state,omitempty"`
	Capabilities *AgentSessionCapabilities `json:"capabilities,omitempty"`
	Policy       *AgentSessionPolicyStatus `json:"policy,omitempty"`
	Workspace    *AgentWorkspaceStatus     `json:"workspace,omitempty"`
	Checkpoints  []AgentCheckpoint         `json:"checkpoints,omitempty"`
	// Parent and Children are FluxVM session ids.
	Parent    string             `json:"parent,omitempty"`
	Children  []string           `json:"children,omitempty"`
	Usage     *AgentSessionUsage `json:"usage,omitempty"`
	CreatedAt *time.Time         `json:"createdAt,omitempty"`
	ExpiresAt *time.Time         `json:"expiresAt,omitempty"`
	// CreateRequestedAt is when kairon-node asked FluxVM for the session,
	// so a restart can find a session created while it was down.
	CreateRequestedAt *time.Time `json:"createRequestedAt,omitempty"`
}

type AgentSessionCapabilities struct {
	Snapshot    bool `json:"snapshot"`
	Fork        bool `json:"fork"`
	Pty         bool `json:"pty"`
	Confinement bool `json:"confinement"`
	HTTPPolicy  bool `json:"httpPolicy"`
}

// AgentSessionPolicyStatus is the policy as FluxVM compiled it, including
// the parts it accepted but does not enforce.
type AgentSessionPolicyStatus struct {
	Preset             string              `json:"preset,omitempty"`
	Offline            bool                `json:"offline"`
	AllowHosts         []string            `json:"allowHosts,omitempty"`
	Process            *AgentProcessPolicy `json:"process,omitempty"`
	Unenforced         []string            `json:"unenforced,omitempty"`
	HTTPRules          []string            `json:"httpRules,omitempty"`
	Enforcement        string              `json:"enforcement,omitempty"`
	NetworkEnforcement string              `json:"networkEnforcement,omitempty"`
}

type AgentWorkspaceStatus struct {
	Path string `json:"path,omitempty"`
	// Git has any userinfo removed.
	Git      string `json:"git,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Cloned   bool   `json:"cloned"`
	Baseline bool   `json:"baseline"`
	Note     string `json:"note,omitempty"`
}

type AgentCheckpoint struct {
	Tag       string     `json:"tag"`
	Memory    bool       `json:"memory"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

type AgentSessionUsage struct {
	Execs          int64 `json:"execs"`
	ExecMillis     int64 `json:"execMillis"`
	Jobs           int64 `json:"jobs"`
	Children       int64 `json:"children"`
	WorkspaceBytes int64 `json:"workspaceBytes,omitempty"`
}

// AgentSessionMachine is the stand-in Machine kairon-controller schedules
// and accounts a session as: its CPU and memory, its placement, and the
// capability label. It tolerates the Mac vm-only taint, since the session
// is a VM.
func AgentSessionMachine(s AgentSession) Machine {
	selector := map[string]string{LabelAgentSessionsCapable: "true"}
	for k, v := range s.Spec.Placement.NodeSelector {
		selector[k] = v
	}
	tolerations := append([]Toleration{{Key: TaintVMOnly, Operator: TolerationOpExists}}, s.Spec.Placement.Tolerations...)
	m := Machine{
		Metadata: ObjectMeta{Name: s.Metadata.Name, Namespace: s.Metadata.Namespace, Labels: s.Metadata.Labels},
		Spec: MachineSpec{
			NodeName:  s.Spec.NodeName,
			Resources: ResourceSpec{CPU: s.Spec.Resources.CPU, Memory: s.Spec.Resources.Memory},
			Placement: PlacementSpec{NodeSelector: selector, Tolerations: tolerations},
		},
	}
	if m.Spec.Resources.CPU == "" {
		m.Spec.Resources.CPU = DefaultAgentSessionCPU
	}
	if m.Spec.Resources.Memory == "" {
		m.Spec.Resources.Memory = DefaultAgentSessionMemory
	}
	return m
}

// What a session is accounted as when spec.resources is empty: FluxVM's
// default sandbox shape.
const (
	DefaultAgentSessionCPU    = "2"
	DefaultAgentSessionMemory = "2Gi"
)

// ValidateAgentSessionSpec mirrors FluxVM's create-time checks, so a bad
// session fails at admission (or in kaironctl) instead of on the node.
func ValidateAgentSessionSpec(s AgentSessionSpec) error {
	if s.Agent != "" && !agentName(s.Agent) {
		return fmt.Errorf("agent must be 1-64 characters of [A-Za-z0-9._-]")
	}
	if w := s.Workspace; w != nil {
		if w.Path != "" && !guestPath(w.Path) {
			return fmt.Errorf("workspace.path must be an absolute guest path of [A-Za-z0-9/._-] without ..")
		}
		if w.Mode != "" && w.Mode != "clone" && w.Mode != "readonly" {
			return fmt.Errorf("workspace.mode must be clone or readonly")
		}
		if w.Git != "" && !gitURL(w.Git) {
			return fmt.Errorf("workspace.git must be an https:// URL")
		}
	}
	if s.Resources.CPU != "" {
		n, err := ParseVCPUs(s.Resources.CPU)
		if err != nil || n == 0 || n > 255 {
			return fmt.Errorf("resources.cpu must be 1-255 vCPUs")
		}
	}
	if s.Resources.Memory != "" {
		if mib, err := ParseMemoryMiB(s.Resources.Memory); err != nil || mib == 0 {
			return fmt.Errorf("resources.memory: invalid quantity %q", s.Resources.Memory)
		}
	}
	if s.TTLSeconds < 0 {
		return fmt.Errorf("ttlSeconds must be positive")
	}
	if err := validateAgentPolicy(s.Policy); err != nil {
		return err
	}
	return validateAgentBudget(s.Budget, s.Workspace != nil)
}

func validateAgentPolicy(p *AgentPolicy) error {
	if p == nil {
		return nil
	}
	switch p.Preset {
	case "", "offline", "developer-safe", "open":
	default:
		return fmt.Errorf("policy.preset must be offline, developer-safe or open")
	}
	if n := p.Network; n != nil {
		if n.Default != "" && n.Default != "deny" && n.Default != "allow" {
			return fmt.Errorf("policy.network.default must be deny or allow")
		}
		if n.Default == "allow" && len(n.Allow) > 0 {
			return fmt.Errorf("policy.network.default allow with an allow list is contradictory; use deny")
		}
		if n.Enforcement != "" && n.Enforcement != "strict" && n.Enforcement != "best-effort" {
			return fmt.Errorf("policy.network.enforcement must be strict or best-effort")
		}
		for i, a := range n.Allow {
			if !allowHost(a.Host) {
				return fmt.Errorf("policy.network.allow[%d].host must be a host name (example.com or *.example.com)", i)
			}
			for _, m := range a.Methods {
				m = strings.TrimSpace(m)
				if m != "*" && !httpMethod(strings.ToUpper(m)) {
					return fmt.Errorf("policy.network.allow[%d].methods: %q is not an HTTP method", i, m)
				}
			}
			for _, path := range a.Paths {
				if path = strings.TrimSpace(path); path != "" && !strings.HasPrefix(path, "/") {
					return fmt.Errorf("policy.network.allow[%d].paths: %q must start with /", i, path)
				}
			}
		}
	}
	if f := p.Filesystem; f != nil {
		if f.Enforcement != "" && f.Enforcement != "strict" && f.Enforcement != "best-effort" {
			return fmt.Errorf("policy.filesystem.enforcement must be strict or best-effort")
		}
		for _, list := range []struct {
			name  string
			paths []string
		}{{"read", f.Read}, {"write", f.Write}} {
			for _, path := range list.paths {
				if !confinePath(path) {
					return fmt.Errorf("policy.filesystem.%s: %q must be an absolute path without .. or wildcards (a trailing /** means the directory)", list.name, path)
				}
			}
		}
	}
	if pr := p.Process; pr != nil {
		for _, list := range []struct {
			name     string
			patterns []string
		}{{"allow", pr.Allow}, {"deny", pr.Deny}} {
			for _, pat := range list.patterns {
				if pat = strings.TrimSpace(pat); pat == "" || len(pat) > 256 {
					return fmt.Errorf("policy.process.%s entries are non-empty strings of at most 256 characters", list.name)
				}
			}
		}
	}
	return nil
}

func validateAgentBudget(b *AgentBudget, hasWorkspace bool) error {
	if b == nil {
		return nil
	}
	for _, f := range []struct {
		name  string
		v     int64
		limit int64
	}{
		{"maxExecs", b.MaxExecs, 10_000_000},
		{"maxExecSeconds", b.MaxExecSeconds, 10_000_000},
		{"maxJobs", b.MaxJobs, 10_000_000},
		{"maxConcurrentJobs", b.MaxConcurrentJobs, 10_000_000},
		{"maxChildren", b.MaxChildren, 10_000_000},
		{"maxComputeSeconds", b.MaxComputeSeconds, 10_000_000},
		{"maxOutputBytes", b.MaxOutputBytes, 256 << 20},
		{"maxWorkspaceBytes", b.MaxWorkspaceBytes, 1 << 40},
	} {
		if f.v < 0 || f.v > f.limit {
			return fmt.Errorf("budget.%s must be 1-%d", f.name, f.limit)
		}
	}
	if b.MaxWorkspaceBytes > 0 && !hasWorkspace {
		return fmt.Errorf("budget.maxWorkspaceBytes needs a workspace to measure")
	}
	return nil
}

func agentName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !isAlnum(c) && !strings.ContainsRune("-_.", c) {
			return false
		}
	}
	return true
}

func guestPath(p string) bool {
	if !strings.HasPrefix(p, "/") || len(p) > 200 || strings.Contains(p, "..") {
		return false
	}
	for _, c := range p {
		if !isAlnum(c) && !strings.ContainsRune("/._-", c) {
			return false
		}
	}
	return true
}

func gitURL(u string) bool {
	return strings.HasPrefix(u, "https://") && len(u) <= 500 && !strings.HasPrefix(u, "https://-") && !strings.ContainsAny(u, " \t\r\n")
}

func confinePath(raw string) bool {
	t := strings.TrimSpace(raw)
	if s, ok := strings.CutSuffix(t, "/**"); ok {
		t = s
	} else if s, ok := strings.CutSuffix(t, "/*"); ok {
		t = s
	}
	if len(t) > 1 {
		t = strings.TrimRight(t, "/")
	}
	if !strings.HasPrefix(t, "/") || len(t) > 512 {
		return false
	}
	for _, c := range strings.Split(t, "/") {
		if c == ".." {
			return false
		}
	}
	for _, c := range t {
		if c < 0x20 || c == 0x7f || strings.ContainsRune("*?[]{}", c) {
			return false
		}
	}
	return true
}

func allowHost(h string) bool {
	h = strings.TrimPrefix(h, "*.")
	if h == "" || len(h) > 253 || strings.ContainsAny(h, "/:@ *") {
		return false
	}
	for _, c := range h {
		if !isAlnum(c) && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func httpMethod(m string) bool {
	if m == "" || len(m) > 16 {
		return false
	}
	for _, c := range m {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func isAlnum(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// RedactGitURL drops any userinfo (a token in https://TOKEN@host/...)
// from a git URL before it is stored or shown.
func RedactGitURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		if i := strings.Index(raw, "@"); err != nil && i > 0 && strings.HasPrefix(raw, "https://") {
			return "https://" + raw[i+1:]
		}
		return raw
	}
	u.User = nil
	return u.String()
}
