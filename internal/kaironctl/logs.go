// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
)

// logComponents maps the short component names to the chart's pod label.
var logComponents = map[string]string{
	"controller":     "kairon-controller",
	"node":           "kairon-node",
	"ui":             "kairon-ui",
	"csi-node":       "kairon-csi-node",
	"csi-controller": "kairon-csi-controller",
}

func logComponentNames() []string {
	names := make([]string, 0, len(logComponents))
	for k := range logComponents {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

type logsOpts struct {
	Namespace string
	Pod       string
	Node      string
	Container string
	Tail      int
	Since     time.Duration
	Previous  bool
	Follow    bool
}

func newLogsCmd() *cobra.Command {
	o := &logsOpts{Namespace: "kairon-system", Tail: 200}
	cmd := &cobra.Command{
		Use:   "logs COMPONENT",
		Short: "Show logs of a Kairon component (controller, node, ui, csi-node, csi-controller)",
		Long: `Stream the logs of every pod of a Kairon component. With several pods (the node agent
runs one per node) each line is prefixed with the pod name; --node or --pod narrows it.`,
		Example: `  $ kaironctl logs controller
  $ kaironctl logs node --node worker-1 -f
  $ kaironctl logs controller --previous --tail 500`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: logComponentNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			label, ok := logComponents[args[0]]
			if !ok {
				return fmt.Errorf("unknown component %q (want one of: %s)", args[0], strings.Join(logComponentNames(), ", "))
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runLogs(ctx, kc, cmd.OutOrStdout(), label, o)
		},
	}
	cmd.Flags().StringVarP(&o.Namespace, "namespace", "n", o.Namespace, "namespace where Kairon is installed")
	cmd.Flags().StringVar(&o.Pod, "pod", "", "only this pod")
	cmd.Flags().StringVar(&o.Node, "node", "", "only pods scheduled on this node")
	cmd.Flags().StringVarP(&o.Container, "container", "c", "", "container name (default: the pod's first container)")
	cmd.Flags().IntVar(&o.Tail, "tail", o.Tail, "lines from the end of each log (0 = all)")
	cmd.Flags().DurationVar(&o.Since, "since", 0, "only logs newer than this duration (for example 15m)")
	cmd.Flags().BoolVar(&o.Previous, "previous", false, "logs of the previous (crashed) container instance")
	cmd.Flags().BoolVarP(&o.Follow, "follow", "f", false, "stream logs until interrupted")
	return cmd
}

func runLogs(ctx context.Context, kc *kube.Client, out io.Writer, label string, o *logsOpts) error {
	pods, err := listPods(ctx, kc, o.Namespace, "app.kubernetes.io/name="+label)
	if err != nil {
		return err
	}
	var sel []podSummary
	for _, p := range pods {
		if o.Pod != "" && p.Name != o.Pod {
			continue
		}
		if o.Node != "" && p.Node != o.Node {
			continue
		}
		sel = append(sel, p)
	}
	if len(sel) == 0 {
		return fmt.Errorf("no %s pods found in namespace %s (check -n/--namespace, --pod and --node)", label, o.Namespace)
	}
	prefix := len(sel) > 1
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, len(sel))
	for _, p := range sel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := podLogs(ctx, kc, o.Namespace, p.Name, logOptions{
				Container: o.Container, Tail: o.Tail, Since: o.Since, Previous: o.Previous, Follow: o.Follow,
			})
			if err != nil {
				errs <- fmt.Errorf("%s: %w", p.Name, err)
				return
			}
			defer func() { _ = rc.Close() }()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 64*1024), 1<<20)
			for sc.Scan() {
				mu.Lock()
				if prefix {
					_, _ = fmt.Fprintf(out, "[%s] %s\n", p.Name, sc.Text())
				} else {
					_, _ = fmt.Fprintln(out, sc.Text())
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	var first error
	for e := range errs {
		if first == nil {
			first = e
		}
	}
	return first
}
