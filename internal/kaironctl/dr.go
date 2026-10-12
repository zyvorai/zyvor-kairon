// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const drUsage = `usage:
  kaironctl dr get [PLAN]
  kaironctl dr status PLAN
  kaironctl dr failover PLAN [--planned]
  kaironctl dr test PLAN
  kaironctl dr test-end PLAN
Run failover and test on the site that should take over: the standby.`

const drExamples = `  $ kaironctl dr status shop -n prod
  $ kaironctl dr test shop -n prod
  $ kaironctl dr test-end shop -n prod
  $ kaironctl dr failover shop -n prod --planned
  $ kaironctl dr failover shop -n prod`

func cmdDR(ctx context.Context, kc *kube.Client, args []string) {
	namespace, args := nsFlag(args)
	ns := &namespace
	if len(args) < 1 {
		fatal(fmt.Errorf("%s", drUsage))
	}
	fs := flag.NewFlagSet("dr "+args[0], flag.ExitOnError)
	planned := fs.Bool("planned", false, "failover only: have the reachable active site halt its Machines and take a final backup first")
	need := func() string {
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			fatal(fmt.Errorf("%s", drUsage))
		}
		_ = fs.Parse(args[2:])
		return args[1]
	}
	switch args[0] {
	case "get", "list":
		name, rest := "", args[1:]
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			name, rest = rest[0], rest[1:]
		}
		_ = fs.Parse(rest)
		var items []model.MachineProtectionPlan
		if name != "" {
			p, err := kc.GetMachineProtectionPlan(ctx, *ns, name)
			if err != nil {
				fatal(err)
			}
			items = []model.MachineProtectionPlan{p}
		} else {
			list, err := kc.ListMachineProtectionPlansNamespace(ctx, *ns)
			if err != nil {
				fatal(err)
			}
			items = list
		}
		printProtectionPlans(os.Stdout, items)
	case "status", "describe":
		name := need()
		p, err := kc.GetMachineProtectionPlan(ctx, *ns, name)
		if err != nil {
			fatal(err)
		}
		printProtectionStatus(os.Stdout, p)
	case "failover", "test", "test-end":
		name := need()
		action := model.ProtectionAction{Type: model.ProtectionActionFailover, ID: drActionID(), Planned: *planned}
		switch args[0] {
		case "test":
			action.Type = model.ProtectionActionTest
		case "test-end":
			action.Type = model.ProtectionActionEndTest
		}
		if *planned && action.Type != model.ProtectionActionFailover {
			fatal(fmt.Errorf("--planned applies to failover only"))
		}
		if err := kc.PatchMachineProtectionPlan(ctx, *ns, name, map[string]any{"spec": map[string]any{"action": action}}); err != nil {
			fatal(err)
		}
		okf("machineprotectionplan/%s: %s %s requested; follow with `kaironctl dr status %s -n %s`", name, strings.ToLower(action.Type), action.ID, name, *ns)
	default:
		fatal(fmt.Errorf("unknown dr action %q\n%s", args[0], drUsage))
	}
}

func drActionID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func printProtectionPlans(w io.Writer, items []model.MachineProtectionPlan) {
	_, _ = fmt.Fprintf(w, "NAME\tROLE\tPHASE\tACTIVE\tEPOCH\tRPO\tPEER\tMACHINES\n")
	for _, p := range items {
		peer := "-"
		if p.Status.Peer != nil {
			peer = map[bool]string{true: "reachable", false: "unreachable"}[p.Status.Peer.Reachable]
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%d\t%s\t%s\t%d\n", p.Metadata.Name, p.Spec.Role, dash(p.Status.Phase), p.Status.Active,
			p.Status.Epoch, seconds(p.Status.RPOSeconds), peer, len(p.Spec.Machines))
	}
}

func printProtectionStatus(w io.Writer, p model.MachineProtectionPlan) {
	st := p.Status
	_, _ = fmt.Fprintf(w, "Plan:\t%s/%s (%s)\nPhase:\t%s\nActive:\t%v (epoch %d)\nRPO:\t%s (objective %s)\n",
		p.Metadata.Namespace, p.Metadata.Name, p.Spec.Role, dash(st.Phase), st.Active, st.Epoch, seconds(st.RPOSeconds), dash(p.Spec.Objectives.RPO))
	if st.Message != "" {
		_, _ = fmt.Fprintf(w, "Message:\t%s\n", st.Message)
	}
	if pr := st.Peer; pr != nil {
		_, _ = fmt.Fprintf(w, "Peer:\t%s reachable=%v active=%v epoch=%d %s\n", p.Spec.Peer.Server, pr.Reachable, pr.Active, pr.Epoch, pr.Message)
	}
	_, _ = fmt.Fprintln(w, "\nMachines:")
	_, _ = fmt.Fprintf(w, "  NAME\tRESTOREPOINT\tPOINTINTIME\tNOTE\n")
	for _, m := range st.Machines {
		pit := "-"
		if m.PointInTime != nil {
			pit = m.PointInTime.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", m.Name, dash(m.RestorePoint), pit, dash(m.Message))
	}
	for _, r := range []struct {
		kind string
		run  *model.ProtectionRun
	}{{"Failover", st.Failover}, {"Test", st.Test}} {
		if r.run == nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n%s %s:\t%s planned=%v rto=%s dataLoss=%s restored=%v\n", r.kind, r.run.ID, r.run.Phase, r.run.Planned,
			seconds(r.run.RTOSeconds), seconds(r.run.DataLossSeconds), r.run.Restored)
		if r.run.Message != "" {
			_, _ = fmt.Fprintf(w, "  %s\n", r.run.Message)
		}
	}
	if h := st.Handoff; h != nil {
		_, _ = fmt.Fprintf(w, "\nHandoff %s:\t%s %v %s\n", h.ID, h.Phase, h.Backups, h.Message)
	}
	for _, c := range st.Conditions {
		_, _ = fmt.Fprintf(w, "Condition %s=%s %s: %s\n", c.Type, c.Status, c.Reason, c.Message)
	}
}

func seconds(v *float64) string {
	if v == nil {
		return "-"
	}
	return (time.Duration(*v) * time.Second).String()
}
