// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentPutFile(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-written"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	content := base64.StdEncoding.EncodeToString([]byte("hello"))
	mode := uint32(0o600)
	if err := c.AgentPutFile(context.Background(), "vm-1", "/etc/app/config", content, &mode); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/agent/put-file" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["path"] != "/etc/app/config" || gotBody["content_base64"] != content || gotBody["mode"] != float64(0o600) {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
}

func TestAgentPutFileOmitsModeWhenNil(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-written"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.AgentPutFile(context.Background(), "vm-1", "/etc/app/config", "aGVsbG8=", nil); err != nil {
		t.Fatal(err)
	}
	if _, hasMode := gotBody["mode"]; hasMode {
		t.Fatalf("did not expect mode to be sent when nil, got %+v", gotBody)
	}
}

func TestAgentPutFilePropagatesGuestError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A guest-side failure is still HTTP 200 -- the error is only in the body.
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "error", "message": "permission denied"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	err := c.AgentPutFile(context.Background(), "vm-1", "/root/secret", "aGVsbG8=", nil)
	if err == nil {
		t.Fatal("expected an error when the guest agent reports one")
	}
}

func TestAgentGetFile(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	content := base64.StdEncoding.EncodeToString([]byte("hello"))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-content", "content_base64": content, "mode": 420})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.AgentGetFile(context.Background(), "vm-1", "/etc/hostname")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/agent/get-file" || gotBody["path"] != "/etc/hostname" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if out.ContentBase64 != content || out.Mode != 0o644 {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestAgentGetFilePropagatesGuestError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "error", "message": "no such file"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.AgentGetFile(context.Background(), "vm-1", "/nope"); err == nil {
		t.Fatal("expected an error when the guest agent reports one")
	}
}

func TestAgentExec(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "exec", "exit_code": 0, "stdout": "hello\n", "stderr": ""})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	timeout := uint64(30)
	out, err := c.AgentExec(context.Background(), "vm-1", "echo hello", &timeout)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/agent" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["command"] != "echo hello" || gotBody["timeout_seconds"] != float64(30) {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestAgentExecOmitsTimeoutWhenNil(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "exec", "exit_code": 0, "stdout": "", "stderr": ""})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.AgentExec(context.Background(), "vm-1", "true", nil); err != nil {
		t.Fatal(err)
	}
	if _, hasTimeout := gotBody["timeout_seconds"]; hasTimeout {
		t.Fatalf("did not expect timeout_seconds to be sent when nil, got %+v", gotBody)
	}
}

func TestAgentExecPropagatesGuestError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "error", "message": "vsock agent unreachable"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.AgentExec(context.Background(), "vm-1", "true", nil); err == nil {
		t.Fatal("expected an error when the guest agent reports one")
	}
}

func TestAgentFilePropagatesTransportErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.AgentPutFile(context.Background(), "vm-1", "/x", "aGVsbG8=", nil); err == nil {
		t.Fatal("expected an error when the server itself rejects the request")
	}
	if _, err := c.AgentGetFile(context.Background(), "vm-1", "/x"); err == nil {
		t.Fatal("expected an error when the server itself rejects the request")
	}
}
