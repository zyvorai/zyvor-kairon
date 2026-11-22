// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package conversion

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Converter converts obj -- one object from a ConversionReview's Objects,
// already decoded to a generic JSON object since no version-specific Go
// struct exists for a version that isn't wired into the rest of Kairon's
// still-single-version reconcile paths -- to toVersion. obj's own current
// version is read from its "apiVersion" field; toVersion is
// Request.DesiredAPIVersion. Implementations only need to handle the
// specific version pairs they're registered for; an unrecognized pair
// should return an error, which Handler turns into a Failure response
// rather than a panic or a silently-wrong object.
type Converter func(obj map[string]any, toVersion string) (map[string]any, error)

// Handler returns an http.HandlerFunc implementing the ConversionReview
// webhook contract for one kind: decode the incoming Review, convert every
// object in request.objects via convert, encode the outgoing Review.
// Mirrors internal/admission.Handler's shape and its "a decode or convert
// failure still gets a well-formed Failure response, never a bare HTTP
// error status" posture -- the API server surfaces response.result.message
// directly to whoever's read/write triggered the conversion, and a non-200
// here would instead be handled according to the CRD's conversion
// strategy configuration in a way that's far less useful to diagnose.
//
// kind is only used for logging -- one kairon-controller binary can
// register a different Handler per CRD kind on its own conversion route
// (see internal/controller/webhook.go), and knowing which kind a given
// failure came from matters when more than one is registered.
func Handler(log *slog.Logger, kind string, convert Converter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in Review
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Request == nil {
			writeFailure(w, "", "malformed ConversionReview request")
			return
		}
		converted := make([]json.RawMessage, 0, len(in.Request.Objects))
		for _, raw := range in.Request.Objects {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				logFailure(log, kind, in.Request.DesiredAPIVersion, err)
				writeFailure(w, in.Request.UID, fmt.Sprintf("decode object: %v", err))
				return
			}
			out, err := convert(obj, in.Request.DesiredAPIVersion)
			if err != nil {
				logFailure(log, kind, in.Request.DesiredAPIVersion, err)
				writeFailure(w, in.Request.UID, err.Error())
				return
			}
			encoded, err := json.Marshal(out)
			if err != nil {
				logFailure(log, kind, in.Request.DesiredAPIVersion, err)
				writeFailure(w, in.Request.UID, fmt.Sprintf("encode converted object: %v", err))
				return
			}
			converted = append(converted, encoded)
		}
		writeReview(w, in.Request.UID, Status{Status: StatusSuccess}, converted)
	}
}

func logFailure(log *slog.Logger, kind, desiredAPIVersion string, err error) {
	if log != nil {
		log.Warn("conversion failed", "kind", kind, "desiredAPIVersion", desiredAPIVersion, "error", err)
	}
}

func writeFailure(w http.ResponseWriter, uid, message string) {
	writeReview(w, uid, Status{Status: StatusFailure, Message: message}, nil)
}

func writeReview(w http.ResponseWriter, uid string, result Status, converted []json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Review{
		APIVersion: APIVersion,
		Kind:       "ConversionReview",
		Response:   &Response{UID: uid, Result: result, ConvertedObjects: converted},
	})
}
