// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQGAFsfreezeFreeze(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/vm-1/qga/fsfreeze" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int64{"frozen": 2})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	n, err := c.QGAFsfreezeFreeze(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 filesystems frozen, got %d", n)
	}
}

func TestQGAFsfreezeThaw(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/vm-1/qga/fsthaw" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int64{"thawed": 2})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	n, err := c.QGAFsfreezeThaw(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 filesystems thawed, got %d", n)
	}
}

func TestQGAFsfreezeStatus(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vms/vm-1/qga/fsfreeze-status" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "frozen"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	status, err := c.QGAFsfreezeStatus(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if status != "frozen" {
		t.Fatalf("expected status %q, got %q", "frozen", status)
	}
}

func TestQGAFirewallOpen(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "ok", "stderr": ""})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	res, err := c.QGAFirewallOpen(context.Background(), "vm-1", "web", 8080, "tcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/qga/firewall/open" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["name"] != "web" || gotBody["port"] != float64(8080) || gotBody["protocol"] != "tcp" {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	if res.ExitCode != 0 || res.Stdout != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestQGAFirewallClose(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "", "stderr": ""})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.QGAFirewallClose(context.Background(), "vm-1", "web", nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/qga/firewall/close" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["name"] != "web" {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	if _, hasTimeout := gotBody["timeout_seconds"]; hasTimeout {
		t.Fatalf("did not expect timeout_seconds when nil, got %+v", gotBody)
	}
}

func TestQGAFirewallPropagatesGuestError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.QGAFirewallOpen(context.Background(), "vm-1", "web", 8080, "", nil); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
	if _, err := c.QGAFirewallClose(context.Background(), "vm-1", "web", nil); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestQGAFsfreezePropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "QGA not enabled for this VM", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.QGAFsfreezeFreeze(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
	if _, err := c.QGAFsfreezeThaw(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestQGAExecWithPathAndArgs(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/vm-1/qga/exec" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "hello\n", "stderr": ""})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	res, err := c.QGAExec(context.Background(), "vm-1", QGAExecRequest{Path: "/bin/echo", Args: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "hello\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if gotBody["path"] != "/bin/echo" {
		t.Fatalf("expected path in request body, got %+v", gotBody)
	}
	if _, hasPowershell := gotBody["powershell"]; hasPowershell {
		t.Fatalf("did not expect a powershell key alongside path, got %+v", gotBody)
	}
}

func TestQGAExecWithPowershell(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 1, "stdout": "", "stderr": "boom"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	timeout := uint64(30)
	res, err := c.QGAExec(context.Background(), "vm-1", QGAExecRequest{Powershell: "Get-Process", TimeoutSeconds: &timeout})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 1 || res.Stderr != "boom" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if gotBody["powershell"] != "Get-Process" || gotBody["timeout_seconds"] != float64(30) {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
}

func TestQGAExecRejectsAmbiguousOrEmptyRequest(t *testing.T) {
	c := New("http://unused", "")
	if _, err := c.QGAExec(context.Background(), "vm-1", QGAExecRequest{}); err == nil {
		t.Fatal("expected an error when neither Path nor Powershell is set")
	}
	if _, err := c.QGAExec(context.Background(), "vm-1", QGAExecRequest{Path: "/bin/echo", Powershell: "Get-Process"}); err == nil {
		t.Fatal("expected an error when both Path and Powershell are set")
	}
}

func TestQGAExecPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.QGAExec(context.Background(), "vm-1", QGAExecRequest{Path: "/bin/echo"}); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}
