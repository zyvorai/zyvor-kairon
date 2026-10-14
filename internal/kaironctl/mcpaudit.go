// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
)

// mcpAudit is the write-tool audit log of the running MCP server. Nil
// outside `mcp serve --allow-write`; replay_audit then needs events passed in.
var mcpAudit *agentplane.FileLog

func defaultAuditLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".kairon-audit.jsonl"
	}
	return filepath.Join(home, ".kairon", "audit.jsonl")
}

func openMCPAudit(path, configMap string) (*agentplane.FileLog, error) {
	log, err := agentplane.OpenFileLog(path)
	if err != nil {
		return nil, err
	}
	if configMap == "" {
		return log, nil
	}
	ns, name, ok := strings.Cut(configMap, "/")
	if !ok || ns == "" || name == "" {
		return nil, fmt.Errorf("--audit-configmap must be namespace/name")
	}
	log.Mirror = func(e agentplane.Event) error {
		kc, err := newKubeClient()
		if err != nil {
			return err
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return kc.PatchConfigMapData(ctx, ns, name, map[string]any{e.ID: string(b)})
	}
	return log, nil
}

func mcpPrincipal() string {
	if p := strings.TrimSpace(os.Getenv("KAIRON_MCP_PRINCIPAL")); p != "" {
		return p
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "mcp"
}

// mcpAuditHook records every write tool call. The claim is taken from a
// "claim" argument, else "name", so replay_audit can filter by it.
func mcpAuditHook(log *agentplane.FileLog) func(context.Context, string, json.RawMessage, string) error {
	return func(_ context.Context, tool string, args json.RawMessage, outcome string) error {
		if tool == "session_input" {
			args = redactTypedText(args)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(args, &fields)
		var claim string
		for _, key := range []string{"claim", "name"} {
			if json.Unmarshal(fields[key], &claim) == nil && claim != "" {
				break
			}
		}
		_, err := log.Append(agentplane.Event{
			Principal: mcpPrincipal(),
			Tool:      tool,
			Tenant:    strings.TrimSpace(os.Getenv("KAIRON_MCP_TENANT")),
			Claim:     claim,
			Diff:      string(args),
			Outcome:   outcome,
		})
		return err
	}
}
