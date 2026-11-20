// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Headline numbers from the README's "Kairon vs KubeVirt" benchmark (same k3s
// node, same Ubuntu 24.04 guest, KubeVirt v1.9.0). Kept here as one source so
// the sign-in page never drifts from what the README claims.

export interface Benchmark {
  big: string;
  label: string;
  detail: string;
}

export const BENCHMARKS: readonly Benchmark[] = [
  { big: '14×', label: 'less idle memory', detail: '63 MiB vs 905 MiB control plane' },
  { big: '2.9×', label: 'faster to SSH', detail: '23.7 s vs 67.6 s, one VM' },
  { big: '7.4×', label: 'faster at five VMs', detail: '24.8 s vs 184.7 s median' },
  { big: '0', label: 'pods per VM', detail: 'no virt-launcher' },
];

export const BENCHMARK_NOTE =
  'Measured against KubeVirt v1.9.0 on the same node and guest image (see the README). Live migration, the eBPF edge and MCP are Preview.';
