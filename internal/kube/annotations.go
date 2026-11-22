// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kube

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// GetAnnotations reads metadata.annotations of a namespaced kairon object
// (resource is the plural, e.g. "machines").
func (c *Client) GetAnnotations(ctx context.Context, resource, ns, name string) (map[string]string, error) {
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := c.request(ctx, http.MethodGet, namespacedObjectPath(ns, resource, name), nil, &obj, ""); err != nil {
		return nil, err
	}
	return obj.Metadata.Annotations, nil
}

// SetAnnotation merge-patches one annotation onto a namespaced kairon object.
func (c *Client) SetAnnotation(ctx context.Context, resource, ns, name, key, value string) error {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: value}}}
	return c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, resource, name), patch, nil, "application/merge-patch+json")
}

// ErrAnnotationChanged reports that ConsumeAnnotation found a different
// value (or none): someone else consumed or replaced it first.
var ErrAnnotationChanged = errors.New("annotation changed concurrently")

// ConsumeAnnotation removes key only while it still equals value, in one
// JSON patch (test + remove), so two callers can never both consume it.
func (c *Client) ConsumeAnnotation(ctx context.Context, resource, ns, name, key, value string) error {
	path := "/metadata/annotations/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
	patch := []map[string]any{
		{"op": "test", "path": path, "value": value},
		{"op": "remove", "path": path},
	}
	err := c.request(ctx, http.MethodPatch, namespacedObjectPath(ns, resource, name), patch, nil, "application/json-patch+json")
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
		return ErrAnnotationChanged
	}
	return err
}
