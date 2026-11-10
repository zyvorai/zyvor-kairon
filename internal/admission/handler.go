// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Validator decides whether to admit req. Implementations never do I/O
// beyond reading from Kubernetes (see internal/controller/webhook.go) --
// this package only handles the AdmissionReview HTTP envelope around them.
type Validator func(r *http.Request, req *Request) Decision

// Handler returns an http.HandlerFunc implementing the AdmissionReview
// webhook contract: decode the incoming Review, call validate, encode the
// outgoing Review. A validate error (malformed request, or the Validator
// itself failing to look something up) always responds with a 200
// AdmissionReview denying the request with the error's message -- never a
// non-200 status, since the API server treats a webhook's HTTP-level
// failure according to failurePolicy but a well-formed *denial* the same
// way regardless, and a clear message here is far more useful to whoever's
// `kubectl apply` just got rejected than an opaque "the webhook errored."
//
// observe, when non-nil, is called once per decision with the resource
// kind, operation, and whether the request was allowed -- kairon-controller
// wires this to internal/metrics for a kairon_webhook_decisions_total
// counter (see internal/controller/webhook.go). Optional, nil-checked,
// same convention as log above, so this package has no hard dependency on
// internal/metrics.
func Handler(log *slog.Logger, validate Validator, observe func(resource, operation string, allowed bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in Review
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Request == nil {
			writeDenied(w, "", "malformed AdmissionReview request")
			return
		}
		decision := validate(r, in.Request)
		if !decision.Allowed && log != nil {
			log.Warn("admission denied", "resource", in.Request.Resource.Resource, "namespace", in.Request.Namespace, "operation", in.Request.Operation, "reason", decision.Reason)
		}
		if observe != nil {
			observe(in.Request.Resource.Resource, in.Request.Operation, decision.Allowed)
		}
		writeReview(w, in.Request.UID, decision)
	}
}

func writeDenied(w http.ResponseWriter, uid, reason string) {
	writeReview(w, uid, Deny(reason))
}

func writeReview(w http.ResponseWriter, uid string, decision Decision) {
	resp := &Response{UID: uid, Allowed: decision.Allowed, Warnings: decision.Warnings}
	if !decision.Allowed {
		resp.Status = &Status{Message: decision.Reason}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Review{APIVersion: APIVersion, Kind: "AdmissionReview", Response: resp})
}
