// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zyvorai/kairon/internal/model"
)

// sandboxCreateRequest mirrors FluxVM's own SandboxCreateRequest exactly
// (fluxvm-scheduler's own struct of the same name) -- Template and Spec
// are mutually exclusive on FluxVM's own side (a template supplies its
// own pre-baked spec; sending both just has Template win, Spec silently
// ignored), so CreateSandboxForMachine below only ever sets one.
type sandboxCreateRequest struct {
	Name           string         `json:"name,omitempty"`
	Template       string         `json:"template,omitempty"`
	Spec           *CreateRequest `json:"spec,omitempty"`
	TTLSeconds     int64          `json:"ttl_seconds,omitempty"`
	HTTPProxyPorts []uint16       `json:"http_proxy_ports,omitempty"`
	// Container track (macOS vz): run an OCI image as the sandbox. FluxVM
	// refuses template/spec with oci, so only one of them is ever set.
	OCI        *ociSandboxRequest `json:"oci,omitempty"`
	Profile    string             `json:"profile,omitempty"`
	VCPUs      uint32             `json:"vcpus,omitempty"`
	MemoryMiB  uint64             `json:"memory_mib,omitempty"`
	Offline    bool               `json:"offline,omitempty"`
	AllowHosts []string           `json:"allow_hosts,omitempty"`
}

// CreateSandboxForMachine creates m as a FluxVM sandbox (POST
// /v1/sandboxes) instead of a plain VM (POST /v1/vms) -- FluxVM's own
// agent-sandbox track, its lightweight in-tree hypervisor
// (BackendKind::FluxVm) for short-lived, ephemeral workloads. FluxVM
// force-enables its own vsock guest agent and forces backend to FluxVm
// server-side regardless of what's sent here -- see
// docs/guides/machine-sandboxes.md.
//
// When m.Spec.Sandbox.TemplateName is set, Spec is omitted entirely:
// FluxVM loads the template's own pre-baked image/resources/kernel and
// would silently ignore Spec if both were sent, so sending it anyway
// would just be misleading about what actually took effect.
func (c *Client) CreateSandboxForMachine(ctx context.Context, m model.Machine) (*Record, error) {
	if len(m.Spec.DeviceClaims) > 0 {
		return nil, fmt.Errorf("spec.deviceClaims is not supported for sandbox Machines (spec.sandbox) -- FluxVM's FluxVm backend doesn't support VFIO passthrough")
	}
	req := sandboxCreateRequest{
		Name:           m.RuntimeName(),
		TTLSeconds:     m.Spec.TTLSeconds,
		HTTPProxyPorts: m.Spec.Sandbox.HTTPProxyPorts,
	}
	switch {
	case m.Spec.Sandbox.OCI != nil:
		if err := fillOCIRequest(&req, m); err != nil {
			return nil, err
		}
	case m.Spec.Sandbox.TemplateName != "":
		req.Template = m.Spec.Sandbox.TemplateName
	default:
		payload, err := buildCreateRequest(m, "flux-vm", nil, nil)
		if err != nil {
			return nil, err
		}
		req.Spec = &payload
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/sandboxes", req)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode FluxVM create sandbox: %w", err)
	}
	return &rec, nil
}

// ListSandboxes returns every FluxVM sandbox on this node -- FluxVM's own
// GET /v1/sandboxes, a server-side filter over the same VM store every
// other route in this client already talks to (backend == flux-vm), not
// a separate resource type.
func (c *Client) ListSandboxes(ctx context.Context) ([]Record, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []Record `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM list sandboxes: %w", err)
	}
	return out.Items, nil
}

// SnapshotSandbox saves a sandbox's rootfs state to an explicit file path
// on the node -- FluxVM's own POST /v1/sandboxes/{id}/snapshot. A
// genuinely different mechanism from Snapshot/RestoreSnapshot
// (internal/fluxvm/hibernate.go): those save/restore a tagged, internally
// managed hypervisor-state checkpoint for any VM; this exports a
// sandbox's own state to a path the caller names, the same building
// block templates.md's own template-building machinery uses internally.
func (c *Client) SnapshotSandbox(ctx context.Context, id, path string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/snapshot", map[string]any{"path": path})
	return err
}

// TemplateInfo mirrors FluxVM's own TemplateInfo (fluxvm-scheduler's
// sandbox module) -- Path is a node-local filesystem path, meaningful
// only on the node that built it (templates, like the image catalog, are
// per-node state, never replicated across nodes).
type TemplateInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Snapshot bool   `json:"snapshot"`
}

// ListTemplates returns every sandbox template built on this node --
// FluxVM's own GET /v1/templates.
func (c *Client) ListTemplates(ctx context.Context) ([]TemplateInfo, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/templates", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []TemplateInfo `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM list templates: %w", err)
	}
	return out.Items, nil
}

// BuildTemplate builds a new sandbox template from an OCI image reference
// (e.g. "docker.io/library/alpine:3.19") -- FluxVM's own
// POST /v1/templates, which shells out to skopeo+umoci to export the
// image's rootfs. FluxVM's own admin-only gate applies here regardless of
// what calls this; internal/uiapi adds its own on top the same way it
// does for every other admin-only route.
func (c *Client) BuildTemplate(ctx context.Context, name, imageRef string) (*TemplateInfo, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/templates", map[string]any{"name": name, "image_ref": imageRef})
	if err != nil {
		return nil, err
	}
	var out TemplateInfo
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM build template: %w", err)
	}
	return &out, nil
}
