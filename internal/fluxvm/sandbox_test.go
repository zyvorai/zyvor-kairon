// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func sandboxMachine(name string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: "/images/sandbox.qcow2"},
			Resources: model.ResourceSpec{CPU: "1", Memory: "512Mi"},
			Sandbox:   &model.SandboxSpec{},
		},
	}
}

func TestCreateSandboxForMachineWithSpec(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(Record{UUID: "sandbox-1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	rec, err := c.CreateSandboxForMachine(context.Background(), sandboxMachine("agent-run"))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/sandboxes" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if rec.Status != "Running" {
		t.Fatalf("unexpected result: %+v", rec)
	}
	spec, ok := gotBody["spec"].(map[string]any)
	if !ok {
		t.Fatalf("expected an embedded spec, got %+v", gotBody)
	}
	if spec["image"] != "/images/sandbox.qcow2" || spec["backend"] != "flux-vm" {
		t.Fatalf("unexpected embedded spec: %+v", spec)
	}
	if _, hasTemplate := gotBody["template"]; hasTemplate {
		t.Fatalf("did not expect a template field when Spec is used, got %+v", gotBody)
	}
}

func TestCreateSandboxForMachineWithTemplate(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(Record{UUID: "sandbox-1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	m := sandboxMachine("agent-run")
	m.Spec.Sandbox = &model.SandboxSpec{TemplateName: "alpine-agent"}
	if _, err := c.CreateSandboxForMachine(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if gotBody["template"] != "alpine-agent" {
		t.Fatalf("expected template to be forwarded, got %+v", gotBody)
	}
	if _, hasSpec := gotBody["spec"]; hasSpec {
		t.Fatalf("did not expect an embedded spec when a template is used, got %+v", gotBody)
	}
}

func TestCreateSandboxForMachineRefusesDeviceClaims(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when device claims are present")
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	m := sandboxMachine("agent-run")
	m.Spec.DeviceClaims = []model.DeviceClaimReference{{Name: "gpu-0"}}
	if _, err := c.CreateSandboxForMachine(context.Background(), m); err == nil {
		t.Fatal("expected an error for a sandbox Machine with deviceClaims")
	}
}

func TestListSandboxes(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []Record{{UUID: "sandbox-1", Status: "Running"}}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	items, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].UUID != "sandbox-1" {
		t.Fatalf("unexpected result: %+v", items)
	}
}

func TestSnapshotSandbox(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.SnapshotSandbox(context.Background(), "sandbox-1", "/tmp/snap.img"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/sandboxes/sandbox-1/snapshot" || gotBody["path"] != "/tmp/snap.img" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestListTemplates(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []TemplateInfo{{Name: "alpine-agent", Path: "/var/lib/fluxvm/templates/alpine-agent", Snapshot: true}}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	items, err := c.ListTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "alpine-agent" || !items[0].Snapshot {
		t.Fatalf("unexpected result: %+v", items)
	}
}

func TestBuildTemplate(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(TemplateInfo{Name: "alpine-agent", Path: "/var/lib/fluxvm/templates/alpine-agent", Snapshot: false})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.BuildTemplate(context.Background(), "alpine-agent", "docker.io/library/alpine:3.19")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/templates" || gotBody["name"] != "alpine-agent" || gotBody["image_ref"] != "docker.io/library/alpine:3.19" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if out.Name != "alpine-agent" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestBuildTemplatePropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "skopeo copy failed: image not found", http.StatusBadGateway)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.BuildTemplate(context.Background(), "bad", "docker.io/nope:latest"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestCreateSandboxForMachineOCI(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(Record{UUID: "ctr-1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	retries := int32(3)
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: model.MachineSpec{
			Resources: model.ResourceSpec{CPU: "2", Memory: "1Gi"},
			Sandbox: &model.SandboxSpec{OCI: &model.OCISpec{
				Image: "envoyproxy/envoy:v1.40.0", Platform: "linux/amd64", Command: []string{"envoy", "-c", "/etc/envoy/envoy.yaml"},
				Env: []string{"A=1"}, Ports: []string{"8080:80"}, Restart: "on-failure", MaxRestarts: &retries,
				HealthCheck: &model.ContainerHealthCheck{Command: []string{"true"}, IntervalSeconds: 5},
			}},
		},
	}
	if _, err := c.CreateSandboxForMachine(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/sandboxes" {
		t.Fatalf("path %q", gotPath)
	}
	if _, has := gotBody["spec"]; has {
		t.Fatalf("a container sandbox must not send spec: %v", gotBody)
	}
	if _, has := gotBody["template"]; has {
		t.Fatalf("a container sandbox must not send template: %v", gotBody)
	}
	oci, ok := gotBody["oci"].(map[string]any)
	if !ok || oci["image"] != "envoyproxy/envoy:v1.40.0" || oci["platform"] != "linux/amd64" || oci["restart"] != "on-failure" {
		t.Fatalf("oci = %v", gotBody["oci"])
	}
	if oci["read_only_root"] != true {
		t.Fatalf("read_only_root must default to true: %v", oci["read_only_root"])
	}
	if ports, _ := oci["ports"].([]any); len(ports) != 1 || ports[0] != "8080:80" {
		t.Fatalf("ports = %v", oci["ports"])
	}
	if hc, _ := oci["healthcheck"].(map[string]any); hc == nil || hc["interval_seconds"] != float64(5) {
		t.Fatalf("healthcheck = %v", oci["healthcheck"])
	}
	if gotBody["vcpus"] != float64(2) || gotBody["memory_mib"] != float64(1024) {
		t.Fatalf("resources: vcpus=%v memory_mib=%v", gotBody["vcpus"], gotBody["memory_mib"])
	}
}

func TestSandboxLogsDecodesContainerState(t *testing.T) {
	var gotQuery string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`{"id":"ctr-1","status":"Stopped","oci":true,"exit_code":3,"init_error":null,"restarts":2,"health":"unhealthy","log":"hello\n"}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	logs, err := c.SandboxLogs(context.Background(), "ctr-1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "/v1/sandboxes/ctr-1/logs?lines=50" {
		t.Fatalf("request %q", gotQuery)
	}
	cs := logs.ContainerStatus()
	if cs == nil || cs.ExitCode == nil || *cs.ExitCode != 3 || cs.Restarts != 2 || cs.Health != "unhealthy" || cs.InitError != "" {
		t.Fatalf("container status = %+v", cs)
	}
	if logs.Log != "hello\n" {
		t.Fatalf("log = %q", logs.Log)
	}
	if (&SandboxLogs{OCI: false}).ContainerStatus() != nil {
		t.Fatal("a non-container sandbox must have no container status")
	}
}
