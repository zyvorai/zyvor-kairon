// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Headline facts shown on the sign-in page.

export interface Benchmark {
  big: string;
  label: string;
  detail: string;
}

export const BENCHMARKS: readonly Benchmark[] = [
  { big: '0', label: 'pods per VM', detail: 'no virt-launcher' },
  { big: '4', label: 'hypervisors', detail: 'one Machine CRD' },
];

export const BENCHMARK_NOTE =
  'Live migration, the eBPF edge and MCP are Preview.';
