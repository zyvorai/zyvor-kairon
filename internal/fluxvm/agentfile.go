// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// agentResponse mirrors the wire shape of every FluxVM AgentResponse
// variant this client cares about -- FluxVM serializes that Rust enum
// internally-tagged as {"result": "<kebab-case-variant>", ...fields}
// (fluxvm-guest-protocol's own #[serde(tag = "result")]). A guest-side
// failure (permission denied, no such file, guest agent unreachable) comes
// back as HTTP 200 with {"result":"error","message":"..."} -- FluxVM
// itself never converts that into an HTTP error status, so every caller
// here must check Result itself.
type agentResponse struct {
	Result        string `json:"result"`
	Message       string `json:"message"`
	ContentBase64 string `json:"content_base64"`
	Mode          uint32 `json:"mode"`
	ExitCode      int32  `json:"exit_code"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
}

func (r agentResponse) err() error {
	if r.Result == "error" {
		return fmt.Errorf("guest agent: %s", r.Message)
	}
	return nil
}

// AgentFileContent is the result of AgentGetFile.
type AgentFileContent struct {
	ContentBase64 string
	// Mode is the Unix permission bits the file had on the guest (e.g. 0644).
	Mode uint32
}

// AgentPutFile writes a file into the guest over FluxVM's own bespoke
// vsock guest agent (POST /v1/vms/{id}/agent/put-file) -- a completely
// different channel from qemu-guest-agent's QGAExec, requiring
// spec.guestAgent.console (FluxVM's proprietary fluxvm-guest-agent, the
// same one the interactive text console uses), not spec.guestAgent.enabled.
// contentBase64 must already be base64-encoded; mode is the Unix
// permission bits to set (e.g. 0644), nil leaves the guest agent's own
// default.
func (c *Client) AgentPutFile(ctx context.Context, id, path, contentBase64 string, mode *uint32) error {
	payload := map[string]any{"path": path, "content_base64": contentBase64}
	if mode != nil {
		payload["mode"] = *mode
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/agent/put-file", payload)
	if err != nil {
		return err
	}
	var out agentResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("decode agent put-file response: %w", err)
	}
	return out.err()
}

// AgentGetFile reads a file from the guest over FluxVM's own bespoke
// vsock guest agent (POST /v1/vms/{id}/agent/get-file) -- see
// AgentPutFile's own doc comment for the channel this requires. The
// response is capped by this client's own 4MiB HTTP response limit
// (internal/fluxvm.Client.do); a base64-encoded file larger than that
// (roughly a 3MB file, once base64's ~4/3 expansion is accounted for)
// truncates mid-JSON and fails to decode here with a clear error, rather
// than silently returning partial content.
func (c *Client) AgentGetFile(ctx context.Context, id, path string) (*AgentFileContent, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/agent/get-file", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	var out agentResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode agent get-file response: %w", err)
	}
	if err := out.err(); err != nil {
		return nil, err
	}
	return &AgentFileContent{ContentBase64: out.ContentBase64, Mode: out.Mode}, nil
}

// AgentExecResult is the result of AgentExec.
type AgentExecResult struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

// AgentExec runs a command in the guest over FluxVM's own bespoke vsock
// guest agent (POST /v1/vms/{id}/agent) -- a genuinely different
// mechanism from QGAExec's qemu-guest-agent guest-exec, despite both
// being called "guest exec": this one is backend-agnostic (works on
// Cloud Hypervisor/Firecracker/FluxVm sandboxes too, anywhere the vsock
// agent runs, not just QEMU), requires spec.guestAgent.console rather
// than spec.guestAgent.enabled, and its request/response shape is
// simpler -- a single shell command string, not a real argv, and no
// PowerShell mode (that's QGA-exec-specific, for Windows guests). Both
// exist and are kept distinct rather than merged: they're different
// FluxVM routes with different backend requirements, not two names for
// the same thing.
func (c *Client) AgentExec(ctx context.Context, id, command string, timeoutSeconds *uint64) (*AgentExecResult, error) {
	payload := map[string]any{"command": command}
	if timeoutSeconds != nil {
		payload["timeout_seconds"] = *timeoutSeconds
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/agent", payload)
	if err != nil {
		return nil, err
	}
	var out agentResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode agent exec response: %w", err)
	}
	if err := out.err(); err != nil {
		return nil, err
	}
	return &AgentExecResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}
