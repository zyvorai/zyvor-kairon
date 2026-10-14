// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/model"
)

// cdromsPatch replaces spec.cdroms, guarded by resourceVersion so a
// concurrent edit is refused rather than lost.
func cdromsPatch(m model.Machine, cdroms []model.MachineCdrom) map[string]any {
	if cdroms == nil {
		cdroms = []model.MachineCdrom{}
	}
	return map[string]any{
		"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
		"spec":     map[string]any{"cdroms": cdroms},
	}
}

func insertCdrom(m model.Machine, cd model.MachineCdrom) ([]model.MachineCdrom, error) {
	if m.DesiredPowerState() != "Stopped" || m.Status.RuntimeID != "" {
		return nil, fmt.Errorf("machine/%s has a runtime; FluxVM can eject media live but not insert it -- stop the Machine first (a Stopped Machine keeps only disks on spec.volumes)", m.Metadata.Name)
	}
	if slices.ContainsFunc(m.Spec.Cdroms, func(c model.MachineCdrom) bool { return c.Name == cd.Name }) {
		return nil, fmt.Errorf("machine/%s already has a cdrom %q; eject it first", m.Metadata.Name, cd.Name)
	}
	out := append(append([]model.MachineCdrom(nil), m.Spec.Cdroms...), cd)
	if err := model.ValidateCdroms(out); err != nil {
		return nil, err
	}
	return out, nil
}

func ejectCdrom(m model.Machine, name string) ([]model.MachineCdrom, error) {
	out := slices.DeleteFunc(append([]model.MachineCdrom(nil), m.Spec.Cdroms...), func(c model.MachineCdrom) bool { return c.Name == name })
	if len(out) == len(m.Spec.Cdroms) {
		return nil, fmt.Errorf("machine/%s has no cdrom %q", m.Metadata.Name, name)
	}
	return out, nil
}

func newCdromCmd(opts *Options) *cobra.Command {
	root := &cobra.Command{
		Use:   "cdrom",
		Short: "List, insert or eject a Machine's install media",
	}
	list := &cobra.Command{
		Use:               "list MACHINE",
		Short:             "List a Machine's CD-ROM drives",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			if len(m.Spec.Cdroms) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "machine/%s has no cdroms\n", args[0])
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tIMAGE\tURL")
			for _, c := range m.Spec.Cdroms {
				src := ""
				if c.Source != nil {
					src = c.Source.HTTPURL
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, dash(c.ImageRef), dash(src))
			}
			return tw.Flush()
		},
	}

	var imageRef, httpURL, digest string
	insert := &cobra.Command{
		Use:   "insert MACHINE NAME (--image ISO-IMAGE | --url URL --sha256 HEX)",
		Short: "Add an ISO to a stopped Machine; it is attached at the next start",
		Long: `Adds a CD-ROM drive to spec.cdroms. FluxVM can eject media from a running VM but
not insert it, so the Machine must be Stopped; the drive appears when it next
starts. --image names a MachineImage of kind ISO; --url downloads raw ISO
bytes over http(s), verified against --sha256.`,
		Example: `  $ kaironctl stop win-1
  $ kaironctl cdrom insert win-1 virtio --image virtio-win
  $ kaironctl start win-1`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			cd := model.MachineCdrom{Name: args[1], ImageRef: imageRef}
			switch {
			case (imageRef == "") == (httpURL == ""):
				return fmt.Errorf("give exactly one of --image or --url")
			case httpURL != "":
				cd.Source = &model.ImageSource{HTTPURL: httpURL}
				cd.Digest = model.ImageDigestPrefix + strings.TrimPrefix(digest, model.ImageDigestPrefix)
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			cdroms, err := insertCdrom(m, cd)
			if err != nil {
				return err
			}
			if err := kc.PatchMachine(cmd.Context(), opts.Namespace, args[0], cdromsPatch(m, cdroms)); err != nil {
				return err
			}
			okf("machine/%s cdrom %s inserted (attached at next start)", args[0], cd.Name)
			return nil
		},
	}
	insert.Flags().StringVar(&imageRef, "image", "", "MachineImage (kind ISO) to insert")
	insert.Flags().StringVar(&httpURL, "url", "", "http(s) URL of a raw ISO")
	insert.Flags().StringVar(&digest, "sha256", "", "SHA-256 of the ISO at --url")

	eject := &cobra.Command{
		Use:               "eject MACHINE NAME",
		Short:             "Remove an ISO; a running Machine has the medium ejected live",
		Example:           `  $ kaironctl cdrom eject win-1 install`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			cdroms, err := ejectCdrom(m, args[1])
			if err != nil {
				return err
			}
			if err := kc.PatchMachine(cmd.Context(), opts.Namespace, args[0], cdromsPatch(m, cdroms)); err != nil {
				return err
			}
			okf("machine/%s cdrom %s ejected", args[0], args[1])
			return nil
		},
	}
	root.AddCommand(list, insert, eject)
	return root
}
