// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/zyvorai/kairon/internal/model"
)

// ociSandboxRequest mirrors FluxVM's OciSandboxSpec (the "oci" object of
// POST /v1/sandboxes). Secret environment values are not part of it: see
// model.OCISpec.
type ociSandboxRequest struct {
	Image        string             `json:"image"`
	Platform     string             `json:"platform,omitempty"`
	Command      []string           `json:"command,omitempty"`
	Entrypoint   []string           `json:"entrypoint,omitempty"`
	Env          []string           `json:"env,omitempty"`
	WorkDir      string             `json:"workdir,omitempty"`
	User         string             `json:"user,omitempty"`
	ReadOnlyRoot bool               `json:"read_only_root"`
	ExitPolicy   string             `json:"exit_policy,omitempty"`
	Ports        []string           `json:"ports,omitempty"`
	Restart      string             `json:"restart,omitempty"`
	MaxRestarts  *int32             `json:"max_restarts,omitempty"`
	HealthCheck  *ociHealthCheckReq `json:"healthcheck,omitempty"`
}

type ociHealthCheckReq struct {
	Command            []string `json:"command"`
	IntervalSeconds    uint32   `json:"interval_seconds,omitempty"`
	TimeoutSeconds     uint32   `json:"timeout_seconds,omitempty"`
	Retries            uint32   `json:"retries,omitempty"`
	StartPeriodSeconds uint32   `json:"start_period_seconds,omitempty"`
}

// fillOCIRequest maps m.Spec.Sandbox.OCI (plus spec.resources, when set)
// onto req. read_only_root defaults to true on FluxVM's side; Kairon sends
// it explicitly so the writable-root opt-in is the only way to lose it.
func fillOCIRequest(req *sandboxCreateRequest, m model.Machine) error {
	o := m.Spec.Sandbox.OCI
	if o == nil {
		return nil
	}
	if len(m.Spec.DeviceClaims) > 0 {
		return fmt.Errorf("spec.deviceClaims is not supported for container sandboxes (spec.sandbox.oci)")
	}
	r := &ociSandboxRequest{
		Image: o.Image, Platform: o.Platform, Command: o.Command, Entrypoint: o.Entrypoint,
		Env: o.Env, WorkDir: o.WorkDir, User: o.User, ReadOnlyRoot: !o.WritableRoot,
		ExitPolicy: o.ExitPolicy, Ports: o.Ports, Restart: o.Restart, MaxRestarts: o.MaxRestarts,
	}
	if h := o.HealthCheck; h != nil {
		r.HealthCheck = &ociHealthCheckReq{
			Command: h.Command, IntervalSeconds: h.IntervalSeconds, TimeoutSeconds: h.TimeoutSeconds,
			Retries: h.Retries, StartPeriodSeconds: h.StartPeriodSeconds,
		}
	}
	req.OCI = r
	req.Profile = o.Profile
	req.Offline = o.Offline
	req.AllowHosts = o.AllowHosts
	if m.Spec.Resources.CPU != "" {
		v, err := model.ParseVCPUs(m.Spec.Resources.CPU)
		if err != nil {
			return fmt.Errorf("spec.resources.cpu: %w", err)
		}
		req.VCPUs = v
	}
	if m.Spec.Resources.Memory != "" {
		mib, err := model.ParseMemoryMiB(m.Spec.Resources.Memory)
		if err != nil {
			return fmt.Errorf("spec.resources.memory: %w", err)
		}
		req.MemoryMiB = mib
	}
	return nil
}

// SandboxLogs is FluxVM's GET /v1/sandboxes/{id}/logs: the console tail,
// and for a container sandbox the process's exit code, restart count and
// health.
type SandboxLogs struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	OCI       bool    `json:"oci"`
	ExitCode  *int32  `json:"exit_code"`
	InitError *string `json:"init_error"`
	Restarts  uint32  `json:"restarts"`
	Health    *string `json:"health"`
	Log       string  `json:"log"`
}

// SandboxLogs returns the console tail (last lines lines; 0 lets FluxVM
// choose) and, for a container sandbox, its process state.
func (c *Client) SandboxLogs(ctx context.Context, id string, lines int) (*SandboxLogs, error) {
	path := "/v1/sandboxes/" + url.PathEscape(id) + "/logs"
	if lines > 0 {
		path += "?lines=" + strconv.Itoa(lines)
	}
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out SandboxLogs
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM sandbox logs: %w", err)
	}
	return &out, nil
}

// ContainerStatus projects the container fields of logs onto the Machine
// status shape; nil when the sandbox is not a container.
func (l *SandboxLogs) ContainerStatus() *model.ContainerStatus {
	if l == nil || !l.OCI {
		return nil
	}
	cs := &model.ContainerStatus{ExitCode: l.ExitCode, Restarts: l.Restarts}
	if l.Health != nil {
		cs.Health = *l.Health
	}
	if l.InitError != nil {
		cs.InitError = *l.InitError
	}
	return cs
}
