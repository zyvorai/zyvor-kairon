// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatJSONFallsBackToJSONMode(t *testing.T) {
	var formats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Model          string `json:"model"`
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		formats = append(formats, body.ResponseFormat.Type)
		if body.ResponseFormat.Type == "json_schema" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"json_schema unsupported"}`)
			return
		}
		_, _ = io.WriteString(w, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"ok\\\":true}\\n```\"}}]}")
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL + "/v1/", APIKey: "k", Model: "m"}
	out, err := c.ChatJSON(context.Background(), []Message{{Role: "user", Content: "hi"}}, "x", map[string]any{"type": "object"})
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(formats) != 2 || formats[0] != "json_schema" || formats[1] != "json_object" {
		t.Fatalf("formats = %v", formats)
	}
}

func TestFromEnvAndNilClient(t *testing.T) {
	t.Setenv("KAIRON_LLM_URL", "")
	t.Setenv("KAIRON_LLM_MODEL", "m")
	if FromEnv() != nil {
		t.Fatal("client without URL")
	}
	var c *Client
	if _, err := c.ChatJSON(context.Background(), nil, "x", nil); err != ErrNotConfigured {
		t.Fatalf("nil client err = %v", err)
	}
}
