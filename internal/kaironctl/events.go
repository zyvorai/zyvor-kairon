// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
)

type eventsOpts struct {
	Namespace string
	All       bool
	Warnings  bool
	Watch     bool
	Output    string
	Interval  time.Duration
}

func newEventsCmd() *cobra.Command {
	o := &eventsOpts{Namespace: "kairon-system", Interval: 2 * time.Second}
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show Kubernetes events for Kairon (default namespace kairon-system)",
		Long: `List core/v1 events, oldest first. Use -n for another namespace (for example where
Machines live), -A for every namespace, --warnings to hide Normal events and -w to keep
printing new events as they arrive.`,
		Example: `  $ kaironctl events --warnings
  $ kaironctl events -n default -w
  $ kaironctl events -A -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runEvents(ctx, kc, cmd.OutOrStdout(), o)
		},
	}
	cmd.Flags().StringVarP(&o.Namespace, "namespace", "n", o.Namespace, "namespace")
	cmd.Flags().BoolVarP(&o.All, "all-namespaces", "A", false, "events from every namespace")
	cmd.Flags().BoolVar(&o.Warnings, "warnings", false, "only Warning events")
	cmd.Flags().BoolVarP(&o.Watch, "watch", "w", false, "keep printing new events")
	cmd.Flags().StringVarP(&o.Output, "output", "o", "", "output format: json|yaml (default: table)")
	return cmd
}

func eventKey(e eventSummary) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%d", e.Namespace, e.Object, e.Reason, e.Message, e.Count, e.Last.UnixNano())
}

func runEvents(ctx context.Context, kc *kube.Client, out io.Writer, o *eventsOpts) error {
	ns := o.Namespace
	if o.All {
		ns = ""
	}
	seen := map[string]bool{}
	first := true
	for {
		evs, err := listNamespaceEvents(ctx, kc, ns)
		if err != nil {
			return err
		}
		var fresh []eventSummary
		for _, e := range evs {
			if o.Warnings && e.Type != "Warning" {
				continue
			}
			k := eventKey(e)
			if seen[k] {
				continue
			}
			seen[k] = true
			fresh = append(fresh, e)
		}
		if o.Output != "" {
			if !o.Watch {
				return writeStructured(out, fresh, o.Output)
			}
			for _, e := range fresh {
				if err := writeStructured(out, e, o.Output); err != nil {
					return err
				}
			}
		} else {
			writeEvents(out, fresh, o.All || o.Namespace == "", first)
			if first && len(fresh) == 0 && !o.All {
				_, _ = fmt.Fprintf(out, "No events in namespace %q. Machine events are recorded in the Machine's namespace: try -n <namespace> or -A.\n", ns)
			}
		}
		first = false
		if !o.Watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.Interval):
		}
	}
}

func writeEvents(w io.Writer, evs []eventSummary, withNS, header bool) {
	if len(evs) == 0 && !header {
		return
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	if header {
		if withNS {
			_, _ = fmt.Fprintln(tw, "LAST SEEN\tNAMESPACE\tTYPE\tREASON\tOBJECT\tMESSAGE")
		} else {
			_, _ = fmt.Fprintln(tw, "LAST SEEN\tTYPE\tREASON\tOBJECT\tMESSAGE")
		}
	}
	for _, e := range evs {
		ts := "-"
		if !e.Last.IsZero() {
			ts = e.Last.Local().Format("15:04:05")
		}
		if withNS {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", ts, e.Namespace, e.Type, e.Reason, e.Object, e.Message)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ts, e.Type, e.Reason, e.Object, e.Message)
		}
	}
	_ = tw.Flush()
}
