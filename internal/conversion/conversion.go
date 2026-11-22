// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package conversion hand-rolls just the slice of the Kubernetes
// apiextensions.k8s.io/v1 ConversionReview wire format a CRD conversion
// webhook needs -- exactly the same shape of deliberate exception
// internal/admission already makes for AdmissionReview, and for the same
// reason: this project has no client-go/k8s.io/api dependency at all (see
// go.mod), and pulling one in just for this one small, stable,
// well-documented JSON schema would be a large new dependency surface for
// a handful of fields. See
// https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/#configure-customresourcedefinitions-to-use-conversion-webhooks
// for the authoritative shape; this is a deliberate subset.
//
// No live Kairon CRD declares a conversion webhook: its served versions
// share one schema and convert with the None strategy -- see
// docs/guides/crd-versioning.md for why, and what a schema-changing
// version requires. This package, its Handler, and the worked ConvertMachineQuota
// example (machinequota.go) are the scaffold: proven, tested machinery
// ready to wire a real CRD version into, not something built from scratch
// the day it's actually needed.
package conversion

import "encoding/json"

const APIVersion = "apiextensions.k8s.io/v1"

// Request is the subset of ConversionRequest this package reads: which
// version the caller wants every object converted to, and the objects
// themselves as raw JSON (left undecoded here -- a Converter decides how
// to interpret them, since their shape is exactly what differs between
// versions).
type Request struct {
	UID               string            `json:"uid"`
	DesiredAPIVersion string            `json:"desiredAPIVersion"`
	Objects           []json.RawMessage `json:"objects"`
}

// Response is the subset of ConversionResponse this package writes back.
type Response struct {
	UID              string            `json:"uid"`
	Result           Status            `json:"result"`
	ConvertedObjects []json.RawMessage `json:"convertedObjects,omitempty"`
}

// Status mirrors the two values the API server actually branches on:
// "Success" with ConvertedObjects populated, or "Failure" with Message set
// to whatever the API server surfaces back to the read/write that
// triggered the conversion.
type Status struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

const (
	StatusSuccess = "Success"
	StatusFailure = "Failure"
)

// Review is the ConversionReview envelope both directions use -- Request
// set on the way in, Response set on the way out.
type Review struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Request    *Request  `json:"request,omitempty"`
	Response   *Response `json:"response,omitempty"`
}
