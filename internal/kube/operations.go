// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// OperationObject reads only the resources supported by change plans. It keeps
// unknown fields intact when inspecting spec; mutation uses JSON Patch CAS.
func (c *Client) OperationObject(ctx context.Context, ns, kind, name string) (json.RawMessage, error) {
	path, err := operationPath(ns, kind, name)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	err = c.request(ctx, http.MethodGet, path, nil, &raw, "")
	return raw, err
}
func (c *Client) PatchOperationObject(ctx context.Context, ns, kind, name string, patch []map[string]any) error {
	path, err := operationPath(ns, kind, name)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPatch, path, patch, nil, "application/json-patch+json")
}
func operationPath(ns, kind, name string) (string, error) {
	switch kind {
	case "Machine":
		return namespacedObjectPath(ns, "machines", name), nil
	case "MachineNetworkPolicy":
		return namespacedObjectPath(ns, "machinenetworkpolicies", name), nil
	default:
		return "", fmt.Errorf("unsupported change-plan kind %q", kind)
	}
}

// SnapshotContentMode reads immutable CSI content metadata, never the opaque
// snapshot handle. Recovery refuses raw Block snapshots in this release.
func (c *Client) SnapshotContentMode(ctx context.Context, name string) (string, error) {
	var content struct {
		Spec struct {
			SourceVolumeMode string `json:"sourceVolumeMode"`
		}
	}
	err := c.request(ctx, http.MethodGet, "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/"+url.PathEscape(name), nil, &content, "")
	if err != nil {
		return "", err
	}
	if content.Spec.SourceVolumeMode == "" {
		return "Filesystem", nil
	}
	return content.Spec.SourceVolumeMode, nil
}
