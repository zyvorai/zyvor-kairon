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

// QGAFsfreezeFreeze calls FluxVM's real qemu-guest-agent
// guest-fsfreeze-freeze -- flushes and freezes every mounted, writable
// filesystem inside the guest, for an application-consistent disk
// snapshot instead of a merely crash-consistent one. Only meaningful for
// a VM created with spec.guestAgent.enabled -- FluxVM returns a clear
// error otherwise ("QGA not enabled for this VM"). Always pair with a
// later QGAFsfreezeThaw call -- see internal/controller/snapshot.go's own
// doc comment for why a caller must never leave a frozen guest
// unresolved. Returns the number of filesystems frozen.
func (c *Client) QGAFsfreezeFreeze(ctx context.Context, id string) (int64, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/qga/fsfreeze", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Frozen int64 `json:"frozen"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("decode qga fsfreeze: %w", err)
	}
	return out.Frozen, nil
}

// QGAFsfreezeThaw calls FluxVM's real qemu-guest-agent guest-fsfreeze-thaw
// -- idempotent per QGA's own semantics (thawing an already-thawed guest
// succeeds as a no-op, not an error), which is exactly what makes this
// safe to retry unconditionally until it succeeds. Returns the number of
// filesystems thawed.
func (c *Client) QGAFsfreezeThaw(ctx context.Context, id string) (int64, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/qga/fsthaw", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Thawed int64 `json:"thawed"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("decode qga fsthaw: %w", err)
	}
	return out.Thawed, nil
}

// QGAExecResult is the outcome of a guest-exec call -- run to completion
// (or until timeoutSeconds elapses) by FluxVM itself before this returns,
// never a fire-and-poll handle. QEMU's own guest-exec/guest-exec-status
// QGA commands are the polling pair underneath; FluxVM does that polling
// internally so Kairon (and its callers) only ever see one synchronous
// round trip.
type QGAExecResult struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// QGAExecRequest names what to run: either Path+Args (a real argv, no
// shell involved -- QGA's guest-exec never invokes a shell, so there's no
// injection surface from Args), or Powershell as a convenience for a
// Windows guest (`powershell.exe -Command <Powershell>`, FluxVM's own
// shorthand). Exactly one of Path or Powershell must be set. TimeoutSeconds
// defaults to FluxVM's own 60s when nil.
type QGAExecRequest struct {
	Path           string
	Args           []string
	Powershell     string
	TimeoutSeconds *uint64
}

// QGAFsfreezeStatus reports whatever qemu-guest-agent's own
// guest-fsfreeze-status currently says (e.g. "thawed"/"frozen") --
// read-only, the GET companion to QGAFsfreezeFreeze/Thaw above, useful
// for confirming a guest's actual filesystem state directly rather than
// inferring it from Kairon's own quiesce-request/-status annotations
// (see internal/agent/quiesce.go).
func (c *Client) QGAFsfreezeStatus(ctx context.Context, id string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/qga/fsfreeze-status", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode qga fsfreeze-status: %w", err)
	}
	return out.Status, nil
}

// QGAFirewallOpen/QGAFirewallClose toggle a named firewall rule inside
// the guest via qemu-guest-agent -- FluxVM implements both as a
// guest-side command it runs over the same guest-exec channel QGAExec
// uses (hence the shared QGAExecResult return shape: exit code, stdout,
// stderr of whatever firewall tool the guest actually has), just with a
// friendlier name/port/protocol request shape than raw QGAExec would
// need. protocol defaults to "tcp" server-side when empty.
func (c *Client) QGAFirewallOpen(ctx context.Context, id, name string, port uint16, protocol string, timeoutSeconds *uint64) (*QGAExecResult, error) {
	payload := map[string]any{"name": name, "port": port}
	if protocol != "" {
		payload["protocol"] = protocol
	}
	if timeoutSeconds != nil {
		payload["timeout_seconds"] = *timeoutSeconds
	}
	return c.qgaFirewallCall(ctx, id, "open", payload)
}

func (c *Client) QGAFirewallClose(ctx context.Context, id, name string, timeoutSeconds *uint64) (*QGAExecResult, error) {
	payload := map[string]any{"name": name}
	if timeoutSeconds != nil {
		payload["timeout_seconds"] = *timeoutSeconds
	}
	return c.qgaFirewallCall(ctx, id, "close", payload)
}

func (c *Client) qgaFirewallCall(ctx context.Context, id, action string, payload map[string]any) (*QGAExecResult, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/qga/firewall/"+action, payload)
	if err != nil {
		return nil, err
	}
	var out struct {
		ExitCode int64  `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode qga firewall %s: %w", action, err)
	}
	return &QGAExecResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

// QGAExec calls FluxVM's real qemu-guest-agent guest-exec (+ its own
// guest-exec-status polling, done entirely on FluxVM's side) to run a
// command inside the guest and return its exit code and captured
// stdout/stderr -- no SSH key, no network path into the guest, and no
// interactive shell required, unlike a VNC/serial console. Only meaningful
// for a VM created with spec.guestAgent.enabled; FluxVM returns a clear
// error otherwise. A command that outlives TimeoutSeconds (or FluxVM's own
// 60s default) returns an error from this call -- there is no partial
// result or way to attach to it afterward, since FluxVM's own guest-exec
// wrapper doesn't expose one either.
func (c *Client) QGAExec(ctx context.Context, id string, req QGAExecRequest) (*QGAExecResult, error) {
	if (req.Path == "") == (req.Powershell == "") {
		return nil, fmt.Errorf("qga exec requires exactly one of Path or Powershell")
	}
	payload := map[string]any{}
	if req.Path != "" {
		payload["path"] = req.Path
		if len(req.Args) > 0 {
			payload["args"] = req.Args
		}
	} else {
		payload["powershell"] = req.Powershell
	}
	if req.TimeoutSeconds != nil {
		payload["timeout_seconds"] = *req.TimeoutSeconds
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/qga/exec", payload)
	if err != nil {
		return nil, err
	}
	var out struct {
		ExitCode int64  `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode qga exec: %w", err)
	}
	return &QGAExecResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}
