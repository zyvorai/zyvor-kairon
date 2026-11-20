// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package conversion

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func postReview(t *testing.T, h http.HandlerFunc, req Request) Review {
	t.Helper()
	body, err := json.Marshal(Review{APIVersion: APIVersion, Kind: "ConversionReview", Request: &req})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/convert", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (ConversionReview responses are always 200, the result is in the body), got %d", rr.Code)
	}
	var out Review
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestHandlerConvertsEveryObjectAndEchoesUID(t *testing.T) {
	convert := func(obj map[string]any, toVersion string) (map[string]any, error) {
		obj["apiVersion"] = toVersion
		obj["converted"] = true
		return obj, nil
	}
	h := Handler(nil, "TestKind", convert)

	obj1, _ := json.Marshal(map[string]any{"apiVersion": "g/v1alpha1", "metadata": map[string]any{"name": "a"}})
	obj2, _ := json.Marshal(map[string]any{"apiVersion": "g/v1alpha1", "metadata": map[string]any{"name": "b"}})
	out := postReview(t, h, Request{UID: "req-1", DesiredAPIVersion: "g/v1beta1", Objects: []json.RawMessage{obj1, obj2}})

	if out.Response == nil {
		t.Fatal("expected a response")
	}
	if out.Response.UID != "req-1" {
		t.Fatalf("expected the response UID to echo the request UID, got %q", out.Response.UID)
	}
	if out.Response.Result.Status != StatusSuccess {
		t.Fatalf("expected Success, got %+v", out.Response.Result)
	}
	if len(out.Response.ConvertedObjects) != 2 {
		t.Fatalf("expected 2 converted objects, got %d", len(out.Response.ConvertedObjects))
	}
	for i, raw := range out.Response.ConvertedObjects {
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode converted object %d: %v", i, err)
		}
		if decoded["apiVersion"] != "g/v1beta1" || decoded["converted"] != true {
			t.Fatalf("object %d not converted as expected: %+v", i, decoded)
		}
	}
}

func TestHandlerReturnsFailureOnConverterError(t *testing.T) {
	convert := func(obj map[string]any, toVersion string) (map[string]any, error) {
		return nil, errors.New("unsupported conversion")
	}
	h := Handler(nil, "TestKind", convert)

	obj, _ := json.Marshal(map[string]any{"apiVersion": "g/v1alpha1"})
	out := postReview(t, h, Request{UID: "req-2", DesiredAPIVersion: "g/v1beta1", Objects: []json.RawMessage{obj}})

	if out.Response == nil || out.Response.Result.Status != StatusFailure {
		t.Fatalf("expected a Failure result, got %+v", out.Response)
	}
	if out.Response.Result.Message == "" {
		t.Fatal("expected a failure message")
	}
	if out.Response.UID != "req-2" {
		t.Fatalf("expected the response UID to echo the request UID even on failure, got %q", out.Response.UID)
	}
}

func TestHandlerRejectsMalformedRequest(t *testing.T) {
	h := Handler(nil, "TestKind", func(obj map[string]any, toVersion string) (map[string]any, error) { return obj, nil })

	httpReq := httptest.NewRequest(http.MethodPost, "/convert", bytes.NewReader([]byte("not json")))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 even for a malformed request, got %d", rr.Code)
	}
	var out Review
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Response == nil || out.Response.Result.Status != StatusFailure {
		t.Fatalf("expected a Failure result for a malformed request, got %+v", out.Response)
	}
}
