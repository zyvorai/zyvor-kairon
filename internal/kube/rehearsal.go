// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// DeleteRehearsalResource requires the UID observed immediately before delete;
// replacing an object with the same name can never make cleanup delete it.
func (c *Client) DeleteRehearsalResource(ctx context.Context, ns, resource, name, uid string) error {
	if uid == "" {
		return fmt.Errorf("cleanup requires a resource UID")
	}
	var path string
	switch resource {
	case "machines":
		path = namespacedObjectPath(ns, resource, name)
	case "persistentvolumeclaims":
		path = "/api/v1/namespaces/" + url.PathEscape(ns) + "/persistentvolumeclaims/" + url.PathEscape(name)
	case "volumesnapshots":
		path = "/apis/snapshot.storage.k8s.io/v1/namespaces/" + url.PathEscape(ns) + "/volumesnapshots/" + url.PathEscape(name)
	default:
		return fmt.Errorf("unsupported rehearsal cleanup resource %q", resource)
	}
	return c.request(ctx, http.MethodDelete, path, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": uid}}, nil, "")
}

// SetRehearsalReservation changes a node's annotations with a resourceVersion
// test. A bridge reservation cannot be acquired or released on a stale read.
func (c *Client) SetRehearsalReservation(ctx context.Context, node, version string, annotations map[string]string) error {
	if version == "" {
		return fmt.Errorf("node reservation requires resourceVersion")
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": version},
		{"op": "add", "path": "/metadata/annotations", "value": annotations},
	}
	return c.request(ctx, http.MethodPatch, "/api/v1/nodes/"+url.PathEscape(node), patch, nil, "application/json-patch+json")
}
