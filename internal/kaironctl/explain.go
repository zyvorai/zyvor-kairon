// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/charts"
)

// crdDoc is the slice of a CustomResourceDefinition `kaironctl explain`
// reads. It comes from the CRDs embedded in the binary, so explain works
// offline and documents exactly the schema this kaironctl ships.
type crdDoc struct {
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Plural     string   `json:"plural"`
			Singular   string   `json:"singular"`
			Kind       string   `json:"kind"`
			ShortNames []string `json:"shortNames"`
		} `json:"names"`
		Versions []struct {
			Name   string `json:"name"`
			Served bool   `json:"served"`
			Schema struct {
				OpenAPIV3Schema *schemaNode `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// schemaNode is the subset of OpenAPI v3 used for field documentation.
type schemaNode struct {
	Type                  string                `json:"type,omitempty"`
	Format                string                `json:"format,omitempty"`
	Description           string                `json:"description,omitempty"`
	Required              []string              `json:"required,omitempty"`
	Enum                  []any                 `json:"enum,omitempty"`
	Default               any                   `json:"default,omitempty"`
	Properties            map[string]schemaNode `json:"properties,omitempty"`
	Items                 *schemaNode           `json:"items,omitempty"`
	AdditionalProperties  *schemaNode           `json:"-"`
	PreserveUnknownFields bool                  `json:"x-kubernetes-preserve-unknown-fields,omitempty"`
}

// UnmarshalJSON decodes additionalProperties, which may be a bool or a schema.
func (s *schemaNode) UnmarshalJSON(b []byte) error {
	type plain schemaNode
	aux := struct {
		*plain
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	s.AdditionalProperties = nil
	if len(aux.AdditionalProperties) > 0 && aux.AdditionalProperties[0] == '{' {
		var ap schemaNode
		if err := json.Unmarshal(aux.AdditionalProperties, &ap); err != nil {
			return err
		}
		s.AdditionalProperties = &ap
	}
	return nil
}

// explainKind is one resource kind with the schema of its served version.
type explainKind struct {
	Kind, Plural, Group, Version string
	Aliases                      []string
	Schema                       *schemaNode
}

// loadExplainKinds reads every CRD embedded in the chart.
func loadExplainKinds() ([]explainKind, error) {
	return loadExplainKindsFS(charts.ChartFS, "kairon/crds")
}

func loadExplainKindsFS(fsys fs.FS, dir string) ([]explainKind, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []explainKind
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		for _, doc := range strings.Split(string(data), "\n---") {
			var crd crdDoc
			if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			if crd.Spec.Names.Kind == "" {
				continue
			}
			k := explainKind{Kind: crd.Spec.Names.Kind, Plural: crd.Spec.Names.Plural, Group: crd.Spec.Group}
			k.Aliases = append([]string{crd.Spec.Names.Singular}, crd.Spec.Names.ShortNames...)
			for _, v := range crd.Spec.Versions {
				if v.Schema.OpenAPIV3Schema != nil {
					k.Version, k.Schema = v.Name, v.Schema.OpenAPIV3Schema
					if v.Served {
						break
					}
				}
			}
			if k.Schema != nil {
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

// findExplainKind matches a kind, plural, singular, short name or any
// spelling `kaironctl get` accepts, case-insensitively.
func findExplainKind(kinds []explainKind, name string) (explainKind, bool) {
	want := strings.ToLower(name)
	plural, _, plok := resourcePlural(name)
	for _, k := range kinds {
		if strings.ToLower(k.Kind) == want || strings.ToLower(k.Plural) == want || (plok && k.Plural == plural) {
			return k, true
		}
		for _, a := range k.Aliases {
			if a != "" && strings.ToLower(a) == want {
				return k, true
			}
		}
	}
	return explainKind{}, false
}

// walkField follows a dotted field path (spec.resources.cpu) from root.
// Array items are traversed transparently, as in kubectl explain.
func walkField(root *schemaNode, path string) (*schemaNode, error) {
	cur := root
	if path == "" {
		return cur, nil
	}
	walked := []string{}
	for _, part := range strings.Split(path, ".") {
		for cur.Type == "array" && cur.Items != nil {
			cur = cur.Items
		}
		next, ok := cur.Properties[part]
		if !ok {
			if cur.AdditionalProperties != nil {
				cur = cur.AdditionalProperties
				walked = append(walked, part)
				continue
			}
			where := "the resource"
			if len(walked) > 0 {
				where = strings.Join(walked, ".")
			}
			return nil, fmt.Errorf("field %q does not exist in %s", part, where)
		}
		cur = &next
		walked = append(walked, part)
	}
	return cur, nil
}

func schemaTypeName(s *schemaNode) string {
	switch {
	case s.Type == "array" && s.Items != nil:
		return "[]" + schemaTypeName(s.Items)
	case s.Type == "object" && s.AdditionalProperties != nil:
		return "map[string]" + schemaTypeName(s.AdditionalProperties)
	case s.Type == "":
		if s.PreserveUnknownFields {
			return "object"
		}
		return "any"
	}
	return s.Type
}

// explainJSON is the machine-readable form (-o json|yaml).
type explainJSON struct {
	Kind        string        `json:"kind"`
	Group       string        `json:"group"`
	Version     string        `json:"version"`
	Field       string        `json:"field,omitempty"`
	Type        string        `json:"type"`
	Description string        `json:"description,omitempty"`
	Required    bool          `json:"required,omitempty"`
	Enum        []any         `json:"enum,omitempty"`
	Fields      []explainJSON `json:"fields,omitempty"`
}

func fieldEntry(name string, s *schemaNode, required bool) explainJSON {
	return explainJSON{Field: name, Type: schemaTypeName(s), Description: strings.TrimSpace(s.Description), Required: required, Enum: s.Enum}
}

// childFields lists the documented sub-fields of s (through array items).
func childFields(s *schemaNode) []explainJSON {
	for s.Type == "array" && s.Items != nil {
		s = s.Items
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]explainJSON, 0, len(names))
	for _, n := range names {
		p := s.Properties[n]
		out = append(out, fieldEntry(n, &p, req[n]))
	}
	return out
}

func buildExplain(k explainKind, path string) (explainJSON, error) {
	node, err := walkField(k.Schema, path)
	if err != nil {
		return explainJSON{}, err
	}
	e := fieldEntry(path, node, false)
	e.Kind, e.Group, e.Version = k.Kind, k.Group, k.Version
	if path == "" {
		e.Type = "object"
	}
	e.Fields = childFields(node)
	return e, nil
}

func writeExplain(w io.Writer, e explainJSON, path, output string) error {
	switch output {
	case "json":
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "yaml":
		b, err := yaml.Marshal(e)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case "", "text":
	default:
		return fmt.Errorf("unknown output format %q (want text, json or yaml)", output)
	}
	fmt.Fprintf(w, "KIND:     %s\nGROUP:    %s\nVERSION:  %s\n", e.Kind, e.Group, e.Version)
	if path != "" {
		fmt.Fprintf(w, "\nFIELD:    %s <%s>\n", path, e.Type)
	}
	if e.Description != "" {
		fmt.Fprintf(w, "\nDESCRIPTION:\n    %s\n", strings.ReplaceAll(e.Description, "\n", "\n    "))
	}
	if len(e.Enum) > 0 {
		vals := make([]string, len(e.Enum))
		for i, v := range e.Enum {
			vals[i] = fmt.Sprint(v)
		}
		fmt.Fprintf(w, "\nVALUES:\n    %s\n", strings.Join(vals, ", "))
	}
	if len(e.Fields) > 0 {
		fmt.Fprintln(w, "\nFIELDS:")
		for _, f := range e.Fields {
			req := ""
			if f.Required {
				req = " -required-"
			}
			fmt.Fprintf(w, "  %s <%s>%s\n", f.Field, f.Type, req)
			if f.Description != "" {
				fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(f.Description, "\n", "\n    "))
			}
		}
	}
	return nil
}

func newExplainCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "explain RESOURCE[.FIELD[.FIELD...]]",
		Short: "Show the documented fields of a Kairon resource (offline)",
		Long: `Print the field documentation of a Kairon resource, taken from the CRD
schemas embedded in this kaironctl binary. It needs no cluster access and
shows the schema this build ships, not what a cluster currently serves.

RESOURCE is a kind, plural, singular or short name (machine, vm,
machinemigrations). Append a dotted path to descend into a field. Without an
argument the known resource kinds are listed.`,
		Example: `  $ kaironctl explain machine
  $ kaironctl explain machine.spec.resources
  $ kaironctl explain machinemigration.spec.strategy -o json`,
		Args: cobra.MaximumNArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 || strings.Contains(toComplete, ".") {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			kinds, err := loadExplainKinds()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			names := make([]string, 0, len(kinds))
			for _, k := range kinds {
				names = append(names, strings.ToLower(k.Kind))
			}
			return filterPrefix(names, strings.ToLower(toComplete)), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			kinds, err := loadExplainKinds()
			if err != nil {
				return fmt.Errorf("read embedded CRDs: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				for _, k := range kinds {
					fmt.Fprintf(out, "%-40s %s/%s\n", strings.ToLower(k.Kind), k.Group, k.Version)
				}
				return nil
			}
			name, path, _ := strings.Cut(args[0], ".")
			k, ok := findExplainKind(kinds, name)
			if !ok {
				return fmt.Errorf("unknown resource %q (run `kaironctl explain` to list kinds)", name)
			}
			e, err := buildExplain(k, path)
			if err != nil {
				return err
			}
			return writeExplain(out, e, path, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text|json|yaml")
	return cmd
}
