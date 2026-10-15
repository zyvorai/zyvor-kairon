// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// printManifest prints obj without the zero values Go's encoder keeps
// (empty strings and structs, a zero creationTimestamp), so the output
// reads like, and applies as, a hand-written manifest.
func printManifest(w io.Writer, obj any, format string) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	v, _ = pruneEmpty(v)
	return printStructured(w, v, format)
}

func pruneEmpty(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case string:
		return t, t != "" && t != "0001-01-01T00:00:00Z"
	case map[string]any:
		for k, child := range t {
			if c, keep := pruneEmpty(child); keep {
				t[k] = c
			} else {
				delete(t, k)
			}
		}
		return t, len(t) > 0
	case []any:
		for i, e := range t {
			if c, keep := pruneEmpty(e); keep {
				t[i] = c
			}
		}
		return t, len(t) > 0
	}
	return v, true
}

// createOutput is the --dry-run / -o pair every `create` kind shares.
type createOutput struct {
	dryRun bool
	format string
}

func createOutputFlags(fs *flag.FlagSet) *createOutput {
	o := &createOutput{}
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the object instead of creating it")
	fs.StringVar(&o.format, "output", "", "yaml or json: print the object (with --dry-run, defaults to yaml)")
	fs.StringVar(&o.format, "o", "", "shorthand for --output")
	return o
}

// preview validates -o and, under --dry-run, prints obj and reports that
// the caller must stop before creating anything.
func (o *createOutput) preview(obj any) bool {
	if err := checkOutput(o.format); err != nil {
		fatal(err)
	}
	if !o.dryRun {
		return false
	}
	format := o.format
	if format == "" {
		format = "yaml"
	}
	if err := printManifest(os.Stdout, obj, format); err != nil {
		fatal(err)
	}
	return true
}

// done prints the created object when -o was given, else the usual line.
func (o *createOutput) done(obj any, format string, args ...any) {
	if o.format == "" {
		okf(format, args...)
		return
	}
	if err := printManifest(os.Stdout, obj, o.format); err != nil {
		fatal(err)
	}
}

// argsDryRun reports whether a legacy flag list asks for --dry-run, so
// create can run without a cluster.
func argsDryRun(args []string) bool {
	for _, a := range args {
		switch a {
		case "--dry-run", "-dry-run", "--dry-run=true", "-dry-run=true":
			return true
		}
	}
	return false
}

func newCreateCmd(opts *Options) *cobra.Command {
	cmd := legacyCmd(opts, "create", "Create a Machine or other Kairon resource", createExamples, cmdCreate)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args = extractConnFlags(args)
		kc, err := newKubeClient()
		if err != nil && !argsDryRun(args) {
			return err
		}
		cmdCreate(ctx, kc, createArgsWithNamespace(opts.Namespace, args))
		return nil
	}
	return cmd
}

// createArgsWithNamespace moves -n/--namespace (or the root default) to
// the end of create's arguments: they start with KIND/NAME positionals,
// and flag parsing stops at the first one.
func createArgsWithNamespace(ns string, args []string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--namespace" || a == "-namespace":
			if i+1 < len(args) {
				ns = args[i+1]
				i++
				continue
			}
		case strings.HasPrefix(a, "--namespace=") || strings.HasPrefix(a, "-namespace=") || strings.HasPrefix(a, "-n="):
			ns = a[strings.IndexByte(a, '=')+1:]
			continue
		}
		out = append(out, a)
	}
	if ns == "" || ns == "default" {
		return out
	}
	return append(out, "--namespace", ns)
}

// specSource is the subset of *kube.Client expandMachine reads.
type specSource interface {
	GetMachineInstanceType(ctx context.Context, ns, name string) (model.MachineInstanceType, error)
	GetMachineClusterInstanceType(ctx context.Context, name string) (model.MachineClusterInstanceType, error)
	GetMachinePreference(ctx context.Context, ns, name string) (model.MachinePreference, error)
	GetMachineClusterPreference(ctx context.Context, name string) (model.MachineClusterPreference, error)
}

// expandMachine applies m's instance type and preference the way
// kairon-controller will (internal/controller/instancetype.go): resources
// only when spec.resources is still empty, preference fields only where
// unset. It returns what it did, one line each.
func expandMachine(ctx context.Context, src specSource, m *model.Machine) ([]string, error) {
	if err := model.ValidateInstanceTypeRef(m.Spec); err != nil {
		return nil, err
	}
	var notes []string
	ns := m.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	if name := m.Spec.InstanceTypeName; name != "" {
		kind := m.Spec.InstanceTypeKind
		if kind == "" {
			kind = model.KindMachineInstanceType
		}
		if m.Spec.Resources.CPU != "" || m.Spec.Resources.Memory != "" {
			notes = append(notes, fmt.Sprintf("%s/%s not applied: spec.resources is already set", kind, name))
		} else {
			var res model.ResourceSpec
			if kind == model.KindMachineClusterInstanceType {
				it, err := src.GetMachineClusterInstanceType(ctx, name)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", kind, name, err)
				}
				res = it.Spec.Resources
			} else {
				it, err := src.GetMachineInstanceType(ctx, ns, name)
				if err != nil {
					return nil, fmt.Errorf("%s %s/%s: %w", kind, ns, name, err)
				}
				res = it.Spec.Resources
			}
			m.Spec.Resources = res
			notes = append(notes, fmt.Sprintf("%s/%s: cpu %s, memory %s", kind, name, orDash(res.CPU), orDash(res.Memory)))
		}
	}
	if name := m.Spec.PreferenceName; name != "" {
		kind := m.Spec.PreferenceKind
		if kind == "" {
			kind = model.KindMachinePreference
		}
		if applied := m.Metadata.Annotations[model.AnnotationPreferenceApplied]; applied != "" {
			notes = append(notes, fmt.Sprintf("%s/%s not applied: %s was already applied", kind, name, applied))
		} else {
			var pref model.MachinePreferenceSpec
			if kind == model.KindMachineClusterPreference {
				p, err := src.GetMachineClusterPreference(ctx, name)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", kind, name, err)
				}
				pref = p.Spec
			} else {
				p, err := src.GetMachinePreference(ctx, ns, name)
				if err != nil {
					return nil, fmt.Errorf("%s %s/%s: %w", kind, ns, name, err)
				}
				pref = p.Spec
			}
			if model.ApplyPreference(&m.Spec, pref) {
				notes = append(notes, fmt.Sprintf("%s/%s: filled unset guest-OS fields", kind, name))
			} else {
				notes = append(notes, fmt.Sprintf("%s/%s: nothing to fill, every field it covers is already set", kind, name))
			}
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "no instance type or preference referenced; the spec is already complete")
	}
	return notes, nil
}

func readMachineManifest(r io.Reader) (model.Machine, error) {
	var m model.Machine
	b, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return m, err
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse Machine manifest: %w", err)
	}
	if m.Kind != "" && m.Kind != model.KindMachine {
		return m, fmt.Errorf("manifest is a %s, want a Machine", m.Kind)
	}
	return m, nil
}

func newExpandCmd(opts *Options) *cobra.Command {
	var file, output string
	cmd := &cobra.Command{
		Use:   "expand (MACHINE | -f FILE)",
		Short: "Print a Machine with its instance type and preference applied",
		Long: `Shows the spec kairon-controller will end up with: spec.resources from the
referenced instance type (only while spec.resources is empty) and unset
guest-OS fields from the referenced preference. Reads the Machine from the
cluster, or from a manifest with -f (- for stdin). Changes nothing.`,
		Example: `  $ kaironctl expand web-1
  $ kaironctl create web-2 --image /img/u.qcow2 --instance-type m.large --dry-run | kaironctl expand -f -`,
		Args: func(cmd *cobra.Command, args []string) error {
			if (file == "") == (len(args) == 0) || len(args) > 1 {
				return fmt.Errorf("give a MACHINE name or -f FILE")
			}
			return nil
		},
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				output = "yaml"
			}
			if err := checkOutput(output); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			var m model.Machine
			if file != "" {
				r := cmd.InOrStdin()
				if file != "-" {
					f, err := os.Open(file)
					if err != nil {
						return err
					}
					defer func() { _ = f.Close() }()
					r = f
				}
				if m, err = readMachineManifest(r); err != nil {
					return err
				}
				if m.Metadata.Namespace == "" {
					m.Metadata.Namespace = opts.Namespace
				}
			} else if m, err = kc.GetMachine(cmd.Context(), opts.Namespace, args[0]); err != nil {
				return err
			}
			notes, err := expandMachine(cmd.Context(), kc, &m)
			if err != nil {
				return err
			}
			for _, n := range notes {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "# "+n)
			}
			m.Status = model.MachineStatus{}
			return printManifest(cmd.OutOrStdout(), m, output)
		},
	}
	cmd.Flags().StringVarP(&file, "filename", "f", "", "Machine manifest (YAML or JSON); - reads stdin")
	cmd.Flags().StringVarP(&output, "output", "o", "yaml", "yaml or json")
	return cmd
}

var _ specSource = (*kube.Client)(nil)
