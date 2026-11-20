// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// volumeInfo is one spec.volumes entry with its provisioning state, shared
// by `kaironctl volumes` and the machine_volumes MCP tool.
type volumeInfo struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	ClaimName string `json:"claimName,omitempty"`
	Size      string `json:"size,omitempty"`
	Phase     string `json:"phase,omitempty"`
	VolumeID  string `json:"atlasVolumeID,omitempty"`
	NativeID  string `json:"nativeID,omitempty"`
	Retain    bool   `json:"retain,omitempty"`
	Message   string `json:"message,omitempty"`
}

func machineVolumes(m model.Machine) []volumeInfo {
	states := model.AtlasVolumeStates(m)
	out := make([]volumeInfo, 0, len(m.Spec.Volumes))
	for _, v := range m.Spec.Volumes {
		info := volumeInfo{Name: v.Name, Source: "pvc", ClaimName: v.ClaimName}
		if v.Atlas != nil {
			st := states[v.Name]
			info.Source = "atlas-" + v.Atlas.EffectiveMode()
			info.Size = v.Atlas.Size
			info.Retain = v.Atlas.Retain
			info.Phase = st.Phase
			info.VolumeID = st.VolumeID
			info.NativeID = st.NativeID
			info.Message = st.Message
			if info.ClaimName == "" {
				info.ClaimName = st.ClaimName
			}
			if info.Phase == "" {
				info.Phase = "Pending"
			}
		}
		out = append(out, info)
	}
	return out
}

func cmdVolumes(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl volumes MACHINE [--namespace NS] [--output table|json]"))
	}
	fs := flag.NewFlagSet("volumes", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	output := fs.String("output", "table", "table or json")
	fs.StringVar(ns, "n", "default", "namespace (shorthand)")
	fs.StringVar(output, "o", "table", "output (shorthand)")
	_ = fs.Parse(args[1:])
	m, err := kc.GetMachine(ctx, *ns, args[0])
	if err != nil {
		fatal(err)
	}
	if err := writeVolumes(os.Stdout, machineVolumes(m), *output); err != nil {
		fatal(err)
	}
}

func writeVolumes(w io.Writer, vols []volumeInfo, output string) error {
	switch output {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(vols)
	case "table", "":
	default:
		return fmt.Errorf("--output must be table or json")
	}
	if len(vols) == 0 {
		_, err := fmt.Fprintln(w, "no spec.volumes (boots from spec.image)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSOURCE\tCLAIM\tSIZE\tPHASE\tBACKEND\tMESSAGE")
	for _, v := range vols {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Source, dash(v.ClaimName), dash(v.Size), dash(v.Phase), dash(v.NativeID), v.Message)
	}
	return tw.Flush()
}
