// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Observe, when set, is called after every apiserver request with the
	// HTTP method, how long it took, and its outcome -- kairon-controller/
	// kairon-node/kairon-ui each wire this to their own
	// internal/metrics.Recorder for a kairon_apiserver_request_duration_seconds
	// histogram (see cmd/*/main.go). Optional so internal/kube has no hard
	// dependency on internal/metrics, the same nil-checked-callback
	// pattern internal/admission.Handler already uses for its own
	// log/observe parameters.
	Observe func(method string, d time.Duration, err error)
}

type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func IsNotFound(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.StatusCode == http.StatusNotFound
	}
	return false
}

// IsConflict reports whether err is the API server rejecting a write
// because the object's resourceVersion is stale -- internal/leaderelection
// relies on this to detect it lost a race to acquire/renew/take over a
// Lease, rather than treating the write as a hard failure.
func IsConflict(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.StatusCode == http.StatusConflict
	}
	return false
}

// IsTransient reports whether err is worth retrying unchanged on a later
// tick: API throttling or a server-side failure, or a transport timeout
// (an apiserver whose datastore stalls longer than the client timeout).
func IsTransient(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func FromEnvironment() (*Client, error) {
	if base := os.Getenv("KAIRON_KUBE_URL"); base != "" {
		return New(base, os.Getenv("KAIRON_KUBE_TOKEN"), os.Getenv("KAIRON_KUBE_CA"), os.Getenv("KAIRON_KUBE_INSECURE") == "true")
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("kubernetes endpoint not configured: set KAIRON_KUBE_URL or run in cluster")
	}
	token, err := os.ReadFile(filepath.Join(serviceAccountDir, "token"))
	if err != nil {
		return nil, fmt.Errorf("read service-account token: %w", err)
	}
	return New("https://"+host+":"+port, strings.TrimSpace(string(token)), filepath.Join(serviceAccountDir, "ca.crt"), false)
}

func New(baseURL, token, caPath string, insecure bool) (*Client, error) {
	var pem []byte
	if caPath != "" && !insecure {
		var err error
		if pem, err = os.ReadFile(caPath); err != nil {
			return nil, fmt.Errorf("read CA file %s: %w", caPath, err)
		}
	}
	c, err := NewWithCA(baseURL, token, pem, insecure)
	if err != nil && caPath != "" {
		return nil, fmt.Errorf("invalid CA file %s", caPath)
	}
	return c, err
}

// NewWithCA is New with the CA bundle in memory; an empty caPEM uses the
// system roots.
func NewWithCA(baseURL, token string, caPEM []byte, insecure bool) (*Client, error) {
	// Reuse connections across reconciliation bursts, but release idle
	// sockets. Keep proxy behavior unchanged and allow long-lived watches.
	tr := &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}, // #nosec G402 - opt-in dev mode
	}
	if len(caPEM) > 0 && !insecure {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("invalid CA bundle")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Transport: tr, Timeout: 20 * time.Second}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, out any, contentType string) error {
	start := time.Now()
	err := c.doRequest(ctx, method, path, body, out, contentType)
	if c.Observe != nil {
		c.Observe(method, time.Since(start), err)
	}
	return err
}

func (c *Client) doRequest(ctx context.Context, method, path string, body any, out any, contentType string) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode Kubernetes response: %w", err)
		}
	}
	return nil
}

func namespacePath(ns, resource string) string {
	return fmt.Sprintf("/apis/kairon.zyvor.dev/v1/namespaces/%s/%s", url.PathEscape(ns), resource)
}

func namespacedObjectPath(ns, resource, name string) string {
	return namespacePath(ns, resource) + "/" + url.PathEscape(name)
}

func (c *Client) ListMachines(ctx context.Context) ([]model.Machine, error) {
	return c.ListMachinesWithSelector(ctx, "")
}

// ListMachinesWithSelector lists Machines cluster-wide, optionally filtered
// by a Kubernetes labelSelector (e.g. kairon.zyvor.dev/assigned-node=worker-1).
func (c *Client) ListMachinesWithSelector(ctx context.Context, labelSelector string) ([]model.Machine, error) {
	var list model.MachineList
	path := "/apis/kairon.zyvor.dev/v1/machines"
	if labelSelector != "" {
		path += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	err := c.request(ctx, http.MethodGet, path, nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachinesNamespace(ctx context.Context, ns string) ([]model.Machine, error) {
	var list model.MachineList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machines"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachine(ctx context.Context, ns, name string) (model.Machine, error) {
	var m model.Machine
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machines", name), nil, &m, "")
	return m, err
}

func (c *Client) CreateMachine(ctx context.Context, ns string, m model.Machine) (model.Machine, error) {
	var out model.Machine
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machines"), m, &out, "")
	return out, err
}

func (c *Client) DeleteMachine(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machines", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) PatchMachine(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machines", name), patch, nil, "application/merge-patch+json")
}

// DryRunPatchMachine sends patch with dryRun=All: admission webhooks and
// schema validation run, nothing is stored. It returns the Machine as the
// apiserver would have stored it.
func (c *Client) DryRunPatchMachine(ctx context.Context, ns, name string, patch map[string]any) (model.Machine, error) {
	var out model.Machine
	err := c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machines", name)+"?dryRun=All", patch, &out, "application/merge-patch+json")
	return out, err
}

// DryRunCreateMachineMigration is CreateMachineMigration with dryRun=All.
func (c *Client) DryRunCreateMachineMigration(ctx context.Context, ns string, m model.MachineMigration) (model.MachineMigration, error) {
	var out model.MachineMigration
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinemigrations")+"?dryRun=All", m, &out, "")
	return out, err
}

func (c *Client) PatchMachineStatus(ctx context.Context, ns, name string, status model.MachineStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machines", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

// PatchMachineStatusFields merge-patches only the given status fields, for
// writers that must not resend a whole status read earlier.
func (c *Client) PatchMachineStatusFields(ctx context.Context, ns, name string, fields map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machines", name)+"/status", map[string]any{"status": fields}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineMigrations(ctx context.Context) ([]model.MachineMigration, error) {
	return c.ListMachineMigrationsWithSelector(ctx, "")
}

// ListMachineMigrationsWithSelector lists MachineMigrations cluster-wide,
// optionally filtered by labelSelector.
func (c *Client) ListMachineMigrationsWithSelector(ctx context.Context, labelSelector string) ([]model.MachineMigration, error) {
	var list model.MachineMigrationList
	path := "/apis/kairon.zyvor.dev/v1/machinemigrations"
	if labelSelector != "" {
		path += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	err := c.request(ctx, http.MethodGet, path, nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineMigrationsNamespace(ctx context.Context, ns string) ([]model.MachineMigration, error) {
	var list model.MachineMigrationList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinemigrations"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineMigration(ctx context.Context, ns, name string) (model.MachineMigration, error) {
	var m model.MachineMigration
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinemigrations", name), nil, &m, "")
	return m, err
}

func (c *Client) CreateMachineMigration(ctx context.Context, ns string, m model.MachineMigration) (model.MachineMigration, error) {
	var out model.MachineMigration
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinemigrations"), m, &out, "")
	return out, err
}

func (c *Client) PatchMachineMigrationStatus(ctx context.Context, ns, name string, status model.MachineMigrationStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinemigrations", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachineMigration(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinemigrations", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) DeleteMachineMigration(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinemigrations", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) ListMachineSnapshots(ctx context.Context) ([]model.MachineSnapshot, error) {
	var list model.MachineSnapshotList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinesnapshots", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineSnapshotsNamespace(ctx context.Context, ns string) ([]model.MachineSnapshot, error) {
	var list model.MachineSnapshotList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinesnapshots"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineSnapshot(ctx context.Context, ns, name string) (model.MachineSnapshot, error) {
	var s model.MachineSnapshot
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinesnapshots", name), nil, &s, "")
	return s, err
}

func (c *Client) CreateMachineSnapshot(ctx context.Context, ns string, s model.MachineSnapshot) (model.MachineSnapshot, error) {
	var out model.MachineSnapshot
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinesnapshots"), s, &out, "")
	return out, err
}

func (c *Client) PatchMachineSnapshotStatus(ctx context.Context, ns, name string, status model.MachineSnapshotStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesnapshots", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachineSnapshot(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesnapshots", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) DeleteMachineSnapshot(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinesnapshots", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) ListMachineSnapshotRestores(ctx context.Context) ([]model.MachineSnapshotRestore, error) {
	var list model.MachineSnapshotRestoreList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinesnapshotrestores", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineSnapshotRestoresNamespace(ctx context.Context, ns string) ([]model.MachineSnapshotRestore, error) {
	var list model.MachineSnapshotRestoreList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinesnapshotrestores"), nil, &list, "")
	return list.Items, err
}

func (c *Client) CreateMachineSnapshotRestore(ctx context.Context, ns string, r model.MachineSnapshotRestore) (model.MachineSnapshotRestore, error) {
	var out model.MachineSnapshotRestore
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinesnapshotrestores"), r, &out, "")
	return out, err
}

func (c *Client) PatchMachineSnapshotRestoreStatus(ctx context.Context, ns, name string, status model.MachineSnapshotRestoreStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesnapshotrestores", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) GetMachineSnapshotRestore(ctx context.Context, ns, name string) (model.MachineSnapshotRestore, error) {
	var r model.MachineSnapshotRestore
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinesnapshotrestores", name), nil, &r, "")
	return r, err
}

func (c *Client) DeleteMachineSnapshotRestore(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinesnapshotrestores", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) ListMachineExports(ctx context.Context) ([]model.MachineExport, error) {
	var list model.MachineExportList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineexports", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineExportsNamespace(ctx context.Context, ns string) ([]model.MachineExport, error) {
	var list model.MachineExportList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machineexports"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineExport(ctx context.Context, ns, name string) (model.MachineExport, error) {
	var e model.MachineExport
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machineexports", name), nil, &e, "")
	return e, err
}

func (c *Client) CreateMachineExport(ctx context.Context, ns string, e model.MachineExport) (model.MachineExport, error) {
	var out model.MachineExport
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machineexports"), e, &out, "")
	return out, err
}

func (c *Client) PatchMachineExportStatus(ctx context.Context, ns, name string, status model.MachineExportStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machineexports", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) DeleteMachineExport(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machineexports", name), nil, nil, "")
}

func (c *Client) ListMachineNetworkPolicies(ctx context.Context) ([]model.MachineNetworkPolicy, error) {
	var list model.MachineNetworkPolicyList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinenetworkpolicies", nil, &list, "")
	return list.Items, err
}

// ListMachineNetworkPoliciesNamespace, GetMachineNetworkPolicy, and
// DeleteMachineNetworkPolicy give MachineNetworkPolicy the same
// get/describe/delete surface every other CRD already has through
// kaironctl/kairon-ui -- until now, this security-relevant CRD (it drives
// FluxVM's real eBPF/TC enforcement, see docs/guides/network-policy.md)
// could only be inspected via `kubectl get machinenetworkpolicies`, unlike
// MachineQuota/MachineSet/MigrationPolicy/etc.
func (c *Client) ListMachineNetworkPoliciesNamespace(ctx context.Context, ns string) ([]model.MachineNetworkPolicy, error) {
	var list model.MachineNetworkPolicyList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinenetworkpolicies"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineNetworkPolicy(ctx context.Context, ns, name string) (model.MachineNetworkPolicy, error) {
	var p model.MachineNetworkPolicy
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinenetworkpolicies", name), nil, &p, "")
	return p, err
}

// CreateMachineNetworkPolicy completes MachineNetworkPolicy's kaironctl CRUD
// surface: get/describe/delete/patch all had a path already (see this type's
// sibling methods' own doc comment above), but nothing let an operator
// create one without hand-writing YAML for `kubectl apply`, unlike
// MigrationPolicy/MachineQuota/MachineDisruptionBudget/
// MachineSnapshotSchedule, which all got a `kaironctl create` subcommand in
// the same session that gave this CRD its dashboard list page.
func (c *Client) CreateMachineNetworkPolicy(ctx context.Context, ns string, p model.MachineNetworkPolicy) (model.MachineNetworkPolicy, error) {
	var out model.MachineNetworkPolicy
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinenetworkpolicies"), p, &out, "")
	return out, err
}

func (c *Client) DeleteMachineNetworkPolicy(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinenetworkpolicies", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) PatchMachineNetworkPolicy(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinenetworkpolicies", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachineNetworkPolicyStatus(ctx context.Context, ns, name string, status model.MachineNetworkPolicyStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinenetworkpolicies", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) ListNetworkSecurityGroups(ctx context.Context) ([]model.NetworkSecurityGroup, error) {
	var list model.NetworkSecurityGroupList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/networksecuritygroups", nil, &list, "")
	return list.Items, err
}

// ListNetworkSecurityGroupsNamespace, GetNetworkSecurityGroup, and
// DeleteNetworkSecurityGroup mirror the three MachineNetworkPolicy methods
// above, for the same reason: NetworkSecurityGroup had no get/describe/
// delete path either.
func (c *Client) ListNetworkSecurityGroupsNamespace(ctx context.Context, ns string) ([]model.NetworkSecurityGroup, error) {
	var list model.NetworkSecurityGroupList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "networksecuritygroups"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetNetworkSecurityGroup(ctx context.Context, ns, name string) (model.NetworkSecurityGroup, error) {
	var g model.NetworkSecurityGroup
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "networksecuritygroups", name), nil, &g, "")
	return g, err
}

// CreateNetworkSecurityGroup is CreateMachineNetworkPolicy's exact
// counterpart for NetworkSecurityGroup, closing the same create-side CRUD
// gap for the same reason.
func (c *Client) CreateNetworkSecurityGroup(ctx context.Context, ns string, g model.NetworkSecurityGroup) (model.NetworkSecurityGroup, error) {
	var out model.NetworkSecurityGroup
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "networksecuritygroups"), g, &out, "")
	return out, err
}

func (c *Client) DeleteNetworkSecurityGroup(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "networksecuritygroups", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) PatchNetworkSecurityGroup(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "networksecuritygroups", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchNetworkSecurityGroupStatus(ctx context.Context, ns, name string, status model.NetworkSecurityGroupStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "networksecuritygroups", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) GetResourceClaim(ctx context.Context, ns, name string) (model.ResourceClaim, error) {
	var claim model.ResourceClaim
	path := fmt.Sprintf("/apis/resource.k8s.io/v1/namespaces/%s/resourceclaims/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodGet, path, nil, &claim, "")
	return claim, err
}

// ListResourceClaims and ListResourceSlices back
// internal/controller's DRA topology-awareness scheduling hint (see
// draPreferredNode) -- both are read-only, cluster-wide lists, called at
// most once per reconcile tick and only when at least one Machine actually
// has spec.deviceClaims set, so a cluster without the DRA API enabled at
// all never pays for (or errors on) either call.
func (c *Client) ListResourceClaims(ctx context.Context) ([]model.ResourceClaim, error) {
	var list model.ResourceClaimList
	err := c.request(ctx, http.MethodGet, "/apis/resource.k8s.io/v1/resourceclaims", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListResourceSlices(ctx context.Context) ([]model.ResourceSlice, error) {
	var list model.ResourceSliceList
	err := c.request(ctx, http.MethodGet, "/apis/resource.k8s.io/v1/resourceslices", nil, &list, "")
	return list.Items, err
}

func (c *Client) GetVolumeSnapshot(ctx context.Context, ns, name string) (model.VolumeSnapshot, error) {
	var snap model.VolumeSnapshot
	path := fmt.Sprintf("/apis/snapshot.storage.k8s.io/v1/namespaces/%s/volumesnapshots/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodGet, path, nil, &snap, "")
	return snap, err
}

// DeleteVolumeSnapshot deletes a CSI VolumeSnapshot (and, with a Delete
// deletion policy, its snapshot content). A MachineSnapshot does not own the
// VolumeSnapshots it creates, so deleting it alone leaves them behind.
func (c *Client) DeleteVolumeSnapshot(ctx context.Context, ns, name string) error {
	path := fmt.Sprintf("/apis/snapshot.storage.k8s.io/v1/namespaces/%s/volumesnapshots/%s", url.PathEscape(ns), url.PathEscape(name))
	return c.request(ctx, http.MethodDelete, path, nil, nil, "")
}

func (c *Client) CreateVolumeSnapshot(ctx context.Context, ns string, snap model.VolumeSnapshot) (model.VolumeSnapshot, error) {
	var out model.VolumeSnapshot
	path := fmt.Sprintf("/apis/snapshot.storage.k8s.io/v1/namespaces/%s/volumesnapshots", url.PathEscape(ns))
	err := c.request(ctx, http.MethodPost, path, snap, &out, "")
	return out, err
}

func (c *Client) ListMachineQuotas(ctx context.Context) ([]model.MachineQuota, error) {
	var list model.MachineQuotaList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinequotas", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineQuotasNamespace(ctx context.Context, ns string) ([]model.MachineQuota, error) {
	var list model.MachineQuotaList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinequotas"), nil, &list, "")
	return list.Items, err
}

func (c *Client) PatchMachineQuotaStatus(ctx context.Context, ns, name string, status model.MachineQuotaStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinequotas", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) GetMachineQuota(ctx context.Context, ns, name string) (model.MachineQuota, error) {
	var q model.MachineQuota
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinequotas", name), nil, &q, "")
	return q, err
}

func (c *Client) DeleteMachineQuota(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinequotas", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) CreateMachineQuota(ctx context.Context, ns string, q model.MachineQuota) (model.MachineQuota, error) {
	var out model.MachineQuota
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinequotas"), q, &out, "")
	return out, err
}

func (c *Client) PatchMachineQuota(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinequotas", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineSets(ctx context.Context) ([]model.MachineSet, error) {
	var list model.MachineSetList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinesets", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineSetsNamespace(ctx context.Context, ns string) ([]model.MachineSet, error) {
	var list model.MachineSetList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinesets"), nil, &list, "")
	return list.Items, err
}

func (c *Client) PatchMachineSetStatus(ctx context.Context, ns, name string, status model.MachineSetStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesets", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) GetMachineSet(ctx context.Context, ns, name string) (model.MachineSet, error) {
	var s model.MachineSet
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinesets", name), nil, &s, "")
	return s, err
}

func (c *Client) DeleteMachineSet(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinesets", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) CreateMachineSet(ctx context.Context, ns string, s model.MachineSet) (model.MachineSet, error) {
	var out model.MachineSet
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinesets"), s, &out, "")
	return out, err
}

func (c *Client) PatchMachineSet(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesets", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) ListMachinePools(ctx context.Context) ([]model.MachinePool, error) {
	var list model.MachinePoolList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinepools", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachinePoolsNamespace(ctx context.Context, ns string) ([]model.MachinePool, error) {
	var list model.MachinePoolList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinepools"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachinePool(ctx context.Context, ns, name string) (model.MachinePool, error) {
	var p model.MachinePool
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinepools", name), nil, &p, "")
	return p, err
}

func (c *Client) CreateMachinePool(ctx context.Context, ns string, p model.MachinePool) (model.MachinePool, error) {
	var out model.MachinePool
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinepools"), p, &out, "")
	return out, err
}

func (c *Client) DeleteMachinePool(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinepools", name), nil, nil, "")
}

func (c *Client) PatchMachinePool(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinepools", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachinePoolStatus(ctx context.Context, ns, name string, status model.MachinePoolStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinepools", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineClaims(ctx context.Context) ([]model.MachineClaim, error) {
	var list model.MachineClaimList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineclaims", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineClaimsNamespace(ctx context.Context, ns string) ([]model.MachineClaim, error) {
	var list model.MachineClaimList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machineclaims"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineClaim(ctx context.Context, ns, name string) (model.MachineClaim, error) {
	var mc model.MachineClaim
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machineclaims", name), nil, &mc, "")
	return mc, err
}

func (c *Client) CreateMachineClaim(ctx context.Context, ns string, mc model.MachineClaim) (model.MachineClaim, error) {
	var out model.MachineClaim
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machineclaims"), mc, &out, "")
	return out, err
}

func (c *Client) DeleteMachineClaim(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machineclaims", name), nil, nil, "")
}

func (c *Client) PatchMachineClaim(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machineclaims", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachineClaimStatus(ctx context.Context, ns, name string, status model.MachineClaimStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machineclaims", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineBackups(ctx context.Context) ([]model.MachineBackup, error) {
	var list model.MachineBackupList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinebackups", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineBackupsNamespace(ctx context.Context, ns string) ([]model.MachineBackup, error) {
	var list model.MachineBackupList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinebackups"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineBackup(ctx context.Context, ns, name string) (model.MachineBackup, error) {
	var b model.MachineBackup
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinebackups", name), nil, &b, "")
	return b, err
}

func (c *Client) CreateMachineBackup(ctx context.Context, ns string, b model.MachineBackup) (model.MachineBackup, error) {
	var out model.MachineBackup
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinebackups"), b, &out, "")
	return out, err
}

func (c *Client) DeleteMachineBackup(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinebackups", name), nil, nil, "")
}

func (c *Client) PatchMachineBackup(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinebackups", name), patch, nil, "application/merge-patch+json")
}

// PatchMachineBackupStatus merge-patches only the given status keys, since
// the controller and kairon-node each own different ones.
func (c *Client) PatchMachineBackupStatus(ctx context.Context, ns, name string, status map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinebackups", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineBackupRestores(ctx context.Context) ([]model.MachineBackupRestore, error) {
	var list model.MachineBackupRestoreList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinebackuprestores", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineBackupRestoresNamespace(ctx context.Context, ns string) ([]model.MachineBackupRestore, error) {
	var list model.MachineBackupRestoreList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinebackuprestores"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineBackupRestore(ctx context.Context, ns, name string) (model.MachineBackupRestore, error) {
	var r model.MachineBackupRestore
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinebackuprestores", name), nil, &r, "")
	return r, err
}

func (c *Client) CreateMachineBackupRestore(ctx context.Context, ns string, r model.MachineBackupRestore) (model.MachineBackupRestore, error) {
	var out model.MachineBackupRestore
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinebackuprestores"), r, &out, "")
	return out, err
}

func (c *Client) DeleteMachineBackupRestore(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinebackuprestores", name), nil, nil, "")
}

// PatchMachineBackupRestoreStatus merge-patches only the given status keys.
func (c *Client) PatchMachineBackupRestoreStatus(ctx context.Context, ns, name string, status map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinebackuprestores", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

// ListMachineImages lists the cluster-scoped MachineImage catalog.
func (c *Client) ListMachineImages(ctx context.Context) ([]model.MachineImage, error) {
	var list model.MachineImageList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineimages", nil, &list, "")
	return list.Items, err
}

// ListNodeTopologies lists every node's cluster-scoped hardware graph.
func (c *Client) ListNodeTopologies(ctx context.Context) ([]model.NodeTopology, error) {
	var list model.NodeTopologyList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/nodetopologies", nil, &list, "")
	return list.Items, err
}

func (c *Client) GetNodeTopology(ctx context.Context, name string) (model.NodeTopology, error) {
	var t model.NodeTopology
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/nodetopologies/"+url.PathEscape(name), nil, &t, "")
	return t, err
}

func (c *Client) CreateNodeTopology(ctx context.Context, t model.NodeTopology) (model.NodeTopology, error) {
	var out model.NodeTopology
	err := c.request(ctx, http.MethodPost, "/apis/kairon.zyvor.dev/v1/nodetopologies", t, &out, "")
	return out, err
}

func (c *Client) PatchNodeTopologyStatus(ctx context.Context, name string, status model.NodeTopologyStatus) error {
	return c.request(ctx, http.MethodPatch, "/apis/kairon.zyvor.dev/v1/nodetopologies/"+url.PathEscape(name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

// ListMachineAcceleratorProfiles lists the cluster-scoped accelerator
// profiles.
func (c *Client) ListMachineAcceleratorProfiles(ctx context.Context) ([]model.MachineAcceleratorProfile, error) {
	var list model.MachineAcceleratorProfileList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineacceleratorprofiles", nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineAcceleratorProfile(ctx context.Context, name string) (model.MachineAcceleratorProfile, error) {
	var p model.MachineAcceleratorProfile
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineacceleratorprofiles/"+url.PathEscape(name), nil, &p, "")
	return p, err
}

// PatchMachineAcceleratorProfileStatus replaces the whole status, so a node
// that no longer fits drops out of status.nodes.
func (c *Client) PatchMachineAcceleratorProfileStatus(ctx context.Context, name string, status model.MachineAcceleratorProfileStatus) error {
	patch := []map[string]any{{"op": "add", "path": "/status", "value": status}}
	return c.request(ctx, http.MethodPatch, "/apis/kairon.zyvor.dev/v1/machineacceleratorprofiles/"+url.PathEscape(name)+"/status", patch, nil, "application/json-patch+json")
}

func (c *Client) ListMachineInstanceTypes(ctx context.Context) ([]model.MachineInstanceType, error) {
	var list model.MachineInstanceTypeList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineinstancetypes", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineInstanceTypesNamespace(ctx context.Context, ns string) ([]model.MachineInstanceType, error) {
	var list model.MachineInstanceTypeList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machineinstancetypes"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineInstanceType(ctx context.Context, ns, name string) (model.MachineInstanceType, error) {
	var it model.MachineInstanceType
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machineinstancetypes", name), nil, &it, "")
	return it, err
}

func (c *Client) DeleteMachineInstanceType(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machineinstancetypes", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) CreateMachineInstanceType(ctx context.Context, ns string, it model.MachineInstanceType) (model.MachineInstanceType, error) {
	var out model.MachineInstanceType
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machineinstancetypes"), it, &out, "")
	return out, err
}

func (c *Client) ListMachineClusterInstanceTypes(ctx context.Context) ([]model.MachineClusterInstanceType, error) {
	var list model.MachineClusterInstanceTypeList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineclusterinstancetypes", nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineClusterInstanceType(ctx context.Context, name string) (model.MachineClusterInstanceType, error) {
	var it model.MachineClusterInstanceType
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineclusterinstancetypes/"+url.PathEscape(name), nil, &it, "")
	return it, err
}

func (c *Client) DeleteMachineClusterInstanceType(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, "/apis/kairon.zyvor.dev/v1/machineclusterinstancetypes/"+url.PathEscape(name), nil, nil, "")
}

func (c *Client) ListMachinePreferences(ctx context.Context) ([]model.MachinePreference, error) {
	var list model.MachinePreferenceList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinepreferences", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachinePreferencesNamespace(ctx context.Context, ns string) ([]model.MachinePreference, error) {
	var list model.MachinePreferenceList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinepreferences"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachinePreference(ctx context.Context, ns, name string) (model.MachinePreference, error) {
	var p model.MachinePreference
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinepreferences", name), nil, &p, "")
	return p, err
}

func (c *Client) DeleteMachinePreference(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinepreferences", name), nil, nil, "")
}

func (c *Client) ListMachineClusterPreferences(ctx context.Context) ([]model.MachineClusterPreference, error) {
	var list model.MachineClusterPreferenceList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineclusterpreferences", nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineClusterPreference(ctx context.Context, name string) (model.MachineClusterPreference, error) {
	var p model.MachineClusterPreference
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineclusterpreferences/"+url.PathEscape(name), nil, &p, "")
	return p, err
}

func (c *Client) DeleteMachineClusterPreference(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, "/apis/kairon.zyvor.dev/v1/machineclusterpreferences/"+url.PathEscape(name), nil, nil, "")
}

func (c *Client) ListMigrationPolicies(ctx context.Context) ([]model.MigrationPolicy, error) {
	var list model.MigrationPolicyList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/migrationpolicies", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMigrationPoliciesNamespace(ctx context.Context, ns string) ([]model.MigrationPolicy, error) {
	var list model.MigrationPolicyList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "migrationpolicies"), nil, &list, "")
	return list.Items, err
}

func (c *Client) PatchMigrationPolicyStatus(ctx context.Context, ns, name string, status model.MigrationPolicyStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "migrationpolicies", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) GetMigrationPolicy(ctx context.Context, ns, name string) (model.MigrationPolicy, error) {
	var p model.MigrationPolicy
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "migrationpolicies", name), nil, &p, "")
	return p, err
}

func (c *Client) DeleteMigrationPolicy(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "migrationpolicies", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) CreateMigrationPolicy(ctx context.Context, ns string, p model.MigrationPolicy) (model.MigrationPolicy, error) {
	var out model.MigrationPolicy
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "migrationpolicies"), p, &out, "")
	return out, err
}

func (c *Client) PatchMigrationPolicy(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "migrationpolicies", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) ListVirtualNetworks(ctx context.Context) ([]model.VirtualNetwork, error) {
	var list model.VirtualNetworkList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/virtualnetworks", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListVirtualNetworksNamespace(ctx context.Context, ns string) ([]model.VirtualNetwork, error) {
	var list model.VirtualNetworkList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "virtualnetworks"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetVirtualNetwork(ctx context.Context, ns, name string) (model.VirtualNetwork, error) {
	var v model.VirtualNetwork
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "virtualnetworks", name), nil, &v, "")
	return v, err
}

func (c *Client) CreateVirtualNetwork(ctx context.Context, ns string, v model.VirtualNetwork) (model.VirtualNetwork, error) {
	var out model.VirtualNetwork
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "virtualnetworks"), v, &out, "")
	return out, err
}

func (c *Client) DeleteVirtualNetwork(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "virtualnetworks", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) PatchVirtualNetwork(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "virtualnetworks", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchVirtualNetworkStatus(ctx context.Context, ns, name string, status model.VirtualNetworkStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "virtualnetworks", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineBackupPolicies(ctx context.Context) ([]model.MachineBackupPolicy, error) {
	var list model.MachineBackupPolicyList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinebackuppolicies", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineBackupPoliciesNamespace(ctx context.Context, ns string) ([]model.MachineBackupPolicy, error) {
	var list model.MachineBackupPolicyList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinebackuppolicies"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineBackupPolicy(ctx context.Context, ns, name string) (model.MachineBackupPolicy, error) {
	var p model.MachineBackupPolicy
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinebackuppolicies", name), nil, &p, "")
	return p, err
}

func (c *Client) CreateMachineBackupPolicy(ctx context.Context, ns string, p model.MachineBackupPolicy) (model.MachineBackupPolicy, error) {
	var out model.MachineBackupPolicy
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinebackuppolicies"), p, &out, "")
	return out, err
}

func (c *Client) DeleteMachineBackupPolicy(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinebackuppolicies", name), nil, nil, "")
}

func (c *Client) PatchMachineBackupPolicy(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinebackuppolicies", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachineBackupPolicyStatus(ctx context.Context, ns, name string, status model.MachineBackupPolicyStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinebackuppolicies", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineProtectionPlans(ctx context.Context) ([]model.MachineProtectionPlan, error) {
	var list model.MachineProtectionPlanList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machineprotectionplans", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineProtectionPlansNamespace(ctx context.Context, ns string) ([]model.MachineProtectionPlan, error) {
	var list model.MachineProtectionPlanList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machineprotectionplans"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineProtectionPlan(ctx context.Context, ns, name string) (model.MachineProtectionPlan, error) {
	var p model.MachineProtectionPlan
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machineprotectionplans", name), nil, &p, "")
	return p, err
}

func (c *Client) PatchMachineProtectionPlan(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machineprotectionplans", name), patch, nil, "application/merge-patch+json")
}

func dnsEndpointPath(ns, name string) string {
	p := fmt.Sprintf("/apis/externaldns.k8s.io/v1alpha1/namespaces/%s/dnsendpoints", url.PathEscape(ns))
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (c *Client) GetDNSEndpoint(ctx context.Context, ns, name string) (model.DNSEndpoint, error) {
	var e model.DNSEndpoint
	err := c.request(ctx, http.MethodGet, dnsEndpointPath(ns, name), nil, &e, "")
	return e, err
}

func (c *Client) CreateDNSEndpoint(ctx context.Context, ns string, e model.DNSEndpoint) error {
	return c.request(ctx, http.MethodPost, dnsEndpointPath(ns, ""), e, nil, "")
}

func (c *Client) PatchDNSEndpoint(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, dnsEndpointPath(ns, name), patch, nil, "application/merge-patch+json")
}

func (c *Client) DeleteDNSEndpoint(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, dnsEndpointPath(ns, name), nil, nil, "")
}

func (c *Client) ListMacNetworks(ctx context.Context) ([]model.MacNetwork, error) {
	var list model.MacNetworkList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/macnetworks", nil, &list, "")
	return list.Items, err
}

func (c *Client) PatchMacNetworkStatus(ctx context.Context, ns, name string, status model.MacNetworkStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "macnetworks", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

// PatchMachineProtectionPlanStatus replaces the whole status (JSON patch):
// a merge patch would keep fields a new run leaves empty, such as the
// previous run's restored Machines.
func (c *Client) PatchMachineProtectionPlanStatus(ctx context.Context, ns, name string, status model.MachineProtectionPlanStatus) error {
	patch := []map[string]any{{"op": "add", "path": "/status", "value": status}}
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machineprotectionplans", name)+"/status", patch, nil, "application/json-patch+json")
}

func (c *Client) ListMachineSnapshotSchedules(ctx context.Context) ([]model.MachineSnapshotSchedule, error) {
	var list model.MachineSnapshotScheduleList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineSnapshotSchedulesNamespace(ctx context.Context, ns string) ([]model.MachineSnapshotSchedule, error) {
	var list model.MachineSnapshotScheduleList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinesnapshotschedules"), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetMachineSnapshotSchedule(ctx context.Context, ns, name string) (model.MachineSnapshotSchedule, error) {
	var s model.MachineSnapshotSchedule
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinesnapshotschedules", name), nil, &s, "")
	return s, err
}

func (c *Client) DeleteMachineSnapshotSchedule(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinesnapshotschedules", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) CreateMachineSnapshotSchedule(ctx context.Context, ns string, s model.MachineSnapshotSchedule) (model.MachineSnapshotSchedule, error) {
	var out model.MachineSnapshotSchedule
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinesnapshotschedules"), s, &out, "")
	return out, err
}

func (c *Client) PatchMachineSnapshotSchedule(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesnapshotschedules", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) PatchMachineSnapshotScheduleStatus(ctx context.Context, ns, name string, status model.MachineSnapshotScheduleStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesnapshotschedules", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

// SetMachineSnapshotScheduleError merge-patches only status.lastRunError.
func (c *Client) SetMachineSnapshotScheduleError(ctx context.Context, ns, name, msg string) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinesnapshotschedules", name)+"/status", map[string]any{"status": map[string]any{"lastRunError": msg}}, nil, "application/merge-patch+json")
}

func (c *Client) ListMachineDisruptionBudgets(ctx context.Context) ([]model.MachineDisruptionBudget, error) {
	var list model.MachineDisruptionBudgetList
	err := c.request(ctx, http.MethodGet, "/apis/kairon.zyvor.dev/v1/machinedisruptionbudgets", nil, &list, "")
	return list.Items, err
}

func (c *Client) ListMachineDisruptionBudgetsNamespace(ctx context.Context, ns string) ([]model.MachineDisruptionBudget, error) {
	var list model.MachineDisruptionBudgetList
	err := c.request(ctx, http.MethodGet, namespacePath(ns, "machinedisruptionbudgets"), nil, &list, "")
	return list.Items, err
}

func (c *Client) PatchMachineDisruptionBudgetStatus(ctx context.Context, ns, name string, status model.MachineDisruptionBudgetStatus) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinedisruptionbudgets", name)+"/status", map[string]any{"status": status}, nil, "application/merge-patch+json")
}

func (c *Client) GetMachineDisruptionBudget(ctx context.Context, ns, name string) (model.MachineDisruptionBudget, error) {
	var b model.MachineDisruptionBudget
	err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, "machinedisruptionbudgets", name), nil, &b, "")
	return b, err
}

func (c *Client) DeleteMachineDisruptionBudget(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, namespacedObjectPath(ns, "machinedisruptionbudgets", name), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}, nil, "")
}

func (c *Client) CreateMachineDisruptionBudget(ctx context.Context, ns string, b model.MachineDisruptionBudget) (model.MachineDisruptionBudget, error) {
	var out model.MachineDisruptionBudget
	err := c.request(ctx, http.MethodPost, namespacePath(ns, "machinedisruptionbudgets"), b, &out, "")
	return out, err
}

func (c *Client) PatchMachineDisruptionBudget(ctx context.Context, ns, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, "machinedisruptionbudgets", name), patch, nil, "application/merge-patch+json")
}

func (c *Client) CreatePersistentVolumeClaim(ctx context.Context, ns string, pvc model.PersistentVolumeClaim) (model.PersistentVolumeClaim, error) {
	var out model.PersistentVolumeClaim
	err := c.request(ctx, http.MethodPost, fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims", url.PathEscape(ns)), pvc, &out, "")
	return out, err
}

func (c *Client) GetPersistentVolumeClaim(ctx context.Context, ns, name string) (model.PersistentVolumeClaim, error) {
	var pvc model.PersistentVolumeClaim
	path := fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodGet, path, nil, &pvc, "")
	return pvc, err
}

func (c *Client) GetPersistentVolume(ctx context.Context, name string) (model.PersistentVolume, error) {
	var pv model.PersistentVolume
	path := "/api/v1/persistentvolumes/" + url.PathEscape(name)
	err := c.request(ctx, http.MethodGet, path, nil, &pv, "")
	return pv, err
}

func (c *Client) GetCSIDriver(ctx context.Context, name string) (model.CSIDriver, error) {
	var d model.CSIDriver
	err := c.request(ctx, http.MethodGet, "/apis/storage.k8s.io/v1/csidrivers/"+url.PathEscape(name), nil, &d, "")
	return d, err
}

func (c *Client) GetVolumeAttachment(ctx context.Context, name string) (model.VolumeAttachment, error) {
	var va model.VolumeAttachment
	err := c.request(ctx, http.MethodGet, "/apis/storage.k8s.io/v1/volumeattachments/"+url.PathEscape(name), nil, &va, "")
	return va, err
}

func (c *Client) CreateVolumeAttachment(ctx context.Context, va model.VolumeAttachment) (model.VolumeAttachment, error) {
	va.APIVersion, va.Kind = "storage.k8s.io/v1", "VolumeAttachment"
	var out model.VolumeAttachment
	err := c.request(ctx, http.MethodPost, "/apis/storage.k8s.io/v1/volumeattachments", va, &out, "")
	return out, err
}

func (c *Client) DeleteVolumeAttachment(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, "/apis/storage.k8s.io/v1/volumeattachments/"+url.PathEscape(name), nil, nil, "")
}

// PatchSecretStringData merge-patches a core/v1 Secret's stringData field
// -- the API server base64-encodes each value into .data itself, so
// callers never handle encoding. Used by internal/uiapi to persist a
// runtime password change back into the kairon-ui-users Secret (see
// uiapi.Server.persistUsers); there is deliberately no generic PatchSecret
// or GetSecret here, mirroring this client's existing one-method-per-intent
// shape rather than a passthrough.
func (c *Client) PatchSecretStringData(ctx context.Context, ns, name string, stringData map[string]string) error {
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", url.PathEscape(ns), url.PathEscape(name))
	return c.request(ctx, http.MethodPatch, path, map[string]any{"stringData": stringData}, nil, "application/merge-patch+json")
}

// GetSecret reads back a core/v1 Secret -- used by internal/uiapi to
// notice a password change another kairon-ui replica made and persisted
// (see PatchSecretStringData), since Server.Users is otherwise only
// loaded once at startup.
func (c *Client) GetSecret(ctx context.Context, ns, name string) (model.Secret, error) {
	var s model.Secret
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodGet, path, nil, &s, "")
	return s, err
}

// GetConfigMap and PatchConfigMapData back internal/uiapi's cross-replica
// session/lockout/console-ticket state (see internal/uiapi/sharedstate.go).
// PatchConfigMapData's data values are `any` rather than `string` so a
// caller can pass a nil value for a key to delete it -- an
// application/merge-patch+json body with a null value removes that key
// (RFC 7386), leaving every other key in .data untouched. That per-key
// merge-patch shape is exactly why concurrent writers touching different
// keys never conflict: there is no whole-object read-modify-write here.
func (c *Client) GetConfigMap(ctx context.Context, ns, name string) (model.ConfigMap, error) {
	var cm model.ConfigMap
	path := fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodGet, path, nil, &cm, "")
	return cm, err
}

func (c *Client) CreateConfigMap(ctx context.Context, cm model.ConfigMap) error {
	cm.APIVersion, cm.Kind = "v1", "ConfigMap"
	path := fmt.Sprintf("/api/v1/namespaces/%s/configmaps", url.PathEscape(cm.Metadata.Namespace))
	return c.request(ctx, http.MethodPost, path, cm, nil, "")
}

func (c *Client) PatchConfigMapData(ctx context.Context, ns, name string, data map[string]any) error {
	path := fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", url.PathEscape(ns), url.PathEscape(name))
	return c.request(ctx, http.MethodPatch, path, map[string]any{"data": data}, nil, "application/merge-patch+json")
}

func (c *Client) ListNodes(ctx context.Context) ([]model.Node, error) {
	var list model.NodeList
	err := c.request(ctx, http.MethodGet, "/api/v1/nodes", nil, &list, "")
	return list.Items, err
}

// GetNode fetches a single real Kubernetes Node by name -- the singular
// counterpart to ListNodes above, added alongside `kaironctl get/describe
// node` (see internal/kaironctl). Nodes are cluster-scoped, hence no
// namespace segment in the path, same as ListNodes' own "/api/v1/nodes".
func (c *Client) GetNode(ctx context.Context, name string) (model.Node, error) {
	var n model.Node
	err := c.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/nodes/%s", url.PathEscape(name)), nil, &n, "")
	return n, err
}

// PatchNode merge-patches a Node, e.g. kairon-node publishing its own
// capability labels.
func (c *Client) PatchNode(ctx context.Context, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%s", url.PathEscape(name)), patch, nil, "application/merge-patch+json")
}

// CreateNode POSTs a Node object. kairon-node uses it on a Mac, where there is no kubelet to register the node.
func (c *Client) CreateNode(ctx context.Context, node map[string]any) error {
	return c.request(ctx, http.MethodPost, "/api/v1/nodes", node, nil, "")
}

// PatchNodeStatus merge-patches a Node's status subresource (capacity, allocatable, the Ready heartbeat).
func (c *Client) PatchNodeStatus(ctx context.Context, name string, patch map[string]any) error {
	return c.request(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%s/status", url.PathEscape(name)), patch, nil, "application/merge-patch+json")
}

// PatchNodeCondition upserts one Node status condition. A strategic merge
// patch merges conditions by type, so the kubelet's own conditions stay; a
// merge patch would replace the whole list.
func (c *Client) PatchNodeCondition(ctx context.Context, name string, cond model.NodeCondition) error {
	patch := map[string]any{"status": map[string]any{"conditions": []model.NodeCondition{cond}}}
	return c.request(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%s/status", url.PathEscape(name)), patch, nil, "application/strategic-merge-patch+json")
}

func leasePath(ns, name string) string {
	return fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", url.PathEscape(ns), url.PathEscape(name))
}

// GetLease, CreateLease, and UpdateLease back kairon-controller's own
// leader election (see internal/leaderelection) -- a coordination.k8s.io/v1
// Lease is a core Kubernetes API type, not one of Kairon's own
// kairon.zyvor.dev CRDs, hence the separate leasePath helper rather than
// namespacePath/namespacedObjectPath above.
func (c *Client) GetLease(ctx context.Context, ns, name string) (model.Lease, error) {
	var l model.Lease
	err := c.request(ctx, http.MethodGet, leasePath(ns, name), nil, &l, "")
	return l, err
}

func (c *Client) CreateLease(ctx context.Context, ns string, lease model.Lease) (model.Lease, error) {
	var out model.Lease
	path := fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases", url.PathEscape(ns))
	err := c.request(ctx, http.MethodPost, path, lease, &out, "")
	return out, err
}

// UpdateLease requires lease.Metadata.ResourceVersion to be the version
// last read. The API server rejects the write with HTTP 409 (surfaced as
// an *APIError that IsConflict recognizes) if the Lease changed since --
// exactly how internal/leaderelection detects it lost a race to renew or
// take over the lease, without needing a separate compare-and-swap
// primitive of its own.
func (c *Client) UpdateLease(ctx context.Context, ns string, lease model.Lease) (model.Lease, error) {
	var out model.Lease
	err := c.request(ctx, http.MethodPut, leasePath(ns, lease.Metadata.Name), lease, &out, "")
	return out, err
}

// RecordEvent posts a real core/v1 Event for involvedObject (a Machine, in
// every caller today), so it shows up in `kubectl describe machine`'s
// Events tab -- not a Kairon-specific notification channel of its own.
// Used by internal/controller's admitQuota blocker branch to surface a
// quota-blocked Machine's already-computed status.Message as a real Event
// too, not just a status field an operator has to already know to read.
// metadata.name is generated the same way client-go's own EventBroadcaster
// names one it creates -- "<involvedObjectName>.<hex nanosecond
// timestamp>" -- so repeated calls for the same object never collide, and
// no separate uniqueness/dedup logic is needed here. There is deliberately
// no per-Event aggregation/counting (Count always 1): a Machine stuck
// blocked for hours produces a proportionally noisy Event history, the
// same honest first-cut limit as this feature's rest.
func (c *Client) RecordEvent(ctx context.Context, namespace, involvedObjectKind, involvedObjectName, involvedObjectUID, reason, message, eventType string) error {
	now := time.Now().UTC()
	ev := model.Event{
		TypeMeta: model.TypeMeta{APIVersion: "v1", Kind: "Event"},
		Metadata: model.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", involvedObjectName, now.UnixNano()),
			Namespace: namespace,
		},
		InvolvedObject: model.ObjectReference{
			Kind:      involvedObjectKind,
			Namespace: namespace,
			Name:      involvedObjectName,
			UID:       involvedObjectUID,
		},
		Reason:         reason,
		Message:        message,
		Source:         model.EventSource{Component: "kairon-controller"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Type:           eventType,
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/events", url.PathEscape(namespace))
	return c.request(ctx, http.MethodPost, path, ev, nil, "")
}

// ListEventsFor returns core/v1 Events whose involvedObject is kind/name
// in namespace, oldest first as the apiserver returns them.
func (c *Client) ListEventsFor(ctx context.Context, namespace, kind, name string) ([]model.Event, error) {
	var list struct {
		Items []model.Event `json:"items"`
	}
	selector := "involvedObject.kind=" + kind + ",involvedObject.name=" + name
	path := fmt.Sprintf("/api/v1/namespaces/%s/events?fieldSelector=%s", url.PathEscape(namespace), url.QueryEscape(selector))
	err := c.request(ctx, http.MethodGet, path, nil, &list, "")
	return list.Items, err
}

// SubjectAccessReview posts a built-in authorization.k8s.io/v1
// SubjectAccessReview and returns its Status -- the caller's own
// ServiceAccount (this Client's Token) must hold "create" on
// subjectaccessreviews.authorization.k8s.io for this to succeed. See
// model.SubjectAccessReview's own doc comment for why this is a real
// SubjectAccessReview, not a SelfSubjectAccessReview.
func (c *Client) SubjectAccessReview(ctx context.Context, sar model.SubjectAccessReview) (model.SubjectAccessReviewStatus, error) {
	sar.TypeMeta = model.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SubjectAccessReview"}
	var out model.SubjectAccessReview
	err := c.request(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/subjectaccessreviews", sar, &out, "")
	if err != nil {
		return model.SubjectAccessReviewStatus{}, err
	}
	return out.Status, nil
}

// PodSummary is the slice of a core/v1 Pod the dashboard shows.
type PodSummary struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	Node       string            `json:"node,omitempty"`
	IP         string            `json:"ip,omitempty"`
	Ready      bool              `json:"ready"`
	Restarts   int               `json:"restarts"`
	Labels     map[string]string `json:"labels,omitempty"`
	StartTime  time.Time         `json:"startTime,omitempty"`
	Containers int               `json:"containers"`
}

// ListPods lists pods in every namespace, or in namespace when non-empty.
func (c *Client) ListPods(ctx context.Context, namespace string) ([]PodSummary, error) {
	path := "/api/v1/pods"
	if namespace != "" {
		path = fmt.Sprintf("/api/v1/namespaces/%s/pods", url.PathEscape(namespace))
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				NodeName   string            `json:"nodeName"`
				Containers []json.RawMessage `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase             string    `json:"phase"`
				PodIP             string    `json:"podIP"`
				StartTime         time.Time `json:"startTime"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.request(ctx, http.MethodGet, path, nil, &list, ""); err != nil {
		return nil, err
	}
	out := make([]PodSummary, 0, len(list.Items))
	for _, it := range list.Items {
		p := PodSummary{
			Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Phase: it.Status.Phase,
			Node: it.Spec.NodeName, IP: it.Status.PodIP, Labels: it.Metadata.Labels,
			StartTime: it.Status.StartTime, Containers: len(it.Spec.Containers),
			Ready: len(it.Status.ContainerStatuses) > 0,
		}
		for _, cs := range it.Status.ContainerStatuses {
			p.Restarts += cs.RestartCount
			if !cs.Ready {
				p.Ready = false
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// DeletePersistentVolumeClaim deletes a PVC; a missing claim is reported as
// a not-found error the caller can ignore with IsNotFound.
func (c *Client) DeletePersistentVolumeClaim(ctx context.Context, ns, name string) error {
	path := fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", url.PathEscape(ns), url.PathEscape(name))
	return c.request(ctx, http.MethodDelete, path, nil, nil, "")
}

// VolumeSnapshotClass is the part of a snapshot.storage.k8s.io class the
// self-test needs.
type VolumeSnapshotClass struct {
	Name    string
	Driver  string
	Default bool
}

// StorageClass is the part of a storage.k8s.io class the self-test needs.
type StorageClass struct {
	Name        string
	Provisioner string
	Immediate   bool // false when volumeBindingMode is WaitForFirstConsumer
	Default     bool
}

// ListVolumeSnapshotClasses lists the cluster's VolumeSnapshotClasses. A
// cluster without the snapshot CRDs returns no classes and no error.
func (c *Client) ListVolumeSnapshotClasses(ctx context.Context) ([]VolumeSnapshotClass, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Driver string `json:"driver"`
		} `json:"items"`
	}
	if err := c.request(ctx, http.MethodGet, "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses", nil, &list, ""); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]VolumeSnapshotClass, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, VolumeSnapshotClass{
			Name: it.Metadata.Name, Driver: it.Driver,
			Default: it.Metadata.Annotations["snapshot.storage.kubernetes.io/is-default-class"] == "true",
		})
	}
	return out, nil
}

// ListStorageClasses lists the cluster's StorageClasses.
func (c *Client) ListStorageClasses(ctx context.Context) ([]StorageClass, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Provisioner       string `json:"provisioner"`
			VolumeBindingMode string `json:"volumeBindingMode"`
		} `json:"items"`
	}
	if err := c.request(ctx, http.MethodGet, "/apis/storage.k8s.io/v1/storageclasses", nil, &list, ""); err != nil {
		return nil, err
	}
	out := make([]StorageClass, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, StorageClass{
			Name: it.Metadata.Name, Provisioner: it.Provisioner,
			Immediate: it.VolumeBindingMode != "WaitForFirstConsumer",
			Default:   it.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true",
		})
	}
	return out, nil
}

// SnapshotTarget is a VolumeSnapshotClass together with a StorageClass
// served by the same CSI driver, so a PVC provisioned from the StorageClass
// can be snapshotted with the class. The zero value means "no snapshot
// support".
type SnapshotTarget struct {
	SnapshotClass string
	StorageClass  string
	Driver        string
	// ImmediateBinding is false when the StorageClass waits for a consumer,
	// in which case an unattached test PVC would never bind.
	ImmediateBinding bool
}

// PickSnapshotTarget chooses the best (class, StorageClass) pair: the same
// driver, preferring Immediate binding (an unattached test PVC only binds
// with it), then default classes.
func PickSnapshotTarget(vcs []VolumeSnapshotClass, scs []StorageClass) SnapshotTarget {
	var best SnapshotTarget
	bestScore := -1
	for _, vc := range vcs {
		for _, sc := range scs {
			if sc.Provisioner != vc.Driver {
				continue
			}
			score := 0
			if sc.Immediate {
				score += 4
			}
			if sc.Default {
				score += 2
			}
			if vc.Default {
				score++
			}
			if score > bestScore {
				bestScore = score
				best = SnapshotTarget{SnapshotClass: vc.Name, StorageClass: sc.Name, Driver: vc.Driver, ImmediateBinding: sc.Immediate}
			}
		}
	}
	if best.SnapshotClass == "" && len(vcs) > 0 {
		// A class exists but no StorageClass uses its driver: still report
		// the class so the skip reason can name the driver.
		best = SnapshotTarget{SnapshotClass: vcs[0].Name, Driver: vcs[0].Driver}
	}
	return best
}
