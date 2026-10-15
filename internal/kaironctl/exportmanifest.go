// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// manifestMachine strips m to what re-creating it elsewhere needs: no
// status, server-set metadata, node pinning or Kairon bookkeeping.
func manifestMachine(m model.Machine, keepNode bool) model.Machine {
	labels := filterKeys(m.Metadata.Labels, []string{"*"}, func(k string) bool { return cloneSystemLabels[k] })
	annotations := filterKeys(m.Metadata.Annotations, []string{"*"}, func(k string) bool {
		return strings.HasPrefix(k, "kairon.zyvor.dev/") || k == "kubectl.kubernetes.io/last-applied-configuration"
	})
	spec := m.Spec
	if !keepNode {
		spec.NodeName = ""
	}
	spec.Network.TapName = ""
	spec.Network.PodUID = ""
	return model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: m.Metadata.Name, Labels: labels, Annotations: annotations},
		Spec:     spec,
	}
}

func manifestService(s kube.Service) kube.Service {
	spec := s.Spec
	spec.ClusterIP = ""
	ports := make([]kube.ServicePort, len(spec.Ports))
	for i, p := range spec.Ports {
		p.NodePort = 0
		ports[i] = p
	}
	spec.Ports = ports
	return kube.Service{
		APIVersion: "v1", Kind: "Service",
		Metadata: kube.ObjectMeta{Name: s.Metadata.Name, Labels: s.Metadata.Labels, Annotations: s.Metadata.Annotations},
		Spec:     spec,
	}
}

func writeMachineManifest(w io.Writer, m model.Machine, services []kube.Service, format string) error {
	objs := make([]any, 0, 1+len(services))
	objs = append(objs, m)
	for _, s := range services {
		objs = append(objs, s)
	}
	if format == "json" {
		return printManifest(w, map[string]any{"apiVersion": "v1", "kind": "List", "items": objs}, "json")
	}
	for i, o := range objs {
		if i > 0 {
			_, _ = fmt.Fprintln(w, "---")
		}
		if err := printManifest(w, o, "yaml"); err != nil {
			return err
		}
	}
	return nil
}

func newExportManifestCmd(opts *Options) *cobra.Command {
	var output string
	var keepNode bool
	cmd := &cobra.Command{
		Use:   "manifest MACHINE",
		Short: "Print a Machine and its exposed Services as clean YAML to re-create elsewhere",
		Long: `Prints the Machine without status, server-set metadata, namespace, node pinning
or Kairon's own bookkeeping labels and annotations, followed by the Services
kaironctl expose created for it -- like virtctl vmexport --manifest. Apply
it with kubectl apply -n NS -f. The disk is not included: pair it with
kaironctl export machine, or point spec.image at an image the target
cluster can reach.`,
		Example: `  $ kaironctl export manifest web-1 > web-1.yaml
  $ kubectl --context other apply -n dev -f web-1.yaml`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "yaml" && output != "json" {
				return fmt.Errorf("--output must be yaml or json")
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			var services []kube.Service
			all, err := kc.ListExposedServices(cmd.Context())
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: exposed Services skipped: %v\n", err)
			}
			for _, s := range all {
				if s.Metadata.Namespace == opts.Namespace && s.Metadata.Labels[kube.LabelExposeMachine] == args[0] {
					services = append(services, manifestService(s))
				}
			}
			return writeMachineManifest(cmd.OutOrStdout(), manifestMachine(m, keepNode), services, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "yaml", "yaml or json (a v1 List)")
	cmd.Flags().BoolVar(&keepNode, "keep-node", false, "keep spec.nodeName")
	return cmd
}
