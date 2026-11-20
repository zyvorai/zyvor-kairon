// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package oteltrace is a tiny, opt-in OTLP/HTTP JSON exporter for
// reconcile spans -- deliberately not the full OpenTelemetry Go SDK
// (third stdlib exception after OIDC and CSI would otherwise pull a large
// dependency tree for a single operator signal). When Endpoint is empty,
// Start is a no-op and End does nothing. When set, End POSTs one span to
// the collector's /v1/traces JSON endpoint.
package oteltrace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Tracer emits reconcile spans when Endpoint is non-empty.
type Tracer struct {
	Endpoint    string // e.g. http://otel-collector:4318/v1/traces
	ServiceName string
	HTTP        *http.Client
}

// Span is one timed reconcile unit (machine, migration, …).
type Span struct {
	tracer *Tracer
	name   string
	start  time.Time
	attrs  map[string]string
}

// Start begins a span. attrs should stay low-cardinality (kind, not every
// Machine name) when used as metric-like dimensions; namespaced name is
// fine as a span attribute for debugging.
func (t *Tracer) Start(_ context.Context, name string, attrs map[string]string) *Span {
	if t == nil || t.Endpoint == "" {
		return &Span{}
	}
	cp := map[string]string{}
	for k, v := range attrs {
		cp[k] = v
	}
	return &Span{tracer: t, name: name, start: time.Now().UTC(), attrs: cp}
}

// End finishes the span. err may be nil.
func (s *Span) End(err error) {
	if s == nil || s.tracer == nil || s.tracer.Endpoint == "" {
		return
	}
	end := time.Now().UTC()
	statusCode := 1 // OK
	statusMsg := ""
	if err != nil {
		statusCode = 2 // ERROR
		statusMsg = err.Error()
	}
	svc := s.tracer.ServiceName
	if svc == "" {
		svc = "kairon"
	}
	attrs := make([]map[string]any, 0, len(s.attrs)+1)
	for k, v := range s.attrs {
		attrs = append(attrs, map[string]any{
			"key": k, "value": map[string]string{"stringValue": v},
		})
	}
	if statusMsg != "" {
		attrs = append(attrs, map[string]any{
			"key": "exception.message", "value": map[string]string{"stringValue": statusMsg},
		})
	}
	payload := map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{
				"attributes": []map[string]any{{
					"key": "service.name", "value": map[string]string{"stringValue": svc},
				}},
			},
			"scopeSpans": []map[string]any{{
				"scope": map[string]string{"name": "github.com/zyvorai/kairon/internal/oteltrace"},
				"spans": []map[string]any{{
					"name":              s.name,
					"startTimeUnixNano": fmt.Sprintf("%d", s.start.UnixNano()),
					"endTimeUnixNano":   fmt.Sprintf("%d", end.UnixNano()),
					"kind":              1,
					"attributes":        attrs,
					"status":            map[string]any{"code": statusCode, "message": statusMsg},
				}},
			}},
		}},
	}
	body, _ := json.Marshal(payload)
	client := s.tracer.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	req, reqErr := http.NewRequest(http.MethodPost, s.tracer.Endpoint, bytes.NewReader(body))
	if reqErr != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := client.Do(req)
	if doErr != nil {
		return
	}
	_ = resp.Body.Close()
}

// FromEnv builds a Tracer from KAIRON_OTEL_ENDPOINT / KAIRON_OTEL_SERVICE_NAME.
func FromEnv() *Tracer {
	ep := os.Getenv("KAIRON_OTEL_ENDPOINT")
	if ep == "" {
		return &Tracer{}
	}
	svc := os.Getenv("KAIRON_OTEL_SERVICE_NAME")
	return &Tracer{Endpoint: ep, ServiceName: svc}
}
