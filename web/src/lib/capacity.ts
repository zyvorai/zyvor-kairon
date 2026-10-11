// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

export interface NodeCapacity {
  node: string;
  ready: boolean;
  unschedulable: boolean;
  taints: number;
  machines: number;
  allocatableCpuMilli: number;
  allocatableMemoryMiB: number;
  requestedCpuMilli: number;
  requestedMemoryMiB: number;
  knownAllocatable: boolean;
  guaranteedMemoryMiB?: number;
  memoryOvercommitRatio?: number;
  memoryPressure?: boolean;
  whatIf?: { fits: boolean; reason?: string };
}

// pct is requested/allocatable as a whole percentage clamped to 0..100+;
// null when allocatable is unknown or zero so the UI shows "-" not NaN.
export function pct(requested: number, allocatable: number): number | null {
  if (!allocatable || allocatable <= 0) return null;
  return Math.max(0, Math.round((requested / allocatable) * 100));
}

// barTone colours a packing bar: comfortable, getting full, nearly full.
export function barTone(p: number | null): 'ok' | 'warn' | 'error' | 'idle' {
  if (p === null) return 'idle';
  if (p >= 90) return 'error';
  if (p >= 75) return 'warn';
  return 'ok';
}

export function formatCores(milli: number): string {
  const c = milli / 1000;
  return Number.isInteger(c) ? `${c}` : c.toFixed(1);
}

export function formatMiB(mib: number): string {
  return mib >= 1024 ? `${(mib / 1024).toFixed(mib % 1024 === 0 ? 0 : 1)} GiB` : `${mib} MiB`;
}

// memoryLimitMiB is how much full-size Machine memory the scheduler lets
// onto a node: allocatable times the node's overcommit ratio (1 when the
// kairon.zyvor.dev/memory-overcommit-ratio annotation is absent).
export function memoryLimitMiB(c: NodeCapacity): number {
  const ratio = c.memoryOvercommitRatio && c.memoryOvercommitRatio > 1 ? c.memoryOvercommitRatio : 1;
  return Math.floor(c.allocatableMemoryMiB * ratio);
}

// formatBalloon reads a Machine's status.memory as "in guest / size".
export function formatBalloon(m: { currentMiB?: number; balloonActualMiB: number; minMiB?: number }): string {
  const cur = m.currentMiB ?? 0;
  const inGuest = m.balloonActualMiB < cur ? cur - m.balloonActualMiB : cur;
  const min = m.minMiB ? `, min ${formatMiB(m.minMiB)}` : '';
  return `${formatMiB(inGuest)} of ${formatMiB(cur)}${min}`;
}
