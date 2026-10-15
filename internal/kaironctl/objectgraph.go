// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// graphNode is one object related to a Machine. Relation says how the
// parent refers to it (spec field, label or owner link).
type graphNode struct {
	Kind      string      `json:"kind"`
	Name      string      `json:"name"`
	Namespace string      `json:"namespace,omitempty"`
	Relation  string      `json:"relation,omitempty"`
	Status    string      `json:"status,omitempty"`
	Children  []graphNode `json:"children,omitempty"`
}

// machineRelated is everything objectgraph lists around one Machine.
// Lists are namespace-scoped except Services, filtered here.
type machineRelated struct {
	Claims      []model.MachineClaim
	Snapshots   []model.MachineSnapshot
	Backups     []model.MachineBackup
	Exports     []model.MachineExport
	Migrations  []model.MachineMigration
	NetPolicies []model.MachineNetworkPolicy
	Budgets     []model.MachineDisruptionBudget
	Services    []kube.Service
}

func labelsMatch(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func buildObjectGraph(m model.Machine, rel machineRelated) graphNode {
	ns := m.Metadata.Namespace
	root := graphNode{Kind: "Machine", Name: m.Metadata.Name, Namespace: ns, Status: m.Status.Phase}
	add := func(kind, name, namespace, relation, status string) {
		if name == "" {
			return
		}
		root.Children = append(root.Children, graphNode{Kind: kind, Name: name, Namespace: namespace, Relation: relation, Status: status})
	}
	labels := m.Metadata.Labels

	add("Node", m.Status.NodeName, "", "status.nodeName", "")
	add("MachineSet", labels[model.LabelMachineSet], ns, "label "+model.LabelMachineSet, "")
	add("MachinePool", labels[model.LabelMachinePool], ns, "label "+model.LabelMachinePool, labels[model.LabelPoolState])
	if claim := labels[model.LabelMachineClaim]; claim != "" {
		status := ""
		for _, c := range rel.Claims {
			if c.Metadata.Name == claim {
				status = c.Status.Phase
			}
		}
		add("MachineClaim", claim, ns, "label "+model.LabelMachineClaim, status)
	}
	if kind := m.Spec.InstanceTypeKind; m.Spec.InstanceTypeName != "" {
		scope := ns
		switch kind {
		case "":
			kind = "MachineInstanceType"
		case "MachineClusterInstanceType":
			scope = ""
		}
		add(kind, m.Spec.InstanceTypeName, scope, "spec.instanceTypeName", "")
	}
	if kind := m.Spec.PreferenceKind; m.Spec.PreferenceName != "" {
		scope := ns
		switch kind {
		case "":
			kind = "MachinePreference"
		case "MachineClusterPreference":
			scope = ""
		}
		add(kind, m.Spec.PreferenceName, scope, "spec.preferenceName", "")
	}
	for _, v := range m.Spec.Volumes {
		add("PersistentVolumeClaim", v.ClaimName, ns, "spec.volumes["+v.Name+"]", "")
	}
	for _, d := range m.Spec.Disks {
		add("PersistentVolumeClaim", d.ClaimName, ns, "spec.disks["+d.Name+"]", "")
	}
	for _, d := range m.Spec.DeviceClaims {
		add("ResourceClaim", d.Name, ns, "spec.deviceClaims", "")
	}
	name := m.Metadata.Name
	for _, s := range rel.Snapshots {
		if s.Spec.MachineName == name {
			add("MachineSnapshot", s.Metadata.Name, ns, "spec.machineName", s.Status.Phase)
		}
	}
	for _, b := range rel.Backups {
		if b.Spec.MachineName == name {
			add("MachineBackup", b.Metadata.Name, ns, "spec.machineName", b.Status.Phase)
		}
	}
	for _, e := range rel.Exports {
		if e.Spec.MachineName == name {
			add("MachineExport", e.Metadata.Name, ns, "spec.machineName", e.Status.Phase)
		}
	}
	for _, mig := range rel.Migrations {
		if mig.Spec.MachineName == name {
			add("MachineMigration", mig.Metadata.Name, ns, "spec.machineName", mig.Status.Phase)
		}
	}
	for _, p := range rel.NetPolicies {
		switch {
		case p.Spec.MachineName == name:
			add("MachineNetworkPolicy", p.Metadata.Name, ns, "spec.machineName", "")
		case p.Spec.MachineName == "" && labelsMatch(p.Spec.Selector, labels):
			add("MachineNetworkPolicy", p.Metadata.Name, ns, "spec.selector", "")
		}
	}
	for _, b := range rel.Budgets {
		if labelsMatch(b.Spec.Selector, labels) {
			add("MachineDisruptionBudget", b.Metadata.Name, ns, "spec.selector", "")
		}
	}
	for _, s := range rel.Services {
		if s.Metadata.Namespace == ns && s.Metadata.Labels[kube.LabelExposeMachine] == name {
			add("Service", s.Metadata.Name, ns, "label "+kube.LabelExposeMachine, s.Spec.Type)
		}
	}
	sort.SliceStable(root.Children, func(i, j int) bool {
		a, b := root.Children[i], root.Children[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return root
}

func (n graphNode) label() string {
	s := n.Kind + "/" + n.Name
	if n.Status != "" {
		s += " (" + n.Status + ")"
	}
	return s
}

func writeGraphTree(w io.Writer, root graphNode) {
	_, _ = fmt.Fprintln(w, root.label())
	for i, c := range root.Children {
		branch := "├── "
		if i == len(root.Children)-1 {
			branch = "└── "
		}
		_, _ = fmt.Fprintf(w, "%s%s  [%s]\n", branch, c.label(), c.Relation)
	}
}

func dotQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func writeGraphDot(w io.Writer, root graphNode) {
	_, _ = fmt.Fprintln(w, "digraph objectgraph {")
	_, _ = fmt.Fprintln(w, "  rankdir=LR;")
	_, _ = fmt.Fprintln(w, "  node [shape=box];")
	id := func(n graphNode) string { return dotQuote(n.Kind + "/" + n.Namespace + "/" + n.Name) }
	_, _ = fmt.Fprintf(w, "  %s [label=%s, style=bold];\n", id(root), dotQuote(root.label()))
	for _, c := range root.Children {
		_, _ = fmt.Fprintf(w, "  %s [label=%s];\n", id(c), dotQuote(c.label()))
		_, _ = fmt.Fprintf(w, "  %s -> %s [label=%s];\n", id(root), id(c), dotQuote(c.Relation))
	}
	_, _ = fmt.Fprintln(w, "}")
}

func writeObjectGraph(w io.Writer, root graphNode, format string) error {
	switch format {
	case "", "tree":
		writeGraphTree(w, root)
	case "dot":
		writeGraphDot(w, root)
	case "json":
		b, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, string(b))
	case "yaml":
		b, err := yaml.Marshal(root)
		if err != nil {
			return err
		}
		_, _ = w.Write(b)
	default:
		return fmt.Errorf("unknown --output %q (tree|json|yaml|dot)", format)
	}
	return nil
}

// collectRelated lists every related kind; a list the caller may not read
// is reported and skipped so the graph still shows what is visible.
func collectRelated(ctx context.Context, kc *kube.Client, ns string) (machineRelated, []string) {
	var rel machineRelated
	var warns []string
	note := func(kind string, err error) {
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", kind, err))
		}
	}
	var err error
	rel.Claims, err = kc.ListMachineClaimsNamespace(ctx, ns)
	note("machineclaims", err)
	rel.Snapshots, err = kc.ListMachineSnapshotsNamespace(ctx, ns)
	note("machinesnapshots", err)
	rel.Backups, err = kc.ListMachineBackupsNamespace(ctx, ns)
	note("machinebackups", err)
	rel.Exports, err = kc.ListMachineExportsNamespace(ctx, ns)
	note("machineexports", err)
	rel.Migrations, err = kc.ListMachineMigrationsNamespace(ctx, ns)
	note("machinemigrations", err)
	rel.NetPolicies, err = kc.ListMachineNetworkPoliciesNamespace(ctx, ns)
	note("machinenetworkpolicies", err)
	rel.Budgets, err = kc.ListMachineDisruptionBudgetsNamespace(ctx, ns)
	note("machinedisruptionbudgets", err)
	rel.Services, err = kc.ListExposedServices(ctx)
	note("services", err)
	return rel, warns
}

func newObjectGraphCmd(opts *Options) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "objectgraph MACHINE",
		Short: "Show the objects a Machine references or is referenced by",
		Long: `List every Kubernetes object tied to a Machine -- node, owning MachineSet,
pool and claim, instance type and preference, PVCs, device claims,
snapshots, backups, exports, migrations, network policies, disruption
budgets and exposed Services -- like virtctl objectgraph. Use -o dot and
pipe into Graphviz for a picture.`,
		Example: `  $ kaironctl objectgraph web-1
  $ kaironctl objectgraph web-1 -o json
  $ kaironctl objectgraph web-1 -o dot | dot -Tsvg > web-1.svg`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := writeObjectGraph(io.Discard, graphNode{}, output); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			rel, warns := collectRelated(cmd.Context(), kc, opts.Namespace)
			for _, w := range warns {
				_, _ = fmt.Fprintf(os.Stderr, "warning: skipped %s\n", w)
			}
			return writeObjectGraph(cmd.OutOrStdout(), buildObjectGraph(m, rel), output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "tree", "output format: tree|json|yaml|dot")
	return cmd
}
