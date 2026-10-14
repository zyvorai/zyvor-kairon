// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeSpecSource struct{}

func (fakeSpecSource) GetMachineInstanceType(_ context.Context, ns, name string) (model.MachineInstanceType, error) {
	if ns != "prod" || name != "m.large" {
		return model.MachineInstanceType{}, &kube.APIError{StatusCode: 404, Body: "not found"}
	}
	return model.MachineInstanceType{Spec: model.MachineInstanceTypeSpec{Resources: model.ResourceSpec{CPU: "4", Memory: "8Gi"}}}, nil
}

func (fakeSpecSource) GetMachineClusterInstanceType(_ context.Context, name string) (model.MachineClusterInstanceType, error) {
	return model.MachineClusterInstanceType{Spec: model.MachineInstanceTypeSpec{Resources: model.ResourceSpec{CPU: "16", Memory: "64Gi"}}}, nil
}

func (fakeSpecSource) GetMachinePreference(context.Context, string, string) (model.MachinePreference, error) {
	return model.MachinePreference{Spec: model.MachinePreferenceSpec{Backend: "cloud-hypervisor", TPM: true, GuestAgent: true}}, nil
}

func (fakeSpecSource) GetMachineClusterPreference(context.Context, string) (model.MachineClusterPreference, error) {
	return model.MachineClusterPreference{}, nil
}

func TestExpandMachine(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"}}
	m.Spec.InstanceTypeName = "m.large"
	m.Spec.PreferenceName = "linux"
	m.Spec.Runtime.Backend = "qemu"
	notes, err := expandMachine(context.Background(), fakeSpecSource{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	if m.Spec.Resources.CPU != "4" || m.Spec.Resources.Memory != "8Gi" {
		t.Fatalf("resources %+v", m.Spec.Resources)
	}
	if m.Spec.Runtime.Backend != "qemu" || !m.Spec.Security.TPM || !m.Spec.GuestAgent.Enabled {
		t.Fatalf("preference must fill only unset fields: %+v", m.Spec)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "cpu 4, memory 8Gi") {
		t.Fatalf("notes %q", notes)
	}

	m2 := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	m2.Spec.InstanceTypeName = "m.large"
	m2.Spec.Resources.CPU = "1"
	notes, _ = expandMachine(context.Background(), fakeSpecSource{}, &m2)
	if m2.Spec.Resources.CPU != "1" || !strings.Contains(notes[0], "already set") {
		t.Fatalf("explicit resources overridden: %+v %q", m2.Spec.Resources, notes)
	}

	m3 := model.Machine{Metadata: model.ObjectMeta{Name: "x", Namespace: "dev"}}
	m3.Spec.InstanceTypeName = "m.large"
	if _, err := expandMachine(context.Background(), fakeSpecSource{}, &m3); err == nil {
		t.Fatal("missing instance type not reported")
	}
}

func TestReadMachineManifest(t *testing.T) {
	m, err := readMachineManifest(strings.NewReader("apiVersion: kairon.zyvor.dev/v1\nkind: Machine\nmetadata: {name: web}\nspec:\n  instanceTypeName: m.large\n"))
	if err != nil || m.Metadata.Name != "web" || m.Spec.InstanceTypeName != "m.large" {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := readMachineManifest(strings.NewReader("kind: MachineSet\nmetadata: {name: x}\n")); err == nil {
		t.Fatal("non-Machine accepted")
	}
}

// TestCreateDryRunNeedsNoCluster prints the Machine create would send,
// with no kubeconfig at all.
func TestCreateDryRunNeedsNoCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent")
	t.Setenv("KAIRON_KUBE_URL", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("HOME", t.TempDir())

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	root := NewRootCmd(nil)
	root.SetArgs([]string{"create", "web-2", "--namespace", "prod", "--image", "/img/u.qcow2", "--instance-type", "m.large", "--cpu", "", "--memory", "", "--dry-run"})
	err := root.Execute()
	_ = w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var m model.Machine
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if m.Kind != model.KindMachine || m.Metadata.Name != "web-2" || m.Metadata.Namespace != "prod" || m.Spec.InstanceTypeName != "m.large" {
		t.Fatalf("%s", out)
	}
}

func TestCreateArgsWithNamespace(t *testing.T) {
	got := strings.Join(createArgsWithNamespace("prod", []string{"machineset", "web", "--image", "/x"}), " ")
	if got != "machineset web --image /x --namespace prod" {
		t.Fatal(got)
	}
	if got := strings.Join(createArgsWithNamespace("prod", []string{"web", "--namespace", "dev"}), " "); got != "web --namespace dev" {
		t.Fatalf("explicit namespace overridden: %v", got)
	}
	if got := strings.Join(createArgsWithNamespace("", []string{"-n", "dev", "web", "--image", "/x"}), " "); got != "web --image /x --namespace dev" {
		t.Fatalf("leading -n: %v", got)
	}
	if got := createArgsWithNamespace("default", []string{"web"}); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestArgsDryRun(t *testing.T) {
	if !argsDryRun([]string{"web", "--image", "x", "--dry-run"}) || argsDryRun([]string{"web", "--dry-run=false"}) {
		t.Fatal("argsDryRun")
	}
}
