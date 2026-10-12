// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/fencing"
)

type nodePatcher interface {
	PatchNode(ctx context.Context, name string, patch map[string]any) error
}

// setNodeFenced sets (reason non-empty) or clears (reason empty) the
// node-fenced annotation that -stale-evacuation acts on.
func setNodeFenced(ctx context.Context, kc nodePatcher, node, reason string) error {
	var value any
	if reason != "" {
		value = reason
	}
	return kc.PatchNode(ctx, node, map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{fencing.AnnotationNodeFenced: value},
		},
	})
}

func newNodeCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "node",
		Short: "Node-level operator actions",
	}
	var reason string
	var clear bool
	fence := &cobra.Command{
		Use:   "fence NODE --reason REASON | --clear",
		Short: "Attest a node is dead, so -stale-evacuation may fence its Machines",
		Long: `Sets the kairon.zyvor.dev/node-fenced annotation on a Node. With
kairon-controller running -stale-evacuation, Machines on that node that are
annotated kairon.zyvor.dev/evacuate=true and show NodeUnreachable=True are
fenced and handed back to the scheduler, as kaironctl fence would. Only set
this once you know the node is powered off or isolated: a VM still running
there would end up running twice. --clear removes the annotation.`,
		Example: `  kaironctl node fence worker-3 --reason "ipmi power-off confirmed, ticket OPS-412"
  kaironctl node fence worker-3 --clear`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason = strings.TrimSpace(reason)
			if clear == (reason != "") {
				return fmt.Errorf("pass exactly one of --reason or --clear")
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			if err := setNodeFenced(cmd.Context(), kc, args[0], reason); err != nil {
				return err
			}
			if clear {
				okf("node/%s: fence attestation cleared", args[0])
			} else {
				okf("node/%s: attested dead (%s)", args[0], reason)
			}
			return nil
		},
	}
	fence.Flags().StringVar(&reason, "reason", "", "out-of-band evidence the node is dead")
	fence.Flags().BoolVar(&clear, "clear", false, "remove the attestation")
	root.AddCommand(fence, newNodeTopologyCmd(), newNodeMaintenanceCmd())
	return root
}
