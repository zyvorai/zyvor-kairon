// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/model"
)

// This file implements `kaironctl preflight FILE...`: an offline check of
// Machine manifests. It reads files (or stdin) and, with --image-doctor,
// runs a local `guestkit` read-only; it never builds a cluster client.

const (
	sevError   = "error"
	sevWarning = "warning"
	sevNote    = "note"
)

// preflightFinding is one problem (or note) found in a manifest.
type preflightFinding struct {
	Severity string `json:"severity"`
	Machine  string `json:"machine"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
	// Rule names the check: "schema", an admission rule such as
	// "resource-quantities", or a preflight-only rule.
	Rule string `json:"rule"`
	// Source is "FILE" or "FILE#N" (document N, from 1) for multi-doc input.
	Source string `json:"source,omitempty"`
}

type preflightSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Notes    int `json:"notes"`
}

type preflightReport struct {
	Findings []preflightFinding `json:"findings"`
	Summary  preflightSummary   `json:"summary"`
}

func (r *preflightReport) add(f preflightFinding) {
	r.Findings = append(r.Findings, f)
	switch f.Severity {
	case sevError:
		r.Summary.Errors++
	case sevWarning:
		r.Summary.Warnings++
	default:
		r.Summary.Notes++
	}
}

// preflightEnv holds the seams for --image-doctor so tests need no guestkit.
type preflightEnv struct {
	lookPath  func(string) (string, error)
	runDoctor func(ctx context.Context, bin, image string) ([]byte, error)
}

func defaultPreflightEnv() preflightEnv {
	return preflightEnv{
		lookPath: exec.LookPath,
		runDoctor: func(ctx context.Context, bin, image string) ([]byte, error) {
			return exec.CommandContext(ctx, bin, "doctor", "--json", image).Output()
		},
	}
}

func newPreflightCmd() *cobra.Command {
	var output string
	var strict, imageDoctor bool
	cmd := &cobra.Command{
		Use:   "preflight FILE...",
		Short: "Check Machine manifests offline, before they are applied",
		Long: `Check Machine manifests (YAML or JSON, several documents per file, - for
stdin) before they are applied, using only what this binary knows:

  1. the CRD schema embedded in kaironctl (the one 'explain' prints):
     unknown fields, wrong types, bad enum values, missing required fields;
  2. the stateless rules the Machine admission webhook enforces, by calling
     the same functions (quantities, image source, disks, network, ...), plus
     the secureBoot/tpm backend rule kairon-controller applies at VM create
     and hotplug headroom notes;
  3. with --image-doctor, a read-only 'guestkit doctor --json' on a local
     spec.image.path, if guestkit is on PATH.

It never contacts a cluster and does not know cluster state: nodes, quotas,
instance types, images already present. It is not a substitute for the
admission webhook. Exit status is 1 if any error is found (warnings too with
--strict), else 0.`,
		Example: `  $ kaironctl preflight machine.yaml
  $ kaironctl preflight a.yaml b.yaml -o json
  $ kaironctl import kubevirt vm.yaml | kaironctl preflight - --strict
  $ kaironctl preflight machine.yaml --image-doctor`,
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				output = "table"
			}
			if output != "table" && output != "json" && output != "yaml" {
				return fmt.Errorf("unknown output %q (want table, json or yaml)", output)
			}
			rep, err := runPreflight(cmd.Context(), args, cmd.InOrStdin(), imageDoctor, defaultPreflightEnv())
			if err != nil {
				return err
			}
			if err := writePreflight(cmd.OutOrStdout(), rep, output); err != nil {
				return err
			}
			if rep.Summary.Errors > 0 || (strict && rep.Summary.Warnings > 0) {
				return &ExitCodeError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json|yaml")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit non-zero on warnings as well as errors")
	cmd.Flags().BoolVar(&imageDoctor, "image-doctor", false, "run guestkit doctor (read-only) on local spec.image.path files, if guestkit is installed")
	return cmd
}

func writePreflight(w io.Writer, rep *preflightReport, output string) error {
	switch output {
	case "json", "yaml":
		if rep.Findings == nil {
			rep.Findings = []preflightFinding{}
		}
		return writeStructured(w, rep, output)
	}
	if len(rep.Findings) == 0 {
		_, _ = fmt.Fprintln(w, "no problems found (offline checks only; the admission webhook may still refuse a Machine)")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SEVERITY\tMACHINE\tFIELD\tMESSAGE")
	for _, f := range rep.Findings {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Severity, f.Machine, orDash(f.Field), f.Message)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "\n%d error(s), %d warning(s), %d note(s)\n", rep.Summary.Errors, rep.Summary.Warnings, rep.Summary.Notes)
	return nil
}

// runPreflight reads every input and checks every document.
func runPreflight(ctx context.Context, files []string, stdin io.Reader, imageDoctor bool, env preflightEnv) (*preflightReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	kinds, err := loadExplainKinds()
	if err != nil {
		return nil, fmt.Errorf("read embedded CRDs: %w", err)
	}
	rep := &preflightReport{}
	pf := &preflighter{rep: rep, kinds: kinds, imageDoctor: imageDoctor, env: env, ctx: ctx}
	stdinUsed := false
	for _, f := range files {
		var r io.Reader
		name := f
		if f == "-" {
			if stdinUsed {
				return nil, errors.New("stdin (-) given more than once")
			}
			stdinUsed = true
			r, name = stdin, "stdin"
		} else {
			fh, err := os.Open(f)
			if err != nil {
				return nil, err
			}
			defer func() { _ = fh.Close() }()
			r = fh
		}
		if err := pf.readDocs(name, r); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if imageDoctor && pf.guestkitMissing {
		rep.add(preflightFinding{Severity: sevNote, Machine: "-", Rule: "image-doctor",
			Message: "--image-doctor skipped: guestkit not found on PATH (install it from https://github.com/zyvorai/zyvor-guestkit)"})
	}
	return rep, nil
}

type preflighter struct {
	rep             *preflightReport
	kinds           []explainKind
	imageDoctor     bool
	env             preflightEnv
	ctx             context.Context
	guestkitMissing bool
	guestkitPath    string
	guestkitProbed  bool
}

const preflightMaxInput = 16 << 20

func (p *preflighter) readDocs(name string, r io.Reader) error {
	yr := yaml.NewYAMLReader(bufio.NewReader(io.LimitReader(r, preflightMaxInput)))
	n := 0
	for {
		raw, err := yr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		n++
		src := fmt.Sprintf("%s#%d", name, n)
		var doc map[string]any
		if err := sigsyaml.Unmarshal(raw, &doc); err != nil {
			p.rep.add(preflightFinding{Severity: sevError, Machine: src, Rule: "parse", Source: src, Message: "not valid YAML/JSON: " + err.Error()})
			continue
		}
		if doc == nil {
			continue // a document holding only comments
		}
		p.checkDoc(src, doc)
	}
}

// checkDoc checks one decoded document, expanding a v1 List.
func (p *preflighter) checkDoc(src string, doc map[string]any) {
	if k, _ := doc["kind"].(string); k == "List" {
		items, _ := doc["items"].([]any)
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				p.rep.add(preflightFinding{Severity: sevError, Machine: src, Field: fmt.Sprintf("items[%d]", i), Rule: "schema", Source: src, Message: "list item is not an object"})
				continue
			}
			p.checkDoc(fmt.Sprintf("%s.items[%d]", src, i), m)
		}
		return
	}
	kind, _ := doc["kind"].(string)
	label := preflightLabel(src, doc)
	add := func(sev, field, rule, msg string) {
		p.rep.add(preflightFinding{Severity: sev, Machine: label, Field: field, Rule: rule, Source: src, Message: msg})
	}
	if kind == "" {
		add(sevError, "kind", "schema", "missing kind")
		return
	}
	ek, ok := findExplainKindExact(p.kinds, kind)
	if !ok {
		add(sevWarning, "kind", "schema", fmt.Sprintf("%s is not a Kairon kind known to this kaironctl; skipped", kind))
		return
	}
	if av, _ := doc["apiVersion"].(string); av != ek.Group+"/"+ek.Version && av != ek.Group+"/v1beta1" && av != ek.Group+"/v1alpha1" {
		add(sevError, "apiVersion", "schema", fmt.Sprintf("apiVersion %q: want %s/%s", av, ek.Group, ek.Version))
	}
	if md, _ := doc["metadata"].(map[string]any); md == nil || strOf(md["name"]) == "" {
		if md == nil || strOf(md["generateName"]) == "" {
			add(sevError, "metadata.name", "schema", "metadata.name is required")
		}
	}
	schemaErrs := false
	for _, e := range validateSchema(ek.Schema, doc, "") {
		if e.path == "apiVersion" || e.path == "kind" || e.path == "metadata" || strings.HasPrefix(e.path, "metadata.") {
			continue // checked above, or left to the API server by the CRD schema
		}
		schemaErrs = true
		add(sevError, e.path, "schema", e.msg)
	}
	if kind != model.KindMachine {
		add(sevNote, "", "scope", fmt.Sprintf("only the schema of %s was checked; semantic rules cover Machine", kind))
		return
	}
	// Decode into the typed model; type errors were already reported above.
	raw, _ := json.Marshal(doc)
	var m model.Machine
	if err := json.Unmarshal(raw, &m); err != nil {
		if !schemaErrs {
			add(sevError, "", "decode", "decode Machine: "+err.Error())
		}
		return
	}
	for _, c := range controller.OfflineMachineChecks(m) {
		add(sevError, errField(c.Err.Error()), c.Rule, c.Err.Error())
	}
	p.preflightOnly(m, add)
	if p.imageDoctor {
		p.doctorImage(m, add)
	}
}

// preflightOnly are rules that kairon enforces later than admission (at VM
// create) or documents as silent clamps; admission itself would accept them.
func (p *preflighter) preflightOnly(m model.Machine, add func(sev, field, rule, msg string)) {
	backend := m.Spec.Runtime.Backend
	explicit := backend != "" && backend != "auto"
	if explicit && m.Spec.Security.SecureBoot && backend != "qemu" {
		add(sevError, "spec.security.secureBoot", "secureboot-backend",
			fmt.Sprintf("secureBoot requires the qemu backend, not %q (rejected by kairon-controller at VM create, not by the webhook)", backend))
	}
	if explicit && m.Spec.Security.TPM && backend != "qemu" && backend != "cloud-hypervisor" {
		add(sevError, "spec.security.tpm", "tpm-backend",
			fmt.Sprintf("tpm requires the qemu or cloud-hypervisor backend, not %q (rejected by kairon-controller at VM create, not by the webhook)", backend))
	}
	if !explicit && (m.Spec.Security.SecureBoot || m.Spec.Security.TPM) {
		add(sevNote, "spec.runtime.backend", "secureboot-backend",
			"secureBoot/tpm depend on the backend; with backend unset or auto the node default decides, which is not known offline. Set backend: qemu to be explicit")
	}
	res := m.Spec.Resources
	if cpu, err := model.ParseVCPUs(res.CPU); err == nil && res.MaxCPU != "" {
		if mx, err := model.ParseVCPUs(res.MaxCPU); err == nil && mx < cpu {
			add(sevWarning, "spec.resources.maxCpu", "hotplug-headroom",
				fmt.Sprintf("maxCpu %s is below cpu %s; FluxVM silently raises it to cpu, so there is no hotplug headroom", res.MaxCPU, res.CPU))
		}
	}
	if mem, err := model.ParseMemoryMiB(res.Memory); err == nil && res.MaxMemory != "" {
		if mx, err := model.ParseMemoryMiB(res.MaxMemory); err == nil && mx <= mem {
			add(sevWarning, "spec.resources.maxMemory", "hotplug-headroom",
				fmt.Sprintf("maxMemory %s is not above memory %s; it is ignored and FluxVM uses its default headroom", res.MaxMemory, res.Memory))
		}
	}
}

// doctorImage runs guestkit on a local spec.image.path.
func (p *preflighter) doctorImage(m model.Machine, add func(sev, field, rule, msg string)) {
	path := m.Spec.Image.Path
	if path == "" {
		return
	}
	if !p.guestkitProbed {
		p.guestkitProbed = true
		bin, err := p.env.lookPath("guestkit")
		if err != nil {
			p.guestkitMissing = true
		}
		p.guestkitPath = bin
	}
	if p.guestkitMissing {
		return
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		add(sevNote, "spec.image.path", "image-doctor", "image-doctor skipped: "+path+" is not a file on this machine (image paths are node-local)")
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 2*time.Minute)
	defer cancel()
	out, err := p.env.runDoctor(ctx, p.guestkitPath, path)
	if err != nil {
		add(sevWarning, "spec.image.path", "image-doctor", "guestkit doctor failed: "+err.Error())
		return
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		add(sevWarning, "spec.image.path", "image-doctor", "guestkit doctor returned output that is not JSON")
		return
	}
	score, ok := findBootScore(v)
	if !ok {
		add(sevNote, "spec.image.path", "image-doctor", "guestkit doctor ran but reported no boot score")
		return
	}
	add(sevNote, "spec.image.path", "image-doctor", fmt.Sprintf("guestkit boot score for %s: %s", path, fmtScore(score)))
}

func fmtScore(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.2f", f)
}

// findBootScore looks for a numeric boot score anywhere in guestkit's JSON
// (keys boot_score, bootScore or boot-score, case-insensitively). The exact
// guestkit output schema is not pinned in this repo, hence the loose match.
func findBootScore(v any) (float64, bool) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			nk := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
			if f, ok := t[k].(float64); ok && nk == "bootscore" {
				return f, true
			}
		}
		for _, k := range keys {
			if f, ok := findBootScore(t[k]); ok {
				return f, true
			}
		}
	case []any:
		for _, e := range t {
			if f, ok := findBootScore(e); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func strOf(v any) string { s, _ := v.(string); return s }

func preflightLabel(src string, doc map[string]any) string {
	md, _ := doc["metadata"].(map[string]any)
	name := strOf(md["name"])
	if name == "" {
		return src
	}
	if ns := strOf(md["namespace"]); ns != "" {
		return ns + "/" + name
	}
	return name
}

var errFieldRE = regexp.MustCompile(`^((?:spec|metadata|status)\.[A-Za-z0-9_.\[\]-]*[A-Za-z0-9_\]])`)

// errField extracts a leading field path ("spec.resources.cpu: ...") from an
// admission error message.
func errField(msg string) string {
	if m := errFieldRE.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

// findExplainKindExact matches only the Kind, not plurals or aliases: a
// manifest's kind field is case-sensitive.
func findExplainKindExact(kinds []explainKind, kind string) (explainKind, bool) {
	for _, k := range kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return explainKind{}, false
}

type schemaIssue struct{ path, msg string }

// validateSchema checks v against s: required fields, unknown fields (only
// where the schema lists properties and does not preserve unknown fields),
// types and enums. Constraints the embedded schemaNode does not model
// (patterns, min/max, oneOf) are not checked here.
func validateSchema(s *schemaNode, v any, path string) []schemaIssue {
	if s == nil || v == nil {
		return nil
	}
	here := path
	if here == "" {
		here = "(root)"
	}
	var out []schemaIssue
	switch s.Type {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return []schemaIssue{{here, fmt.Sprintf("must be an object, got %s", jsonKind(v))}}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, req := range s.Required {
			if _, ok := obj[req]; !ok {
				out = append(out, schemaIssue{join(path, req), "required field is missing"})
			}
		}
		for _, k := range keys {
			child, known := s.Properties[k]
			switch {
			case known:
				out = append(out, validateSchema(&child, obj[k], join(path, k))...)
			case s.AdditionalProperties != nil:
				out = append(out, validateSchema(s.AdditionalProperties, obj[k], join(path, k))...)
			case len(s.Properties) > 0 && !s.PreserveUnknownFields:
				out = append(out, schemaIssue{join(path, k), "unknown field (not in the schema this kaironctl ships; the API server would prune or reject it)"})
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return []schemaIssue{{here, fmt.Sprintf("must be a list, got %s", jsonKind(v))}}
		}
		for i, e := range arr {
			out = append(out, validateSchema(s.Items, e, fmt.Sprintf("%s[%d]", path, i))...)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return []schemaIssue{{here, fmt.Sprintf("must be a string, got %s", jsonKind(v))}}
		}
		out = append(out, checkEnum(s, str, here)...)
	case "integer":
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) {
			return []schemaIssue{{here, fmt.Sprintf("must be an integer, got %s", jsonKind(v))}}
		}
		out = append(out, checkEnum(s, v, here)...)
	case "number":
		if _, ok := v.(float64); !ok {
			return []schemaIssue{{here, fmt.Sprintf("must be a number, got %s", jsonKind(v))}}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return []schemaIssue{{here, fmt.Sprintf("must be a boolean, got %s", jsonKind(v))}}
		}
	}
	return out
}

func checkEnum(s *schemaNode, v any, path string) []schemaIssue {
	if len(s.Enum) == 0 {
		return nil
	}
	want := make([]string, 0, len(s.Enum))
	for _, e := range s.Enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return nil
		}
		want = append(want, fmt.Sprint(e))
	}
	return []schemaIssue{{path, fmt.Sprintf("%v is not one of: %s", v, strings.Join(want, ", "))}}
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func jsonKind(v any) string {
	switch t := v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if t == math.Trunc(t) {
			return "integer"
		}
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "list"
	}
	return fmt.Sprintf("%T", v)
}
