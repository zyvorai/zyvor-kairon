// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
)

func TestValidateAgentSessionSpec(t *testing.T) {
	ok := []AgentSessionSpec{
		{},
		{Agent: "claude-code", Workspace: &AgentWorkspace{Git: "https://github.com/zyvorai/x", Path: "/workspace/x", Mode: "readonly"}},
		{Resources: AgentSessionResources{CPU: "4", Memory: "8Gi"}, TTLSeconds: 600},
		{Policy: &AgentPolicy{Preset: "developer-safe", Network: &AgentNetworkPolicy{Allow: []AgentNetworkAllow{
			{Host: "*.github.com"},
			{Host: "api.example.com", Methods: []string{"get", "POST", "*"}, Paths: []string{"/v1/"}},
		}}}},
		{Policy: &AgentPolicy{Filesystem: &AgentFilesystemPolicy{Read: []string{"/workspace/**"}, Write: []string{"/tmp/*"}, Enforcement: "best-effort"}}},
		{Policy: &AgentPolicy{Process: &AgentProcessPolicy{Allow: []string{"git *"}, Deny: []string{"rm -rf *"}}}},
		{Workspace: &AgentWorkspace{}, Budget: &AgentBudget{MaxExecs: 10, MaxWorkspaceBytes: 1 << 30}},
	}
	for i, s := range ok {
		if err := ValidateAgentSessionSpec(s); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]AgentSessionSpec{
		"agent":                 {Agent: "has space"},
		"workspace.path":        {Workspace: &AgentWorkspace{Path: "relative"}},
		"workspace.path.dotdot": {Workspace: &AgentWorkspace{Path: "/a/../b"}},
		"workspace.mode":        {Workspace: &AgentWorkspace{Mode: "overlay"}},
		"workspace.git":         {Workspace: &AgentWorkspace{Git: "git@github.com:x/y"}},
		"resources.cpu":         {Resources: AgentSessionResources{CPU: "0"}},
		"resources.mem":         {Resources: AgentSessionResources{Memory: "lots"}},
		"preset":                {Policy: &AgentPolicy{Preset: "paranoid"}},
		"network.default":       {Policy: &AgentPolicy{Network: &AgentNetworkPolicy{Default: "maybe"}}},
		"network.contradiction": {Policy: &AgentPolicy{Network: &AgentNetworkPolicy{Default: "allow", Allow: []AgentNetworkAllow{{Host: "a.com"}}}}},
		"network.host.url":      {Policy: &AgentPolicy{Network: &AgentNetworkPolicy{Allow: []AgentNetworkAllow{{Host: "https://a.com"}}}}},
		"network.method":        {Policy: &AgentPolicy{Network: &AgentNetworkPolicy{Allow: []AgentNetworkAllow{{Host: "a.com", Methods: []string{"G3T"}}}}}},
		"network.path":          {Policy: &AgentPolicy{Network: &AgentNetworkPolicy{Allow: []AgentNetworkAllow{{Host: "a.com", Paths: []string{"v1"}}}}}},
		"fs.wildcard":           {Policy: &AgentPolicy{Filesystem: &AgentFilesystemPolicy{Read: []string{"/home/*/x"}}}},
		"fs.relative":           {Policy: &AgentPolicy{Filesystem: &AgentFilesystemPolicy{Write: []string{"tmp"}}}},
		"process.empty":         {Policy: &AgentPolicy{Process: &AgentProcessPolicy{Deny: []string{"  "}}}},
		"budget.range":          {Budget: &AgentBudget{MaxOutputBytes: 1 << 40}},
		"budget.workspace":      {Budget: &AgentBudget{MaxWorkspaceBytes: 10}},
	}
	for name, s := range bad {
		if err := ValidateAgentSessionSpec(s); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestRedactGitURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/a/b":                    "https://github.com/a/b",
		"https://x-token:ghp_secret@github.com/a/b": "https://github.com/a/b",
		"https://ghp_secret@github.com/a/b.git":     "https://github.com/a/b.git",
	} {
		got := RedactGitURL(in)
		if got != want || strings.Contains(got, "secret") {
			t.Errorf("RedactGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentSessionMachine(t *testing.T) {
	s := AgentSession{Metadata: ObjectMeta{Name: "s", Namespace: "ns"}, Spec: AgentSessionSpec{
		Placement: AgentSessionPlacement{NodeSelector: map[string]string{"zone": "a"}},
	}}
	m := AgentSessionMachine(s)
	if m.Spec.Placement.NodeSelector[LabelAgentSessionsCapable] != "true" || m.Spec.Placement.NodeSelector["zone"] != "a" {
		t.Fatalf("nodeSelector = %v", m.Spec.Placement.NodeSelector)
	}
	if m.Spec.Resources.CPU != DefaultAgentSessionCPU || m.Spec.Resources.Memory != DefaultAgentSessionMemory {
		t.Fatalf("resources = %+v", m.Spec.Resources)
	}
	if len(m.Spec.Placement.Tolerations) != 1 || m.Spec.Placement.Tolerations[0].Key != TaintVMOnly {
		t.Fatalf("tolerations = %+v", m.Spec.Placement.Tolerations)
	}
}
