// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
)

// versionInfo is what `kaironctl version --server` reports.
type versionInfo struct {
	Client string         `json:"client"`
	Server *serverVersion `json:"server,omitempty"`
}

type serverVersion struct {
	Release    string            `json:"release"`
	Namespace  string            `json:"namespace"`
	Revision   int               `json:"revision"`
	Status     string            `json:"status"`
	Chart      string            `json:"chart"`
	AppVersion string            `json:"appVersion"`
	Images     map[string]string `json:"images,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// workloadImagesFn reads container images of the Kairon workloads; replaced in tests.
var workloadImagesFn = workloadImages

func newVersionCmd(opts *Options) *cobra.Command {
	ref := newReleaseRef()
	var server bool
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print kaironctl version (and, with --server, the installed release)",
		Long: `Print the kaironctl version. With no flags the output is exactly the client version string.

--server also reads the installed Helm release (chart, app version, revision, status)
and the container images of the controller, node agent and dashboard.`,
		Example: `  $ kaironctl version
  $ kaironctl version --server
  $ kaironctl version --server -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if !server && output == "" {
				_, err := fmt.Fprintln(out, opts.Version)
				return err
			}
			info := versionInfo{Client: opts.Version}
			if server {
				sv, err := collectServerVersion(cmd.Context(), ref)
				if err != nil {
					return err
				}
				info.Server = sv
			}
			if output == "json" || output == "yaml" {
				return writeStructured(out, info, output)
			}
			if output != "" && output != "text" {
				return fmt.Errorf("unknown output %q (want text, json or yaml)", output)
			}
			return writeVersionText(out, info)
		},
	}
	ref.bind(cmd)
	cmd.Flags().BoolVar(&server, "server", false, "also report the installed release and workload images")
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: text|json|yaml")
	return cmd
}

func collectServerVersion(ctx context.Context, ref *releaseRef) (*serverVersion, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rel, err := getReleaseFn(ref)
	if err != nil {
		return nil, fmt.Errorf("read release %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	sv := &serverVersion{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version}
	if rel.Info != nil {
		sv.Status = string(rel.Info.Status)
	}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		sv.Chart = rel.Chart.Metadata.Name + "-" + rel.Chart.Metadata.Version
		sv.AppVersion = rel.Chart.Metadata.AppVersion
	}
	imgs, warn := workloadImagesFn(ctx, ref.Namespace)
	sv.Images = imgs
	sv.Warnings = warn
	return sv, nil
}

// workloadImages returns component -> image for the Kairon workloads that
// exist in ns. Failures are returned as warnings so a partial answer still prints.
func workloadImages(ctx context.Context, ns string) (map[string]string, []string) {
	kc, err := newKubeClient()
	if err != nil {
		return nil, []string{"workload images unavailable: " + err.Error()}
	}
	imgs := map[string]string{}
	var warn []string
	for _, w := range []struct{ component, kind, name string }{
		{"controller", "deployments", "kairon-controller"},
		{"node", "daemonsets", "kairon-node"},
		{"ui", "deployments", "kairon-ui"},
	} {
		var obj struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s", ns, w.kind, w.name)
		if err := kubeGetJSON(ctx, kc, path, &obj); err != nil {
			if !kube.IsNotFound(err) {
				warn = append(warn, fmt.Sprintf("%s: %v", w.name, err))
			}
			continue
		}
		if cs := obj.Spec.Template.Spec.Containers; len(cs) > 0 {
			imgs[w.component] = cs[0].Image
		}
	}
	return imgs, warn
}

func writeVersionText(w io.Writer, info versionInfo) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "Client:\t%s\n", info.Client)
	if s := info.Server; s != nil {
		_, _ = fmt.Fprintf(tw, "Release:\t%s/%s (revision %d, %s)\n", s.Namespace, s.Release, s.Revision, s.Status)
		_, _ = fmt.Fprintf(tw, "Chart:\t%s (app %s)\n", s.Chart, s.AppVersion)
		for _, c := range []string{"controller", "node", "ui"} {
			if img, ok := s.Images[c]; ok {
				_, _ = fmt.Fprintf(tw, "Image %s:\t%s\n", c, img)
			}
		}
		for _, wmsg := range s.Warnings {
			_, _ = fmt.Fprintf(tw, "Warning:\t%s\n", wmsg)
		}
	}
	return tw.Flush()
}
