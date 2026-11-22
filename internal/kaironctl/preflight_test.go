// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pfHeader = "apiVersion: kairon.zyvor.dev/v1\nkind: Machine\nmetadata:\n  name: vm1\n  namespace: dev\n"

func pfMachine(spec string) string { return pfHeader + "spec:\n" + spec }

const pfGoodSpec = "  image: {path: /var/lib/fluxvm/images/ubuntu.qcow2}\n  resources: {cpu: \"2\", memory: 2Gi}\n"

func pfRun(t *testing.T, docs string, doctor bool, env preflightEnv) *preflightReport {
	t.Helper()
	rep, err := runPreflight(context.Background(), []string{"-"}, strings.NewReader(docs), doctor, env)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func pfHas(rep *preflightReport, sev, rule, field string) bool {
	for _, f := range rep.Findings {
		if f.Severity == sev && f.Rule == rule && (field == "" || f.Field == field) {
			return true
		}
	}
	return false
}

func TestPreflightRules(t *testing.T) {
	tests := []struct {
		name  string
		spec  string // appended to the good spec
		sev   string // "" means the manifest must be clean
		rule  string
		field string
	}{
		{name: "good manifest"},
		{name: "unknown field", spec: "  bogus: 1\n", sev: sevError, rule: "schema", field: "spec.bogus"},
		{name: "wrong type", spec: "  nodeName: [a]\n", sev: sevError, rule: "schema", field: "spec.nodeName"},
		{name: "wrong type integer for string", spec: "  tenant: 5\n", sev: sevError, rule: "schema", field: "spec.tenant"},
		{name: "bad enum", spec: "  instanceTypeKind: Nope\n  instanceTypeName: x\n", sev: sevError, rule: "schema", field: "spec.instanceTypeKind"},
		{name: "good enum", spec: "  instanceTypeKind: MachineInstanceType\n  instanceTypeName: x\n"},
		{name: "cpu NaN", spec: "  resources: {cpu: NaN, memory: 2Gi}\n", sev: sevError, rule: "resource-quantities", field: "spec.resources.cpu"},
		{name: "memory overflow", spec: "  resources: {cpu: \"2\", memory: 1e999Gi}\n", sev: sevError, rule: "resource-quantities", field: "spec.resources.memory"},
		{name: "memory garbage", spec: "  resources: {cpu: \"2\", memory: lots}\n", sev: sevError, rule: "resource-quantities", field: "spec.resources.memory"},
		{name: "memory ok", spec: "  resources: {cpu: 500m, memory: 512Mi, maxCpu: \"4\", maxMemory: 4Gi}\n"},
		{name: "bad mac", spec: "  network: {mac: \"zz:00\"}\n", sev: sevError, rule: "network-mac", field: "spec.network.mac"},
		{name: "good mac", spec: "  network: {mac: \"52:54:00:12:34:56\"}\n"},
		{name: "image source needs digest", spec: "  image: {source: {httpURL: \"https://e.example/a.qcow2\"}}\n", sev: sevError, rule: "image-source", field: "spec.image.digest"},
		{name: "image source not http", spec: "  image: {source: {httpURL: \"ftp://e.example/a.qcow2\"}, digest: \"sha256:" + strings.Repeat("a", 64) + "\"}\n", sev: sevError, rule: "image-source", field: "spec.image.source.httpURL"},
		{name: "blank needs disk size", spec: "  image: {blank: true}\n", sev: sevError, rule: "root-image", field: "spec.image.blank"},
		{name: "duplicate disks", spec: "  disks:\n  - {name: data, claimName: a}\n  - {name: data, claimName: b}\n", sev: sevError, rule: "disks"},
		{name: "unique disks", spec: "  disks:\n  - {name: data, claimName: a}\n  - {name: logs, claimName: b}\n"},
		{name: "secureBoot on firecracker", spec: "  runtime: {backend: firecracker}\n  security: {secureBoot: true}\n", sev: sevError, rule: "secureboot-backend", field: "spec.security.secureBoot"},
		{name: "secureBoot on qemu", spec: "  runtime: {backend: qemu}\n  security: {secureBoot: true, tpm: true}\n"},
		{name: "tpm on firecracker", spec: "  runtime: {backend: firecracker}\n  security: {tpm: true}\n", sev: sevError, rule: "tpm-backend", field: "spec.security.tpm"},
		{name: "tpm on cloud-hypervisor", spec: "  runtime: {backend: cloud-hypervisor}\n  security: {tpm: true}\n"},
		{name: "secureBoot with auto backend is a note", spec: "  security: {secureBoot: true}\n", sev: sevNote, rule: "secureboot-backend", field: "spec.runtime.backend"},
		{name: "maxCpu below cpu", spec: "  resources: {cpu: \"4\", memory: 2Gi, maxCpu: \"2\"}\n", sev: sevWarning, rule: "hotplug-headroom", field: "spec.resources.maxCpu"},
		{name: "maxMemory not above memory", spec: "  resources: {cpu: \"2\", memory: 4Gi, maxMemory: 2Gi}\n", sev: sevWarning, rule: "hotplug-headroom", field: "spec.resources.maxMemory"},
		{name: "elastic memory with hugepages", spec: "  resources: {cpu: \"2\", memory: 4Gi, hugepages: true, elasticMemory: {min: 1Gi}}\n", sev: sevError, rule: "elastic-memory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := pfGoodSpec + tc.spec
			// a later key replaces an earlier one only if we drop the default.
			for _, k := range []string{"image:", "resources:"} {
				if strings.Contains(tc.spec, "\n  "+k) || strings.HasPrefix(tc.spec, "  "+k) {
					spec = strings.Join(dropDefault(strings.Split(pfGoodSpec, "\n"), k), "\n") + tc.spec
				}
			}
			rep := pfRun(t, pfMachine(spec), false, preflightEnv{})
			if tc.sev == "" {
				if len(rep.Findings) != 0 {
					t.Fatalf("want clean, got %+v", rep.Findings)
				}
				return
			}
			if !pfHas(rep, tc.sev, tc.rule, tc.field) {
				t.Fatalf("want %s %s %s, got %+v", tc.sev, tc.rule, tc.field, rep.Findings)
			}
			if rep.Findings[0].Machine != "dev/vm1" {
				t.Errorf("machine label = %q", rep.Findings[0].Machine)
			}
		})
	}
}

func dropDefault(lines []string, key string) []string {
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), key) {
			out = append(out, l)
		}
	}
	return out
}

func TestPreflightMissingNameAndKind(t *testing.T) {
	rep := pfRun(t, "apiVersion: kairon.zyvor.dev/v1\nkind: Machine\nspec: {}\n", false, preflightEnv{})
	if !pfHas(rep, sevError, "schema", "metadata.name") {
		t.Fatalf("got %+v", rep.Findings)
	}
	rep = pfRun(t, "apiVersion: v1\nmetadata: {name: x}\n", false, preflightEnv{})
	if !pfHas(rep, sevError, "schema", "kind") {
		t.Fatalf("got %+v", rep.Findings)
	}
	rep = pfRun(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: x}\n", false, preflightEnv{})
	if !pfHas(rep, sevWarning, "schema", "kind") {
		t.Fatalf("got %+v", rep.Findings)
	}
}

func TestPreflightMissingRequiredOtherKind(t *testing.T) {
	doc := "apiVersion: kairon.zyvor.dev/v1\nkind: MachineSnapshotRestore\nmetadata: {name: r}\nspec: {}\n"
	rep := pfRun(t, doc, false, preflightEnv{})
	if !pfHas(rep, sevError, "schema", "spec.snapshotName") {
		t.Fatalf("got %+v", rep.Findings)
	}
	if !pfHas(rep, sevNote, "scope", "") {
		t.Fatalf("want a scope note, got %+v", rep.Findings)
	}
}

func TestPreflightMultiDocAndList(t *testing.T) {
	docs := pfMachine(pfGoodSpec) + "---\n" + strings.Replace(pfMachine(pfGoodSpec+"  bogus: 1\n"), "vm1", "vm2", 1) +
		"---\n# only a comment\n---\napiVersion: v1\nkind: List\nitems:\n" +
		"- {apiVersion: kairon.zyvor.dev/v1, kind: Machine, metadata: {name: vm3}, spec: {resources: {cpu: NaN, memory: 1Gi}}}\n"
	rep := pfRun(t, docs, false, preflightEnv{})
	var bad []string
	for _, f := range rep.Findings {
		if f.Severity == sevError {
			bad = append(bad, f.Machine)
		}
	}
	if strings.Join(bad, ",") != "dev/vm2,vm3" {
		t.Fatalf("error machines = %v, all: %+v", bad, rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Machine == "dev/vm1" {
			t.Errorf("vm1 is good: %+v", f)
		}
	}
}

func TestPreflightJSONInput(t *testing.T) {
	doc := `{"apiVersion":"kairon.zyvor.dev/v1","kind":"Machine","metadata":{"name":"j"},"spec":{"resources":{"cpu":"1","memory":"1Gi"},"image":{"path":"/a"}}}`
	if rep := pfRun(t, doc, false, preflightEnv{}); len(rep.Findings) != 0 {
		t.Fatalf("%+v", rep.Findings)
	}
}

func TestPreflightInvalidYAML(t *testing.T) {
	rep := pfRun(t, "kind: [unclosed\n", false, preflightEnv{})
	if !pfHas(rep, sevError, "parse", "") {
		t.Fatalf("%+v", rep.Findings)
	}
}

func TestPreflightImageDoctor(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.qcow2")
	if err := os.WriteFile(img, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := pfMachine("  image: {path: " + img + "}\n  resources: {cpu: \"1\", memory: 1Gi}\n")

	// guestkit missing: exactly one note, never an error.
	missing := preflightEnv{lookPath: func(string) (string, error) { return "", errors.New("not found") }}
	rep := pfRun(t, doc+"---\n"+strings.Replace(doc, "vm1", "vm2", 1), true, missing)
	notes := 0
	for _, f := range rep.Findings {
		if f.Rule == "image-doctor" {
			notes++
			if f.Severity != sevNote || !strings.Contains(f.Message, "guestkit not found") {
				t.Errorf("unexpected %+v", f)
			}
		}
	}
	if notes != 1 || rep.Summary.Errors != 0 {
		t.Fatalf("want one note, got %+v", rep.Findings)
	}

	var gotArgs string
	present := preflightEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/guestkit", nil },
		runDoctor: func(_ context.Context, bin, image string) ([]byte, error) {
			gotArgs = bin + " " + image
			return []byte(`{"report":{"boot_score": 82}}`), nil
		},
	}
	rep = pfRun(t, doc, true, present)
	if !pfHas(rep, sevNote, "image-doctor", "spec.image.path") || !strings.Contains(rep.Findings[0].Message, "82") {
		t.Fatalf("%+v", rep.Findings)
	}
	if gotArgs != "/usr/bin/guestkit "+img {
		t.Errorf("args %q", gotArgs)
	}

	// Not requested: guestkit is never looked up.
	called := false
	probe := preflightEnv{lookPath: func(string) (string, error) { called = true; return "", errors.New("x") }}
	pfRun(t, doc, false, probe)
	if called {
		t.Error("guestkit probed without --image-doctor")
	}

	// A path that is not on this machine is a note, and a guestkit failure a warning.
	rep = pfRun(t, pfMachine("  image: {path: /nonexistent/node-local.qcow2}\n"), true, present)
	if !pfHas(rep, sevNote, "image-doctor", "spec.image.path") {
		t.Fatalf("%+v", rep.Findings)
	}
	failing := preflightEnv{
		lookPath:  present.lookPath,
		runDoctor: func(context.Context, string, string) ([]byte, error) { return nil, errors.New("boom") },
	}
	rep = pfRun(t, doc, true, failing)
	if !pfHas(rep, sevWarning, "image-doctor", "") || rep.Summary.Errors != 0 {
		t.Fatalf("%+v", rep.Findings)
	}
}

func runPreflightCmd(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newPreflightCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func TestPreflightExitCodesAndStrict(t *testing.T) {
	good := pfMachine(pfGoodSpec)
	warn := pfMachine("  image: {path: /a}\n  resources: {cpu: \"4\", memory: 2Gi, maxCpu: \"2\"}\n")
	bad := pfMachine(pfGoodSpec + "  bogus: 1\n")
	code := func(err error) int {
		var ec *ExitCodeError
		if errors.As(err, &ec) {
			return ec.Code
		}
		if err != nil {
			return -1
		}
		return 0
	}
	for _, tc := range []struct {
		name, doc string
		args      []string
		want      int
	}{
		{"clean", good, nil, 0},
		{"warning passes", warn, nil, 0},
		{"warning fails with strict", warn, []string{"--strict"}, 1},
		{"error", bad, nil, 1},
		{"clean strict", good, []string{"--strict"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runPreflightCmd(t, tc.doc, append([]string{"-"}, tc.args...)...)
			if got := code(err); got != tc.want {
				t.Fatalf("exit %d, want %d (err %v)", got, tc.want, err)
			}
		})
	}
}

func TestPreflightOutputFormats(t *testing.T) {
	bad := pfMachine(pfGoodSpec + "  bogus: 1\n")
	out, _, _ := runPreflightCmd(t, bad, "-")
	if !strings.HasPrefix(out, "SEVERITY") || !strings.Contains(out, "MACHINE") || !strings.Contains(out, "FIELD") || !strings.Contains(out, "MESSAGE") || !strings.Contains(out, "spec.bogus") {
		t.Fatalf("table:\n%s", out)
	}
	out, _, _ = runPreflightCmd(t, bad, "-", "-o", "json")
	var rep preflightReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if rep.Summary.Errors != 1 || len(rep.Findings) != 1 || rep.Findings[0].Field != "spec.bogus" || rep.Findings[0].Machine != "dev/vm1" {
		t.Fatalf("%+v", rep)
	}
	out, _, _ = runPreflightCmd(t, bad, "-", "-o", "yaml")
	if !strings.Contains(out, "severity: error") || !strings.Contains(out, "field: spec.bogus") {
		t.Fatalf("yaml:\n%s", out)
	}
	out, _, _ = runPreflightCmd(t, pfMachine(pfGoodSpec), "-", "-o", "json")
	if !strings.Contains(out, `"findings": []`) {
		t.Fatalf("clean json must have an empty findings list:\n%s", out)
	}
	if _, _, err := runPreflightCmd(t, bad, "-", "-o", "xml"); err == nil {
		t.Error("want an error for -o xml")
	}
}

func TestPreflightFileErrors(t *testing.T) {
	if _, _, err := runPreflightCmd(t, "", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file must fail")
	}
	if _, _, err := runPreflightCmd(t, "", "-", "-"); err == nil {
		t.Error("stdin twice must fail")
	}
}

// TestPreflightRunNoKubeconfig drives the real entry point with no
// kubeconfig and an unreachable API address: it must neither need nor touch a cluster.
func TestPreflightRunNoKubeconfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("KUBECONFIG", filepath.Join(dir, "does-not-exist"))
	t.Setenv("KAIRON_KUBE_URL", "http://127.0.0.1:1")
	file := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(file, []byte(pfMachine(pfGoodSpec)+"---\n"+strings.Replace(pfMachine(pfGoodSpec+"  bogus: 1\n"), "vm1", "vm2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := Run([]string{"preflight", file, "-o", "json"}, "test")
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if code != 1 {
		t.Fatalf("exit %d, output %s", code, out)
	}
	var rep preflightReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Summary.Errors != 1 || rep.Findings[0].Machine != "dev/vm2" {
		t.Fatalf("%+v", rep)
	}
	if !strings.HasPrefix(rep.Findings[0].Source, file+"#2") {
		t.Errorf("source %q", rep.Findings[0].Source)
	}
}
