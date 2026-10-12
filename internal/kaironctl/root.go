// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
)

// Options carries process-wide CLI settings shared by root and subcommands.
type Options struct {
	Name      string // "kaironctl" or "kubectl kairon"
	Version   string
	Namespace string
	Color     string
}

// Run dispatches one kaironctl invocation and returns the process exit
// code -- args is the command line without the program name itself
// (os.Args[1:]), the same convention kubectl hands a plugin binary.
// Prefer RunAs when the binary display name differs (kubectl-kairon).
func Run(args []string, version string) int {
	return RunAs("kaironctl", args, version)
}

// RunAs is Run with an explicit root command name for help banners.
func RunAs(name string, args []string, version string) int {
	opts := &Options{Name: name, Version: version, Namespace: "default", Color: "auto"}
	root := NewRootCmd(opts)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

// NewRootCmd builds the Cobra command tree.
func NewRootCmd(opts *Options) *cobra.Command {
	if opts == nil {
		opts = &Options{Name: "kaironctl", Version: "dev", Namespace: "default", Color: "auto"}
	}
	root := &cobra.Command{
		Use:   opts.Name,
		Short: "Operate Kairon Machines on Kubernetes",
		Long: `Kaironctl is the CLI for Kairon — real VMs on Kubernetes without KubeVirt.

Install the control plane with Helm, then create and operate Machines,
migrations, snapshots, and more. Same command tree as kubectl-kairon.`,
		Example: `  # Install into kairon-system
  $ kaironctl install

  # Create and list a Machine
  $ kaironctl create demo --image /var/lib/fluxvm/images/ubuntu.qcow2 --cpu 2 --memory 2Gi
  $ kaironctl get machines

  # Cluster status
  $ kaironctl status`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			mode, err := style.ParseColorMode(opts.Color)
			if err != nil {
				return err
			}
			style.SetColorMode(mode)
			return nil
		},
	}
	root.PersistentFlags().StringVarP(&opts.Namespace, "namespace", "n", "default", "Kubernetes namespace for namespaced resources (install defaults to kairon-system)")
	root.PersistentFlags().StringVar(&kubeFlags.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	root.PersistentFlags().StringVar(&kubeFlags.Context, "context", "", "kubeconfig context to use")
	root.PersistentFlags().StringVar(&opts.Color, "color", "auto", "colorize output: auto|always|never (also respects NO_COLOR)")

	root.AddCommand(
		newVersionCmd(opts),
		newFleetCmd(opts),
		newCompletionCmd(root),
		newInstallCmd(opts),
		newUninstallCmd(opts),
		newUpgradeCmd(opts),
		newStatusCmd(opts),
		newConfigCmd(opts),
		newDoctorCmd(opts),
		newLogsCmd(),
		newEventsCmd(),
		newSysdumpCmd(opts),
		newConnectivityCmd(),
		newSelftestCmd(opts),
		newUICmd(),
		newHistoryCmd(),
		newRollbackCmd(),
		newNetworkCmd(opts),
		newImageCmd(opts),
		newMCPCmd(opts),
		newApproveCmd(opts),
		newNodeCmd(),
		newDRSCmd(),
		newAgentCmd(),
		newDiagnoseCmd(opts),
		legacyCmd(opts, "get", "List Kairon resources", getExamples, cmdGet),
		legacyCmd(opts, "describe", "Show details of a Kairon resource", describeExamples, cmdDescribe),
		legacyCmd(opts, "create", "Create a Machine or other Kairon resource", createExamples, cmdCreate),
		legacyCmd(opts, "delete", "Delete a Kairon resource", deleteExamples, cmdDelete),
		legacyCmd(opts, "edit", "Edit a Kairon resource (only flags you pass are patched)", editExamples, cmdEdit),
		legacyCmd(opts, "scale", "Scale a MachineSet or MachinePool", scaleExamples, cmdScale),
		legacyCmd(opts, "claim", "Claim a warm Machine from a MachinePool", claimExamples, cmdClaim),
		legacyCmd(opts, "fork", "Fork a running Machine into live copies", forkExamples, cmdFork),
		legacyCmd(opts, "import", "Create a Machine from a VMware OVA", importExamples, cmdImport),
		legacyCmd(opts, "top", "Show live Machine or node resource usage", topExamples, cmdTop),
		legacyCmd(opts, "trigger", "Trigger a SnapshotSchedule run now", triggerExamples, cmdTrigger),
		powerCmd(opts, "start", "Running"),
		powerCmd(opts, "stop", "Stopped"),
		powerCmd(opts, "pause", "Paused"),
		powerCmd(opts, "resume", "Running"),
		powerCmd(opts, "halt", "Halted"),
		powerCmd(opts, "hibernate", "Hibernated"),
		legacyCmd(opts, "migrate", "Create a MachineMigration", migrateExamples, cmdMigrate),
		legacyCmd(opts, "evacuate", "Migrate all Machines off a node", evacuateExamples, cmdEvacuate),
		legacyCmd(opts, "recover", "Attest recovery for a NeedsRecovery migration", recoverExamples, cmdRecover),
		legacyCmd(opts, "cancel-migration", "Cancel an in-flight live migration", cancelExamples, cmdCancelMigration),
		legacyCmd(opts, "fence", "Fence a Machine on a dead node for reschedule", fenceExamples, cmdFence),
		legacyCmd(opts, "snapshot", "Create a MachineSnapshot", snapshotExamples, cmdSnapshot),
		legacyCmd(opts, "restore", "Restore a MachineSnapshot to a PVC", restoreExamples, cmdRestore),
		legacyCmd(opts, "backup", "Back up a Machine's disks or restore a MachineBackup", backupExamples, cmdBackup),
		legacyCmd(opts, "backuppolicy", "Schedule incremental S3 backups with GFS retention", backupPolicyExamples, cmdBackupPolicy),
		legacyCmd(opts, "dr", "Cross-cluster disaster recovery: plan status, failover, DR tests", drExamples, cmdDR),
		legacyCmd(opts, "mac", "Mac nodes' Apple capabilities and the MacNetworks between them", macExamples, cmdMac),
		legacyCmd(opts, "volumes", "List a Machine's volumes and their provisioning state", volumesExamples, cmdVolumes),
		legacyCmd(opts, "disk", "Hot-attach or detach a PVC-backed disk on a Machine", diskExamples, cmdDisk),
		legacyCmd(opts, "nic", "Hot-add or remove an extra NIC on a Machine", nicExamples, cmdNIC),
	)
	return root
}

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion script",
		Long: `Generate completion script for bash, zsh, fish, or powershell.

To load completions:

  # bash
  $ source <(kaironctl completion bash)

  # zsh
  $ source <(kaironctl completion zsh)
`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}

// legacyCmd wraps an existing flag.FlagSet-based handler. DisableFlagParsing
// keeps the historical flag surface working unchanged for tests and operators.
func legacyCmd(opts *Options, use, short, example string, fn func(context.Context, *kube.Client, []string)) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		Example:            example,
		DisableFlagParsing: true,
		ValidArgsFunction:  legacyCompletion(opts, use),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args = extractConnFlags(args)
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			// Prefer root -n when the user put it before the verb; handlers
			// still honor -n/--namespace inside args via nsFlag.
			fn(ctx, kc, withDefaultNamespace(opts.Namespace, args))
			return nil
		},
	}
}

func powerCmd(opts *Options, use, state string) *cobra.Command {
	return &cobra.Command{
		Use:                use + " MACHINE",
		Short:              "Set Machine powerState to " + state,
		DisableFlagParsing: true,
		ValidArgsFunction:  completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args = extractConnFlags(args)
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			cmdPower(ctx, kc, withDefaultNamespace(opts.Namespace, args), state)
			return nil
		},
	}
}

// withDefaultNamespace prepends --namespace NS when the root persistent
// flag is set to a non-default value and args do not already set
// namespace. Handlers that use flag.FlagSet only define -namespace (no
// short -n), so we must inject the long form — never -n.
func withDefaultNamespace(ns string, args []string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-n" || a == "--namespace" || a == "-namespace" {
			return args
		}
		if strings.HasPrefix(a, "--namespace=") || strings.HasPrefix(a, "-namespace=") {
			return args
		}
	}
	if ns == "" || ns == "default" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--namespace", ns)
	out = append(out, args...)
	return out
}

const getExamples = `  $ kaironctl get machines
  $ kaironctl get migrations --selector tier=web
  $ kaironctl get nodes`

const describeExamples = `  $ kaironctl describe machine demo
  $ kaironctl describe quota team-payments`

const createExamples = `  $ kaironctl create demo --image /var/lib/fluxvm/images/ubuntu.qcow2 --cpu 2 --memory 2Gi
  $ kaironctl create machineset web --image /img.qcow2 --replicas 3 --label tier=web`

const deleteExamples = `  $ kaironctl delete machine demo
  $ kaironctl delete machines --selector tier=web --dry-run`

const editExamples = `  $ kaironctl edit machine demo --priority 10
  $ kaironctl edit machineset web --replicas 5`

const importExamples = `  $ kaironctl import ova https://files.example.com/web01.ova
  $ kaironctl import ova ./web01.ova --url https://files.example.com/web01.ova --name web01 --network tap
  $ kaironctl import ova https://files.example.com/db.ova --cpu 8 --memory 32Gi --sha256 sha256:... --dry-run`

const forkExamples = `  $ kaironctl fork base
  $ kaironctl fork base --count 8 --prefix trial
  $ kaironctl fork base --count 4 --no-wait`

const claimExamples = `  $ kaironctl claim agents
  $ kaironctl claim agents job-42 --label team=ml --wait 30s
  $ kaironctl claim agents --ttl 1h
  $ kaironctl claim agents --retain --no-wait`

const scaleExamples = `  $ kaironctl scale machinepool agents --replicas 10
  $ kaironctl scale machineset web --replicas 5
  $ kaironctl scale machineset --selector tier=web --replicas 2`

const topExamples = `  $ kaironctl top machines
  $ kaironctl top nodes`

const triggerExamples = `  $ kaironctl trigger snapshotschedule nightly`

const migrateExamples = `  $ kaironctl migrate demo --strategy cold --target-node worker-2`

const evacuateExamples = `  $ kaironctl evacuate worker-1 --strategy cold --wait`

const recoverExamples = `  $ kaironctl recover demo-mig --action confirm-committed --diagnosis "..." --reason "..."`

const cancelExamples = `  $ kaironctl cancel-migration demo-mig`

const diskExamples = `  $ kaironctl disk attach db data --claim db-data
  $ kaironctl disk list db
  $ kaironctl disk detach db data`

const nicExamples = `  $ kaironctl nic add fw lan --bridge br-lan
  $ kaironctl nic list fw
  $ kaironctl nic remove fw lan`

const fenceExamples = `  $ kaironctl fence demo --reason "node worker-1 confirmed dead"`

const snapshotExamples = `  $ kaironctl snapshot demo --name before-upgrade --class csi-snapclass
  $ kaironctl snapshot demo --volume data`

const volumesExamples = `  $ kaironctl volumes demo
  $ kaironctl volumes demo -n prod -o json`

const restoreExamples = `  $ kaironctl restore before-upgrade --target-claim demo-restored`

const backupExamples = `  $ kaironctl backup create web --name nightly
  $ kaironctl backup create db --atlas --keep 7
  $ kaironctl backup list
  $ kaironctl halt web && kaironctl backup restore nightly
  $ kaironctl backup delete nightly
  $ kaironctl backup create web --s3-endpoint http://minio:9000 --s3-bucket vm --s3-credentials s3-creds
  $ kaironctl backup create web --mode incremental --parent web-20260101-000000
  $ kaironctl backup chain web`

// legacyCompletion picks the completion behaviour of a legacy verb by name.
func legacyCompletion(opts *Options, use string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	switch strings.Fields(use)[0] {
	case "get", "describe", "delete", "edit":
		return completeKindsThenNames(&opts.Namespace)
	case "top":
		return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(positional(args)) == 0 {
				return filterPrefix([]string{"machines", "nodes"}, toComplete), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	case "volumes", "fork", "snapshot":
		return completeMachineNames(&opts.Namespace)
	case "claim":
		return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			ns, words := completionNamespace(args, &opts.Namespace)
			if len(positional(words)) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return completeNames("machinepools", ns, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
	}
	return nil
}
