// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const macUsage = `usage:
  kaironctl mac nodes
  kaironctl mac networks [-n NAMESPACE]
  kaironctl mac network NAME [-n NAMESPACE]`

const macExamples = `  $ kaironctl mac nodes
  $ kaironctl mac networks -n ai
  $ kaironctl mac network shard-net -n ai`

func cmdMac(ctx context.Context, kc *kube.Client, args []string) {
	namespace, args := nsFlag(args)
	if len(args) < 1 {
		fatal(fmt.Errorf("%s", macUsage))
	}
	switch args[0] {
	case "nodes":
		nodes, err := kc.ListNodes(ctx)
		if err != nil {
			fatal(err)
		}
		machines, err := kc.ListMachines(ctx)
		if err != nil {
			fatal(err)
		}
		printMacNodes(os.Stdout, nodes, machines)
	case "networks", "network":
		nets, err := kc.ListMacNetworks(ctx)
		if err != nil {
			fatal(err)
		}
		var mine []model.MacNetwork
		for _, n := range nets {
			if n.Namespace() == namespace && (args[0] == "networks" || len(args) > 1 && n.Metadata.Name == args[1]) {
				mine = append(mine, n)
			}
		}
		if args[0] == "networks" {
			printMacNetworks(os.Stdout, mine)
			return
		}
		if len(args) < 2 {
			fatal(fmt.Errorf("%s", macUsage))
		}
		if len(mine) == 0 {
			fatal(fmt.Errorf("macnetwork %s/%s not found", namespace, args[1]))
		}
		printMacNetwork(os.Stdout, mine[0])
	default:
		fatal(fmt.Errorf("%s", macUsage))
	}
}

func yesNo(v string) string {
	if v == "true" {
		return "yes"
	}
	return "no"
}

func printMacNodes(w io.Writer, nodes []model.Node, machines []model.Machine) {
	count := map[string]int{}
	for _, m := range machines {
		if m.Spec.NodeName != "" {
			count[m.Spec.NodeName]++
		}
	}
	_, _ = fmt.Fprintf(w, "NAME\tREADY\tCHIP\tCORES\tMEMORY\tMACOS\tROSETTA\tNESTED\tTHUNDERBOLT\tFABRIC\tMACHINES\n")
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Metadata.Name < nodes[j].Metadata.Name })
	for _, n := range nodes {
		l, a := n.Metadata.Labels, n.Metadata.Annotations
		if l[model.LabelBackendVZ] != "true" {
			continue
		}
		ready := "False"
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status
			}
		}
		tb := dash(a[model.AnnotationThunderboltAddresses])
		if iface := a[model.AnnotationThunderboltInterface]; iface != "" && tb != "-" {
			tb = iface + " " + tb
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", n.Metadata.Name, ready, dash(l[model.LabelAppleChip]),
			dash(a[model.AnnotationAppleCores]), dash(n.Status.Allocatable["memory"]), dash(l[model.LabelMacOSVersion]),
			yesNo(l[model.LabelVZRosetta]), yesNo(l[model.LabelVZNested]), tb, dash(l[model.LabelThunderboltFabric]), count[n.Metadata.Name])
	}
}

func printMacNetworks(w io.Writer, nets []model.MacNetwork) {
	_, _ = fmt.Fprintf(w, "NAME\tFABRIC\tPHASE\tSUBNET\tPORT\tMTU\tKEYEPOCH\tMEMBERS\n")
	for _, n := range nets {
		var members []string
		for _, m := range n.Status.Members {
			members = append(members, m.Node)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n", n.Metadata.Name, dash(n.Spec.Fabric), dash(n.Status.Phase),
			dash(n.Status.Subnet), n.Status.Port, n.Status.MTU, n.Status.KeyEpoch, dash(strings.Join(members, ",")))
	}
}

func printMacNetwork(w io.Writer, n model.MacNetwork) {
	st := n.Status
	_, _ = fmt.Fprintf(w, "Network:\t%s/%s (on the Macs: %s)\nFabric:\t%s\nPhase:\t%s\nSubnet:\t%s, UDP port %d, MTU %d\nKey epoch:\t%d\n",
		n.Namespace(), n.Metadata.Name, dash(st.Name), dash(n.Spec.Fabric), dash(st.Phase), dash(st.Subnet), st.Port, st.MTU, st.KeyEpoch)
	if st.Message != "" {
		_, _ = fmt.Fprintf(w, "Message:\t%s\n", st.Message)
	}
	_, _ = fmt.Fprintln(w, "\nMembers:")
	_, _ = fmt.Fprintf(w, "  NODE\tHOSTS\tADDRESS\tUNDERLAY\n")
	for _, m := range st.Members {
		_, _ = fmt.Fprintf(w, "  %s\t.%d-.%d\t%s\t%s\n", m.Node, m.Hosts[0], m.Hosts[1], m.Address, m.Underlay)
	}
}
