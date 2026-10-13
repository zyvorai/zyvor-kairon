// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	APIVersion           = "kairon.zyvor.dev/v1"
	KindMachine          = "Machine"
	KindMachineMigration = "MachineMigration"
	KindMachineSnapshot  = "MachineSnapshot"
	Finalizer            = "kairon.zyvor.dev/runtime-cleanup"
	// FinalizerSnapshotQuiesce guards a MachineSnapshot's deletion while it
	// may still hold a guest filesystem frozen on its target Machine (see
	// internal/controller/snapshot.go's guest quiesce protocol). Added the
	// moment a freeze is first requested, never before -- a snapshot whose
	// target Machine has no guest agent enabled, or that hasn't reached
	// that point yet, deletes exactly as before this existed.
	FinalizerSnapshotQuiesce = "kairon.zyvor.dev/snapshot-quiesce"
	// FinalizerAtlasSnapshots deletes a MachineSnapshot's Atlas snapshots.
	FinalizerAtlasSnapshots = "kairon.zyvor.dev/atlas-snapshots"
	CapableLabel            = "kairon.zyvor.dev/capable"
	// AssignedNodeLabel is stamped onto a Machine whenever the controller
	// (or a migration cutover) sets spec.nodeName. kairon-node lists with
	// this labelSelector so each agent only receives its own Machines
	// instead of listing the whole cluster every tick.
	AssignedNodeLabel = "kairon.zyvor.dev/assigned-node"
	// MigrationSourceNodeLabel is stamped when a MachineMigration's
	// status.sourceNode is known so the source node agent can list only
	// migrations it must drive.
	MigrationSourceNodeLabel = "kairon.zyvor.dev/migration-source-node"
	DefaultNamespace         = "default"
	AnnotationAdoptOnly      = "kairon.zyvor.dev/adopt-only"
	AnnotationVFIOBDF        = "kairon.zyvor.dev/vfio-bdf"
	// AnnotationForkFrom names a running Machine in the same namespace and
	// on the same node. When this Machine has no runtime yet, kairon-node
	// forks the parent's FluxVM runtime (POST /v1/vms/{id}/fork) instead
	// of booting a new one. See docs/guides/machine-fork.md.
	AnnotationForkFrom = "kairon.zyvor.dev/fork-from"
	// LabelForkedFrom is stamped on fork children so they can be listed
	// by parent.
	LabelForkedFrom        = "kairon.zyvor.dev/forked-from"
	AnnotationMigrationRef = "kairon.zyvor.dev/migration"
	// AnnotationConsoleAllowedUsers opts a Machine into a per-Machine VNC
	// console access allowlist -- a comma-separated list of kairon-ui
	// operator usernames (see internal/uiapi's consoleAuthorized). Unset
	// (the default) means every authenticated operator may open this
	// Machine's console, exactly as before this annotation existed --
	// this is opt-in, never a behavior change for a Machine that doesn't
	// set it.
	AnnotationConsoleAllowedUsers = "kairon.zyvor.dev/console-allowed-users"
	// AnnotationCordonEvacuateAttemptedAt records the last time
	// kairon-controller's opt-in cordon-evacuation reconcile step (see
	// internal/controller/cordon.go) created, or tried and was
	// disruption-budget-blocked from creating, a MachineMigration for
	// this Machine because its node was cordoned -- a per-Machine
	// cooldown so a controller reconciling every few seconds doesn't
	// create (or attempt) a new MachineMigration every single tick for a
	// Machine a budget is currently blocking. RFC3339 UTC.
	AnnotationCordonEvacuateAttemptedAt = "kairon.zyvor.dev/cordon-evacuate-attempted-at"
	// AnnotationMemoryEvacuateAttemptedAt is the memory-pressure
	// evacuation's retry cooldown, the same pattern as
	// AnnotationCordonEvacuateAttemptedAt. RFC3339 UTC.
	AnnotationMemoryEvacuateAttemptedAt = "kairon.zyvor.dev/memory-evacuate-attempted-at"
	// AnnotationQuiesceRequest/AnnotationQuiesceStatus are a request/
	// response pair kairon-controller and kairon-node use to coordinate
	// guest filesystem quiesce (real guest-fsfreeze/-thaw) around a
	// MachineSnapshot -- kairon-controller has no direct network path to
	// a Machine's FluxVM instance (only the node it's scheduled on does),
	// so this is the same "durable request in the API, not memory"
	// pattern AnnotationAdoptOnly/AnnotationMigrationRef already
	// establish for controller<->node coordination, rather than a new
	// RPC of its own. See internal/controller/snapshot.go and
	// internal/agent/quiesce.go.
	//
	// AnnotationQuiesceRequest is "<MachineSnapshot name>@<RFC3339
	// request time>", set by kairon-controller on the Machine to ask its
	// node to freeze the guest for that specific snapshot (the timestamp
	// lets the controller give up waiting after a timeout and fall back
	// to a crash-consistent snapshot, without needing a separate status
	// field to track it), and cleared (removed) by kairon-controller once
	// it's done capturing the snapshot, as the request to thaw.
	// AnnotationQuiesceStatus is the same "<name>@<time>" value, set by
	// kairon-node once it has confirmed the freeze actually happened, and
	// removed by kairon-node once it has confirmed the thaw did.
	// kairon-node treats "request present, status doesn't match" as
	// "freeze this," and "request absent, status still present" as "thaw
	// this" -- retried every reconcile tick, indefinitely, for thaw in
	// particular: a stuck-frozen guest filesystem is a real, guest-visible
	// failure mode kairon-node must never silently abandon.
	AnnotationQuiesceRequest = "kairon.zyvor.dev/quiesce-request"
	AnnotationQuiesceStatus  = "kairon.zyvor.dev/quiesce-status"
	// AnnotationSnapshotScheduleTriggerNow requests an immediate,
	// out-of-band MachineSnapshotSchedule run -- kairon-controller's
	// analog of `kubectl create job --from=cronjob`, but implemented as
	// the same durable request/status-handled annotation pattern
	// AnnotationQuiesceRequest/AnnotationCordonEvacuateAttemptedAt already
	// establish, rather than a bespoke RPC or subresource of its own. Set
	// (typically by `kaironctl trigger snapshotschedule NAME`, though any
	// distinct value works -- a plain `kubectl annotate --overwrite` with
	// any new string is just as valid a request) to any value that
	// doesn't already equal status.lastHandledTriggerTime,
	// reconcileMachineSnapshotSchedules treats that as an unhandled
	// request on its very next tick and fires this schedule immediately,
	// bypassing BOTH spec.suspend and spec.startingDeadlineSeconds (a
	// request to run right now is never "too late", and a paused schedule
	// can still be asked for one snapshot without permanently unpausing
	// it first) -- but never spec.selector, which still governs which
	// Machines actually get a new MachineSnapshot. The value itself is
	// never parsed as a timestamp by kairon-controller (unlike
	// AnnotationQuiesceRequest's "<name>@<time>" wire format) -- it's
	// compared only for equality against status.lastHandledTriggerTime,
	// so `kaironctl trigger`'s use of an RFC3339 timestamp is a convention
	// for human readability, not a contract this annotation itself
	// enforces. See MachineSnapshotScheduleStatus.LastHandledTriggerTime
	// and TriggerNowRequested.
	AnnotationSnapshotScheduleTriggerNow = "kairon.zyvor.dev/trigger-now"
	// ConditionNodeUnreachable is a MachineStatus.Conditions[].Type
	// kairon-controller sets/clears every reconcile tick to reflect
	// whether spec.nodeName currently names a Ready, present Kubernetes
	// Node -- see internal/controller/fencing.go. Detection only: nothing
	// automatically reschedules the Machine when this goes True (that
	// would risk running the same VM twice if the node isn't actually
	// dead, just unreachable) -- see ConditionFenced and
	// `kaironctl fence`.
	ConditionNodeUnreachable = "NodeUnreachable"
	// ConditionFenced records an operator-attested `kaironctl fence`
	// action: the operator has confirmed (out-of-band, e.g. power-off)
	// that the node named in Reason/Message is truly gone, not just
	// unreachable, and it's safe to let the Machine be rescheduled
	// elsewhere. Kairon cannot verify this itself.
	ConditionFenced = "Fenced"
	// NodeConditionKaironMemoryPressure is a Node condition kairon-node
	// sets True when host memory stays under pressure after every
	// elastic-memory Machine on the node is ballooned down to its minimum.
	// The scheduler places nothing new on such a node, and
	// kairon-controller -memory-pressure-evacuate migrates Machines off it.
	NodeConditionKaironMemoryPressure = "KaironMemoryPressure"
	// AnnotationMemoryOvercommitRatio on a Node lets the full memory of
	// elastic-memory Machines add up to this multiple of allocatable (a
	// decimal from 1.0 to 4.0; default 1.0, no overcommit). Guaranteed
	// memory always fits allocatable.
	AnnotationMemoryOvercommitRatio = "kairon.zyvor.dev/memory-overcommit-ratio"
)

type TypeMeta struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

type ObjectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	UID       string `json:"uid,omitempty"`
	// Generation is the apiserver's monotonically increasing desired-state
	// counter. Mirrored into status.observedGeneration on meaningful
	// Machine status writes so operators can tell whether status reflects
	// the current spec. Zero on locally constructed fixtures that never
	// round-tripped through the API.
	Generation int64 `json:"generation,omitempty"`
	// CreationTimestamp is always set by the apiserver on a real object;
	// only ever zero-value on an object this process constructed itself
	// (a request body, a test fixture) and hasn't round-tripped through
	// the API yet. internal/controller/restore.go's own
	// missingSnapshotGracePeriod check relies on that zero-value default
	// to fall safely back to prior (non-grace-period) behavior rather
	// than mistake an untimestamped object for a brand-new one.
	CreationTimestamp time.Time         `json:"creationTimestamp,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
}

type Machine struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta    `json:"metadata"`
	Spec     MachineSpec   `json:"spec"`
	Status   MachineStatus `json:"status,omitempty"`
}

type MachineList struct {
	TypeMeta `json:",inline"`
	Items    []Machine `json:"items"`
}

type MachineSpec struct {
	NodeName string `json:"nodeName,omitempty"`
	// InstanceTypeName optionally names a MachineInstanceType (same
	// namespace) kairon-controller resolves into Resources exactly once
	// -- see internal/controller/instancetype.go and
	// docs/guides/machine-instance-types.md. Only takes effect the first
	// time this is set while Resources is still empty; editing either
	// field afterward never re-resolves, matching every other
	// creation-time-only field in this project.
	InstanceTypeName string            `json:"instanceTypeName,omitempty"`
	Image            ImageSpec         `json:"image"`
	Resources        ResourceSpec      `json:"resources"`
	Runtime          RuntimeSpec       `json:"runtime,omitempty"`
	Network          NetworkSpec       `json:"network,omitempty"`
	CloudInit        CloudInitSpec     `json:"cloudInit,omitempty"`
	ServiceFabric    ServiceFabricSpec `json:"serviceFabric,omitempty"`
	PowerState       string            `json:"powerState,omitempty"`
	// Tenant is free-form metadata unless the Machine opts in. Admission
	// enforces a DNS-1123 label when it is set, and rejects a later
	// rename (internal/tenantfence). kairon.zyvor.dev/tenant-fence=true
	// makes the controller own a NetworkSecurityGroup that denies other
	// tenants' observed guest addresses; kairon-node merges that list
	// into the FluxVM policy it already posts. agent-pool and
	// require-tenant still require a tenant (internal/agentplane). MCP
	// tools honour KAIRON_MCP_TENANT the same way. Namespace remains the
	// quota and disruption boundary.
	Tenant     string `json:"tenant,omitempty"`
	TTLSeconds int64  `json:"ttlSeconds,omitempty"`
	// Priority orders competition among Machines that are all still
	// unscheduled (spec.nodeName empty) at the start of the same
	// reconcile tick -- kairon-controller attempts higher-Priority
	// pending Machines first, so a burst of new Machines that exceeds
	// available node capacity or MachineQuota headroom admits the
	// highest-Priority ones rather than whichever happened to list
	// first. Same "no fixed range, compare directly" convention as
	// PlacementSpec's own Weight fields -- higher wins, negative values
	// are valid for a below-default class, and the zero value (every
	// Machine before this field existed) sorts identically to today's
	// plain list order since sort.SliceStable never reorders equal
	// keys. Admission order for this tick's pending Machines. It does not
	// change which node an eligible Machine lands on. It preempts only
	// when the pending Machine sets kairon.zyvor.dev/preempt=true and a
	// lower-priority scheduled Machine in the same namespace sets
	// kairon.zyvor.dev/preemption-policy=Halt (internal/preempt). Halt
	// is the only action: Paused still consumes node capacity. See
	// internal/scheduler.SortByPriorityDesc and
	// docs/guides/machine-placement.md.
	Priority  int32         `json:"priority,omitempty"`
	Placement PlacementSpec `json:"placement,omitempty"`
	// Apple asks for Apple silicon features on the vz backend. See
	// docs/guides/mac-fabric.md.
	Apple    *AppleSpec      `json:"apple,omitempty"`
	Security SecuritySpec    `json:"security,omitempty"`
	Volumes  []MachineVolume `json:"volumes,omitempty"`
	// Disks are PVC-backed block disks hot-attached to the running guest
	// as SCSI disks (serial = name), added and removed live as this list
	// changes. See MachineDisk.
	Disks []MachineDisk `json:"disks,omitempty"`
	// Cdroms attach install media read-only (qemu backend) from the first
	// boot. After creation entries can only be removed: kairon-node then
	// ejects that drive's medium (live when running), so the VM can
	// live-migrate. See MachineCdrom.
	Cdroms       []MachineCdrom         `json:"cdroms,omitempty"`
	DeviceClaims []DeviceClaimReference `json:"deviceClaims,omitempty"`
	// Topology places CPUs, memory and devices by host NUMA locality
	// from the node's NodeTopology. See docs/guides/machine-topology.md.
	Topology *TopologySpec `json:"topology,omitempty"`
	// Devices are PCI devices the scheduler picks from NodeTopology and
	// kairon-node passes through with VFIO (qemu backend).
	Devices []MachineDevice `json:"devices,omitempty"`
	// Accelerators name cluster-scoped MachineAcceleratorProfiles.
	// kairon-controller expands each into spec.devices once, before the
	// Machine is scheduled. See docs/guides/accelerator-profiles.md.
	Accelerators []MachineAccelerator `json:"accelerators,omitempty"`
	// TopologyAllocation is set by the scheduler with spec.nodeName.
	TopologyAllocation *TopologyAllocation `json:"topologyAllocation,omitempty"`
	GuestAgent         GuestAgentSpec      `json:"guestAgent,omitempty"`
	// Sandbox opts this Machine into FluxVM's own agent-sandbox track
	// (BackendKind::FluxVm, its own in-tree lightweight hypervisor) --
	// see SandboxSpec's own doc comment. Nil for every other Machine.
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`
}

// SandboxSpec creates the Machine as a FluxVM "sandbox" (POST
// /v1/sandboxes) instead of a plain VM (POST /v1/vms) -- FluxVM's own
// lightweight, fast-boot in-tree hypervisor track for short-lived,
// ephemeral workloads (e.g. running an AI agent's own generated code),
// distinct from the qemu/cloud-hypervisor/firecracker backends every
// other Machine uses. FluxVM always force-enables its own vsock guest
// agent for a sandbox regardless of spec.guestAgent -- set
// spec.guestAgent.console: true anyway to unlock Kairon's own guest file
// access/agent-exec/text console features for it, which still gate on
// that flag client-side even though FluxVM itself doesn't require it.
type SandboxSpec struct {
	// TemplateName references a template built via the templates admin
	// API (see docs/guides/machine-sandboxes.md) instead of booting from
	// spec.image directly -- FluxVM loads the template's own pre-baked
	// spec (image, resources, kernel) and ignores spec.image/
	// spec.resources/spec.runtime.kernel entirely when this is set. Kept
	// separate from spec.image.source (image import): a template is a
	// pre-built, ready-to-boot rootfs snapshot, not just a cached disk
	// image.
	TemplateName string `json:"templateName,omitempty"`
	// HTTPProxyPorts are the sandbox's own guest TCP ports reachable
	// through Kairon's HTTP proxy relay (see machine-sandboxes.md) --
	// e.g. a webhook receiver or dev server running inside the sandbox.
	// Requires spec.network to actually give the sandbox a guest IP
	// (tap/bridge networking, not user/SLIRP -- FluxVM's own proxy needs
	// to dial the guest directly).
	HTTPProxyPorts []uint16 `json:"httpProxyPorts,omitempty"`
}

// GuestAgentSpec opts a Machine into FluxVM's real qemu-guest-agent
// (virtio-serial) channel -- off by default, since it requires the guest
// image to actually run qemu-guest-agent (e.g. via
// spec.cloudInit.packages) to be useful. Once enabled, kairon-node uses it
// to resolve status.guestIP for network modes with no DHCP lease file to
// parse (spec.network.mode: user/SLIRP in particular) -- see
// docs/guides/machine-guest-agent.md.
type GuestAgentSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// Console opts into a completely different channel: FluxVM's own
	// bespoke vsock guest agent (fluxvm.AgentSpec), which requires FluxVM's
	// own proprietary fluxvm-guest-agent binary installed and running
	// inside the guest image -- not the standard qemu-guest-agent Enabled
	// above needs. It backs the interactive text console
	// (internal/consoleproxy's text-console relay) -- see
	// docs/guides/machine-text-console.md. Independent of Enabled: a
	// Machine can set either, both, or neither.
	Console bool `json:"console,omitempty"`
}

// CloudInitSpec injects operator-supplied guest customization at first boot,
// forwarded verbatim into FluxVM's own cloud-init NoCloud seed image
// (github.com/zyvorai/fluxvm crates/fluxvm-core/src/model.rs CloudInitSpec).
// Only applied when a Machine's FluxVM runtime is first created
// (internal/agent.reconcileMachine) -- editing it on an existing Machine has
// no effect, same as spec.network.forwards and spec.resources.
type CloudInitSpec struct {
	Hostname          string   `json:"hostname,omitempty"`
	User              string   `json:"user,omitempty"`
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
	Packages          []string `json:"packages,omitempty"`
	RunCmd            []string `json:"runCmd,omitempty"`
	// WriteFiles drops files into the guest before first boot via
	// cloud-init's own write_files module -- e.g. a systemd unit or an
	// app config file, without a custom image build. Forwarded verbatim
	// to FluxVM's own CloudInitSpec.write_files, which already existed
	// and was simply never surfaced here until now.
	WriteFiles []CloudInitFile `json:"writeFiles,omitempty"`
}

// CloudInitFile is one entry of CloudInitSpec.WriteFiles, mirroring
// FluxVM's own CloudInitFile exactly.
type CloudInitFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Permissions is an octal file mode string, e.g. "0644" -- defaults to
	// cloud-init's own default (0644) when unset.
	Permissions string `json:"permissions,omitempty"`
}

type ImageSpec struct {
	// Path is the boot disk's real path on the target node -- required
	// unless Source is set, in which case kairon-node fills this in
	// itself (see internal/agent/imageimport.go) after resolving Source
	// into its local image cache; a Machine author never needs to know
	// or predict that path themselves.
	Path   string       `json:"path,omitempty"`
	Digest string       `json:"digest,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
	// CatalogName references a FluxVM image-catalog entry (registered via
	// the node-scoped catalog admin API, see docs/guides/machine-image-catalog.md)
	// by name instead of a raw disk path -- FluxVM's own CreateVmRequest.image
	// field already accepts either interchangeably, so this is passed
	// through as-is rather than resolved by kairon-node itself the way
	// Source is. Mutually exclusive with Path/Source in practice (whichever
	// is checked first at reconcile time wins); exempt from --image-root
	// path fencing since it was never a filesystem path to begin with --
	// FluxVM's own catalog integrity checks (mandatory SHA-256, optional
	// Ed25519 signature) are the trust boundary here instead.
	CatalogName string `json:"catalogName,omitempty"`
	// ImageRef names a cluster-scoped MachineImage of kind disk.
	// kairon-controller copies its source and digest into this spec once,
	// before the Machine boots, so republishing the image never changes what
	// this Machine boots. See docs/guides/machine-images.md.
	ImageRef string `json:"imageRef,omitempty"`
	// Blank boots an empty root disk of DiskSize instead of an image -- the
	// target of an install from spec.cdroms. With spec.volumes[0], the empty
	// disk is created inside the volume instead.
	Blank bool `json:"blank,omitempty"`
	// DiskSize grows the root disk to this quantity (e.g. 60Gi) at creation.
	// Required with Blank; creation-time-only like the rest of spec.image.
	DiskSize string `json:"diskSize,omitempty"`
	// Storage is the FluxVM storage backend kairon-node chose for this boot
	// disk (e.g. ceph-rbd-in-place for an Atlas RBD volume). Never read
	// from or written to the API, so a Machine author can't select it.
	Storage string `json:"-"`
	// IO tunes the root disk's I/O on the qemu backend: aio, cache,
	// iothread and queues (the root is always virtio-blk). Creation-time
	// only.
	IO *DiskIO `json:"io,omitempty"`
}

// DiskIO is how FluxVM's QEMU attaches one disk and does its I/O. Unset
// fields keep FluxVM's defaults (SCSI for data disks, writeback cache, the
// thread pool, no iothread). See docs/guides/machine-storage-nvmeof.md.
type DiskIO struct {
	// Bus is scsi (default), virtio-blk, or vhost-user-blk (served by a
	// per-disk qemu-storage-daemon). Data disks only.
	Bus string `json:"bus,omitempty"`
	// AIO is io_uring, native (needs cache none or directsync) or threads.
	AIO string `json:"aio,omitempty"`
	// Cache is none, writeback, writethrough, directsync or unsafe.
	Cache string `json:"cache,omitempty"`
	// IOThread gives the disk a dedicated QEMU iothread (virtio buses).
	IOThread bool `json:"iothread,omitempty"`
	// Queues is the number of virtio queues, 1-64 (virtio buses).
	Queues int `json:"queues,omitempty"`
}

// ImageSource names a golden image kairon-node itself downloads into a
// node-local, digest-keyed cache instead of requiring an operator to have
// already placed a file at Path -- see internal/agent/imageimport.go.
// Digest is mandatory whenever Source is set: it's the cache key, so
// without it two Machines naming the same (mutable) HTTPURL would have no
// way to know whether they mean the same bytes.
type ImageSource struct {
	// HTTPURL is a plain http(s):// URL to a qcow2/raw image file.
	HTTPURL string `json:"httpURL,omitempty"`
	// OCI is a containerDisk registry reference
	// ([registry/]repository[:tag][@sha256:...]). kairon-node pulls the
	// manifest named by spec.image.digest over HTTPS and extracts the
	// image's disk/ file. Mutually exclusive with HTTPURL.
	OCI string `json:"oci,omitempty"`
	// Format is the downloaded file's format: qcow2 or raw (default,
	// booted as-is), or ova, vmdk, vhd, vhdx, which kairon-node converts
	// through FluxVM's image import before boot.
	Format string `json:"format,omitempty"`
	// Repair runs FluxVM's offline guest repair for VMs coming from
	// VMware or another hypervisor (virtio initramfs, /dev/sdX to
	// /dev/vdX, VMware tools disabled, DHCP fallback). Implies an import.
	Repair bool `json:"repair,omitempty"`
	// InsecureSkipTLSVerify downloads httpURL without verifying the server
	// certificate (a self-signed image server). The digest check still
	// guarantees the bytes; only confidentiality of the transfer is lost.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// ImageSourceFormats are the accepted ImageSource.Format values.
var ImageSourceFormats = []string{"", "qcow2", "raw", "ova", "vmdk", "vhd", "vhdx"}

// NeedsImport reports whether the source goes through FluxVM's import
// (conversion and/or repair) rather than being booted as downloaded.
func (s ImageSource) NeedsImport() bool {
	switch s.Format {
	case "", "qcow2", "raw":
		return s.Repair
	}
	return true
}

type ResourceSpec struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	// MaxCPU/MaxMemory request more CPU/DIMM hotplug headroom than
	// FluxVM's own default (roughly double CPU, and Memory+2Gi or double,
	// whichever is larger -- see docs/guides/machine-hotplug.md). Creation-
	// time-only, like CPU/Memory's own boot-time sizing; unlike them,
	// there's no live-reconcile path for these, since headroom is baked
	// into the VM's boot-time -smp/-m arguments (maxcpus=/maxmem=) and
	// can't be changed on an already-running Machine at all. Same string
	// quantity format as CPU/Memory. FluxVM silently clamps MaxCPU up to
	// at least CPU if set too low (never an error). A MaxMemory at or
	// below Memory is not sent, so FluxVM uses its default headroom.
	MaxCPU    string `json:"maxCpu,omitempty"`
	MaxMemory string `json:"maxMemory,omitempty"`
	// Hugepages, NUMANode, and CPUSet are direct, opt-in passthroughs to
	// FluxVM's own existing QEMU-backend-only support for the same
	// (fluxvm-core's CreateVmRequest already has hugepages/numa_node/
	// cpuset -- this is genuinely new plumbing on Kairon's side, not
	// something blocked upstream the way VFIO-through-live-migration
	// was). See docs/guides/machine-cpu-numa.md for exactly what each
	// does and doesn't guarantee, and internal/fluxvm.Client.CreateWithVFIO
	// for the qemu-backend-only enforcement.
	//
	Hugepages bool   `json:"hugepages,omitempty"`
	NUMANode  *int   `json:"numaNode,omitempty"`
	CPUSet    string `json:"cpuSet,omitempty"`
	// CPUPinning opts a Machine into real, exclusive host-core allocation
	// -- distinct from CPUSet above, which is a pure guest-visible vNUMA
	// hint with no host-side enforcement at all. When true,
	// internal/scheduler allocates AllocatedCPUSet (below) from a node's
	// operator-asserted PinnableCPUsLabel at scheduling time, as a real
	// capacity filter (a node without enough free exclusive cores is
	// ineligible), the capacity-allocator problem the CPUSet doc comment
	// above used to flag as out of scope. QEMU-only, same as
	// Hugepages/NUMANode/CPUSet. See docs/guides/machine-cpu-pinning.md.
	CPUPinning bool `json:"cpuPinning,omitempty"`
	// AllocatedCPUSet is the scheduler's own decision of exactly which
	// host CPU numbers this Machine exclusively owns -- system-computed,
	// the same "user doesn't set this, the scheduler fills it in" relationship
	// spec.nodeName itself already has, not an operator-facing request
	// field. Patched atomically alongside spec.nodeName by
	// internal/controller at scheduling time (never by kairon-node), so
	// the two can never land separately. See status.appliedCpuSet for
	// confirmation this was actually written to the cgroup.
	AllocatedCPUSet []uint32 `json:"allocatedCpuSet,omitempty"`
	// Limits is a live, mutable host-side cap on the Machine's already-running
	// VMM process cgroup -- a ceiling the host enforces, distinct from CPU/
	// Memory above (which describe what the *guest* sees, realized via boot-time
	// sizing plus internal/agent/hotplug.go's live grow-only hotplug). Backed by
	// FluxVM's own POST /v1/vms/{id}/resources (cgroup v2 CPU quota/memory.max/
	// io.weight/pids.max), which is backend-agnostic (it operates on the cgroup
	// the VMM process itself runs in, not a backend-specific API) -- unlike
	// Hugepages/NUMANode/CPUSet above, Limits works for every backend, not just
	// qemu. Reconciled continuously (internal/agent/resourcelimits.go), and
	// -- unlike hotplug -- can be raised or lowered freely at any time, since a
	// cgroup limit change has no "can't unplug a vCPU" asymmetry to worry about.
	Limits *ResourceLimits `json:"limits,omitempty"`
	// ElasticMemory gives the Machine a virtio-balloon that kairon-node
	// inflates under host memory pressure, never below Min, and deflates
	// again once the pressure clears. The scheduler counts Min as the
	// guaranteed share and Memory against the node's overcommit ratio.
	// QEMU only; refused with Hugepages and passthrough devices, whose
	// guest memory is pinned. See docs/guides/machine-elastic-memory.md.
	ElasticMemory *ElasticMemorySpec `json:"elasticMemory,omitempty"`
}

// ElasticMemorySpec is the reclaimable part of a Machine's memory.
type ElasticMemorySpec struct {
	// Min is the memory the guest always keeps, as a quantity (for
	// example "1Gi"). Must be at most spec.resources.memory.
	Min string `json:"min"`
	// FreePageReporting lets the guest hand freed pages back to the host
	// without a balloon target (guest kernel 5.8 or newer). Defaults to
	// true.
	FreePageReporting *bool `json:"freePageReporting,omitempty"`
}

// FreePageReportingEnabled reports the effective FreePageReporting.
func (e *ElasticMemorySpec) FreePageReportingEnabled() bool {
	return e == nil || e.FreePageReporting == nil || *e.FreePageReporting
}

// MemoryStatus is the balloon state of an elastic-memory Machine as
// kairon-node last applied it.
type MemoryStatus struct {
	// CurrentMiB is guest memory including hot-added DIMMs.
	CurrentMiB uint64 `json:"currentMiB,omitempty"`
	// MinMiB is spec.resources.elasticMemory.min in MiB.
	MinMiB uint64 `json:"minMiB,omitempty"`
	// BalloonTargetMiB is the memory kairon-node asked the guest to give
	// back; 0 means fully deflated.
	BalloonTargetMiB uint64 `json:"balloonTargetMiB"`
	// BalloonActualMiB is what the guest driver has given back so far.
	BalloonActualMiB uint64 `json:"balloonActualMiB"`
	// GuestAvailableMiB is the guest's own MemAvailable, when its balloon
	// driver reports statistics.
	GuestAvailableMiB *uint64 `json:"guestAvailableMiB,omitempty"`
	// Reason is why the target was last changed: HostPressure, Relief or
	// Restart.
	Reason string `json:"reason,omitempty"`
	// LastChangeTime is when kairon-node last changed the target.
	LastChangeTime string `json:"lastChangeTime,omitempty"`
}

// ResourceLimits are host cgroup v2 controls for an already-running Machine
// -- see ResourceSpec.Limits's own doc comment for why this is a separate,
// live-mutable field rather than a creation-time-only one.
type ResourceLimits struct {
	// CPUQuotaPercent caps CPU as a percentage of one host core (200 = 2
	// full cores) -- FluxVM's own cgroup v2 cpu.max, translated from a
	// percentage the same way its ResourcePatch::cpu_quota_percent already
	// is.
	CPUQuotaPercent *uint32 `json:"cpuQuotaPercent,omitempty"`
	// MemoryMaxBytes caps the VMM process's own cgroup memory.max -- a
	// hard host-enforced ceiling, distinct from spec.resources.memory
	// (what the guest is told it has).
	MemoryMaxBytes *uint64 `json:"memoryMaxBytes,omitempty"`
	// IOWeight is a relative cgroup v2 io.weight (1-10000, FluxVM's own
	// default 100) against other cgroups competing for the same block
	// device -- a priority, not an absolute cap.
	IOWeight *uint32 `json:"ioWeight,omitempty"`
	// PIDsMax caps the number of processes/threads inside the VMM
	// process's own cgroup (pids.max) -- a containment bound against a
	// runaway guest/VMM process tree, not a guest-visible limit.
	PIDsMax *uint64 `json:"pidsMax,omitempty"`
	// CPUSetCPUs is a real cgroup v2 cpuset.cpus write -- exact,
	// exclusive host core numbers, FluxVM's own ResourcePatch.cpuset_cpus.
	// Populated from spec.resources.allocatedCpuSet by
	// internal/agent/resourcelimits.go once the scheduler has made an
	// allocation decision (see ResourceSpec.CPUPinning) -- never set
	// directly by a Machine author the way the other fields in this
	// struct are.
	CPUSetCPUs []uint32 `json:"cpuSetCpus,omitempty"`
}

// ResourceUsage mirrors FluxVM's own VmMetrics exactly (GET
// /v1/vms/{id}/stats) -- see MachineStatus.ResourceUsage's own doc
// comment.
type ResourceUsage struct {
	// CPUPercent is a percentage of one core, averaged over the VMM
	// process's entire lifetime (not an instantaneous rate) -- can
	// exceed 100 for a multi-vCPU Machine using more than one core's
	// worth of time.
	CPUPercent     float64 `json:"cpuPercent"`
	MemoryBytes    uint64  `json:"memoryBytes"`
	DiskReadBytes  uint64  `json:"diskReadBytes"`
	DiskWriteBytes uint64  `json:"diskWriteBytes"`
}

type RuntimeSpec struct {
	Backend string `json:"backend,omitempty"`
	Kernel  string `json:"kernel,omitempty"`
}

type PlacementSpec struct {
	Architecture string            `json:"architecture,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Affinity/AntiAffinity are required (hard) constraints -- filters
	// consulted by internal/scheduler.Scheduler.eligible against every
	// other currently-scheduled Machine; a node failing one of these is
	// never a candidate at all. PreferredAffinity/PreferredAntiAffinity and
	// TopologySpreadConstraints below are soft: they only ever influence
	// which *eligible* node wins via internal/scheduler's weighted scoring,
	// they can never reject a node outright.
	Affinity     []MachineAffinityTerm `json:"affinity,omitempty"`
	AntiAffinity []MachineAffinityTerm `json:"antiAffinity,omitempty"`
	// PreferredAffinity/PreferredAntiAffinity add/subtract each term's
	// Weight to a candidate node's score when satisfied -- see
	// internal/scheduler.score. A Machine with none of these set schedules
	// identically to before this field existed (least-loaded, deterministic
	// hash tie-break): the scoring pass degenerates to exactly that when
	// there's nothing to prefer.
	PreferredAffinity     []WeightedAffinityTerm `json:"preferredAffinity,omitempty"`
	PreferredAntiAffinity []WeightedAffinityTerm `json:"preferredAntiAffinity,omitempty"`
	// TopologySpreadConstraints favors, for each constraint, whichever
	// eligible node's TopologyKey-domain currently has the fewest other
	// Machines matching LabelSelector -- this scoring pass (see
	// internal/scheduler.topologySpreadPenalty) applies to every
	// constraint regardless of WhenUnsatisfiable, mirroring how a real
	// Kubernetes topology-spread scoring plugin doesn't care about that
	// field either. WhenUnsatisfiable: "DoNotSchedule" additionally
	// hard-filters: a node is never a candidate at all if placing this
	// Machine there would push that constraint's skew over MaxSkew (see
	// internal/scheduler.filterMaxSkew) -- the default, "" (equivalent to
	// "ScheduleAnyway"), keeps every existing topologySpreadConstraints
	// spec scheduling identically to before this field existed: scoring
	// only, never a hard rejection.
	TopologySpreadConstraints []TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// Tolerations lets this Machine schedule onto a node despite a
	// matching Node.spec.taint -- see internal/scheduler.eligible/score
	// and Taint's own doc comment. A Machine with no Tolerations set
	// schedules identically to before this field existed: every tainted
	// node is either already excluded (NoSchedule/NoExecute) or already
	// penalized (PreferNoSchedule) regardless, so an empty Tolerations
	// list changes nothing about a fleet running on untainted nodes.
	Tolerations []Toleration `json:"tolerations,omitempty"`
}

// MachineAffinityTerm is satisfied when at least one (Affinity) / no
// (AntiAffinity) other Machine matching LabelSelector currently sits on a
// node sharing the candidate node's value for the TopologyKey label
// (e.g. "kubernetes.io/hostname" for same/different-node, or a rack/zone
// label). Mirrors the shape of Kubernetes Pod affinity terms closely enough
// to be immediately familiar, deliberately not reinvented.
type MachineAffinityTerm struct {
	LabelSelector map[string]string `json:"labelSelector"`
	TopologyKey   string            `json:"topologyKey"`
}

// WeightedAffinityTerm is a MachineAffinityTerm plus a Weight (mirrors
// Kubernetes' own PreferredSchedulingTerm shape): internal/scheduler.score
// adds Weight to a candidate node's score when the term is satisfied (for
// PreferredAffinity) or subtracts it (for PreferredAntiAffinity). Weight
// has no fixed range -- it's compared directly against the scheduler's
// load-balancing penalty (1 point per already-assigned Machine on that
// node), so a Weight of, say, 20 comfortably outweighs a handful of
// existing Machines' worth of load imbalance.
type WeightedAffinityTerm struct {
	Weight              int32 `json:"weight"`
	MachineAffinityTerm `json:",inline"`
}

// TopologySpreadConstraint favors spreading Machines matching
// LabelSelector evenly across the distinct values of the TopologyKey
// label -- always, as a soft scoring signal. WhenUnsatisfiable additionally
// controls whether MaxSkew is hard-enforced -- see PlacementSpec's doc
// comment.
type TopologySpreadConstraint struct {
	TopologyKey   string            `json:"topologyKey"`
	LabelSelector map[string]string `json:"labelSelector"`
	MaxSkew       int32             `json:"maxSkew,omitempty"`
	// WhenUnsatisfiable mirrors the Kubernetes field: "DoNotSchedule" hard-
	// enforces MaxSkew (a node that would exceed it is never eligible);
	// "ScheduleAnyway", or empty/unset (the default, and this project's
	// original behavior), never rejects a node over skew -- MaxSkew only
	// ever influences scoring.
	WhenUnsatisfiable string `json:"whenUnsatisfiable,omitempty"`
}

// Toleration mirrors the real Kubernetes core/v1.Toleration shape closely
// enough to be immediately familiar, same reasoning MachineAffinityTerm's
// own doc comment gives for its Pod-affinity-term shape. See
// internal/scheduler.toleratesTaint for exact matching semantics
// (mirroring Kubernetes' own Toleration.ToleratesTaint):
//
//   - Operator "Equal" (the default when empty, same as Kubernetes) requires
//     Key and Value to both match the taint exactly.
//   - Operator "Exists" matches any taint with the given Key (Value is
//     ignored) -- an empty Key with Operator "Exists" tolerates every taint,
//     regardless of key, value, or effect (Kubernetes' own "tolerate
//     everything" wildcard toleration shape).
//   - Effect, if set, additionally restricts which taint effect this
//     toleration applies to; empty Effect tolerates a matching Key/Value
//     under any effect.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

const (
	TolerationOpEqual  = "Equal"
	TolerationOpExists = "Exists"
)

const (
	WhenUnsatisfiableDoNotSchedule  = "DoNotSchedule"
	WhenUnsatisfiableScheduleAnyway = "ScheduleAnyway"
)

type SecuritySpec struct {
	SecureBoot bool `json:"secureBoot,omitempty"`
	TPM        bool `json:"tpm,omitempty"`
}

// MachineDisk attaches a PersistentVolumeClaim as a block disk. A
// Block-mode hostPath/local PV attaches its device path; a Filesystem-mode
// one attaches the disk.img inside its directory. Removing the entry
// detaches the disk; the PVC's data is never touched.
type MachineDisk struct {
	// Name is the FluxVM disk name and the guest-visible SCSI serial
	// (/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_<name>).
	Name      string `json:"name"`
	ClaimName string `json:"claimName"`
	// IO picks the bus and I/O options FluxVM attaches this disk with.
	IO *DiskIO `json:"io,omitempty"`
}

// MachineCdrom is one read-only ISO attached as a SATA CD-ROM. Set either
// ImageRef (a MachineImage of kind iso, resolved by kairon-controller like
// spec.image.imageRef) or Source plus Digest. kairon-node downloads it into
// the same digest-keyed cache as spec.image.source.
type MachineCdrom struct {
	Name     string       `json:"name"`
	ImageRef string       `json:"imageRef,omitempty"`
	Source   *ImageSource `json:"source,omitempty"`
	Digest   string       `json:"digest,omitempty"`
	// Path is the cached ISO kairon-node resolved; never read from or
	// written to the API.
	Path string `json:"-"`
}

// MaxCdroms matches FluxVM's limit (AHCI ports on q35).
const MaxCdroms = 4

// ValidateCdroms checks names and sources. An entry with only ImageRef is
// valid here; kairon-controller fills in Source and Digest before boot.
func ValidateCdroms(cdroms []MachineCdrom) error {
	if len(cdroms) > MaxCdroms {
		return fmt.Errorf("spec.cdroms: at most %d entries", MaxCdroms)
	}
	seen := map[string]bool{}
	for i, c := range cdroms {
		if !diskNameRE.MatchString(c.Name) || c.Name == "root" {
			return fmt.Errorf("spec.cdroms[%d].name %q: use 1-32 of [a-z0-9-], starting alphanumeric, not 'root'", i, c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("spec.cdroms[%d]: duplicate name %q", i, c.Name)
		}
		seen[c.Name] = true
		if c.Source == nil {
			if strings.TrimSpace(c.ImageRef) == "" {
				return fmt.Errorf("spec.cdroms[%d] needs imageRef or source", i)
			}
			continue
		}
		if c.Source.OCI != "" || (c.Source.Format != "" && c.Source.Format != "raw") || c.Source.Repair {
			return fmt.Errorf("spec.cdroms[%d].source: install media must be an http(s) URL to raw bytes (no oci, format or repair)", i)
		}
		if err := ValidateImageSource(ImageSpec{Source: c.Source, Digest: c.Digest}); err != nil {
			return fmt.Errorf("spec.cdroms[%d]: %s", i, strings.TrimPrefix(err.Error(), "spec.image."))
		}
	}
	return nil
}

// ValidateCdromsUpdate allows an update to spec.cdroms only to remove
// entries: FluxVM can eject install media from a created VM but not insert
// new media. An imageRef entry that kairon-controller has since resolved
// (Source and Digest filled in) is still the same entry.
func ValidateCdromsUpdate(old, updated []MachineCdrom) error {
	prev := make(map[string]MachineCdrom, len(old))
	for _, c := range old {
		prev[c.Name] = c
	}
	for i, c := range updated {
		o, ok := prev[c.Name]
		if !ok {
			return fmt.Errorf("spec.cdroms[%d] %q: entries can't be added after creation (only removed, which ejects the medium)", i, c.Name)
		}
		if o.ImageRef != c.ImageRef || (o.Source != nil && (!reflect.DeepEqual(o.Source, c.Source) || o.Digest != c.Digest)) {
			return fmt.Errorf("spec.cdroms[%d] %q: entries can't be changed after creation (only removed, which ejects the medium)", i, c.Name)
		}
	}
	return nil
}

// ValidateRootImage checks the spec.image fields added for catalog images
// and blank installs: Blank excludes every other boot source and needs a
// DiskSize, and DiskSize must parse.
func ValidateRootImage(img ImageSpec) error {
	if img.DiskSize != "" {
		if _, err := ParseDiskSizeGiB(img.DiskSize); err != nil {
			return fmt.Errorf("spec.image.diskSize: %w", err)
		}
	}
	if img.Blank {
		if img.DiskSize == "" {
			return fmt.Errorf("spec.image.blank needs spec.image.diskSize")
		}
		if img.Path != "" || img.Source != nil || img.CatalogName != "" || img.ImageRef != "" {
			return fmt.Errorf("spec.image.blank excludes path, source, catalogName and imageRef")
		}
	}
	if img.ImageRef != "" && (img.Path != "" || img.CatalogName != "") {
		return fmt.Errorf("spec.image.imageRef excludes path and catalogName")
	}
	return nil
}

// ValidateDisks checks spec.disks: unique FluxVM-safe names and a claim.
// ImportDiskName is the data disk name kairon-node gives the n-th extra
// disk (from 1) of an imported multi-disk image. spec.disks may not use the
// prefix.
func ImportDiskName(n int) string { return fmt.Sprintf("%s%d", importDiskPrefix, n) }

const importDiskPrefix = "import-disk"

func ValidateDisks(disks []MachineDisk) error {
	seen := map[string]bool{}
	for i, d := range disks {
		if !diskNameRE.MatchString(d.Name) || d.Name == "root" {
			return fmt.Errorf("spec.disks[%d].name %q: use 1-32 of [a-z0-9-], starting alphanumeric, not 'root'", i, d.Name)
		}
		if strings.HasPrefix(d.Name, importDiskPrefix) {
			return fmt.Errorf("spec.disks[%d].name %q: the %s prefix is reserved for imported image disks", i, d.Name, importDiskPrefix)
		}
		if seen[d.Name] {
			return fmt.Errorf("spec.disks[%d]: duplicate name %q", i, d.Name)
		}
		seen[d.Name] = true
		if strings.TrimSpace(d.ClaimName) == "" {
			return fmt.Errorf("spec.disks[%d] requires claimName", i)
		}
	}
	return nil
}

var diskNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type MachineVolume struct {
	Name      string `json:"name"`
	ClaimName string `json:"claimName"`
	// GuestPath is where volumes[1+] are mounted inside the guest via
	// FluxVM virtiofs (shared_folders). Empty defaults to /mnt/<name>.
	// volumes[0] remains the boot disk (disk.img) and ignores GuestPath.
	GuestPath string `json:"guestPath,omitempty"`
	// ReadOnly marks a data volume (volumes[1+]) as a read-only virtiofs share.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Atlas asks kairon-controller to provision this volume through the
	// Atlas storage control plane before the Machine is scheduled. In
	// pvc mode the controller writes the resulting PVC name into
	// ClaimName; ClaimName must be empty or already match it.
	Atlas *AtlasVolumeSource `json:"atlas,omitempty"`
}

const (
	AtlasModePVC = "pvc"
	AtlasModeRBD = "rbd"
	// FinalizerAtlasVolumes keeps a Machine around until kairon-controller
	// has released its Atlas volumes. Released only after the node has
	// dropped Finalizer (runtime-cleanup), so no VM still has the disk open.
	FinalizerAtlasVolumes = "kairon.zyvor.dev/atlas-volumes"
	// AnnotationAtlasVolumes holds the controller-owned JSON map of
	// volume name -> AtlasVolumeState. Kept out of status because
	// kairon-node patches status wholesale from its own (possibly stale) copy.
	AnnotationAtlasVolumes = "kairon.zyvor.dev/atlas-volumes"

	AtlasPhaseProvisioning = "Provisioning"
	AtlasPhaseReady        = "Ready"
	AtlasPhaseFailed       = "Failed"
	AtlasPhaseDeleting     = "Deleting"
)

type AtlasVolumeSource struct {
	// Size is a Kubernetes-style quantity (e.g. 20Gi).
	Size string `json:"size"`
	// Mode is pvc (default: Atlas creates a PVC on a StorageClass) or rbd
	// (Atlas creates a raw Ceph RBD image; needs FluxVM in-place RBD).
	Mode         string `json:"mode,omitempty"`
	Policy       string `json:"policy,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	BackendID    string `json:"backendID,omitempty"`
	Pool         string `json:"pool,omitempty"`
	// Retain keeps the Atlas volume when the Machine is deleted.
	Retain bool `json:"retain,omitempty"`
}

var atlasNameInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// AtlasVolumeName is the Atlas volume / PVC / RBD image name for one
// Machine volume: RFC 1123, at most 63 characters, and always suffixed with
// a hash of namespace/machine/volume so names never collide across
// namespaces. kairon-node recomputes it to check that an RBD image recorded
// in the (user-writable) annotation really belongs to this Machine.
func AtlasVolumeName(ns, machine, volume string) string {
	sum := sha256.Sum256([]byte(ns + "/" + machine + "/" + volume))
	readable := atlasNameInvalid.ReplaceAllString(strings.ToLower(machine+"-"+volume), "-")
	readable = strings.Trim(readable, "-")
	if len(readable) > 52 {
		readable = strings.TrimRight(readable[:52], "-")
	}
	return readable + "-" + hex.EncodeToString(sum[:])[:10]
}

// AtlasVolumeStates decodes the controller-owned volume state annotation.
func AtlasVolumeStates(m Machine) map[string]AtlasVolumeState {
	out := map[string]AtlasVolumeState{}
	if raw := m.Metadata.Annotations[AnnotationAtlasVolumes]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// ParseRBDNativeID splits an Atlas "rbd:pool/image" backend id.
func ParseRBDNativeID(id string) (pool, image string, ok bool) {
	rest, found := strings.CutPrefix(id, "rbd:")
	if !found {
		return "", "", false
	}
	pool, image, ok = strings.Cut(rest, "/")
	return pool, image, ok && pool != "" && image != "" && !strings.Contains(image, "/")
}

func (s *AtlasVolumeSource) EffectiveMode() string {
	if s == nil || s.Mode == "" {
		return AtlasModePVC
	}
	return s.Mode
}

type AtlasVolumeState struct {
	VolumeID  string `json:"volumeID,omitempty"`
	JobID     string `json:"jobID,omitempty"`
	NativeID  string `json:"nativeID,omitempty"`
	ClaimName string `json:"claimName,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Message   string `json:"message,omitempty"`
}

type DeviceClaimReference struct {
	Name string `json:"name"`
}

type MachineStatus struct {
	Phase     string `json:"phase,omitempty"`
	NodeName  string `json:"nodeName,omitempty"`
	RuntimeID string `json:"runtimeID,omitempty"`
	// GuestIP is the single "primary" address (fluxvm.BestGuestIP's pick,
	// or the DHCP-lease address when one exists) -- kept for backward
	// compatibility with existing consumers of this field (e.g. the
	// `kubectl get machine` IP printer column). GuestIPs is the full
	// address list (multi-NIC, IPv4 and IPv6); when non-empty,
	// GuestIPs[0] == GuestIP.
	GuestIP            string                `json:"guestIP,omitempty"`
	GuestIPs           []string              `json:"guestIPs,omitempty"`
	Network            *MachineNetworkStatus `json:"network,omitempty"`
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	// Message deliberately has no omitempty: this whole status object is
	// sent as one JSON merge patch (internal/kube.Client.PatchMachineStatus),
	// and RFC 7386 merge-patch semantics treat an *absent* key as "leave
	// unchanged", not "clear" -- only an explicit `null` (or, for a plain
	// string field, an explicit `""` present in the patch) can clear a
	// previously-set value. Dropping this field via omitempty when a
	// reconcile recovers from an earlier error would silently leave the
	// stale error message in status forever.
	Message    string      `json:"message"`
	Conditions []Condition `json:"conditions,omitempty"`
	// AppliedVCPUs/AppliedMemoryMiB track what Kairon has actually hotplugged
	// into the live FluxVM runtime so far -- FluxVM has no query endpoint for
	// "current live vcpus/memory" (hotplugged CPUs/DIMMs are pure QMP-time
	// state, never persisted back into its own VM record), so Kairon is the
	// only source of truth for how much of spec.resources has been realized.
	// Seeded from spec.resources at creation time; reset whenever the FluxVM
	// runtime is recreated (a stop/start cycle loses every hotplugged
	// resource, since they're not part of the boot-time -smp/-m args).
	AppliedVCPUs     uint32 `json:"appliedVCPUs,omitempty"`
	AppliedMemoryMiB uint64 `json:"appliedMemoryMiB,omitempty"`
	// AppliedResourceLimits records the last spec.resources.limits Kairon
	// successfully applied to the live FluxVM cgroup -- unlike
	// AppliedVCPUs/AppliedMemoryMiB (a running tally of monotonically-grown
	// hotplug additions), this is a straight copy of the last-applied desired
	// state, since cgroup limits can be raised or lowered freely; it exists
	// purely so internal/agent/resourcelimits.go can skip a redundant FluxVM
	// call when nothing has actually changed since the last reconcile tick.
	AppliedResourceLimits *ResourceLimits `json:"appliedResourceLimits,omitempty"`
	// AppliedTopology is the NUMA binding FluxVM reported for the running
	// VM (spec.topologyAllocation Machines only).
	AppliedTopology *AppliedTopology `json:"appliedTopology,omitempty"`
	// Memory is the balloon state of a spec.resources.elasticMemory
	// Machine, written by kairon-node when it changes the target.
	Memory *MemoryStatus `json:"memory,omitempty"`
	// Hibernation is set while the guest's state is saved
	// (spec.powerState: Hibernated) and until it has resumed.
	Hibernation *HibernationStatus `json:"hibernation,omitempty"`
	// ResourceUsage is this Machine's cgroup-derived resource usage as of
	// the last *meaningful* status write (FluxVM's GET /v1/vms/{id}/stats).
	// High-frequency samples go to Prometheus (kairon_machine_resource_usage)
	// instead of continuous etcd patches; kaironctl top / UI node-usage
	// therefore reflect the last phase/condition/etc. change, not every
	// agent tick. Nil until the first successful status write after the
	// runtime exists. See docs/guides/machine-resource-limits.md.
	ResourceUsage *ResourceUsage `json:"resourceUsage,omitempty"`
	// VolumeStagingPath/VolumePublishPath record that kairon-node has
	// already called NodeStageVolume/NodePublishVolume (see
	// internal/agent/storage.go, internal/csinode) for this Machine's
	// spec.volumes[0] when it's CSI-backed -- kairon-csi-node's Node
	// service has no query for "already staged/published" (NodeStageVolume
	// is idempotent, but calling it every 3s reconcile tick would still
	// mean an iscsiadm/mount syscall every tick for no reason), so Kairon
	// tracks the fact itself in status, the same pattern
	// AppliedVCPUs/AppliedMemoryMiB already use for FluxVM hotplug. Both
	// empty for a hostPath/local-backed volume, or when spec.volumes is
	// unset -- neither of those is ever staged/published, only read
	// directly.
	VolumeStagingPath string `json:"volumeStagingPath,omitempty"`
	VolumePublishPath string `json:"volumePublishPath,omitempty"`
	// VolumeHandle mirrors the CSI volume_id NodeStageVolume was called
	// with -- kept in status (not just re-read from the PV at teardown
	// time) so cleanup can call NodeUnstageVolume correctly even if the
	// PVC/PV has already been deleted by the time the Machine itself
	// finishes tearing down.
	VolumeHandle string `json:"volumeHandle,omitempty"`
	// VolumeDriver names which CSI driver staged/published this Machine's
	// volume -- Kairon's own (csinode.DriverName) when empty, for backward
	// compatibility with every Machine created before third-party CSI
	// support existed; a third-party driver name (see
	// docs/guides/machine-storage-thirdparty-csi.md) otherwise. Recorded
	// for the same reason VolumeHandle is kept here rather than re-derived
	// from the PV: teardown must route to the correct driver's own socket
	// even if the PVC/PV is already gone by the time cleanup runs.
	VolumeDriver string `json:"volumeDriver,omitempty"`
	// AppliedServiceFabricMemberships is the exact set of Service Fabric
	// backend entries Kairon has actually registered for this Machine as
	// of the last successful reconcile -- see
	// AppliedServiceFabricMembership's own doc comment for why this has to
	// be tracked as ground truth rather than re-derived from
	// spec.serviceFabric.services each tick. Cleared to nil once
	// deregisterServiceFabric successfully removes every entry it names
	// (Machine deletion, spec.powerState: Stopped, spec.powerState:
	// Halted).
	AppliedServiceFabricMemberships []AppliedServiceFabricMembership `json:"appliedServiceFabricMemberships,omitempty"`
	// AttachedDisks names the spec.disks entries kairon-node has attached
	// to the running VM. Only these are ever detached when removed from
	// spec, so disks attached by other means are left alone.
	AttachedDisks []string `json:"attachedDisks,omitempty"`
	// DiskVolumes are the CSI volumes kairon-node has staged and published
	// for CSI-backed spec.disks entries, kept so a removed disk or a
	// deleted Machine can be unpublished even after its PV is gone.
	DiskVolumes []DiskVolume `json:"diskVolumes,omitempty"`
	// AttachedInterfaces are the spec.network.extraInterfaces kairon-node
	// has hot-added, with the MAC it used (generated when spec left it
	// empty), so removal can unplug the exact NIC.
	AttachedInterfaces []AttachedInterface `json:"attachedInterfaces,omitempty"`
}

// DiskVolume is one CSI volume published for a spec.disks entry on Node.
// Block volumes are published as a device at PublishPath; Filesystem ones
// as a directory holding disk.img.
type DiskVolume struct {
	Name         string `json:"name"`
	Node         string `json:"node"`
	Driver       string `json:"driver"`
	VolumeHandle string `json:"volumeHandle"`
	StagingPath  string `json:"stagingPath"`
	PublishPath  string `json:"publishPath"`
	Block        bool   `json:"block,omitempty"`
}

// AttachedInterface is one hot-added extra NIC.
type AttachedInterface struct {
	Name   string `json:"name"`
	Bridge string `json:"bridge"`
	MAC    string `json:"mac"`
}

type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

type MachineMigration struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta             `json:"metadata"`
	Spec     MachineMigrationSpec   `json:"spec"`
	Status   MachineMigrationStatus `json:"status,omitempty"`
}

type MachineMigrationList struct {
	TypeMeta `json:",inline"`
	Items    []MachineMigration `json:"items"`
}

type MachineMigrationSpec struct {
	MachineName     string `json:"machineName"`
	Strategy        string `json:"strategy,omitempty"` // auto|live|cold
	TargetNode      string `json:"targetNode,omitempty"`
	Mode            string `json:"mode,omitempty"` // pre-copy|post-copy
	BandwidthMbps   uint64 `json:"bandwidthMbps,omitempty"`
	MaxDowntimeMs   uint64 `json:"maxDowntimeMs,omitempty"`
	MultifdChannels uint8  `json:"multifdChannels,omitempty"`
	// MigrationNetwork names a migration network configured on the
	// destination node's adapter (--migration-network name=ip), used to
	// select which address the RAM/state transfer binds/advertises.
	// Empty uses the adapter's default advertise address.
	MigrationNetwork string `json:"migrationNetwork,omitempty"`
	// Recovery is the operator's explicit, attested instruction for
	// resolving a migration parked in NeedsRecovery -- never set by Kairon
	// itself, and never acted on unless AcknowledgedDiagnosis matches
	// Action (see internal/agent's reconcileNeedsRecovery). Left unset
	// (nil), a NeedsRecovery migration stays parked indefinitely with no
	// automatic resolution, by design.
	Recovery *MachineMigrationRecoverySpec `json:"recovery,omitempty"`
	// Cancel requests that Kairon abort a healthy *live* migration while it
	// is still safely reversible -- phase Starting or Running, strictly
	// before the destination has committed. Set by an operator (via
	// `kaironctl cancel-migration` or the dashboard), never by Kairon
	// itself. internal/agent's reconcileMigration honors it by reusing the
	// exact same source/destination Abort primitives an unrequested
	// transfer failure already calls, then lands the migration in the
	// Cancelled terminal phase with the source runtime left untouched.
	// Deliberately a one-way latch: Kairon never clears it back to false,
	// so a stale or duplicated MachineMigration object can't accidentally
	// un-cancel. Ignored (a documented no-op, not an error) once phase has
	// advanced to Cutover or later -- by then the destination has already
	// committed, and aborting would recreate exactly the split-brain risk
	// NeedsRecovery exists to avoid -- and for cold-strategy migrations,
	// which have no in-flight transfer to abort at all; use the Machine's
	// own spec.powerState/spec.nodeName instead.
	Cancel bool `json:"cancel,omitempty"`
	// Adaptive, when set, runs this live migration under kairon-node's
	// adaptive engine. The controller snapshots it from the matching
	// MigrationPolicy at admission, filling only fields left unset here, so
	// node agents never read policies and policy edits never change a
	// migration already in flight.
	Adaptive *MachineMigrationAdaptiveSpec `json:"adaptive,omitempty"`
}

// MachineMigrationAdaptiveSpec mirrors MigrationPolicySpec's adaptive
// fields; MaxDowntimeMs and the starting bandwidth live in the
// migration's own maxDowntimeMs and bandwidthMbps.
type MachineMigrationAdaptiveSpec struct {
	// Policy names the MigrationPolicy the settings were copied from.
	Policy                  string                 `json:"policy,omitempty"`
	CompletionTimeoutPerGiB string                 `json:"completionTimeoutPerGiB,omitempty"`
	ProgressTimeout         string                 `json:"progressTimeout,omitempty"`
	AutoConverge            *MigrationAutoConverge `json:"autoConverge,omitempty"`
	PostCopy                *MigrationPostCopy     `json:"postCopy,omitempty"`
	Compression             *MigrationCompression  `json:"compression,omitempty"`
	Bandwidth               *MigrationBandwidth    `json:"bandwidth,omitempty"`
}

// MachineMigrationRecoverySpec is the operator's attested recovery
// instruction: what to do (Action), what they observed that justifies it
// (AcknowledgedDiagnosis), and why (Reason, free text). All three are
// required together -- reconcileNeedsRecovery refuses to act if
// AcknowledgedDiagnosis doesn't match Action, or Reason is empty, rather
// than silently guessing.
type MachineMigrationRecoverySpec struct {
	// One of ConfirmDestinationCommitted, ConfirmDestinationNotCommitted, ForceAbort.
	Action string `json:"action"`
	// One of DestinationCommitted, DestinationNotCommitted, Unknown --
	// must match Action (Confirm*Committed requires DestinationCommitted,
	// Confirm*NotCommitted requires DestinationNotCommitted; ForceAbort
	// accepts any).
	AcknowledgedDiagnosis string `json:"acknowledgedDiagnosis"`
	// Free-text evidence for what the operator observed. Required --
	// this is what turns a guess into an attested, audited decision.
	Reason string `json:"reason"`
}

const (
	RecoveryActionConfirmDestinationCommitted    = "ConfirmDestinationCommitted"
	RecoveryActionConfirmDestinationNotCommitted = "ConfirmDestinationNotCommitted"
	RecoveryActionForceAbort                     = "ForceAbort"

	RecoveryDiagnosisDestinationCommitted    = "DestinationCommitted"
	RecoveryDiagnosisDestinationNotCommitted = "DestinationNotCommitted"
	RecoveryDiagnosisUnknown                 = "Unknown"
)

type MachineMigrationStatus struct {
	Phase string `json:"phase,omitempty"`
	// No omitempty -- see MachineStatus.Message's comment: this status is
	// patched as one whole object, so an omitted key never clears a
	// previously-set value under JSON merge-patch semantics.
	Message           string `json:"message"`
	SourceNode        string `json:"sourceNode,omitempty"`
	TargetNode        string `json:"targetNode,omitempty"`
	EffectiveStrategy string `json:"effectiveStrategy,omitempty"`
	RuntimeID         string `json:"runtimeID,omitempty"`
	SessionID         string `json:"sessionID,omitempty"`
	TransferID        string `json:"transferID,omitempty"`
	TransferPhase     string `json:"transferPhase,omitempty"`
	Backend           string `json:"backend,omitempty"`
	RAMTransferred    uint64 `json:"ramTransferred,omitempty"`
	RAMRemaining      uint64 `json:"ramRemaining,omitempty"`
	RAMTotal          uint64 `json:"ramTotal,omitempty"`
	TotalTimeMs       uint64 `json:"totalTimeMs,omitempty"`
	DowntimeMs        uint64 `json:"downtimeMs,omitempty"`
	// DataPlaneEncrypted reports whether the destination adapter had TLS
	// configured (-migration-data-tls) when this migration's receiver was
	// prepared -- the QEMU RAM/state stream itself, not the always-on mTLS
	// control-plane RPCs. Set once, at Starting, from the adapter's own
	// prepare() response (see internal/migration.PrepareResult); an older
	// adapter that predates this field simply omits it from JSON, which
	// decodes to false -- the safe, conservative assumption (unencrypted),
	// not fail-open.
	DataPlaneEncrypted bool `json:"dataPlaneEncrypted,omitempty"`
	// Recovery is populated only while Phase == NeedsRecovery: a live
	// diagnosis snapshot refreshed every reconcile tick, plus a permanent
	// record of whatever recovery action was actually applied (if any) --
	// kept even if spec.recovery is later edited or cleared.
	Recovery *MachineMigrationRecoveryStatus `json:"recovery,omitempty"`
	// Progress is the transfer telemetry from the latest adapter poll.
	Progress *MachineMigrationProgress `json:"progress,omitempty"`
	// Adaptive is the adaptive engine's state for migrations with
	// spec.adaptive. Any live migration also gets it once post-copy starts,
	// to carry the PostCopyStarted latch.
	Adaptive *MachineMigrationAdaptiveStatus `json:"adaptive,omitempty"`
	// TargetTopologyAllocation and TargetAllocatedCPUSet are the live
	// migration target's placement for a spec.topology Machine: the same
	// guest NUMA shape on the target's NUMA nodes. The source agent hands
	// it to the receiver; cutover copies it into the Machine's spec.
	TargetTopologyAllocation *TopologyAllocation `json:"targetTopologyAllocation,omitempty"`
	TargetAllocatedCPUSet    []uint32            `json:"targetAllocatedCpuSet,omitempty"`
}

// MachineMigrationProgress is integer-only telemetry (no floats in the
// API): rates are rounded down.
type MachineMigrationProgress struct {
	DirtyPagesRate uint64 `json:"dirtyPagesRate,omitempty"`
	DirtyRateMbps  uint64 `json:"dirtyRateMbps,omitempty"`
	ThroughputMbps uint64 `json:"throughputMbps,omitempty"`
	DuplicatePages uint64 `json:"duplicatePages,omitempty"`
	DirtySyncCount uint64 `json:"dirtySyncCount,omitempty"`
	// CompressionRatioPercent is uncompressed page bytes over bytes sent,
	// times 100; set only when compression is on.
	CompressionRatioPercent    uint64     `json:"compressionRatioPercent,omitempty"`
	ExpectedDowntimeMs         uint64     `json:"expectedDowntimeMs,omitempty"`
	EstimatedCompletionSeconds uint64     `json:"estimatedCompletionSeconds,omitempty"`
	CPUThrottlePct             uint64     `json:"cpuThrottlePct,omitempty"`
	PostcopyRequests           uint64     `json:"postcopyRequests,omitempty"`
	UpdatedAt                  *time.Time `json:"updatedAt,omitempty"`
}

const (
	AdaptiveStagePreCopy       = "PreCopy"
	AdaptiveStageAutoConverge  = "AutoConverge"
	AdaptiveStageRelaxDowntime = "RelaxDowntime"
	AdaptiveStagePostCopy      = "PostCopy"
	AdaptiveStageAbort         = "Abort"
	AdaptiveStageUnsupported   = "Unsupported"

	// MaxAdaptiveActions caps status.adaptive.actions, oldest dropped first.
	MaxAdaptiveActions = 20
)

// MachineMigrationAdaptiveStatus holds just enough state for a restarted
// kairon-node to resume the adaptive engine's decisions.
type MachineMigrationAdaptiveStatus struct {
	Stage      string     `json:"stage,omitempty"`
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	StageSince *time.Time `json:"stageSince,omitempty"`
	// BestRAMRemaining is the lowest remaining RAM seen, and
	// BestProgressAt when it last improved meaningfully.
	BestRAMRemaining uint64     `json:"bestRAMRemaining,omitempty"`
	BestProgressAt   *time.Time `json:"bestProgressAt,omitempty"`
	// PostCopyStarted latches once the transfer entered post-copy. From
	// then on the source can't be resumed.
	PostCopyStarted bool `json:"postCopyStarted,omitempty"`
	// PostCopyArmed records that both ends enabled post-copy at stream
	// start, the precondition for switching to it later.
	PostCopyArmed bool `json:"postCopyArmed,omitempty"`
	// Currently applied settings.
	BandwidthMbps   uint64 `json:"bandwidthMbps,omitempty"`
	DowntimeLimitMs uint64 `json:"downtimeLimitMs,omitempty"`
	MaxThrottlePct  uint8  `json:"maxThrottlePct,omitempty"`
	// LastBandwidthChangeAt paces adaptive bandwidth steps.
	LastBandwidthChangeAt *time.Time                       `json:"lastBandwidthChangeAt,omitempty"`
	Actions               []MachineMigrationAdaptiveAction `json:"actions,omitempty"`
}

type MachineMigrationAdaptiveAction struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
}

// MachineMigrationRecoveryStatus mirrors migration.DiagnosisResult (the
// live ground truth Kairon can currently see) plus an audit trail of
// whatever recovery action was actually applied.
type MachineMigrationRecoveryStatus struct {
	SourceRuntimeStatus      string     `json:"sourceRuntimeStatus,omitempty"`
	DestinationSessionPhase  string     `json:"destinationSessionPhase,omitempty"`
	DestinationRuntimeStatus string     `json:"destinationRuntimeStatus,omitempty"`
	DestinationRuntimeFound  bool       `json:"destinationRuntimeFound,omitempty"`
	DiagnosedAt              *time.Time `json:"diagnosedAt,omitempty"`
	// AppliedAction/AppliedReason/AppliedAcknowledgedDiagnosis/AppliedAt
	// echo spec.recovery at the moment an action was actually taken --
	// a permanent audit record independent of later spec.recovery edits.
	AppliedAction                string     `json:"appliedAction,omitempty"`
	AppliedReason                string     `json:"appliedReason,omitempty"`
	AppliedAcknowledgedDiagnosis string     `json:"appliedAcknowledgedDiagnosis,omitempty"`
	AppliedAt                    *time.Time `json:"appliedAt,omitempty"`
}

func (m MachineMigration) Namespace() string {
	if m.Metadata.Namespace == "" {
		return DefaultNamespace
	}
	return m.Metadata.Namespace
}

type MachineSnapshot struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta            `json:"metadata"`
	Spec     MachineSnapshotSpec   `json:"spec"`
	Status   MachineSnapshotStatus `json:"status,omitempty"`
}

type MachineSnapshotList struct {
	TypeMeta `json:",inline"`
	Items    []MachineSnapshot `json:"items"`
}

type MachineSnapshotSpec struct {
	MachineName             string `json:"machineName"`
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
	// VolumeNames limits the snapshot to these spec.volumes[].name entries;
	// empty means every volume.
	VolumeNames []string `json:"volumeNames,omitempty"`
}

type MachineSnapshotStatus struct {
	Phase string `json:"phase,omitempty"`
	// No omitempty -- see MachineStatus.Message's comment.
	Message         string                    `json:"message"`
	ReadyToUse      bool                      `json:"readyToUse,omitempty"`
	VolumeSnapshots []VolumeSnapshotReference `json:"volumeSnapshots,omitempty"`
}

type VolumeSnapshotReference struct {
	VolumeName         string `json:"volumeName"`
	VolumeSnapshotName string `json:"volumeSnapshotName"`
	ReadyToUse         bool   `json:"readyToUse,omitempty"`
	// AtlasSnapshotID / AtlasJobID are set instead of a CSI VolumeSnapshot
	// for a volume provisioned through Atlas (spec.volumes[].atlas).
	AtlasSnapshotID string `json:"atlasSnapshotID,omitempty"`
	AtlasJobID      string `json:"atlasJobID,omitempty"`
}

func (s MachineSnapshot) Namespace() string {
	if s.Metadata.Namespace == "" {
		return DefaultNamespace
	}
	return s.Metadata.Namespace
}

// Minimal Kubernetes ResourceClaim representation used by the node agent
// and, for the topology hint in internal/controller's scheduling pass, by
// the controller too.
type ResourceClaim struct {
	Metadata ObjectMeta          `json:"metadata"`
	Status   ResourceClaimStatus `json:"status,omitempty"`
}

type ResourceClaimList struct {
	TypeMeta `json:",inline"`
	Items    []ResourceClaim `json:"items"`
}

type ResourceClaimStatus struct {
	Allocation *ResourceClaimAllocation `json:"allocation,omitempty"`
}

type ResourceClaimAllocation struct {
	Devices ResourceClaimDeviceAllocation `json:"devices,omitempty"`
}

type ResourceClaimDeviceAllocation struct {
	Results []DeviceRequestAllocationResult `json:"results,omitempty"`
}

type DeviceRequestAllocationResult struct {
	Request string `json:"request,omitempty"`
	Driver  string `json:"driver,omitempty"`
	Pool    string `json:"pool,omitempty"`
	Device  string `json:"device,omitempty"`
}

// ResourceSlice is a minimal resource.k8s.io/v1 ResourceSlice
// representation: enough to answer "which node hosts {Driver, Pool}",
// which is exactly what internal/controller needs to give the scheduler a
// DRA topology-awareness hint -- see internal/controller's
// draPreferredNode. Cluster-scoped, unlike ResourceClaim.
type ResourceSlice struct {
	Metadata ObjectMeta        `json:"metadata"`
	Spec     ResourceSliceSpec `json:"spec"`
}

type ResourceSliceSpec struct {
	Driver string                `json:"driver"`
	Pool   ResourceSlicePoolInfo `json:"pool"`
	// NodeName is empty for a pool not tied to a specific node (e.g. a
	// network-attached device pool) -- draPreferredNode contributes no
	// hint in that case, the same as if the claim isn't allocated yet.
	NodeName string `json:"nodeName,omitempty"`
}

type ResourceSlicePoolInfo struct {
	Name string `json:"name"`
}

type ResourceSliceList struct {
	TypeMeta `json:",inline"`
	Items    []ResourceSlice `json:"items"`
}

// Minimal CSI snapshot.storage.k8s.io/v1 representation.
type VolumeSnapshot struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta           `json:"metadata"`
	Spec     VolumeSnapshotSpec   `json:"spec"`
	Status   VolumeSnapshotStatus `json:"status,omitempty"`
}

type VolumeSnapshotSpec struct {
	Source                  VolumeSnapshotSource `json:"source"`
	VolumeSnapshotClassName *string              `json:"volumeSnapshotClassName,omitempty"`
}

type VolumeSnapshotSource struct {
	PersistentVolumeClaimName *string `json:"persistentVolumeClaimName,omitempty"`
}

type VolumeSnapshotStatus struct {
	ReadyToUse *bool                `json:"readyToUse,omitempty"`
	Error      *VolumeSnapshotError `json:"error,omitempty"`
	// RestoreSize is the minimum size a PVC restored from this snapshot
	// must request -- set by the real CSI driver, read by
	// MachineSnapshotRestore to size the new PVC when the request doesn't
	// override it explicitly.
	RestoreSize *string `json:"restoreSize,omitempty"`
}

type VolumeSnapshotError struct {
	Message *string `json:"message,omitempty"`
}

type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable,omitempty"`
		// Taints is the real Kubernetes Node.spec.taints -- decoded here
		// (previously ignored entirely by this hand-rolled struct) so
		// internal/scheduler can honor them the same way a real
		// kube-scheduler does: a NoSchedule/NoExecute taint the Machine
		// doesn't tolerate makes the node ineligible; PreferNoSchedule only
		// penalizes its score. See PlacementSpec.Tolerations.
		Taints []Taint `json:"taints,omitempty"`
	} `json:"spec"`
	Status NodeStatus `json:"status"`
}

// NodeStatus mirrors the slice of core/v1.NodeStatus Kairon reads.
type NodeStatus struct {
	Conditions []NodeCondition `json:"conditions,omitempty"`
	Addresses  []NodeAddress   `json:"addresses,omitempty"`
	// Allocatable mirrors Node.status.allocatable (cpu, memory,
	// hugepages-2Mi, …). Used by the capacity-aware scheduler; when
	// absent, capacity hard-filters are skipped for that node.
	Allocatable map[string]string `json:"allocatable,omitempty"`
}

// Taint mirrors the real Kubernetes core/v1.Taint wire shape exactly (Key,
// Value, Effect) -- Kairon doesn't set these itself (nothing here calls
// `kaironctl taint` or similar; a real Kubernetes Node object may already
// carry taints an operator or another controller/cloud-provider set, e.g.
// `node.kubernetes.io/unschedulable`, a spot-instance taint, or a manual
// `kubectl taint`), it only reads and respects whatever's already there.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

const (
	TaintEffectNoSchedule       = "NoSchedule"
	TaintEffectPreferNoSchedule = "PreferNoSchedule"
	TaintEffectNoExecute        = "NoExecute"
)

type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// SubjectAccessReview hand-rolls just the slice of the built-in
// authorization.k8s.io/v1 SubjectAccessReview wire format
// internal/uiapi's own console-access RBAC check needs (see
// internal/kube.Client.SubjectAccessReview) -- this project has no
// client-go/k8s.io/api dependency at all, the same reasoning
// internal/admission's own doc comment gives for hand-rolling
// AdmissionReview. Deliberately SubjectAccessReview, not
// SelfSubjectAccessReview: the caller (kairon-ui's own ServiceAccount
// token) and the subject being checked (an app-level kairon-ui operator
// identity, local account or OIDC claim) are different.
type SubjectAccessReview struct {
	TypeMeta `json:",inline"`
	Spec     SubjectAccessReviewSpec   `json:"spec"`
	Status   SubjectAccessReviewStatus `json:"status,omitempty"`
}

type SubjectAccessReviewSpec struct {
	User               string              `json:"user,omitempty"`
	Groups             []string            `json:"groups,omitempty"`
	ResourceAttributes *ResourceAttributes `json:"resourceAttributes,omitempty"`
}

type ResourceAttributes struct {
	Namespace   string `json:"namespace,omitempty"`
	Verb        string `json:"verb,omitempty"`
	Group       string `json:"group,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

type SubjectAccessReviewStatus struct {
	Allowed bool   `json:"allowed"`
	Denied  bool   `json:"denied,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type NodeCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastHeartbeatTime  string `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

type NodeList struct {
	Items []Node `json:"items"`
}

func (m Machine) Namespace() string {
	if m.Metadata.Namespace == "" {
		return DefaultNamespace
	}
	return m.Metadata.Namespace
}

func (m Machine) RuntimeName() string {
	return "kairon-" + m.Namespace() + "-" + m.Metadata.Name
}

func (m Machine) DesiredPowerState() string {
	if m.Spec.PowerState == "" {
		return "Running"
	}
	return m.Spec.PowerState
}

func HasFinalizer(m Machine, name string) bool {
	return HasFinalizerList(m.Metadata.Finalizers, name)
}

func HasFinalizerList(finalizers []string, name string) bool {
	for _, f := range finalizers {
		if f == name {
			return true
		}
	}
	return false
}

func RemoveFinalizer(finalizers []string, name string) []string {
	out := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	return out
}

func AnnotationTrue(meta ObjectMeta, key string) bool {
	return meta.Annotations != nil && meta.Annotations[key] == "true"
}
