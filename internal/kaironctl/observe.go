// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type observeOpts struct {
	AllNamespaces bool
	By            string
	Top           int
	Limit         int
	Output        string
	Follow        bool
	Interval      time.Duration
}

func newNetworkObserveCmd(opts *Options) *cobra.Command {
	o := &observeOpts{By: "reason", Top: 10, Limit: 200, Interval: 5 * time.Second}
	cmd := &cobra.Command{
		Use:   "observe",
		Short: "Summarise eBPF edge drops across Machines (what is being denied, and where)",
		Long: `Collect the attributed eBPF drops of every Running Machine in the namespace (or all
namespaces) from kairon-ui and rank them by reason, policy or Machine. This is the
fleet-level view that "network drops MACHINE" gives for a single Machine.

Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN when kairon-ui authenticates). The drop payload
is FluxVM's; fields named reason, policy and count (or packets) are recognised, anything
else counts as one drop with an unknown reason. Use "network drops MACHINE" for the raw
records.`,
		Example: `  $ kaironctl network observe
  $ kaironctl network observe -A --by policy --top 5
  $ kaironctl network observe --follow --interval 10s
  $ kaironctl network observe -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch o.By {
			case "reason", "policy", "machine":
			default:
				return fmt.Errorf("--by must be reason, policy or machine, got %q", o.By)
			}
			if o.Output != "" && o.Output != "json" && o.Output != "yaml" {
				return fmt.Errorf("unknown output %q (want json or yaml)", o.Output)
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
			defer cancel()
			ns := opts.Namespace
			if o.AllNamespaces {
				ns = ""
			}
			out := cmd.OutOrStdout()
			for {
				sum, err := observeDrops(ctx, kc, ns, o)
				if err != nil {
					return err
				}
				if err := writeObserve(out, sum, o); err != nil {
					return err
				}
				if !o.Follow {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(o.Interval):
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&o.AllNamespaces, "all-namespaces", "A", false, "observe Machines in every namespace")
	cmd.Flags().StringVar(&o.By, "by", o.By, "group by reason|policy|machine")
	cmd.Flags().IntVar(&o.Top, "top", o.Top, "show the N largest groups (0 = all)")
	cmd.Flags().IntVar(&o.Limit, "limit", o.Limit, "drop records requested per Machine")
	cmd.Flags().StringVarP(&o.Output, "output", "o", "", "output format: json|yaml (default: table)")
	cmd.Flags().BoolVar(&o.Follow, "follow", false, "repeat every --interval until interrupted")
	cmd.Flags().DurationVar(&o.Interval, "interval", o.Interval, "refresh interval for --follow")
	return cmd
}

type dropRecord struct {
	Reason string
	Policy string
	Count  int
}

type observeRow struct {
	Key      string `json:"key"`
	Drops    int    `json:"drops"`
	Machines int    `json:"machines"`
}

type observeSummary struct {
	By            string       `json:"by"`
	MachinesSeen  int          `json:"machinesObserved"`
	MachinesError int          `json:"machinesFailed"`
	TotalDrops    int          `json:"totalDrops"`
	Rows          []observeRow `json:"rows"`
	Errors        []string     `json:"errors,omitempty"`
}

// parseDropRecords extracts drop records from FluxVM's network-drops (or
// drop-reasons) payload. It accepts an array, an object holding an array under
// drops/items/events/reasons/data, or a reason -> count map.
func parseDropRecords(body []byte) []dropRecord {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	var elems []any
	switch x := v.(type) {
	case []any:
		elems = x
	case map[string]any:
		for _, k := range []string{"drops", "items", "events", "reasons", "data"} {
			if arr, ok := x[k].([]any); ok {
				elems = arr
				break
			}
		}
		if elems == nil {
			// reason -> count map
			var recs []dropRecord
			for k, val := range x {
				if n, ok := val.(float64); ok {
					recs = append(recs, dropRecord{Reason: k, Count: int(n)})
				}
			}
			return recs
		}
	}
	recs := make([]dropRecord, 0, len(elems))
	for _, e := range elems {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		r := dropRecord{Count: 1}
		r.Reason = firstString(m, "reason", "drop_reason", "reasonName", "reason_name")
		r.Policy = firstString(m, "policy", "policyName", "policy_name")
		for _, k := range []string{"count", "packets", "n", "hits"} {
			if n, ok := m[k].(float64); ok && n > 0 {
				r.Count = int(n)
				break
			}
		}
		recs = append(recs, r)
	}
	return recs
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func observeDrops(ctx context.Context, kc *kube.Client, ns string, o *observeOpts) (observeSummary, error) {
	var machines []model.Machine
	var err error
	if ns == "" {
		machines, err = kc.ListMachines(ctx)
	} else {
		machines, err = kc.ListMachinesNamespace(ctx, ns)
	}
	if err != nil {
		return observeSummary{}, err
	}
	sum := observeSummary{By: o.By}
	type agg struct {
		drops    int
		machines map[string]bool
	}
	groups := map[string]*agg{}
	for _, m := range machines {
		if m.Status.Phase != "Running" {
			continue
		}
		body, err := fetchObservability(ctx, m.Namespace(), m.Metadata.Name, "network-drops", o.Limit)
		if err != nil {
			sum.MachinesError++
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s/%s: %v", m.Namespace(), m.Metadata.Name, err))
			if strings.Contains(err.Error(), "KAIRON_UI_URL") {
				return sum, err // not configured: no point trying the rest
			}
			continue
		}
		sum.MachinesSeen++
		id := m.Namespace() + "/" + m.Metadata.Name
		for _, r := range parseDropRecords(body) {
			key := r.Reason
			switch o.By {
			case "policy":
				key = r.Policy
			case "machine":
				key = id
			}
			if key == "" {
				key = "(unknown)"
			}
			g := groups[key]
			if g == nil {
				g = &agg{machines: map[string]bool{}}
				groups[key] = g
			}
			g.drops += r.Count
			g.machines[id] = true
			sum.TotalDrops += r.Count
		}
	}
	for k, g := range groups {
		sum.Rows = append(sum.Rows, observeRow{Key: k, Drops: g.drops, Machines: len(g.machines)})
	}
	sort.Slice(sum.Rows, func(i, j int) bool {
		if sum.Rows[i].Drops != sum.Rows[j].Drops {
			return sum.Rows[i].Drops > sum.Rows[j].Drops
		}
		return sum.Rows[i].Key < sum.Rows[j].Key
	})
	if o.Top > 0 && len(sum.Rows) > o.Top {
		sum.Rows = sum.Rows[:o.Top]
	}
	return sum, nil
}

func writeObserve(w io.Writer, sum observeSummary, o *observeOpts) error {
	if o.Output != "" {
		return writeStructured(w, sum, o.Output)
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "%s\tDROPS\tMACHINES\n", strings.ToUpper(sum.By))
	for _, r := range sum.Rows {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\n", r.Key, r.Drops, r.Machines)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\n%d drop(s) across %d Machine(s)", sum.TotalDrops, sum.MachinesSeen)
	if sum.MachinesError > 0 {
		_, _ = fmt.Fprintf(w, "; %d Machine(s) could not be queried (first: %s)", sum.MachinesError, sum.Errors[0])
	}
	_, _ = fmt.Fprintln(w)
	return nil
}
