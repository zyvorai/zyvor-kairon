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

type exposeOptions struct {
	Name       string
	Type       string
	Protocol   string
	Port       int32
	TargetPort int32
	NodePort   int32
}

// exposeService builds the selectorless Service for machine m. It is owned
// by the Machine so deleting the Machine deletes it.
func exposeService(m model.Machine, ns string, o exposeOptions) (kube.Service, error) {
	switch o.Type {
	case "ClusterIP", "NodePort", "LoadBalancer":
	default:
		return kube.Service{}, fmt.Errorf("--type %q: want ClusterIP, NodePort or LoadBalancer", o.Type)
	}
	proto := strings.ToUpper(o.Protocol)
	if proto != "TCP" && proto != "UDP" && proto != "SCTP" {
		return kube.Service{}, fmt.Errorf("--protocol %q: want TCP, UDP or SCTP", o.Protocol)
	}
	if o.Port < 1 || o.Port > 65535 {
		return kube.Service{}, fmt.Errorf("--port is required (1-65535)")
	}
	if o.TargetPort == 0 {
		o.TargetPort = o.Port
	}
	if o.TargetPort < 1 || o.TargetPort > 65535 {
		return kube.Service{}, fmt.Errorf("--target-port %d: want 1-65535", o.TargetPort)
	}
	if o.NodePort != 0 && o.Type == "ClusterIP" {
		return kube.Service{}, fmt.Errorf("--node-port needs --type NodePort or LoadBalancer")
	}
	name := o.Name
	if name == "" {
		name = m.Metadata.Name
	}
	if errs := validDNSLabel(name); errs != "" {
		return kube.Service{}, fmt.Errorf("service name %q: %s (use --name)", name, errs)
	}
	f := false
	return kube.Service{
		Metadata: kube.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{kube.LabelExposeMachine: m.Metadata.Name},
			OwnerReferences: []kube.OwnerReference{{
				APIVersion: model.APIVersion, Kind: "Machine", Name: m.Metadata.Name, UID: m.Metadata.UID, BlockOwnerDeletion: &f,
			}},
		},
		Spec: kube.ServiceSpec{
			Type: o.Type,
			Ports: []kube.ServicePort{{
				Name:       fmt.Sprintf("%s-%d", strings.ToLower(proto), o.Port),
				Protocol:   proto,
				Port:       o.Port,
				TargetPort: &kube.IntOrString{Int: o.TargetPort},
				NodePort:   o.NodePort,
			}},
		},
	}, nil
}

// validDNSLabel returns why s is not an RFC 1123 label (a Service name), or "".
func validDNSLabel(s string) string {
	if len(s) == 0 || len(s) > 63 {
		return "must be 1-63 characters"
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(s)-1
		if !ok {
			return "must be lowercase letters, digits and inner '-'"
		}
	}
	if s[0] >= '0' && s[0] <= '9' {
		return "must start with a letter"
	}
	return ""
}

func writeExposed(w io.Writer, svc kube.Service, slice kube.EndpointSlice) {
	p := svc.Spec.Ports[0]
	backend := "no guest IP yet; kairon-controller fills it in once the Machine reports one"
	if len(slice.Endpoints) > 0 && len(slice.Endpoints[0].Addresses) > 0 {
		backend = fmt.Sprintf("%s:%d", slice.Endpoints[0].Addresses[0], *slice.Ports[0].Port)
	}
	fmt.Fprintf(w, "service/%s exposed (%s)\n", svc.Metadata.Name, svc.Spec.Type)
	fmt.Fprintf(w, "  %s.%s.svc:%d/%s -> %s\n", svc.Metadata.Name, svc.Metadata.Namespace, p.Port, p.Protocol, backend)
	if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
		fmt.Fprintf(w, "  cluster IP  %s\n", svc.Spec.ClusterIP)
	}
	if p.NodePort != 0 {
		fmt.Fprintf(w, "  node port   %d\n", p.NodePort)
	}
	for _, in := range svc.Status.LoadBalancer.Ingress {
		fmt.Fprintf(w, "  external    %s\n", orDash(in.IP+in.Hostname))
	}
}

func newExposeCmd(opts *Options) *cobra.Command {
	var o exposeOptions
	var dryRun bool
	var output string
	cmd := &cobra.Command{
		Use:   "expose MACHINE --port PORT [--target-port PORT] [--type ClusterIP|NodePort|LoadBalancer] [--name NAME]",
		Short: "Expose a Machine's guest port as a Kubernetes Service",
		Long: `Creates a Service that sends traffic to the Machine's guest IP. Machines are
VMs, not pods, so the Service has no selector: kaironctl writes an EndpointSlice
with the guest IP, and kairon-controller keeps it current when the IP changes
(restart, migration). The endpoint is ready only while the Machine is Running.
Deleting the Machine deletes the Service.

The guest IP has to be routable from the cluster's nodes (bridged or routed
networks); a NAT-only guest network is reachable from its own node alone.`,
		Example: `  $ kaironctl expose web-1 --port 80
  $ kaironctl expose web-1 --name web-ssh --port 22 --type NodePort
  $ kaironctl expose db-1 --port 5432 --target-port 5432 --type LoadBalancer --dry-run -o yaml`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			ns := opts.Namespace
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), ns, args[0])
			if err != nil {
				return fmt.Errorf("machine/%s: %w", args[0], err)
			}
			svc, err := exposeService(m, ns, o)
			if err != nil {
				return err
			}
			if dryRun {
				svc.APIVersion, svc.Kind = "v1", "Service"
				if output == "" {
					output = "yaml"
				}
				return printManifest(os.Stdout, svc, output)
			}
			created, err := kc.CreateService(cmd.Context(), ns, svc)
			if kube.IsConflict(err) {
				return fmt.Errorf("service/%s already exists in %s; pick another --name or delete it first", svc.Metadata.Name, ns)
			}
			if err != nil {
				return err
			}
			slice, err := kc.SyncExposeSlice(cmd.Context(), created, &m)
			if err != nil {
				return fmt.Errorf("service/%s created, but writing its endpoints failed (kairon-controller retries): %w", created.Metadata.Name, err)
			}
			if output != "" {
				created.APIVersion, created.Kind = "v1", "Service"
				return printStructured(os.Stdout, created, output)
			}
			writeExposed(os.Stdout, created, slice)
			return nil
		},
	}
	f := cmd.Flags()
	f.Int32Var(&o.Port, "port", 0, "Service port (required)")
	f.Int32Var(&o.TargetPort, "target-port", 0, "guest port; defaults to --port")
	f.StringVar(&o.Type, "type", "ClusterIP", "ClusterIP, NodePort or LoadBalancer")
	f.StringVar(&o.Name, "name", "", "Service name; defaults to the Machine name")
	f.StringVar(&o.Protocol, "protocol", "TCP", "TCP, UDP or SCTP")
	f.Int32Var(&o.NodePort, "node-port", 0, "fixed node port for NodePort/LoadBalancer")
	f.BoolVar(&dryRun, "dry-run", false, "print the Service instead of creating it")
	f.StringVarP(&output, "output", "o", "", "json or yaml")
	_ = cmd.MarkFlagRequired("port")
	return cmd
}
