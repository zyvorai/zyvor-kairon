// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// CiliumExternalWorkload is a minimal cluster-scoped cilium.io/v2 object.
// Kept as raw JSON maps in the controller; this typed view is only for
// status projection (no Cilium Go SDK).
type CiliumExternalWorkload struct {
	APIVersion string                       `json:"apiVersion"`
	Kind       string                       `json:"kind"`
	Metadata   ObjectMetaLite               `json:"metadata"`
	Spec       map[string]any               `json:"spec,omitempty"`
	Status     CiliumExternalWorkloadStatus `json:"status,omitempty"`
}

type ObjectMetaLite struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
}

type CiliumExternalWorkloadStatus struct {
	ID  uint32   `json:"id,omitempty"`
	IP  string   `json:"ip,omitempty"`
	IPs []string `json:"ips,omitempty"`
}

// CiliumNetworkPolicy is a namespaced cilium.io/v2 policy document.
type CiliumNetworkPolicy struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   ObjectMetaLite `json:"metadata"`
	Spec       map[string]any `json:"spec,omitempty"`
}

var dns1123Label = regexp.MustCompile(`[^a-z0-9-]`)

// ExternalWorkloadName returns a DNS-1123 subdomain for a Machine's CEW.
func ExternalWorkloadName(ns, name string) string {
	raw := strings.ToLower("kairon-" + ns + "-" + name)
	raw = dns1123Label.ReplaceAllString(raw, "-")
	raw = strings.Trim(raw, "-")
	if raw == "" {
		raw = "kairon-workload"
	}
	if len(raw) > 253 {
		raw = raw[:253]
		raw = strings.TrimRight(raw, "-")
	}
	return raw
}

func (c *Client) GetCiliumExternalWorkload(ctx context.Context, name string) (CiliumExternalWorkload, error) {
	var out CiliumExternalWorkload
	path := "/apis/cilium.io/v2/ciliumexternalworkloads/" + url.PathEscape(name)
	err := c.request(ctx, http.MethodGet, path, nil, &out, "")
	return out, err
}

func (c *Client) CreateCiliumExternalWorkload(ctx context.Context, cew CiliumExternalWorkload) (CiliumExternalWorkload, error) {
	var out CiliumExternalWorkload
	err := c.request(ctx, http.MethodPost, "/apis/cilium.io/v2/ciliumexternalworkloads", cew, &out, "")
	return out, err
}

func (c *Client) PatchCiliumExternalWorkload(ctx context.Context, name string, patch map[string]any) error {
	path := "/apis/cilium.io/v2/ciliumexternalworkloads/" + url.PathEscape(name)
	return c.request(ctx, http.MethodPatch, path, patch, nil, "application/merge-patch+json")
}

func (c *Client) DeleteCiliumExternalWorkload(ctx context.Context, name string) error {
	path := "/apis/cilium.io/v2/ciliumexternalworkloads/" + url.PathEscape(name)
	err := c.request(ctx, http.MethodDelete, path, nil, nil, "")
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) GetCiliumNetworkPolicy(ctx context.Context, ns, name string) (CiliumNetworkPolicy, error) {
	var out CiliumNetworkPolicy
	path := fmt.Sprintf("/apis/cilium.io/v2/namespaces/%s/ciliumnetworkpolicies/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodGet, path, nil, &out, "")
	return out, err
}

func (c *Client) CreateCiliumNetworkPolicy(ctx context.Context, ns string, cnp CiliumNetworkPolicy) (CiliumNetworkPolicy, error) {
	var out CiliumNetworkPolicy
	path := fmt.Sprintf("/apis/cilium.io/v2/namespaces/%s/ciliumnetworkpolicies", url.PathEscape(ns))
	err := c.request(ctx, http.MethodPost, path, cnp, &out, "")
	return out, err
}

func (c *Client) PatchCiliumNetworkPolicy(ctx context.Context, ns, name string, patch map[string]any) error {
	path := fmt.Sprintf("/apis/cilium.io/v2/namespaces/%s/ciliumnetworkpolicies/%s", url.PathEscape(ns), url.PathEscape(name))
	return c.request(ctx, http.MethodPatch, path, patch, nil, "application/merge-patch+json")
}

func (c *Client) DeleteCiliumNetworkPolicy(ctx context.Context, ns, name string) error {
	path := fmt.Sprintf("/apis/cilium.io/v2/namespaces/%s/ciliumnetworkpolicies/%s", url.PathEscape(ns), url.PathEscape(name))
	err := c.request(ctx, http.MethodDelete, path, nil, nil, "")
	if IsNotFound(err) {
		return nil
	}
	return err
}
