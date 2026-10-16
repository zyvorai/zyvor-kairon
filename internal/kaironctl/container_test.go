// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

type fakeContainerAPI struct {
	machines  map[string]model.Machine
	created   []model.Machine
	deleted   []string
	getCalls  int
	exitAfter int // GetMachine calls before the container reports an exit
	exitCode  int32
}

func (f *fakeContainerAPI) CreateMachine(_ context.Context, ns string, m model.Machine) (model.Machine, error) {
	m.Metadata.Namespace = ns
	f.created = append(f.created, m)
	if f.machines == nil {
		f.machines = map[string]model.Machine{}
	}
	f.machines[m.Metadata.Name] = m
	return m, nil
}

func (f *fakeContainerAPI) GetMachine(_ context.Context, _, name string) (model.Machine, error) {
	m, ok := f.machines[name]
	if !ok {
		return model.Machine{}, fmt.Errorf("machine %q not found", name)
	}
	f.getCalls++
	if f.exitAfter > 0 && f.getCalls >= f.exitAfter {
		code := f.exitCode
		m.Status.Phase = "Stopped"
		m.Status.Container = &model.ContainerStatus{ExitCode: &code}
	} else {
		m.Status.Phase = "Running"
	}
	return m, nil
}

func (f *fakeContainerAPI) DeleteMachine(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	delete(f.machines, name)
	return nil
}

func (f *fakeContainerAPI) ListMachinesNamespace(context.Context, string) ([]model.Machine, error) {
	out := make([]model.Machine, 0, len(f.machines))
	for _, m := range f.machines {
		out = append(out, m)
	}
	return out, nil
}

func TestContainerFlagsBuildMachine(t *testing.T) {
	f := &containerFlags{
		name: "web", env: []string{"A=1"}, publish: []string{"8080:80"}, restart: "always", entrypoint: "envoy -c",
		healthCmd: "wget -q http://127.0.0.1", healthInterval: 5, cpu: "2", memory: "1Gi", ttl: 600,
		nodeSelector: []string{"kubernetes.io/os=darwin"}, maxRestarts: 4, rm: true,
	}
	m, err := f.machine("envoyproxy/envoy:v1.40.0", []string{"-c", "x"}, true)
	if err != nil {
		t.Fatal(err)
	}
	o := m.Spec.Sandbox.OCI
	if m.Metadata.Name != "web" || o.Image != "envoyproxy/envoy:v1.40.0" || len(o.Command) != 2 || o.Restart != "always" ||
		len(o.Entrypoint) != 2 || o.MaxRestarts == nil || *o.MaxRestarts != 4 || o.ExitPolicy != "poweroff" {
		t.Fatalf("oci = %+v", o)
	}
	if o.HealthCheck == nil || len(o.HealthCheck.Command) != 3 || o.HealthCheck.IntervalSeconds != 5 {
		t.Fatalf("health = %+v", o.HealthCheck)
	}
	if m.Spec.Resources.CPU != "2" || m.Spec.Resources.Memory != "1Gi" || m.Spec.TTLSeconds != 600 ||
		m.Spec.Placement.NodeSelector["kubernetes.io/os"] != "darwin" {
		t.Fatalf("spec = %+v", m.Spec)
	}
	if err := model.ValidateSandboxOCI(m.Spec); err != nil {
		t.Fatalf("flags produced an invalid spec: %v", err)
	}
	if _, err := (&containerFlags{nodeSelector: []string{"nokv"}}).machine("x", nil, false); err == nil {
		t.Fatal("a node selector without = must be rejected")
	}
	auto, err := (&containerFlags{}).machine("alpine", nil, false)
	if err != nil || !strings.HasPrefix(auto.Metadata.Name, "ctr-") {
		t.Fatalf("auto name = %q err=%v", auto.Metadata.Name, err)
	}
}

func TestRunContainerWaitPropagatesExitCode(t *testing.T) {
	old := containerPollInterval
	containerPollInterval = time.Millisecond
	defer func() { containerPollInterval = old }()

	api := &fakeContainerAPI{exitAfter: 3, exitCode: 3}
	var out bytes.Buffer
	f := &containerFlags{name: "job", wait: true, rm: true}
	m, _ := f.machine("alpine:3.22", []string{"sh", "-c", "exit 3"}, false)
	err := runContainer(context.Background(), &out, api, "default", m, f)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 3 {
		t.Fatalf("want exit code 3, got %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "job" {
		t.Fatalf("--rm must delete the Machine: %v", api.deleted)
	}
	if !strings.Contains(out.String(), "exited with code 3") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunContainerDetachedDoesNotWait(t *testing.T) {
	api := &fakeContainerAPI{}
	var out bytes.Buffer
	f := &containerFlags{name: "bg", detach: true}
	m, _ := f.machine("envoyproxy/envoy:v1.40.0", nil, false)
	if err := runContainer(context.Background(), &out, api, "default", m, f); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 0 || len(api.created) != 1 {
		t.Fatalf("detached run polled: gets=%d created=%d", api.getCalls, len(api.created))
	}
}

func TestWaitContainerExitReportsInitErrorAndTimeout(t *testing.T) {
	old := containerPollInterval
	containerPollInterval = time.Millisecond
	defer func() { containerPollInterval = old }()

	api := &fakeContainerAPI{machines: map[string]model.Machine{"c": {
		Metadata: model.ObjectMeta{Name: "c"},
		Status:   model.MachineStatus{Container: &model.ContainerStatus{InitError: "no such file: /app"}},
	}}}
	if _, err := waitContainerExit(context.Background(), api, "default", "c"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("init error not surfaced: %v", err)
	}
	api2 := &fakeContainerAPI{machines: map[string]model.Machine{"d": {Metadata: model.ObjectMeta{Name: "d"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := waitContainerExit(ctx, api2, "default", "d"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout not reported: %v", err)
	}
}

func TestContainerPsAndRmOnlyTouchContainers(t *testing.T) {
	code := int32(0)
	api := &fakeContainerAPI{machines: map[string]model.Machine{
		"web": {Metadata: model.ObjectMeta{Name: "web"}, Spec: model.MachineSpec{Sandbox: &model.SandboxSpec{OCI: &model.OCISpec{Image: "envoyproxy/envoy:v1.40.0", Ports: []string{"8080:80"}}}},
			Status: model.MachineStatus{Phase: "Running", NodeName: "mac-1"}},
		"done": {Metadata: model.ObjectMeta{Name: "done"}, Spec: model.MachineSpec{Sandbox: &model.SandboxSpec{OCI: &model.OCISpec{Image: "alpine:3.22"}}},
			Status: model.MachineStatus{Phase: "Stopped", Container: &model.ContainerStatus{ExitCode: &code}}},
		"vm": {Metadata: model.ObjectMeta{Name: "vm"}, Spec: model.MachineSpec{}},
	}}
	ms, _ := api.ListMachinesNamespace(context.Background(), "default")
	only := containerMachines(ms)
	if len(only) != 2 {
		t.Fatalf("ps must list only container Machines: %d", len(only))
	}
	var out bytes.Buffer
	_ = writeContainerTable(&out, only, false)
	if !strings.Contains(out.String(), "web") || strings.Contains(out.String(), "done") {
		t.Fatalf("ps without -a must hide exited containers:\n%s", out.String())
	}
	out.Reset()
	_ = writeContainerTable(&out, only, true)
	if !strings.Contains(out.String(), "Exited (0)") || !strings.Contains(out.String(), "8080:80") {
		t.Fatalf("ps -a:\n%s", out.String())
	}

	out.Reset()
	err := removeContainers(context.Background(), &out, api, "default", []string{"web", "vm"})
	if err == nil || !strings.Contains(err.Error(), "vm: not a container") {
		t.Fatalf("rm must refuse a non-container Machine: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "web" {
		t.Fatalf("deleted = %v", api.deleted)
	}
}
