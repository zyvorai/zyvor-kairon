// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func newNetworkCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Inspect Machine network / eBPF dataplane status",
		Long: `Network diagnostics for Machines and policies (FluxVM TC/eBPF edge).

  kaironctl network status MACHINE       dataplane, Cilium attach, CNP sync
  kaironctl network flows MACHINE        recent eBPF flows
  kaironctl network drop-reasons MACHINE recent drop reasons
  kaironctl network drops MACHINE        attributed drops (policy, spoof, sni)
  kaironctl network stats MACHINE        dataplane stats
  kaironctl network effective MACHINE    merged effective policy
  kaironctl network observe              drops summarised across Machines (-A, --by, --top)
  kaironctl network policies             list MachineNetworkPolicies
  kaironctl network identity MACHINE     stable eBPF identity (survives IP move)
  kaironctl network capture MACHINE      bounded packet capture (max 30s), --output FILE for the pcap
  kaironctl network captures MACHINE     capture sessions and their state
`,
	}
	cmd.AddCommand(newNetworkStatusCmd(opts))
	cmd.AddCommand(newNetworkPoliciesCmd(opts))
	cmd.AddCommand(newNetworkObserveCmd(opts))
	cmd.AddCommand(newNetworkObservabilityCmd(opts, "flows", "network-flows", "Show recent eBPF flows for a Machine"))
	cmd.AddCommand(newNetworkObservabilityCmd(opts, "drop-reasons", "network-drop-reasons", "Show recent eBPF drop reasons for a Machine"))
	cmd.AddCommand(newNetworkObservabilityCmd(opts, "drops", "network-drops", "Show attributed eBPF drops for a Machine"))
	cmd.AddCommand(newNetworkObservabilityCmd(opts, "stats", "network-stats", "Show eBPF dataplane stats for a Machine"))
	cmd.AddCommand(newNetworkObservabilityCmd(opts, "effective", "network-effective", "Show merged effective network policy for a Machine"))
	cmd.AddCommand(newNetworkObservabilityCmd(opts, "captures", "network-capture", "List packet captures for a Machine"))
	for _, sub := range newNetworkEdgeCmds(opts) {
		cmd.AddCommand(sub)
	}
	return cmd
}

const networkPoliciesExamples = `  $ kaironctl network policies
  $ kaironctl -n default network policies`

func newNetworkPoliciesCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "policies",
		Aliases: []string{"get", "networkpolicies", "networkpolicy"},
		Short:   "List MachineNetworkPolicies",
		Example: networkPoliciesExamples,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			items, err := kc.ListMachineNetworkPoliciesNamespace(ctx, opts.Namespace)
			if err != nil {
				return err
			}
			fmt.Printf("NAME\tTARGET\tDEFAULT-ALLOW\tPHASE\tSYNCED\n")
			for _, p := range items {
				target := p.Spec.MachineName
				if target == "" && len(p.Spec.Selector) > 0 {
					parts := make([]string, 0, len(p.Spec.Selector))
					for k, v := range p.Spec.Selector {
						parts = append(parts, k+"="+v)
					}
					sort.Strings(parts)
					target = strings.Join(parts, ",")
				}
				if target == "" {
					target = "-"
				}
				fmt.Printf("%s\t%s\t%t\t%s\t%s\n",
					p.Metadata.Name,
					target,
					p.Spec.Policy.DefaultAllow,
					style.Phase(os.Stdout, p.Status.Phase),
					style.BoolReady(os.Stdout, p.Status.EffectiveSynced),
				)
				if p.Status.CiliumNetworkPolicyRef != "" {
					style.Log(style.EmojiOK, "%s CNP → %s", p.Metadata.Name, p.Status.CiliumNetworkPolicyRef)
				}
			}
			return nil
		},
	}
}

func newNetworkStatusCmd(opts *Options) *cobra.Command {
	var flows, dropReasons bool
	var limit int
	cmd := &cobra.Command{
		Use:   "status MACHINE",
		Short: "Show dataplane / Cilium attach status for a Machine",
		Example: `  $ kaironctl network status web
  $ kaironctl network status web --flows
  $ kaironctl network status web --drop-reasons`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			return runNetworkStatus(ctx, kc, opts.Namespace, args[0], flows, dropReasons, limit)
		},
	}
	cmd.Flags().BoolVar(&flows, "flows", false, "also print recent eBPF flows (via UI or node console proxy)")
	cmd.Flags().BoolVar(&dropReasons, "drop-reasons", false, "also print recent drop reasons")
	cmd.Flags().IntVar(&limit, "limit", 10, "limit for --flows / --drop-reasons")
	return cmd
}

func newNetworkObservabilityCmd(opts *Options, use, kind, short string) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:               use + " MACHINE",
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(ctx, opts.Namespace, args[0])
			if err != nil {
				return err
			}
			return printObservability(ctx, m, kind, limit)
		},
	}
	if use == "flows" || use == "drop-reasons" || use == "drops" {
		cmd.Flags().IntVar(&limit, "limit", 10, "max entries to request")
	}
	return cmd
}

func runNetworkStatus(ctx context.Context, kc *kube.Client, ns, name string, flows, dropReasons bool, limit int) error {
	style.Log(style.EmojiDetect, "Inspecting network for %s/%s", ns, name)
	m, err := kc.GetMachine(ctx, ns, name)
	if err != nil {
		style.Failf("%v", err)
		return err
	}

	fmt.Println(style.Wrap(os.Stdout, style.Cyan+style.Bold, "Machine "+m.Metadata.Name))
	fmt.Printf("  phase: %s\n", style.Phase(os.Stdout, m.Status.Phase))

	net := m.Status.Network
	if net == nil {
		style.Log(style.EmojiWarn, "No status.network projected yet")
	} else {
		printDataplane(net)
		printCilium(m, net)
	}

	printPolicyHints(ctx, kc, m)

	if flows {
		if err := printObservability(ctx, m, "network-flows", limit); err != nil {
			style.Log(style.EmojiWarn, "flows: %v", err)
		}
	}
	if dropReasons {
		if err := printObservability(ctx, m, "network-drop-reasons", limit); err != nil {
			style.Log(style.EmojiWarn, "drop-reasons: %v", err)
		}
	}

	style.Log(style.EmojiOK, "Network status complete")
	return nil
}

func printDataplane(net *model.MachineNetworkStatus) {
	if net.Dataplane == nil {
		style.Log(style.EmojiWarn, "Dataplane status unavailable (legacy FluxVM or not yet attached)")
		return
	}
	dp := net.Dataplane
	mode := dp.Mode
	if mode == "" {
		mode = "?"
	}
	if dp.Attached {
		style.Log(style.EmojiOK, "Dataplane mode=%s attached identity=%d", mode, dp.Identity)
	} else {
		style.Log(style.EmojiFail, "Dataplane mode=%s not attached", mode)
	}
	if dp.PolicySynced {
		style.Log(style.EmojiOK, "Policy synced fingerprint=%s", dash(dp.PolicyFingerprint))
	} else if mode == "ebpf" || mode == "cilium" {
		style.Log(style.EmojiWarn, "Policy not synced yet")
	}
}

func printCilium(m model.Machine, net *model.MachineNetworkStatus) {
	if !m.Spec.Network.CiliumAttach && (net.Cilium == nil || net.Cilium.ExternalWorkload == "") {
		return
	}
	if !m.Spec.Network.CiliumAttach {
		style.Log(style.EmojiInfo, "Cilium attach not requested on this Machine")
		return
	}
	style.Log(style.EmojiDetect, "Cilium attach requested (tap+netns)")
	if net.Cilium == nil {
		style.Log(style.EmojiWait, "Awaiting CiliumExternalWorkload projection")
		return
	}
	c := net.Cilium
	if c.Message != "" {
		style.Log(style.EmojiFail, "Cilium: %s", c.Message)
		return
	}
	if c.ExternalWorkload != "" {
		style.Log(style.EmojiOK, "ExternalWorkload %s", c.ExternalWorkload)
	}
	if c.Identity != 0 {
		style.Log(style.EmojiOK, "Cilium identity %d", c.Identity)
	} else {
		style.Log(style.EmojiWait, "Cilium identity pending")
	}
	if c.IPv4 != "" {
		style.Log(style.EmojiOK, "Cilium IPv4 %s", c.IPv4)
	}
	if m.Spec.Network.PodUID != "" {
		style.Log(style.EmojiOK, "podUID %s", m.Spec.Network.PodUID)
	}
}

func printPolicyHints(ctx context.Context, kc *kube.Client, m model.Machine) {
	policies, err := kc.ListMachineNetworkPoliciesNamespace(ctx, m.Namespace())
	if err != nil {
		return
	}
	for _, p := range policies {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		match := false
		if p.Spec.MachineName != "" {
			match = p.Spec.MachineName == m.Metadata.Name
		} else {
			match = model.LabelsMatch(m.Metadata.Labels, p.Spec.Selector)
		}
		if !match {
			continue
		}
		phase := style.Phase(os.Stdout, p.Status.Phase)
		style.Log(style.EmojiOK, "Matched policy %s phase=%s", p.Metadata.Name, phase)
		if p.Spec.Cilium != nil && p.Spec.Cilium.Sync {
			if p.Status.CiliumNetworkPolicyRef != "" {
				style.Log(style.EmojiOK, "CNP sync → %s (%s)", p.Status.CiliumNetworkPolicyRef, dash(p.Status.CiliumSyncMessage))
			} else {
				style.Log(style.EmojiWarn, "CNP sync enabled but not projected yet (%s)", dash(p.Status.CiliumSyncMessage))
			}
		}
	}
}

// printObservability fetches flows/drop-reasons via the UI API when
// KAIRON_UI_URL is set (same pass-through uiapi already exposes).
func printObservability(ctx context.Context, m model.Machine, kind string, limit int) error {
	body, err := fetchObservability(ctx, m.Namespace(), m.Metadata.Name, kind, limit)
	if err != nil {
		return err
	}
	style.Log(style.EmojiInfo, "%s:", kind)
	var pretty any
	if err := json.Unmarshal(body, &pretty); err != nil {
		fmt.Println(string(body))
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("  ", "  ")
	_ = enc.Encode(pretty)
	return nil
}

// fetchObservability GETs a Machine's uiapi pass-through route and
// returns the body.
func fetchObservability(ctx context.Context, namespace, name, kind string, limit int) ([]byte, error) {
	base := uiBaseURL()
	if base == "" {
		return nil, fmt.Errorf("set KAIRON_UI_URL (for example http://127.0.0.1:18082, and KAIRON_UI_TOKEN when kairon-ui authenticates) to reach uiapi %s pass-through", kind)
	}
	token := uiToken(true)
	path := fmt.Sprintf("%s/api/v1/machines/%s/%s/%s", base, url.PathEscape(namespace), url.PathEscape(name), kind)
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
