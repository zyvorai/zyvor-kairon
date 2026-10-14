// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
	"github.com/zyvorai/kairon/internal/model"
)

const mcpCallTimeout = 30 * time.Second

func newMCPCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol server for AI agents",
	}
	var allowWrite, requireApproval bool
	var auditLog, auditConfigMap string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve Kairon tools over MCP stdio (for Hermes Agent and other MCP clients)",
		Long: `Speaks MCP (JSON-RPC 2.0) on stdin/stdout. Read tools list and inspect
Machines, network policies and VM-edge observability. With --allow-write,
tools that set power state, create snapshots and run packet captures are
also offered. Destructive tools (delete_machine, fork_machine,
machine_backup restore/delete) then also need a human to approve each call
with 'kaironctl approve', unless --require-approval=false.

Needs KAIRON_KUBE_URL/KAIRON_KUBE_TOKEN (or in-cluster credentials) for
Machines, and KAIRON_UI_URL/KAIRON_UI_TOKEN for network observability and
capture. Logs go to stderr; stdout carries only protocol messages.`,
		Example: `  kaironctl mcp serve
  kaironctl mcp serve --allow-write`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			style.SetQuiet(true)
			s := mcp.NewServer("kairon", opts.Version, allowWrite)
			if allowWrite {
				log, err := openMCPAudit(auditLog, auditConfigMap)
				if err != nil {
					return fmt.Errorf("audit log: %w", err)
				}
				mcpAudit = log
				s.Audit = mcpAuditHook(log)
				if requireApproval {
					s.Approve = mcpApproveHook(opts.Namespace, func() (approvalStore, error) { return newKubeClient() }, time.Now)
					if os.Getenv("KAIRON_MCP_APPROVAL_MODE") == "resource" {
						s.Approve = mcpResourceApproveHook(opts.Namespace)
					}
				}
			}
			s.Add(kaironTools(opts, newKubeClient)...)
			return s.Serve(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
	serve.Flags().BoolVar(&allowWrite, "allow-write", false, "offer tools that change state (power, snapshot, capture)")
	serve.Flags().BoolVar(&requireApproval, "require-approval", true, "with --allow-write, delete_machine, fork_machine and machine_backup restore/delete each need `kaironctl approve` by a human first")
	serve.Flags().StringVar(&auditLog, "audit-log", defaultAuditLogPath(), "hash-chained JSONL audit log for write tools (used with --allow-write)")
	serve.Flags().StringVar(&auditConfigMap, "audit-configmap", "", "also mirror audit records into this existing ConfigMap, as namespace/name")
	cmd.AddCommand(serve)
	return cmd
}

var networkKinds = []string{"network-effective", "network-stats", "network-flows", "network-drops", "network-drop-reasons", "network-capture"}

type machineRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r machineRef) ns(def string) string {
	if r.Namespace != "" {
		return r.Namespace
	}
	if def != "" {
		return def
	}
	return "default"
}

func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

type machineSummary struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Phase      string `json:"phase,omitempty"`
	PowerState string `json:"powerState,omitempty"`
	Node       string `json:"node,omitempty"`
	GuestIP    string `json:"guestIP,omitempty"`
	CPU        string `json:"cpu,omitempty"`
	Memory     string `json:"memory,omitempty"`
	Message    string `json:"message,omitempty"`
}

func summarize(m model.Machine) machineSummary {
	return machineSummary{
		Namespace:  m.Namespace(),
		Name:       m.Metadata.Name,
		Phase:      m.Status.Phase,
		PowerState: m.Spec.PowerState,
		Node:       m.Status.NodeName,
		GuestIP:    m.Status.GuestIP,
		CPU:        m.Spec.Resources.CPU,
		Memory:     m.Spec.Resources.Memory,
		Message:    m.Status.Message,
	}
}

// kaironTools builds the tool set; newKube is injected so tests can
// point it at a fake API server.
func kaironTools(opts *Options, newKube func() (*kube.Client, error)) []mcp.Tool {
	nsDefault := opts.Namespace
	nsProp := mcp.String("Kubernetes namespace; defaults to " + nsOrDefault(nsDefault))
	refSchema := func(extra map[string]any, required ...string) map[string]any {
		props := map[string]any{"namespace": nsProp, "name": mcp.String("Machine name")}
		for k, v := range extra {
			props[k] = v
		}
		return mcp.Object(props, append([]string{"name"}, required...)...)
	}
	withKube := func(ctx context.Context, timeout time.Duration, fn func(context.Context, *kube.Client) (string, error)) (string, error) {
		kc, err := newKube()
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return fn(ctx, kc)
	}

	tools := []mcp.Tool{
		{
			Name:        "list_machines",
			Description: "List Kairon Machines (VMs) with phase, power state, node, guest IP and size. Omit namespace to list all namespaces.",
			Schema:      mcp.Object(map[string]any{"namespace": mcp.String("Kubernetes namespace; omit for all")}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					var items []model.Machine
					var err error
					if a.Namespace != "" {
						items, err = kc.ListMachinesNamespace(ctx, a.Namespace)
					} else {
						items, err = kc.ListMachines(ctx)
					}
					if err != nil {
						return "", err
					}
					out := make([]machineSummary, 0, len(items))
					for _, m := range items {
						out = append(out, summarize(m))
					}
					return mcp.JSON(map[string]any{"machines": out})
				})
			},
		},
		{
			Name:        "get_machine",
			Description: "Get one Machine's full spec and status, including network, edge and conditions.",
			Schema:      refSchema(nil),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					m, err := kc.GetMachine(ctx, a.ns(nsDefault), a.Name)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"metadata": m.Metadata, "spec": m.Spec, "status": m.Status})
				})
			},
		},
		{
			Name:        "list_network_policies",
			Description: "List MachineNetworkPolicies (selector, allow lists, rate limits, status). Omit namespace for all.",
			Schema:      mcp.Object(map[string]any{"namespace": mcp.String("Kubernetes namespace; omit for all")}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					items, err := kc.ListMachineNetworkPolicies(ctx)
					if err != nil {
						return "", err
					}
					out := []model.MachineNetworkPolicy{}
					for _, p := range items {
						if a.Namespace == "" || p.Metadata.Namespace == a.Namespace {
							out = append(out, p)
						}
					}
					return mcp.JSON(map[string]any{"policies": out})
				})
			},
		},
		{
			Name: "machine_network",
			Description: "Live VM-edge network data for a Machine from FluxVM, via kairon-ui: " +
				"network-effective (merged policy), network-stats, network-flows, network-drops (attributed drops with reason and policy), " +
				"network-drop-reasons (kernel reasons) or network-capture (packet capture sessions).",
			Schema: refSchema(map[string]any{
				"kind":  mcp.String("which view", networkKinds...),
				"limit": mcp.Integer("max entries for flows and drops", 1, 1000),
			}, "kind"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					Kind  string `json:"kind"`
					Limit int    `json:"limit"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if !contains(networkKinds, a.Kind) {
					return "", fmt.Errorf("kind must be one of %s", strings.Join(networkKinds, ", "))
				}
				ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
				defer cancel()
				body, err := fetchObservability(ctx, a.ns(nsDefault), a.Name, a.Kind, a.Limit)
				if err != nil {
					return "", err
				}
				var v any
				if json.Unmarshal(body, &v) == nil {
					return mcp.JSON(v)
				}
				return string(body), nil
			},
		},
		{
			Name:        "machine_edge_identity",
			Description: "The Machine's stable VM-edge identity (FNV-1a of namespace/name), as used in policy and conntrack. Computed locally.",
			Schema:      refSchema(nil),
			Call: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				ns := a.ns(nsDefault)
				return mcp.JSON(map[string]any{"namespace": ns, "name": a.Name, "identity": ebpfedge.StableIdentity(ns, a.Name)})
			},
		},
		{
			Name:        "set_power_state",
			Description: "Set a Machine's spec.powerState: Running (start/resume), Stopped, Paused, Halted or Hibernated. kairon-node applies it on its next tick.",
			Write:       true,
			Schema:      refSchema(map[string]any{"state": mcp.String("desired power state", "Running", "Stopped", "Paused", "Halted", "Hibernated")}, "state"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					State string `json:"state"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if !contains([]string{"Running", "Stopped", "Paused", "Halted", "Hibernated"}, a.State) {
					return "", fmt.Errorf("state must be Running, Stopped, Paused, Halted or Hibernated")
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					if err := setPowerState(ctx, kc, ns, a.Name, a.State); err != nil {
						return "", err
					}
					return fmt.Sprintf("machine %s/%s powerState set to %s", ns, a.Name, a.State), nil
				})
			},
		},
		{
			Name:        "create_snapshot",
			Description: "Create a MachineSnapshot of a Machine's volumes. The name is generated when omitted.",
			Write:       true,
			Schema: refSchema(map[string]any{
				"snapshotName": mcp.String("MachineSnapshot name (optional)"),
				"class":        mcp.String("VolumeSnapshotClass name (optional)"),
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					SnapshotName string `json:"snapshotName"`
					Class        string `json:"class"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					out, err := createSnapshot(ctx, kc, a.ns(nsDefault), a.Name, a.SnapshotName, a.Class)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("machinesnapshot %s/%s created", out.Metadata.Namespace, out.Metadata.Name), nil
				})
			},
		},
		{
			Name: "machine_volumes",
			Description: "A Machine's volumes: source (pvc, atlas-pvc, atlas-rbd), claim, size, Atlas provisioning phase, " +
				"backend id (pvc:NAME or rbd:POOL/IMAGE) and error message.",
			Schema: refSchema(nil),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					m, err := kc.GetMachine(ctx, a.ns(nsDefault), a.Name)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"volumes": machineVolumes(m)})
				})
			},
		},
		{
			Name: "snapshot_volume",
			Description: "Snapshot one of a Machine's volumes (a MachineSnapshot with spec.volumeNames). Atlas volumes use Atlas " +
				"snapshots, others CSI VolumeSnapshots. Check progress with get_machine_snapshot.",
			Write: true,
			Schema: refSchema(map[string]any{
				"volume":       mcp.String("spec.volumes name to snapshot"),
				"snapshotName": mcp.String("MachineSnapshot name (optional)"),
				"class":        mcp.String("VolumeSnapshotClass name for CSI volumes (optional)"),
			}, "volume"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					Volume       string `json:"volume"`
					SnapshotName string `json:"snapshotName"`
					Class        string `json:"class"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Volume == "" {
					return "", fmt.Errorf("volume is required")
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					m, err := kc.GetMachine(ctx, ns, a.Name)
					if err != nil {
						return "", err
					}
					found := false
					for _, v := range m.Spec.Volumes {
						found = found || v.Name == a.Volume
					}
					if !found {
						return "", fmt.Errorf("machine %s/%s has no volume %q", ns, a.Name, a.Volume)
					}
					name := a.SnapshotName
					if name == "" {
						name = resourceName(a.Name + "-" + a.Volume + "-" + time.Now().UTC().Format("20060102-150405"))
					}
					out, err := createSnapshot(ctx, kc, ns, a.Name, name, a.Class, a.Volume)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("machinesnapshot %s/%s created for volume %s", out.Metadata.Namespace, out.Metadata.Name, a.Volume), nil
				})
			},
		},
		{
			Name: "machine_disk",
			Description: "Hot-attach (action attach, needs claim) or detach (action detach) a PVC-backed block disk on a " +
				"running Machine via spec.disks. The guest sees a SCSI disk whose serial is the disk name.",
			Write: true,
			Schema: refSchema(map[string]any{
				"action": mcp.String("attach or detach"),
				"disk":   mcp.String("disk name ([a-z0-9-], not 'root')"),
				"claim":  mcp.String("PersistentVolumeClaim (attach only)"),
			}, "action", "disk"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					Action string `json:"action"`
					Disk   string `json:"disk"`
					Claim  string `json:"claim"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Action != "attach" && a.Action != "detach" {
					return "", fmt.Errorf("action must be attach or detach")
				}
				if a.Action == "attach" && a.Claim == "" {
					return "", fmt.Errorf("claim is required to attach")
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					m, err := kc.GetMachine(ctx, ns, a.Name)
					if err != nil {
						return "", err
					}
					disks, err := withDisk(m.Spec.Disks, a.Disk, a.Claim, a.Action == "attach")
					if err != nil {
						return "", err
					}
					if err := kc.PatchMachine(ctx, ns, a.Name, map[string]any{
						"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
						"spec":     map[string]any{"disks": disks},
					}); err != nil {
						return "", err
					}
					return fmt.Sprintf("machine %s/%s: disk %s %s requested; status.attachedDisks shows when it is live", ns, a.Name, a.Disk, a.Action), nil
				})
			},
		},
		{
			Name: "machine_nic",
			Description: "Hot-add (action add, needs bridge) or remove (action remove) an extra bridged NIC on a running " +
				"tap-mode Machine via spec.network.extraInterfaces. At most 3 extra NICs.",
			Write: true,
			Schema: refSchema(map[string]any{
				"action": mcp.String("add or remove"),
				"nic":    mcp.String("interface name"),
				"bridge": mcp.String("host bridge (add only)"),
				"mac":    mcp.String("MAC address (add only, optional)"),
			}, "action", "nic"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					Action string `json:"action"`
					NIC    string `json:"nic"`
					Bridge string `json:"bridge"`
					MAC    string `json:"mac"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Action != "add" && a.Action != "remove" {
					return "", fmt.Errorf("action must be add or remove")
				}
				if a.Action == "add" && a.Bridge == "" {
					return "", fmt.Errorf("bridge is required to add")
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					m, err := kc.GetMachine(ctx, ns, a.Name)
					if err != nil {
						return "", err
					}
					list, err := withInterface(m.Spec.Network, model.ExtraInterface{Name: a.NIC, Bridge: a.Bridge, MAC: a.MAC}, a.Action == "add")
					if err != nil {
						return "", err
					}
					if err := kc.PatchMachine(ctx, ns, a.Name, map[string]any{
						"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
						"spec":     map[string]any{"network": map[string]any{"extraInterfaces": list}},
					}); err != nil {
						return "", err
					}
					return fmt.Sprintf("machine %s/%s: nic %s %s requested; status.attachedInterfaces shows when it is live", ns, a.Name, a.NIC, a.Action), nil
				})
			},
		},
		{
			Name: "list_backups",
			Description: "List MachineBackups and MachineBackupRestores in a namespace: phase, node, size, whether the guest " +
				"was quiesced, Atlas volume backup ids and messages.",
			Schema: mcp.Object(map[string]any{"namespace": nsProp}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					backups, err := kc.ListMachineBackupsNamespace(ctx, ns)
					if err != nil {
						return "", err
					}
					restores, err := kc.ListMachineBackupRestoresNamespace(ctx, ns)
					if err != nil && !kube.IsNotFound(err) {
						return "", err
					}
					type item struct {
						Name    string `json:"name"`
						Machine string `json:"machine"`
						Status  any    `json:"status"`
					}
					out := map[string][]item{"backups": {}, "restores": {}}
					for _, b := range backups {
						out["backups"] = append(out["backups"], item{b.Metadata.Name, b.Spec.MachineName, b.Status})
					}
					for _, r := range restores {
						out["restores"] = append(out["restores"], item{r.Metadata.Name, r.Status.MachineName, r.Status})
					}
					return mcp.JSON(out)
				})
			},
		},
		{
			Name: "machine_backup",
			Description: "Back up a Machine (action create): FluxVM copies the disks of an image-booted Machine on its node, " +
				"freezing guest filesystems through the guest agent; atlas=true also backs up Atlas volumes to S3. " +
				"action restore copies MachineBackup `backup` back into Machine `name`, which must be halted first " +
				"(Atlas volumes restore into new volumes). action delete removes MachineBackup `backup` and its FluxVM copy. " +
				"mode (full, incremental or synthetic-full) makes an incremental-capable S3 backup instead: it takes the S3 target from " +
				"MachineBackupPolicy `policy`, or from `parent` (the backup an incremental builds on, or the point a synthetic full consolidates). " +
				"restore and delete may need a human's approval: a refused call says what to ask for; retry with the same arguments.",
			Write:    true,
			Approval: true,
			Schema: refSchema(map[string]any{
				"action":       mcp.String("create, restore or delete", "create", "restore", "delete"),
				"backup":       mcp.String("MachineBackup name: optional for create, required for restore and delete"),
				"quiesce":      mcp.String("guest fsfreeze: auto (default), required or never", "auto", "required", "never"),
				"atlas":        map[string]any{"type": "boolean", "description": "also back up Atlas volumes to S3 (create only)"},
				"storageClass": mcp.String("StorageClass for restored Atlas volumes (restore only)"),
				"mode":         mcp.String("S3 backup mode (create only)", "full", "incremental", "synthetic-full"),
				"parent":       mcp.String("S3 backups: parent MachineBackup (create only)"),
				"policy":       mcp.String("S3 backups: MachineBackupPolicy whose target to use (create only)"),
			}, "action"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					Action       string `json:"action"`
					Backup       string `json:"backup"`
					Quiesce      string `json:"quiesce"`
					Atlas        bool   `json:"atlas"`
					StorageClass string `json:"storageClass"`
					Mode         string `json:"mode"`
					Parent       string `json:"parent"`
					Policy       string `json:"policy"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Action != "create" && a.Backup == "" {
					return "", fmt.Errorf("backup is required for %s", a.Action)
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					switch a.Action {
					case "create":
						var atlasSpec *model.MachineBackupAtlas
						if a.Atlas {
							atlasSpec = &model.MachineBackupAtlas{}
						}
						nb := newBackup(ns, a.Name, a.Backup, a.Quiesce, atlasSpec)
						nb.Spec.Mode, nb.Spec.Parent = a.Mode, a.Parent
						if err := inheritCBTTarget(ctx, kc, &nb, a.Policy); err != nil {
							return "", err
						}
						b, err := submitBackup(ctx, kc, nb)
						if err != nil {
							return "", err
						}
						return fmt.Sprintf("machinebackup %s/%s created; list_backups shows its progress", ns, b.Metadata.Name), nil
					case "restore":
						r, err := createBackupRestore(ctx, kc, ns, a.Backup, "", a.Name, a.StorageClass)
						if err != nil {
							return "", err
						}
						return fmt.Sprintf("machinebackuprestore %s/%s created; the disk copy starts once machine %s is halted", ns, r.Metadata.Name, a.Name), nil
					case "delete":
						if err := kc.DeleteMachineBackup(ctx, ns, a.Backup); err != nil {
							return "", err
						}
						return fmt.Sprintf("machinebackup %s/%s deleted", ns, a.Backup), nil
					}
					return "", fmt.Errorf("action must be create, restore or delete")
				})
			},
		},
		{
			Name:        "get_machine_snapshot",
			Description: "Get a MachineSnapshot's phase, readiness and per-volume snapshots (CSI VolumeSnapshot or Atlas snapshot id).",
			Schema: mcp.Object(map[string]any{
				"namespace": nsProp,
				"name":      mcp.String("MachineSnapshot name"),
			}, "name"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					s, err := kc.GetMachineSnapshot(ctx, a.ns(nsDefault), a.Name)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"metadata": s.Metadata, "spec": s.Spec, "status": s.Status})
				})
			},
		},
		{
			Name:        "list_machine_pools",
			Description: "List MachinePools (warm, pre-booted Machines) with warm size, ready and claimed counts.",
			Schema:      mcp.Object(map[string]any{"namespace": nsProp}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					pools, err := kc.ListMachinePoolsNamespace(ctx, machineRef{Namespace: a.Namespace}.ns(nsDefault))
					if err != nil {
						return "", err
					}
					out := make([]map[string]any, 0, len(pools))
					for _, p := range pools {
						out = append(out, map[string]any{"name": p.Metadata.Name, "warm": p.Spec.Replicas, "ready": p.Status.ReadyReplicas, "claimed": p.Status.Claimed, "message": p.Status.Message})
					}
					return mcp.JSON(map[string]any{"pools": out})
				})
			},
		},
		{
			Name: "claim_machine",
			Description: "Claim a booted Machine from a MachinePool (binds in one reconcile tick instead of a cold boot). " +
				"Waits up to waitSeconds for the bind and returns the Machine name. Deleting the claim (release_claim) deletes the Machine unless retain is set. " +
				"egress confines the claimed Machine to the listed destinations (everything else is dropped at the VM edge) until the claim is released.",
			Write: true,
			Schema: mcp.Object(map[string]any{
				"namespace":   nsProp,
				"pool":        mcp.String("MachinePool name"),
				"name":        mcp.String("MachineClaim name (optional)"),
				"labels":      map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "labels added to the claimed Machine"},
				"retain":      map[string]any{"type": "boolean", "description": "keep the Machine when the claim is deleted"},
				"ttlSeconds":  mcp.Integer("delete the claim this many seconds after it binds (optional)", 1, 7*24*3600),
				"waitSeconds": mcp.Integer("seconds to wait for the bind, default 30; 0 returns at once", 0, 120),
				"egress": map[string]any{"type": "object", "description": "egress allowlist for the claimed Machine (default deny)", "properties": map[string]any{
					"allowFqdns": stringList("hostnames the guest may resolve and reach"),
					"allowSNI":   stringList("TLS SNI names allowed (exact or *.suffix)"),
					"allowCidrs": stringList("destination CIDRs allowed"),
					"allowPorts": stringList("destination ports allowed, e.g. 443 or 8000-8100"),
					"allowDNS":   stringList("DNS query names allowed; empty falls back to allowFqdns"),
					"allowIcmp":  map[string]any{"type": "boolean"},
				}},
			}, "pool"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace   string             `json:"namespace"`
					Pool        string             `json:"pool"`
					Name        string             `json:"name"`
					Labels      map[string]string  `json:"labels"`
					Retain      bool               `json:"retain"`
					TTLSeconds  int64              `json:"ttlSeconds"`
					WaitSeconds *int               `json:"waitSeconds"`
					Egress      *model.ClaimEgress `json:"egress"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Pool == "" {
					return "", fmt.Errorf("pool is required")
				}
				wait := 30
				if a.WaitSeconds != nil {
					wait = min(max(*a.WaitSeconds, 0), 120)
				}
				return withKube(ctx, time.Duration(wait)*time.Second+mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := machineRef{Namespace: a.Namespace}.ns(nsDefault)
					name := a.Name
					if name == "" {
						name = resourceName(a.Pool + "-claim-" + time.Now().UTC().Format("150405.000"))
					}
					claim := newMachineClaim(ns, name, a.Pool, a.Labels, a.Retain)
					claim.Spec.TTLSeconds = a.TTLSeconds
					claim.Spec.Egress = a.Egress
					if _, err := kc.CreateMachineClaim(ctx, ns, claim); err != nil {
						return "", err
					}
					if wait == 0 {
						return mcp.JSON(map[string]any{"claim": name, "namespace": ns, "phase": model.ClaimPending})
					}
					got, err := waitForClaim(ctx, kc, ns, name, time.Duration(wait)*time.Second)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"claim": name, "namespace": ns, "phase": got.Status.Phase, "machine": got.Status.MachineName, "bindMillis": got.Status.BindMillis, "egressPolicy": got.Status.EgressPolicy})
				})
			},
		},
		{
			Name: "fork_machine",
			Description: "Fork a Running Machine into count live copies on the same node: memory, CPU state and disk are copied from one snapshot, " +
				"so children start where the parent is, in milliseconds. Needs a flux-vm backend Machine with user or no networking. " +
				"Children are normal Machines (labelled kairon.zyvor.dev/forked-from); delete them with delete_machine. " +
				"May need a human's approval: a refused call says what to ask for; retry with the same arguments.",
			Write:    true,
			Approval: true,
			Schema: mcp.Object(map[string]any{
				"namespace":   nsProp,
				"name":        mcp.String("Machine to fork"),
				"count":       mcp.Integer("number of children, default 1", 1, maxForkCount),
				"prefix":      mcp.String("child name prefix; children are PREFIX-1..N (optional)"),
				"waitSeconds": mcp.Integer("seconds to wait for the children to run, default 60; 0 returns at once", 0, 300),
			}, "name"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace   string `json:"namespace"`
					Name        string `json:"name"`
					Count       int    `json:"count"`
					Prefix      string `json:"prefix"`
					WaitSeconds *int   `json:"waitSeconds"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Name == "" {
					return "", fmt.Errorf("name is required")
				}
				if a.Count == 0 {
					a.Count = 1
				}
				wait := 60
				if a.WaitSeconds != nil {
					wait = min(max(*a.WaitSeconds, 0), 300)
				}
				return withKube(ctx, time.Duration(wait)*time.Second+mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := machineRef{Namespace: a.Namespace}.ns(nsDefault)
					names, err := forkMachine(ctx, kc, ns, a.Name, a.Count, a.Prefix)
					if err != nil {
						return "", err
					}
					if wait > 0 {
						if err := waitForRunning(ctx, kc, ns, names, time.Duration(wait)*time.Second); err != nil {
							return "", err
						}
					}
					return mcp.JSON(map[string]any{"parent": a.Name, "namespace": ns, "children": names, "running": wait > 0})
				})
			},
		},
		{
			Name: "describe_claim",
			Description: "Show a MachineClaim: phase, bound Machine, bind time, and metered usage since binding " +
				"(active and suspended seconds, vCPU-seconds, memory GiB-seconds).",
			Schema: mcp.Object(map[string]any{
				"namespace": nsProp,
				"name":      mcp.String("MachineClaim name"),
			}, "name"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					c, err := kc.GetMachineClaim(ctx, a.ns(nsDefault), a.Name)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"claim": c.Metadata.Name, "namespace": c.Namespace(), "pool": c.Spec.PoolName,
						"phase": c.Status.Phase, "machine": c.Status.MachineName, "boundAt": c.Status.BoundAt, "bindMillis": c.Status.BindMillis,
						"ttlSeconds": c.Spec.TTLSeconds, "usage": c.Status.Usage, "message": c.Status.Message})
				})
			},
		},
		{
			Name:        "release_claim",
			Description: "Delete a MachineClaim. With reclaimPolicy Delete (the default) its Machine is deleted too.",
			Write:       true,
			Schema: mcp.Object(map[string]any{
				"namespace": nsProp,
				"name":      mcp.String("MachineClaim name"),
			}, "name"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					if err := kc.DeleteMachineClaim(ctx, a.ns(nsDefault), a.Name); err != nil {
						return "", err
					}
					return fmt.Sprintf("machineclaim %s/%s deleted", a.ns(nsDefault), a.Name), nil
				})
			},
		},
		{
			Name: "delete_machine",
			Description: "Delete a Machine and its VM. Machines owned by a MachineSet are recreated by it; scale the set instead. " +
				"May need a human's approval: a refused call says what to ask for; retry with the same arguments.",
			Write:    true,
			Approval: true,
			Schema:   refSchema(map[string]any{}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a machineRef
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					ns := a.ns(nsDefault)
					m, err := kc.GetMachine(ctx, ns, a.Name)
					if err != nil {
						return "", err
					}
					if set := m.Metadata.Labels[model.LabelMachineSet]; set != "" {
						return "", fmt.Errorf("machine %s/%s is owned by MachineSet %s; scale the set instead", ns, a.Name, set)
					}
					if err := kc.DeleteMachine(ctx, ns, a.Name); err != nil {
						return "", err
					}
					return fmt.Sprintf("machine %s/%s deleted", ns, a.Name), nil
				})
			},
		},
		{
			Name: "network_capture",
			Description: "Run a bounded tcpdump capture (1-30 s) on a Machine's VM edge via kairon-ui and FluxVM. " +
				"With output, waits for it and writes the pcap to that local path; otherwise returns the session token " +
				"(check progress with machine_network kind=network-capture).",
			Write: true,
			Schema: refSchema(map[string]any{
				"seconds": mcp.Integer("capture length in seconds", 1, 30),
				"filter":  mcp.String("optional tcpdump filter expression, e.g. \"udp port 53\""),
				"output":  mcp.String("optional local file path for the pcap"),
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					machineRef
					Seconds int    `json:"seconds"`
					Filter  string `json:"filter"`
					Output  string `json:"output"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if a.Seconds == 0 {
					a.Seconds = 10
				}
				ns := a.ns(nsDefault)
				session, err := ebpfedge.NewCapture(ns, a.Name, a.Filter, a.Seconds, time.Now().UTC())
				if err != nil {
					return "", err
				}
				ctx, cancel := context.WithTimeout(ctx, time.Duration(a.Seconds)*time.Second+45*time.Second)
				defer cancel()
				posted, err := postCapture(ctx, ns, a.Name, session)
				if err != nil {
					return "", err
				}
				if !posted {
					return "", fmt.Errorf("set KAIRON_UI_URL to run captures")
				}
				if a.Output == "" {
					return mcp.JSON(map[string]any{"token": session.Token, "seconds": session.Seconds, "state": "running"})
				}
				n, err := downloadCapture(ctx, ns, a.Name, session, a.Output)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("capture %s done: wrote %d bytes to %s", session.Token, n, a.Output), nil
			},
		},
	}
	tools = append(tools, agentPlaneTools()...)
	tools = append(tools, sessionTools(nsDefault, withKube)...)
	return append(tools, agentPlaneKubeTools(nsDefault, withKube)...)
}

func nsOrDefault(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func stringList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
