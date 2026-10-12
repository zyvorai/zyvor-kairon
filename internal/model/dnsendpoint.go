// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

// DNSEndpoint is external-dns's CRD (externaldns.k8s.io/v1alpha1): the
// records in spec.endpoints are published by an external-dns run with
// --source=crd.
type DNSEndpoint struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta      `json:"metadata"`
	Spec     DNSEndpointSpec `json:"spec"`
}

type DNSEndpointSpec struct {
	Endpoints []DNSEndpointRecord `json:"endpoints"`
}

type DNSEndpointRecord struct {
	DNSName    string   `json:"dnsName"`
	RecordType string   `json:"recordType"`
	Targets    []string `json:"targets"`
	RecordTTL  int64    `json:"recordTTL,omitempty"`
}

const (
	DNSEndpointAPIVersion = "externaldns.k8s.io/v1alpha1"
	KindDNSEndpoint       = "DNSEndpoint"
)
