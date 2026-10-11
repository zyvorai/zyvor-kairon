// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// completionKinds are the resource spellings `get`, `describe`, `delete` and
// `edit` accept, in the short form shell completion offers.
var completionKinds = []string{
	"backuppolicies", "backups", "backuprestores", "budgets", "claims", "instancetypes", "machinepools", "machinesets",
	"machines", "migrationpolicies", "migrations", "networkpolicies", "nodes", "quotas", "restores",
	"securitygroups", "snapshotschedules", "snapshots",
}

// completeKindsThenNames completes `VERB KIND NAME`: the kind first, then the
// names of that kind from the cluster. It works with DisableFlagParsing
// commands, where every word (including -n NS) arrives in args.
func completeKindsThenNames(namespace *string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		ns, words := completionNamespace(args, namespace)
		words = positional(words)
		switch len(words) {
		case 0:
			return filterPrefix(completionKinds, toComplete), cobra.ShellCompDirectiveNoFileComp
		case 1:
			return completeNames(words[0], ns, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeMachineNames completes the Machine argument of power and per-Machine verbs.
func completeMachineNames(namespace *string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		ns, words := completionNamespace(args, namespace)
		if len(positional(words)) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeNames("machines", ns, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completionNamespace extracts -n/--namespace from the typed words, falling
// back to the root flag value.
func completionNamespace(args []string, rootNS *string) (string, []string) {
	ns := "default"
	if rootNS != nil && *rootNS != "" {
		ns = *rootNS
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-n" || args[i] == "--namespace") && i+1 < len(args):
			ns = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--namespace="):
			ns = strings.TrimPrefix(args[i], "--namespace=")
		default:
			rest = append(rest, args[i])
		}
	}
	return ns, rest
}

func positional(words []string) []string {
	var out []string
	for _, w := range words {
		if !strings.HasPrefix(w, "-") {
			out = append(out, w)
		}
	}
	return out
}

func filterPrefix(all []string, prefix string) []string {
	var out []string
	for _, s := range all {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// completeNames lists object names of kind in ns. Any failure yields no
// suggestions: completion must never print errors into the shell.
func completeNames(kind, ns, prefix string) []string {
	plural, core, ok := resourcePlural(kind)
	if !ok {
		return nil
	}
	kc, err := newKubeClient()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := kubeGetJSON(ctx, kc, listPath(plural, core, ns), &list); err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		if strings.HasPrefix(it.Metadata.Name, prefix) {
			names = append(names, it.Metadata.Name)
		}
	}
	sort.Strings(names)
	return names
}
