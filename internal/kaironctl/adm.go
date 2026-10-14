// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/loglevel"
	"github.com/zyvorai/kairon/internal/model"
)

func newAdmCmd() *cobra.Command {
	adm := &cobra.Command{
		Use:   "adm",
		Short: "Cluster administration commands",
	}
	adm.AddCommand(newLogVerbosityCmd())
	return adm
}

// logLevelPatch returns the ConfigMap data patch for components; a nil
// value removes the key so the component falls back to info.
func logLevelPatch(components []string, node, level string, reset bool) (map[string]any, error) {
	if !reset {
		if _, err := loglevel.Parse(level); err != nil {
			return nil, err
		}
		level = strings.ToLower(level)
	}
	patch := map[string]any{}
	for _, c := range components {
		if reset {
			patch[loglevel.Key(c, node)] = nil
		} else {
			patch[loglevel.Key(c, node)] = level
		}
	}
	return patch, nil
}

func writeLogLevels(w io.Writer, data map[string]string) error {
	keys := []string{}
	for k := range data {
		keys = append(keys, k)
	}
	for _, c := range loglevel.Components {
		if _, ok := data[c]; !ok {
			keys = append(keys, c)
		}
	}
	sort.Strings(keys)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "COMPONENT\tLEVEL")
	for _, k := range keys {
		v := data[k]
		if v == "" {
			v = "info (default)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", k, v)
	}
	return tw.Flush()
}

func applyLogLevels(ctx context.Context, kc *kube.Client, ns string, patch map[string]any) error {
	err := kc.PatchConfigMapData(ctx, ns, loglevel.ConfigMapName, patch)
	if !kube.IsNotFound(err) {
		return err
	}
	data := map[string]string{}
	for k, v := range patch {
		if s, ok := v.(string); ok {
			data[k] = s
		}
	}
	if len(data) == 0 {
		return nil
	}
	err = kc.CreateConfigMap(ctx, model.ConfigMap{
		Metadata: model.ObjectMeta{Name: loglevel.ConfigMapName, Namespace: ns},
		Data:     data,
	})
	if kube.IsConflict(err) {
		return kc.PatchConfigMapData(ctx, ns, loglevel.ConfigMapName, patch)
	}
	return err
}

func newLogVerbosityCmd() *cobra.Command {
	var component, node, level, systemNS string
	var reset bool
	cmd := &cobra.Command{
		Use:   "log-verbosity",
		Short: "Show or change Kairon components' log level at runtime",
		Long: `Each kairon-controller, kairon-ui and kairon-node polls the kairon-log-levels
ConfigMap in the Kairon namespace every 15s and applies the level without a
restart -- like virtctl adm log-verbosity. --node narrows a kairon-node
change to one node. With no --level or --reset, prints the current levels.`,
		Example: `  $ kaironctl adm log-verbosity
  $ kaironctl adm log-verbosity --component controller --level debug
  $ kaironctl adm log-verbosity --component node --node worker-1 --level debug
  $ kaironctl adm log-verbosity --component all --reset`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			if level == "" && !reset {
				cm, err := kc.GetConfigMap(cmd.Context(), systemNS, loglevel.ConfigMapName)
				if err != nil && !kube.IsNotFound(err) {
					return err
				}
				return writeLogLevels(cmd.OutOrStdout(), cm.Data)
			}
			if level != "" && reset {
				return fmt.Errorf("--level and --reset are mutually exclusive")
			}
			components := []string{component}
			switch {
			case component == "all":
				components = loglevel.Components
			case !slices.Contains(loglevel.Components, component):
				return fmt.Errorf("--component must be controller, ui, node or all")
			}
			if node != "" && component != "node" {
				return fmt.Errorf("--node needs --component node")
			}
			patch, err := logLevelPatch(components, node, level, reset)
			if err != nil {
				return err
			}
			if err := applyLogLevels(cmd.Context(), kc, systemNS, patch); err != nil {
				return err
			}
			target := strings.Join(components, ", ")
			if node != "" {
				target = "node " + node
			}
			if reset {
				okf("log level for %s reset to info (applies within %s)", target, loglevel.PollInterval)
			} else {
				okf("log level for %s set to %s (applies within %s)", target, strings.ToLower(level), loglevel.PollInterval)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&component, "component", "all", "controller|ui|node|all")
	cmd.Flags().StringVar(&node, "node", "", "only this kairon-node (with --component node)")
	cmd.Flags().StringVar(&level, "level", "", "debug|info|warn|error")
	cmd.Flags().BoolVar(&reset, "reset", false, "remove the override so the component logs at info")
	cmd.Flags().StringVar(&systemNS, "system-namespace", "kairon-system", "namespace Kairon is installed in")
	return cmd
}
