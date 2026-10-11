// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const backupPolicyUsage = `usage:
  kaironctl backuppolicy create NAME --selector k=v... --incremental-every 1h [--full-every 168h] [--mode incremental|full]
      --s3-endpoint URL --s3-bucket NAME --s3-credentials SECRET [--s3-prefix P] [--s3-region R] [--s3-virtual-host]
      [--encryption-key SECRET] [--immutable-for 30d] [--quiesce auto|required|never] [--hourly N] [--daily N] [--monthly N]
  kaironctl backuppolicy get [NAME]
  kaironctl backuppolicy describe NAME
  kaironctl backuppolicy suspend NAME
  kaironctl backuppolicy resume NAME
  kaironctl backuppolicy delete NAME`

const backupPolicyExamples = `  $ kaironctl backuppolicy create web-hourly --selector tier=web --incremental-every 1h --full-every 168h \
      --s3-endpoint http://minio.backup:9000 --s3-bucket vm-backups --s3-credentials s3-creds --hourly 24 --daily 7 --monthly 3
  $ kaironctl backuppolicy describe web-hourly
  $ kaironctl backup chain web-1
  $ kaironctl backuppolicy suspend web-hourly`

func cmdBackupPolicy(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("%s", backupPolicyUsage))
	}
	fs := flag.NewFlagSet("backuppolicy "+args[0], flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	need := func() string {
		if len(args) < 2 {
			fatal(fmt.Errorf("%s", backupPolicyUsage))
		}
		return args[1]
	}
	switch args[0] {
	case "create":
		name := need()
		var selector stringSliceFlag
		fs.Var(&selector, "selector", "label key=value a Machine must match (repeatable, required)")
		incr := fs.String("incremental-every", "", "interval between backups, e.g. 1h (minimum 5m; required)")
		full := fs.String("full-every", "", "start a new chain with a full this often (default 168h)")
		mode := fs.String("mode", "", "incremental (default) or full (every run is a full)")
		quiesce := fs.String("quiesce", "", "guest filesystem freeze: auto (default), required or never")
		hourly := fs.Int("hourly", 0, "keep the newest backup of each of the last N hours")
		daily := fs.Int("daily", 0, "keep the newest backup of each of the last N days")
		monthly := fs.Int("monthly", 0, "keep the newest backup of each of the last N months")
		var s3 s3Flags
		s3.register(fs)
		_ = fs.Parse(args[2:])
		if len(selector) == 0 {
			fatal(fmt.Errorf("--selector k=v is required (repeatable)"))
		}
		sel, err := parseKeyValues(selector)
		if err != nil {
			fatal(err)
		}
		p := model.MachineBackupPolicy{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupPolicy},
			Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
			Spec: model.MachineBackupPolicySpec{
				Selector: sel, Mode: *mode, FullEvery: *full, IncrementalEvery: *incr,
				Target:     *s3.target(),
				Encryption: s3.encryption(), ImmutableFor: s3.immutableFor, Quiesce: *quiesce,
				Retention: model.BackupRetention{Hourly: *hourly, Daily: *daily, Monthly: *monthly},
			},
		}
		if err := p.Spec.Validate(); err != nil {
			fatal(err)
		}
		out, err := kc.CreateMachineBackupPolicy(ctx, *ns, p)
		if err != nil {
			fatal(err)
		}
		okf("machinebackuppolicy/%s created", out.Metadata.Name)
	case "get", "list":
		name, rest := "", args[1:]
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			name, rest = rest[0], rest[1:]
		}
		_ = fs.Parse(rest)
		var items []model.MachineBackupPolicy
		if name != "" {
			p, err := kc.GetMachineBackupPolicy(ctx, *ns, name)
			if err != nil {
				fatal(err)
			}
			items = []model.MachineBackupPolicy{p}
		} else {
			list, err := kc.ListMachineBackupPoliciesNamespace(ctx, *ns)
			if err != nil {
				fatal(err)
			}
			items = list
		}
		printBackupPolicies(os.Stdout, items)
	case "describe":
		name := need()
		_ = fs.Parse(args[2:])
		describeBackupPolicy(ctx, kc, *ns, name)
	case "suspend", "resume":
		name := need()
		_ = fs.Parse(args[2:])
		if err := kc.PatchMachineBackupPolicy(ctx, *ns, name, map[string]any{"spec": map[string]any{"suspend": args[0] == "suspend"}}); err != nil {
			fatal(err)
		}
		okf("machinebackuppolicy/%s %sd", name, args[0])
	case "delete":
		name := need()
		_ = fs.Parse(args[2:])
		if err := kc.DeleteMachineBackupPolicy(ctx, *ns, name); err != nil {
			fatal(err)
		}
		okf("machinebackuppolicy/%s deleted (its MachineBackups stay)", name)
	default:
		fatal(fmt.Errorf("unknown backuppolicy action %q\n%s", args[0], backupPolicyUsage))
	}
}

func printBackupPolicies(w io.Writer, items []model.MachineBackupPolicy) {
	_, _ = fmt.Fprintf(w, "NAME\tMODE\tINCREMENTAL\tFULL\tRETENTION\tSUSPEND\tMACHINES\n")
	for _, p := range items {
		mode := p.Spec.Mode
		if mode == "" {
			mode = model.BackupModeIncremental
		}
		r := p.Spec.Retention
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%dh/%dd/%dm\t%v\t%d\n", p.Metadata.Name, mode, dash(p.Spec.IncrementalEvery), dash(p.Spec.FullEvery),
			r.Hourly, r.Daily, r.Monthly, p.Spec.Suspend, len(p.Status.Machines))
	}
}

// describeBackupPolicy prints the policy as JSON, then one line per
// selected Machine: its chain, last full and incremental, next run and
// anything skipped, failing or held under Object Lock.
func describeBackupPolicy(ctx context.Context, kc *kube.Client, ns, name string) {
	p, err := kc.GetMachineBackupPolicy(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	printPolicyMachines(os.Stdout, p.Status.Machines)
}

func printPolicyMachines(w io.Writer, machines []model.PolicyMachineStatus) {
	_, _ = fmt.Fprintln(w, "\nMachines:")
	if len(machines) == 0 {
		_, _ = fmt.Fprintln(w, "  (none selected yet)")
		return
	}
	ts := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(w, "  MACHINE\tCHAIN\tLASTFULL\tLASTINCREMENTAL\tNEXTRUN\tNOTE\n")
	for _, m := range machines {
		note := m.Skipped
		if m.LastError != "" {
			note = "error: " + m.LastError
		}
		if len(m.RetentionHeld) > 0 {
			note = fmt.Sprintf("%s held by Object Lock: %v", note, m.RetentionHeld)
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", m.MachineName, dash(m.ChainID), ts(m.LastFull), ts(m.LastIncremental), ts(m.NextRun), dash(note))
	}
}
