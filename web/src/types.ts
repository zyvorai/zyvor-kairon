// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Mirrors internal/model/types.go's JSON shapes, trimmed to the fields
// the UI actually renders or submits. Kept as one shared file since every
// page depends on the same handful of resource shapes.

export interface ObjectMeta {
  name: string;
  namespace?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  creationTimestamp?: string;
}

export interface Machine {
  metadata: ObjectMeta;
  spec: {
    nodeName?: string;
    image: { path: string };
    resources: { cpu: string; memory: string; elasticMemory?: { min: string; freePageReporting?: boolean } };
    runtime?: { backend?: string };
    network?: { mode?: string; netns?: boolean };
    powerState?: string;
    guestAgent?: { enabled?: boolean; console?: boolean };
    priority?: number;
  };
  status?: {
    phase?: string;
    nodeName?: string;
    guestIP?: string;
    message?: string;
    resourceUsage?: { cpuPercent?: number; memoryBytes?: number; diskReadBytes?: number; diskWriteBytes?: number };
    guestIPs?: string[];
    network?: MachineNetworkStatus;
    memory?: MachineMemoryStatus;
  };
}

// Mirrors internal/model/types.go's MemoryStatus, written by kairon-node's
// reclaim loop for spec.resources.elasticMemory Machines.
export interface MachineMemoryStatus {
  currentMiB?: number;
  minMiB?: number;
  balloonTargetMiB: number;
  balloonActualMiB: number;
  guestAvailableMiB?: number;
  reason?: string;
  lastChangeTime?: string;
}

// Mirrors internal/model/network.go's MachineNetworkStatus.
export interface MachineNetworkStatus {
  guestIP?: string;
  guestIPs?: string[];
  tapName?: string;
  dataplane?: { attached: boolean; mode?: string; identity?: number; policyFingerprint?: string; policySynced?: boolean; interface?: string };
  cilium?: { externalWorkload?: string; identity?: number; ipv4?: string; message?: string };
  edge?: { identity?: number; guestIPSource?: string; antiSpoof?: boolean; policyName?: string; conntrackRestored?: number; blackholeWindowMs?: number };
}

export interface RecoveryStatus {
  sourceRuntimeStatus?: string;
  destinationSessionPhase?: string;
  destinationRuntimeStatus?: string;
  destinationRuntimeFound?: boolean;
  diagnosedAt?: string;
  appliedAction?: string;
  appliedReason?: string;
  appliedAcknowledgedDiagnosis?: string;
  appliedAt?: string;
}

// Adaptive live migration settings, shared by MigrationPolicy.spec and
// MachineMigration.spec.adaptive (internal/model/migrationpolicy.go).
export interface MigrationAdaptiveSettings {
  completionTimeoutPerGiB?: string;
  progressTimeout?: string;
  autoConverge?: { enabled?: boolean; dirtyRateThresholdMbps?: number };
  postCopy?: { enabled?: boolean; triggerAfter?: string };
  compression?: { algorithm: 'none' | 'zlib' | 'zstd'; level?: number };
  bandwidth?: { maxMbps?: number; adaptive?: boolean };
}

export interface MigrationProgress {
  dirtyPagesRate?: number;
  dirtyRateMbps?: number;
  throughputMbps?: number;
  duplicatePages?: number;
  dirtySyncCount?: number;
  compressionRatioPercent?: number;
  expectedDowntimeMs?: number;
  estimatedCompletionSeconds?: number;
  cpuThrottlePct?: number;
  postcopyRequests?: number;
  updatedAt?: string;
}

export interface MigrationAdaptiveStatus {
  stage?: string;
  message?: string;
  startedAt?: string;
  stageSince?: string;
  bestRAMRemaining?: number;
  bestProgressAt?: string;
  postCopyStarted?: boolean;
  postCopyArmed?: boolean;
  bandwidthMbps?: number;
  downtimeLimitMs?: number;
  maxThrottlePct?: number;
  lastBandwidthChangeAt?: string;
  actions?: { at: string; action: string; detail?: string }[];
}

export interface MachineMigration {
  metadata: ObjectMeta;
  spec: {
    machineName: string;
    strategy?: string;
    targetNode?: string;
    mode?: string;
    bandwidthMbps?: number;
    maxDowntimeMs?: number;
    multifdChannels?: number;
    migrationNetwork?: string;
    cancel?: boolean;
    adaptive?: MigrationAdaptiveSettings & { policy?: string };
  };
  status?: {
    phase?: string;
    message?: string;
    sourceNode?: string;
    targetNode?: string;
    effectiveStrategy?: string;
    transferPhase?: string;
    backend?: string;
    ramTransferred?: number;
    ramRemaining?: number;
    ramTotal?: number;
    totalTimeMs?: number;
    downtimeMs?: number;
    dataPlaneEncrypted?: boolean;
    recovery?: RecoveryStatus;
    progress?: MigrationProgress;
    adaptive?: MigrationAdaptiveStatus;
  };
}

export interface MachineSnapshot {
  metadata: ObjectMeta;
  spec: { machineName: string; volumeSnapshotClassName?: string };
  status?: { phase?: string; readyToUse?: boolean; message?: string };
}

export interface MachineSnapshotRestore {
  metadata: ObjectMeta;
  spec: {
    snapshotName: string;
    volumeName?: string;
    targetClaimName: string;
    storageClassName?: string;
    storageSize?: string;
  };
  status?: { phase?: string; message?: string; restoredClaimName?: string };
}

// KaironNode mirrors internal/model.Node -- the real Kubernetes Node
// object, not a kairon.zyvor.dev CRD (see the block below for those).
// spec.taints/unschedulable and status.conditions were added alongside
// this file's own Nodes page (web/src/pages/Nodes.tsx): before that page
// existed, only status.addresses was ever read by the frontend (the
// Overview tile only needed a count, from GET /api/v1/overview, not the
// node list itself), so nothing else was worth mirroring here yet.
export interface KaironNode {
  metadata: ObjectMeta;
  spec?: {
    unschedulable?: boolean;
    taints?: { key: string; value?: string; effect: string }[];
  };
  status?: {
    conditions?: { type: string; status: string }[];
    addresses?: { type: string; address: string }[];
  };
}

// NodeUsage mirrors internal/model.NodeUsageAggregate -- GET
// /api/v1/nodes/usage's one row per node, the same rollup `kaironctl top
// nodes` prints (Status.ResourceUsage of every Machine scheduled there,
// summed). "node" is "-" for the bucket of not-yet-scheduled Machines
// (see AggregateUsageByNode's own doc comment), which never matches a
// real KaironNode's metadata.name, so joining this list onto the Nodes
// page's own KaironNode rows by name naturally leaves it unmatched.
export interface NodeUsage {
  node: string;
  machines: number;
  cpuPercent: number;
  memoryBytes: number;
}

export interface Overview {
  machines: { total: number; byPhase: Record<string, number> };
  migrations: { total: number; active: number; needsRecovery: number; byPhase: Record<string, number> };
  nodes: number;
}

// The five resource kinds below are read-only, list-only in the dashboard
// today (see internal/uiapi/fleet.go) -- kaironctl/kubectl remain the way
// to create or mutate any of them.

export interface MachineQuota {
  metadata: ObjectMeta;
  spec: { maxMachines?: number; maxTotalCpu?: string; maxTotalMemory?: string };
  status?: { usedMachines?: number; usedTotalCpuCores?: number; usedTotalMemoryMiB?: number };
}

export interface MachineDisruptionBudget {
  metadata: ObjectMeta;
  spec: { selector: Record<string, string>; minAvailable?: string; maxUnavailable?: string };
  status?: { expectedMachines: number; currentHealthy: number; desiredHealthy: number; disruptionsAllowed: number };
}

export interface MachineSet {
  metadata: ObjectMeta;
  spec: { replicas: number; strategy?: string; maxUnavailable?: string };
  status?: { replicas?: number; readyReplicas?: number; updatedReplicas?: number; message?: string };
}

export interface MachineInstanceType {
  metadata: ObjectMeta;
  spec: {
    resources: {
      cpu: string;
      memory: string;
      maxCpu?: string;
      maxMemory?: string;
      hugepages?: boolean;
      numaNode?: number;
      cpuSet?: string;
      cpuPinning?: boolean;
    };
  };
}

export interface MigrationPolicy {
  metadata: ObjectMeta;
  spec: MigrationAdaptiveSettings & {
    selector: Record<string, string>;
    // Deprecated alias for bandwidth.maxMbps.
    bandwidthMbps?: number;
    maxConcurrent?: number;
    strategy?: 'static' | 'adaptive';
    maxDowntimeMs?: number;
  };
  status?: { activeMigrations?: number };
}

export interface MachineSnapshotSchedule {
  metadata: ObjectMeta;
  spec: {
    selector: Record<string, string>;
    intervalSeconds: number;
    volumeSnapshotClassName?: string;
    suspend?: boolean;
    keepLast?: number;
  };
  status?: { lastRunTime?: string; lastRunSnapshotCount?: number; lastRunError?: string; nextRunTime?: string };
}

// VmNetworkPolicy mirrors internal/model/network.go's struct of the same
// name -- the FluxVM-shaped edge policy embedded in both CRDs below.
export interface VmNetworkPolicy {
  defaultAllow: boolean;
  allowCidrs?: string[];
  denyCidrs?: string[];
  allowPorts?: string[];
  maxEgressMbps?: number;
  maxEgressPps?: number;
  allowFqdns?: string[];
  groups?: string[];
  labels?: string[];
  entities?: string[];
  auditMode?: boolean;
  allowIcmp?: boolean;
  sampleRate?: number;
}

// MachineNetworkPolicy and NetworkSecurityGroup: list-only, same as the
// five resource kinds above -- kaironctl/kubectl remain the way to create
// or mutate either.
export interface MachineNetworkPolicy {
  metadata: ObjectMeta;
  spec: { machineName?: string; selector?: Record<string, string>; policy: VmNetworkPolicy };
  status?: { phase?: string; message?: string; observedMachines?: number; lastAppliedTime?: string; effectiveSynced?: boolean };
}

export interface NetworkSecurityGroup {
  metadata: ObjectMeta;
  spec: { groupName?: string; labels?: string[]; priority?: number; description?: string; policy: VmNetworkPolicy };
  status?: { phase?: string; message?: string; identity?: number; appliedOn?: string };
}
