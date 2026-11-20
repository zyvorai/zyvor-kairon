// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/model"
)

func newFleetCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "fleet", Short: "Operate enterprise fleet policies, plans, templates and usage ledgers"}
	cmd.AddCommand(&cobra.Command{Use: "resources", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		for _, r := range fleet.SortedResources() {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), r)
		}
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "get RESOURCE", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := fleet.ResourceForKind(args[0])
		if err != nil {
			return err
		}
		client, err := newKubeClient()
		if err != nil {
			return err
		}
		items, err := client.ListFleet(cmd.Context(), opts.Namespace, r)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
	}})
	cmd.AddCommand(&cobra.Command{Use: "create FILE.json", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		b, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		var o model.FleetResource
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		o.Metadata.Namespace = opts.Namespace
		o.APIVersion = model.FleetAPIVersion
		if err := fleet.Validate(o); err != nil {
			return err
		}
		if o.Kind == "MachineActionApproval" {
			return fmt.Errorf("use approve-action for authenticated approval")
		}
		r, err := fleet.ResourceForKind(o.Kind)
		if err != nil {
			return err
		}
		client, err := newKubeClient()
		if err != nil {
			return err
		}
		out, err := client.CreateFleet(cmd.Context(), opts.Namespace, r, o)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}})
	cmd.AddCommand(&cobra.Command{Use: "delete RESOURCE NAME", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := fleet.ResourceForKind(args[0])
		if err != nil {
			return err
		}
		client, err := newKubeClient()
		if err != nil {
			return err
		}
		return client.DeleteFleet(cmd.Context(), opts.Namespace, r, args[1])
	}})
	cmd.AddCommand(&cobra.Command{Use: "approve-action REQUEST.json", Short: "Approve an exact MCP action using your Kubernetes identity", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		b, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		var spec fleet.ApprovalSpec
		if err := json.Unmarshal(b, &spec); err != nil {
			return err
		}
		client, err := newKubeClient()
		if err != nil {
			return err
		}
		spec.Approver, err = client.AuthenticatedUsername(cmd.Context())
		if err != nil {
			return err
		}
		spec.ExpiresAt = time.Now().Add(5 * time.Minute).UTC()
		payload, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		obj := model.FleetResource{TypeMeta: model.TypeMeta{APIVersion: model.FleetAPIVersion, Kind: "MachineActionApproval"}, Metadata: model.ObjectMeta{Name: fleet.ApprovalName(spec), Namespace: opts.Namespace}, Spec: payload}
		if err := fleet.Validate(obj); err != nil {
			return err
		}
		out, err := client.CreateFleet(cmd.Context(), opts.Namespace, "machineactionapprovals", obj)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}})
	cmd.AddCommand(&cobra.Command{Use: "release-address NETWORK CLAIM-UID", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newKubeClient()
		if err != nil {
			return err
		}
		e := fleet.Engine{Kube: client}
		return e.ReleaseAddress(cmd.Context(), opts.Namespace, args[0], args[1])
	}})
	return cmd
}
