// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zyvorai/kairon/internal/model"
)

func fleetPath(ns, resource, name string) (string, error) {
	if _, ok := model.FleetKinds[resource]; !ok {
		return "", fmt.Errorf("unknown fleet resource %q", resource)
	}
	path := "/apis/" + model.FleetAPIVersion
	if ns != "" {
		path += "/namespaces/" + url.PathEscape(ns)
	}
	path += "/" + resource
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path, nil
}
func (c *Client) ListFleet(ctx context.Context, ns, resource string) ([]model.FleetResource, error) {
	path, err := fleetPath(ns, resource, "")
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []model.FleetResource `json:"items"`
	}
	err = c.request(ctx, http.MethodGet, path, nil, &out, "")
	return out.Items, err
}
func (c *Client) GetFleet(ctx context.Context, ns, resource, name string) (model.FleetResource, error) {
	path, err := fleetPath(ns, resource, name)
	if err != nil {
		return model.FleetResource{}, err
	}
	var out model.FleetResource
	err = c.request(ctx, http.MethodGet, path, nil, &out, "")
	return out, err
}
func (c *Client) CreateFleet(ctx context.Context, ns, resource string, in model.FleetResource) (model.FleetResource, error) {
	path, err := fleetPath(ns, resource, "")
	if err != nil {
		return model.FleetResource{}, err
	}
	var out model.FleetResource
	err = c.request(ctx, http.MethodPost, path, in, &out, "")
	return out, err
}
func (c *Client) PatchFleet(ctx context.Context, ns, resource, name string, patch map[string]any) error {
	path, err := fleetPath(ns, resource, name)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPatch, path, patch, nil, "application/merge-patch+json")
}
func (c *Client) PatchFleetStatus(ctx context.Context, resource string, in model.FleetResource, status model.FleetStatus) error {
	path, err := fleetPath(in.Metadata.Namespace, resource, in.Metadata.Name)
	if err != nil {
		return err
	}
	// resourceVersion makes accounting, allocation and cursor updates CAS writes.
	return c.request(ctx, http.MethodPatch, path+"/status", map[string]any{"metadata": map[string]any{"resourceVersion": in.Metadata.ResourceVersion}, "status": status}, nil, "application/merge-patch+json")
}
func (c *Client) DeleteFleet(ctx context.Context, ns, resource, name string) error {
	path, err := fleetPath(ns, resource, name)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodDelete, path, nil, nil, "")
}

func (c *Client) PatchFleetStatusFields(ctx context.Context, resource string, in model.FleetResource, fields map[string]any) error {
	path, err := fleetPath(in.Metadata.Namespace, resource, in.Metadata.Name)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPatch, path+"/status", map[string]any{"metadata": map[string]any{"resourceVersion": in.Metadata.ResourceVersion}, "status": fields}, nil, "application/merge-patch+json")
}

// AuthenticatedUsername asks the API server to identify this credential.
// Approval authority never comes from an environment-provided display name.
func (c *Client) AuthenticatedUsername(ctx context.Context) (string, error) {
	var out struct {
		Status struct {
			UserInfo struct {
				Username string `json:"username"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	err := c.request(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", map[string]string{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}, &out, "")
	if err != nil {
		return "", err
	}
	if out.Status.UserInfo.Username == "" {
		return "", fmt.Errorf("API server returned no authenticated identity")
	}
	return out.Status.UserInfo.Username, nil
}
