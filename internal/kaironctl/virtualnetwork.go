// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func printVirtualNetworks(w io.Writer, items []model.VirtualNetwork) {
	_, _ = fmt.Fprintf(w, "NAME\tCIDR\tVNI\tGATEWAY\tSNAT\tMACHINES\tPHASE\n")
	for _, v := range items {
		vni := "-"
		if v.Status.VNI != 0 {
			vni = fmt.Sprint(v.Status.VNI)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%d\t%s\n", v.Metadata.Name, v.Spec.CIDR, vni, dash(v.Status.Gateway), v.Spec.EgressSNAT, len(v.Status.Allocations), dash(v.Status.Phase))
	}
}

// describeVirtualNetwork prints the network as JSON, then its IPAM: one line
// per Machine with its address, floating IP and node.
func describeVirtualNetwork(ctx context.Context, kc *kube.Client, ns, name string) {
	v, err := kc.GetVirtualNetwork(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
	printVirtualNetworkAllocations(os.Stdout, v.Status)
}

func printVirtualNetworkAllocations(w io.Writer, st model.VirtualNetworkStatus) {
	if st.Message != "" {
		_, _ = fmt.Fprintf(w, "\nMessage: %s\n", st.Message)
	}
	_, _ = fmt.Fprintln(w, "\nAllocations:")
	if len(st.Allocations) == 0 {
		_, _ = fmt.Fprintln(w, "  (none)")
		return
	}
	_, _ = fmt.Fprintf(w, "  MACHINE\tIP\tMAC\tFLOATINGIP\tNODE\tNODEIP\n")
	for _, a := range st.Allocations {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", a.Machine, a.IP, a.MAC, dash(a.FloatingIP), dash(a.Node), dash(a.NodeIP))
	}
}
