// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
)

func TestLogLevelPatch(t *testing.T) {
	p, err := logLevelPatch([]string{"node"}, "w1", "DEBUG", false)
	if err != nil || len(p) != 1 || p["node.w1"] != "debug" {
		t.Fatalf("%v %v", p, err)
	}
	p, _ = logLevelPatch([]string{"controller", "ui"}, "", "", true)
	if v, ok := p["ui"]; !ok || v != nil || len(p) != 2 {
		t.Fatalf("reset %v", p)
	}
	if _, err := logLevelPatch([]string{"ui"}, "", "loud", false); err == nil {
		t.Fatal("bad level accepted")
	}
}

func TestApplyLogLevelsCreatesMissingConfigMap(t *testing.T) {
	var methods []string
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodPatch:
			http.Error(w, `{"reason":"NotFound"}`, http.StatusNotFound)
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	kc := &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := applyLogLevels(context.Background(), kc, "kairon-system", map[string]any{"controller": "debug"}); err != nil {
		t.Fatal(err)
	}
	want := "PATCH /api/v1/namespaces/kairon-system/configmaps/kairon-log-levels,POST /api/v1/namespaces/kairon-system/configmaps"
	if strings.Join(methods, ",") != want {
		t.Fatalf("calls %v", methods)
	}
	if d, _ := created["data"].(map[string]any); d["controller"] != "debug" || created["kind"] != "ConfigMap" {
		t.Fatalf("created %v", created)
	}

	methods = nil
	if err := applyLogLevels(context.Background(), kc, "kairon-system", map[string]any{"controller": nil}); err != nil || len(methods) != 1 {
		t.Fatalf("reset on a missing ConfigMap must not create it: %v %v", err, methods)
	}
}

func TestWriteLogLevels(t *testing.T) {
	var buf bytes.Buffer
	_ = writeLogLevels(&buf, map[string]string{"node.w1": "debug"})
	out := buf.String()
	for _, s := range []string{"controller", "info (default)", "node.w1", "debug", "ui"} {
		if !strings.Contains(out, s) {
			t.Fatalf("missing %q:\n%s", s, out)
		}
	}
}
