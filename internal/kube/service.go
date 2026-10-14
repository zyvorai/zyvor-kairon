// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/zyvorai/kairon/internal/model"
)

// Machines are VMs, not pods, so a Service can't select them. An exposed
// Machine is a selectorless Service labeled LabelExposeMachine plus one
// EndpointSlice carrying the Machine's guest IP, which kairon-controller
// keeps current as the IP changes.
const (
	LabelExposeMachine     = "kairon.zyvor.dev/expose-machine"
	LabelServiceName       = "kubernetes.io/service-name"
	LabelEndpointManagedBy = "endpointslice.kubernetes.io/managed-by"
	EndpointSliceManager   = "kairon.zyvor.dev"
)

// ExposeSliceName is the EndpointSlice kairon manages for an exposed Service.
func ExposeSliceName(service string) string { return service + "-kairon" }

type OwnerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller,omitempty"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion,omitempty"`
}

type ObjectMeta struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []OwnerReference  `json:"ownerReferences,omitempty"`
}

// IntOrString is the apiserver's int-or-name port reference.
type IntOrString struct {
	Int int32
	Str string
}

func (v IntOrString) MarshalJSON() ([]byte, error) {
	if v.Str != "" {
		return json.Marshal(v.Str)
	}
	return json.Marshal(v.Int)
}

func (v *IntOrString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &v.Str)
	}
	return json.Unmarshal(b, &v.Int)
}

func (v IntOrString) String() string {
	if v.Str != "" {
		return v.Str
	}
	return strconv.Itoa(int(v.Int))
}

type ServicePort struct {
	Name       string       `json:"name,omitempty"`
	Protocol   string       `json:"protocol,omitempty"`
	Port       int32        `json:"port"`
	TargetPort *IntOrString `json:"targetPort,omitempty"`
	NodePort   int32        `json:"nodePort,omitempty"`
}

type ServiceSpec struct {
	Type      string            `json:"type,omitempty"`
	Ports     []ServicePort     `json:"ports,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
	ClusterIP string            `json:"clusterIP,omitempty"`
}

type LoadBalancerIngress struct {
	IP       string `json:"ip,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

type ServiceStatus struct {
	LoadBalancer struct {
		Ingress []LoadBalancerIngress `json:"ingress,omitempty"`
	} `json:"loadBalancer"`
}

type Service struct {
	APIVersion string        `json:"apiVersion,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Metadata   ObjectMeta    `json:"metadata"`
	Spec       ServiceSpec   `json:"spec"`
	Status     ServiceStatus `json:"status,omitempty"`
}

type ServiceList struct {
	Items []Service `json:"items"`
}

type EndpointConditions struct {
	Ready *bool `json:"ready,omitempty"`
}

type Endpoint struct {
	Addresses  []string           `json:"addresses"`
	Conditions EndpointConditions `json:"conditions"`
	NodeName   string             `json:"nodeName,omitempty"`
}

type EndpointPort struct {
	Name     *string `json:"name,omitempty"`
	Protocol string  `json:"protocol,omitempty"`
	Port     *int32  `json:"port,omitempty"`
}

type EndpointSlice struct {
	APIVersion  string         `json:"apiVersion,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Metadata    ObjectMeta     `json:"metadata"`
	AddressType string         `json:"addressType"`
	Endpoints   []Endpoint     `json:"endpoints"`
	Ports       []EndpointPort `json:"ports"`
}

// DesiredExposeSlice is the EndpointSlice svc should have for machine m
// (nil when the Machine is gone): the Machine's primary guest IP on each
// Service port's numeric target port, ready only while it is Running.
// Named target ports can't be resolved against a VM and fall back to the
// Service port.
func DesiredExposeSlice(svc Service, m *model.Machine) EndpointSlice {
	t := true
	s := EndpointSlice{
		Metadata: ObjectMeta{
			Name:      ExposeSliceName(svc.Metadata.Name),
			Namespace: svc.Metadata.Namespace,
			Labels: map[string]string{
				LabelServiceName:       svc.Metadata.Name,
				LabelEndpointManagedBy: EndpointSliceManager,
				LabelExposeMachine:     svc.Metadata.Labels[LabelExposeMachine],
			},
			OwnerReferences: []OwnerReference{{APIVersion: "v1", Kind: "Service", Name: svc.Metadata.Name, UID: svc.Metadata.UID, Controller: &t}},
		},
		AddressType: "IPv4",
		Endpoints:   []Endpoint{},
	}
	for _, p := range svc.Spec.Ports {
		name, port := p.Name, p.Port
		if p.TargetPort != nil && p.TargetPort.Int > 0 {
			port = p.TargetPort.Int
		}
		s.Ports = append(s.Ports, EndpointPort{Name: &name, Protocol: p.Protocol, Port: &port})
	}
	if m == nil || m.Status.GuestIP == "" {
		return s
	}
	ip := net.ParseIP(m.Status.GuestIP)
	if ip == nil {
		return s
	}
	if ip.To4() == nil {
		s.AddressType = "IPv6"
	}
	ready := m.Status.Phase == "Running"
	s.Endpoints = []Endpoint{{Addresses: []string{ip.String()}, Conditions: EndpointConditions{Ready: &ready}, NodeName: m.Status.NodeName}}
	return s
}

// SameEndpoints reports whether two slices route the same way.
func SameEndpoints(a, b EndpointSlice) bool {
	type view struct {
		T string
		E []Endpoint
		P []EndpointPort
	}
	norm := func(s EndpointSlice) string {
		v := view{T: s.AddressType}
		if len(s.Endpoints) > 0 {
			v.E = s.Endpoints
		}
		if len(s.Ports) > 0 {
			v.P = s.Ports
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	return norm(a) == norm(b)
}

// SyncExposeSlice creates or updates svc's managed EndpointSlice to match
// machine m (nil when the Machine is gone).
func (c *Client) SyncExposeSlice(ctx context.Context, svc Service, m *model.Machine) (EndpointSlice, error) {
	ns := svc.Metadata.Namespace
	want := DesiredExposeSlice(svc, m)
	cur, err := c.GetEndpointSlice(ctx, ns, want.Metadata.Name)
	if IsNotFound(err) {
		return c.CreateEndpointSlice(ctx, ns, want)
	}
	if err != nil {
		return cur, err
	}
	if SameEndpoints(cur, want) {
		return cur, nil
	}
	want.Metadata.ResourceVersion = cur.Metadata.ResourceVersion
	return c.UpdateEndpointSlice(ctx, ns, want)
}

func servicesPath(ns string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services", url.PathEscape(ns))
}

func endpointSlicesPath(ns string) string {
	return fmt.Sprintf("/apis/discovery.k8s.io/v1/namespaces/%s/endpointslices", url.PathEscape(ns))
}

func (c *Client) GetService(ctx context.Context, ns, name string) (Service, error) {
	var s Service
	err := c.request(ctx, http.MethodGet, servicesPath(ns)+"/"+url.PathEscape(name), nil, &s, "")
	return s, err
}

func (c *Client) CreateService(ctx context.Context, ns string, s Service) (Service, error) {
	s.APIVersion, s.Kind = "v1", "Service"
	var out Service
	err := c.request(ctx, http.MethodPost, servicesPath(ns), s, &out, "")
	return out, err
}

func (c *Client) DeleteService(ctx context.Context, ns, name string) error {
	return c.request(ctx, http.MethodDelete, servicesPath(ns)+"/"+url.PathEscape(name), nil, nil, "")
}

// ListExposedServices lists every Service in the cluster that exposes a Machine.
func (c *Client) ListExposedServices(ctx context.Context) ([]Service, error) {
	var list ServiceList
	err := c.request(ctx, http.MethodGet, "/api/v1/services?labelSelector="+url.QueryEscape(LabelExposeMachine), nil, &list, "")
	return list.Items, err
}

func (c *Client) GetEndpointSlice(ctx context.Context, ns, name string) (EndpointSlice, error) {
	var s EndpointSlice
	err := c.request(ctx, http.MethodGet, endpointSlicesPath(ns)+"/"+url.PathEscape(name), nil, &s, "")
	return s, err
}

func (c *Client) CreateEndpointSlice(ctx context.Context, ns string, s EndpointSlice) (EndpointSlice, error) {
	s.APIVersion, s.Kind = "discovery.k8s.io/v1", "EndpointSlice"
	var out EndpointSlice
	err := c.request(ctx, http.MethodPost, endpointSlicesPath(ns), s, &out, "")
	return out, err
}

// UpdateEndpointSlice replaces s; s.Metadata.ResourceVersion must be the
// one read, so a concurrent writer surfaces as IsConflict.
func (c *Client) UpdateEndpointSlice(ctx context.Context, ns string, s EndpointSlice) (EndpointSlice, error) {
	s.APIVersion, s.Kind = "discovery.k8s.io/v1", "EndpointSlice"
	var out EndpointSlice
	err := c.request(ctx, http.MethodPut, endpointSlicesPath(ns)+"/"+url.PathEscape(s.Metadata.Name), s, &out, "")
	return out, err
}
