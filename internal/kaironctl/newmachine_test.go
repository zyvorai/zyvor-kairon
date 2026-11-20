// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/charts"
	"github.com/zyvorai/kairon/internal/model"
)

// machineCRDSchema loads the openAPIV3Schema of the Machine CRD embedded in
// the binary (the same files `kaironctl explain` reads).
func machineCRDSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := charts.ChartFS.ReadFile("kairon/crds/machines.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	vers := crd["spec"].(map[string]any)["versions"].([]any)
	return vers[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
}

// validateCRDSchemaMap checks v against the structural subset of OpenAPI v3 the
// Machine CRD uses. CEL rules (x-kubernetes-validations) are not evaluated.
func validateCRDSchemaMap(s map[string]any, v any, path string) []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, path+": "+fmt.Sprintf(f, a...)) }
	if s["x-kubernetes-int-or-string"] == true {
		switch v.(type) {
		case string, float64:
			return nil
		}
		add("want int or string")
		return errs
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			add("want object, got %T", v)
			return errs
		}
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, have := obj[r.(string)]; !have {
					add("missing required field %q", r)
				}
			}
		}
		addl, hasAddl := s["additionalProperties"].(map[string]any)
		for k, child := range obj {
			switch {
			case props[k] != nil:
				errs = append(errs, validateCRDSchemaMap(props[k].(map[string]any), child, path+"."+k)...)
			case hasAddl:
				errs = append(errs, validateCRDSchemaMap(addl, child, path+"."+k)...)
			case s["x-kubernetes-preserve-unknown-fields"] == true || (len(props) == 0 && s["additionalProperties"] == true):
			// The API server owns apiVersion, kind and metadata.
			case path == "" && (k == "apiVersion" || k == "kind" || k == "metadata"):
			default:
				add("unknown field %q", k)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			add("want array, got %T", v)
			return errs
		}
		if n, ok := s["maxItems"].(float64); ok && float64(len(arr)) > n {
			add("more than %v items", n)
		}
		if n, ok := s["minItems"].(float64); ok && float64(len(arr)) < n {
			add("fewer than %v items", n)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, e := range arr {
				errs = append(errs, validateCRDSchemaMap(items, e, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			add("want string, got %T", v)
			return errs
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			add("%q does not match %s", str, p)
		}
		if n, ok := s["minLength"].(float64); ok && float64(len(str)) < n {
			add("shorter than %v", n)
		}
		if n, ok := s["maxLength"].(float64); ok && float64(len(str)) > n {
			add("longer than %v", n)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			add("want boolean, got %T", v)
		}
	case "integer", "number":
		n, ok := v.(float64)
		if !ok {
			add("want number, got %T", v)
			return errs
		}
		if m, ok := s["minimum"].(float64); ok && n < m {
			add("%v below minimum %v", n, m)
		}
		if m, ok := s["maximum"].(float64); ok && n > m {
			add("%v above maximum %v", n, m)
		}
	}
	if en, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if reflect.DeepEqual(e, v) {
				found = true
			}
		}
		if !found {
			add("%v not in enum %v", v, en)
		}
	}
	return errs
}

// emittedPaths lists the comment-table path of every key in a tree.
func emittedPaths(n *onode, path string, out *[]string) {
	switch n.kind {
	case 'o':
		for i, k := range n.keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			*out = append(*out, p)
			emittedPaths(n.vals[i], p, out)
		}
	case 'a':
		for _, it := range n.items {
			emittedPaths(it, path+"[]", out)
		}
	}
}

func scaffold(t *testing.T, o newMachineOptions) (model.Machine, string) {
	t.Helper()
	m, err := buildNewMachine(o)
	if err != nil {
		t.Fatalf("buildNewMachine(%+v): %v", o, err)
	}
	p, _ := findMachinePreset(o.Preset)
	text, err := renderMachineYAML(m, o.Preset, p)
	if err != nil {
		t.Fatal(err)
	}
	return m, text
}

// checkScaffold runs every check the generated manifest must pass.
func checkScaffold(t *testing.T, o newMachineOptions) {
	t.Helper()
	m, text := scaffold(t, o)
	schema := machineCRDSchema(t)

	// 1. YAML parses and satisfies the embedded CRD schema.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("YAML does not parse: %v\n%s", err, text)
	}
	if errs := validateCRDSchemaMap(schema, doc, ""); len(errs) > 0 {
		t.Errorf("CRD schema violations:\n%s\n%s", strings.Join(errs, "\n"), text)
	}
	// 2. Strict decode into the real type: no field outside model.Machine.
	jsonDoc, _ := json.Marshal(doc)
	dec := json.NewDecoder(bytes.NewReader(jsonDoc))
	dec.DisallowUnknownFields()
	var back model.Machine
	if err := dec.Decode(&back); err != nil {
		t.Errorf("strict decode into model.Machine: %v", err)
	}
	// 3. The admission webhook's pure checks.
	if err := validateNewMachine(back); err != nil {
		t.Errorf("admission checks: %v", err)
	}
	// 4. The YAML carries exactly what the struct says, and JSON matches.
	js, err := renderMachineJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON map[string]any
	if err := json.Unmarshal([]byte(js), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, fromJSON) {
		t.Errorf("YAML and JSON output differ:\nyaml: %v\njson: %v", doc, fromJSON)
	}
	// 5. Every emitted field has a comment.
	tree, err := machineTree(m)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	emittedPaths(tree, "", &paths)
	for _, p := range paths {
		if commentFor(p) == "" {
			t.Errorf("no comment for emitted field %s", p)
		}
	}
	if !strings.Contains(text, "# ") || !strings.HasPrefix(text, "# Generated by") {
		t.Errorf("missing comments:\n%s", text)
	}
}

func TestNewMachineEveryPresetIsValid(t *testing.T) {
	for _, p := range machinePresets {
		t.Run(p.Name, func(t *testing.T) {
			checkScaffold(t, newMachineOptions{Name: "demo", Namespace: "team-a", Preset: p.Name})
		})
	}
	// Default preset when none is given.
	m, err := buildNewMachine(newMachineOptions{Name: "demo"})
	if err != nil || m.Spec.Resources.CPU != "2" || m.Metadata.Namespace != "default" {
		t.Fatalf("default preset: %+v %v", m, err)
	}
}

func TestNewMachineSandboxKeepsEmptySandbox(t *testing.T) {
	_, text := scaffold(t, newMachineOptions{Name: "s", Preset: "sandbox"})
	if !strings.Contains(text, "sandbox: {}") {
		t.Errorf("sandbox opt-in was pruned:\n%s", text)
	}
	m, _ := buildNewMachine(newMachineOptions{Name: "s", Preset: "sandbox"})
	js, _ := renderMachineJSON(m)
	if !strings.Contains(js, `"sandbox": {}`) {
		t.Errorf("sandbox opt-in missing from JSON:\n%s", js)
	}
}

func TestNewMachineOverrides(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(key, []byte("# my key\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest user@host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ci := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(ci, []byte("#cloud-config\nhostname: custom\npackages: [htop]\nruncmd:\n  - echo hi\nusers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warns []string
	o := newMachineOptions{
		Name: "web", Namespace: "prod", Preset: "small", CPU: "6", Memory: "12Gi", DiskSize: "250Gi",
		Image: "catalog:ubuntu-2404", SSHKeyFile: key, CloudInitFile: ci, SecureBoot: true, TPM: true,
		Warnf: func(s string) { warns = append(warns, s) },
	}
	checkScaffold(t, o)
	warns = nil
	m, err := buildNewMachine(o)
	if err != nil {
		t.Fatal(err)
	}
	s := m.Spec
	if m.Metadata.Namespace != "prod" || s.Resources.CPU != "6" || s.Resources.Memory != "12Gi" || s.Image.DiskSize != "250Gi" {
		t.Errorf("overrides not applied: %+v", m)
	}
	if s.Image.CatalogName != "ubuntu-2404" || s.Image.Path != "" {
		t.Errorf("image ref not applied: %+v", s.Image)
	}
	if !s.Security.SecureBoot || !s.Security.TPM {
		t.Errorf("security not applied: %+v", s.Security)
	}
	if len(s.CloudInit.SSHAuthorizedKeys) != 1 || !strings.HasPrefix(s.CloudInit.SSHAuthorizedKeys[0], "ssh-ed25519 ") {
		t.Errorf("ssh key: %v", s.CloudInit.SSHAuthorizedKeys)
	}
	if s.CloudInit.Hostname != "custom" || len(s.CloudInit.Packages) != 1 || len(s.CloudInit.RunCmd) != 1 {
		t.Errorf("cloud-init: %+v", s.CloudInit)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "users") {
		t.Errorf("want one warning about the unmapped key, got %v", warns)
	}
	// Image forms.
	for ref, want := range map[string]model.ImageSpec{
		"/images/a.qcow2": {Path: "/images/a.qcow2", DiskSize: "40Gi"},
		"image:ubuntu":    {ImageRef: "ubuntu", DiskSize: "40Gi"},
	} {
		m, err := buildNewMachine(newMachineOptions{Name: "x", Image: ref})
		if err != nil || m.Spec.Image != want {
			t.Errorf("--image %s: %+v %v", ref, m.Spec.Image, err)
		}
	}
	// Instance type replaces cpu/memory unless overridden.
	m, err = buildNewMachine(newMachineOptions{Name: "x", InstanceType: "std", ClusterInstanceType: true, Preference: "linux", CPU: "3"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Spec.InstanceTypeName != "std" || m.Spec.InstanceTypeKind != model.KindMachineClusterInstanceType ||
		m.Spec.Resources.CPU != "3" || m.Spec.Resources.Memory != "" || m.Spec.PreferenceName != "linux" {
		t.Errorf("instance type: %+v", m.Spec)
	}
}

func TestNewMachineRejectsBadValues(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id")
	if err := os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nSECRETSECRET\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(dir, "junk.pub")
	_ = os.WriteFile(junk, []byte("hello world\n"), 0o600)
	script := filepath.Join(dir, "script.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o600)

	cases := []struct {
		name string
		o    newMachineOptions
		want string
	}{
		{"negative cpu", newMachineOptions{Name: "a", CPU: "-2"}, "spec.resources.cpu"},
		{"zero cpu", newMachineOptions{Name: "a", CPU: "0"}, "spec.resources.cpu"},
		{"text cpu", newMachineOptions{Name: "a", CPU: "lots"}, "spec.resources.cpu"},
		{"bad memory", newMachineOptions{Name: "a", Memory: "-1Gi"}, "spec.resources.memory"},
		{"bad disk", newMachineOptions{Name: "a", DiskSize: "big"}, "diskSize"},
		{"zero disk", newMachineOptions{Name: "a", DiskSize: "0"}, "diskSize"},
		{"uppercase name", newMachineOptions{Name: "Web"}, "invalid Machine name"},
		{"underscore name", newMachineOptions{Name: "my_vm"}, "invalid Machine name"},
		{"long name", newMachineOptions{Name: strings.Repeat("a", 64)}, "invalid Machine name"},
		{"empty name", newMachineOptions{Name: ""}, "invalid Machine name"},
		{"bad namespace", newMachineOptions{Name: "a", Namespace: "Bad_NS"}, "invalid namespace"},
		{"unknown preset", newMachineOptions{Name: "a", Preset: "huge"}, "unknown preset"},
		{"relative image", newMachineOptions{Name: "a", Image: "ubuntu.qcow2"}, "--image"},
		{"sandbox secure boot", newMachineOptions{Name: "a", Preset: "sandbox", SecureBoot: true}, "sandbox"},
		{"private key", newMachineOptions{Name: "a", SSHKeyFile: priv}, "private key"},
		{"junk key", newMachineOptions{Name: "a", SSHKeyFile: junk}, "not an SSH public key"},
		{"missing key", newMachineOptions{Name: "a", SSHKeyFile: filepath.Join(dir, "none")}, "--ssh-key-file"},
		{"script cloud-init", newMachineOptions{Name: "a", CloudInitFile: script}, "#cloud-config"},
		{"cluster flag alone", newMachineOptions{Name: "a", ClusterInstanceType: true}, "--instance-type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildNewMachine(tc.o)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "SECRETSECRET") {
				t.Errorf("error leaks key material: %v", err)
			}
		})
	}
}

func TestNewMachineSchemaValidatorCatchesErrors(t *testing.T) {
	schema := machineCRDSchema(t)
	bad := map[string]any{
		"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
		"spec": map[string]any{"bogusField": 1, "resources": map[string]any{"cpu": 2.0}},
	}
	errs := strings.Join(validateCRDSchemaMap(schema, bad, ""), "\n")
	if !strings.Contains(errs, "bogusField") || !strings.Contains(errs, "want string") {
		t.Errorf("validator missed errors: %q", errs)
	}
}

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	// No kubeconfig anywhere: the command must not need one.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("HOME", t.TempDir())
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	code := Run(args, "test")
	_ = w.Close()
	os.Stdout = old
	return code, <-done
}

func TestRunNewMachineOffline(t *testing.T) {
	code, out := runCLI(t, "new", "machine", "web-1", "--preset", "database", "-n", "prod", "--cpu", "8")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var doc model.Machine
	if err := yaml.UnmarshalStrict([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a Machine: %v\n%s", err, out)
	}
	if doc.Metadata.Name != "web-1" || doc.Metadata.Namespace != "prod" || doc.Spec.Resources.CPU != "8" || doc.Spec.Resources.Memory != "16Gi" {
		t.Errorf("unexpected manifest: %+v", doc)
	}

	code, out = runCLI(t, "new", "machine", "web-1", "--preset", "small", "-o", "json")
	if code != 0 || !json.Valid([]byte(out)) {
		t.Fatalf("json output (exit %d): %s", code, out)
	}

	code, out = runCLI(t, "new", "machine", "--list-presets")
	if code != 0 {
		t.Fatalf("list-presets exit %d", code)
	}
	for _, p := range machinePresets {
		if !strings.Contains(out, p.Name) {
			t.Errorf("--list-presets misses %s:\n%s", p.Name, out)
		}
	}

	for _, args := range [][]string{
		{"new", "machine"},
		{"new", "machine", "Bad_Name"},
		{"new", "machine", "ok", "--cpu", "-1"},
		{"new", "machine", "ok", "-o", "toml"},
		{"new", "machine", "ok", "--preset", "nope"},
	} {
		if code, _ := runCLI(t, args...); code == 0 {
			t.Errorf("%v: want a non-zero exit", args)
		}
	}
}
