// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react';
import { KaironNode } from '../types';

// Label and annotation keys kairon-node publishes on a Mac (internal/model/apple.go).
const K = 'kairon.zyvor.dev/';

export function isMac(n: KaironNode): boolean {
  return n.metadata.labels?.[K + 'backend.vz'] === 'true';
}

function chipName(label?: string): string | undefined {
  if (!label) return undefined;
  const [gen, tier] = label.split('-');
  return gen.toUpperCase() + (tier ? ' ' + tier[0].toUpperCase() + tier.slice(1) : '');
}

// platformSummary is the Nodes table's Platform cell: Linux nodes by
// architecture, Macs by chip, macOS and Thunderbolt fabric.
export function platformSummary(n: KaironNode): string {
  const l = n.metadata.labels ?? {};
  if (!isMac(n)) {
    return ['Linux', l['kubernetes.io/arch']].filter(Boolean).join(' ');
  }
  const parts = ['Mac'];
  const chip = chipName(l[K + 'apple-chip']);
  if (chip) parts.push(chip);
  if (l[K + 'macos-version']) parts.push('macOS ' + l[K + 'macos-version']);
  if (l[K + 'thunderbolt-fabric']) parts.push('TB ' + l[K + 'thunderbolt-fabric']);
  return parts.join(' · ');
}

// macDetails are the Inspector rows for a Mac; none for other nodes.
export function macDetails(n?: KaironNode): [string, ReactNode][] {
  if (!n || !isMac(n)) return [];
  const l = n.metadata.labels ?? {};
  const a = n.metadata.annotations ?? {};
  const yes = (v?: string) => (v === 'true' ? 'yes' : 'no');
  const tb = a[K + 'thunderbolt-addresses'];
  return [
    ['Chip', chipName(l[K + 'apple-chip'])],
    ['Model', a[K + 'apple-model']],
    ['Cores', a[K + 'apple-cores']],
    ['macOS', l[K + 'macos-version']],
    ['Rosetta', yes(l[K + 'vz.rosetta'])],
    ['Nested virtualization', yes(l[K + 'vz.nested-virtualization'])],
    ['Thunderbolt', tb ? `${a[K + 'thunderbolt-interface'] ?? ''} ${tb}`.trim() : 'no'],
    ['Thunderbolt fabric', l[K + 'thunderbolt-fabric']],
  ];
}
