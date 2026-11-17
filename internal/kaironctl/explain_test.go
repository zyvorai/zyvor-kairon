// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

const explainTestCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: example.dev
  names: {plural: widgets, singular: widget, kind: Widget, shortNames: [wd]}
  versions:
  - name: v1
    served: true
    schema:
      openAPIV3Schema:
        type: object
        description: A widget.
        properties:
          spec:
            type: object
            required: [size]
            properties:
              size: {type: string, description: How big.}
              mode: {type: string, enum: [fast, slow]}
              ports:
                type: array
                items:
                  type: object
                  properties:
                    port: {type: integer, description: Port number.}
              labels:
                type: object
                additionalProperties: {type: string}
`

func testKinds(t *testing.T) []explainKind {
	t.Helper()
	fsys := fstest.MapFS{"crds/widgets.yaml": {Data: []byte(explainTestCRD)}}
	kinds, err := loadExplainKindsFS(fsys, "crds")
	if err != nil {
		t.Fatal(err)
	}
	return kinds
}

func TestFindExplainKind(t *testing.T) {
	kinds := testKinds(t)
	for _, n := range []string{"Widget", "widgets", "wd", "WIDGET"} {
		if _, ok := findExplainKind(kinds, n); !ok {
			t.Errorf("findExplainKind(%q) not found", n)
		}
	}
	if _, ok := findExplainKind(kinds, "nope"); ok {
		t.Error("unexpected match for nope")
	}
}

func TestWalkFieldThroughArraysAndMaps(t *testing.T) {
	k := testKinds(t)[0]
	n, err := walkField(k.Schema, "spec.ports.port")
	if err != nil || n.Type != "integer" {
		t.Fatalf("spec.ports.port = %+v, %v", n, err)
	}
	if n, err = walkField(k.Schema, "spec.labels.anything"); err != nil || n.Type != "string" {
		t.Fatalf("map value = %+v, %v", n, err)
	}
	if _, err = walkField(k.Schema, "spec.bogus"); err == nil || !strings.Contains(err.Error(), "spec") {
		t.Fatalf("want error naming spec, got %v", err)
	}
}

func TestExplainTextAndJSON(t *testing.T) {
	k := testKinds(t)[0]
	e, err := buildExplain(k, "spec")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeExplain(&buf, e, "spec", "text"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"KIND:     Widget", "FIELD:    spec <object>", "size <string> -required-", "How big.", "ports <[]object>", "labels <map[string]string>"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	e, _ = buildExplain(k, "spec.mode")
	buf.Reset()
	_ = writeExplain(&buf, e, "spec.mode", "text")
	if !strings.Contains(buf.String(), "fast, slow") {
		t.Errorf("enum values missing:\n%s", buf.String())
	}
	buf.Reset()
	if err := writeExplain(&buf, e, "spec.mode", "json"); err != nil {
		t.Fatal(err)
	}
	var got explainJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Type != "string" || len(got.Enum) != 2 {
		t.Fatalf("bad json %q: %+v %v", buf.String(), got, err)
	}
	if err := writeExplain(&buf, e, "", "xml"); err == nil {
		t.Error("want error for unknown format")
	}
}

// The embedded chart CRDs must load and cover the core kinds, so a CRD
// that explain cannot parse fails CI rather than a user.
func TestExplainEmbeddedCRDs(t *testing.T) {
	kinds, err := loadExplainKinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) < 20 {
		t.Fatalf("only %d kinds loaded from the embedded chart", len(kinds))
	}
	for _, name := range []string{"machine", "vm", "machinemigrations", "MachineSnapshot"} {
		k, ok := findExplainKind(kinds, name)
		if !ok {
			t.Fatalf("%s not found", name)
		}
		if _, err := buildExplain(k, "spec"); err != nil {
			t.Errorf("%s spec: %v", name, err)
		}
	}
	k, _ := findExplainKind(kinds, "machine")
	if _, err := buildExplain(k, "spec.resources.cpu"); err != nil {
		t.Errorf("machine spec.resources.cpu: %v", err)
	}
}

func TestExplainCmd(t *testing.T) {
	cmd := newExplainCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"machine.spec.resources", "-o", "yaml"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "kind: Machine") {
		t.Errorf("yaml output:\n%s", buf.String())
	}
	cmd = newExplainCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"nonesuch"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown resource") {
		t.Errorf("want unknown resource error, got %v", err)
	}
}
