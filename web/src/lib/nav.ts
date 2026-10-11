// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Single source of truth for navigation: the grouped mega-menu, the mobile
// sheet, hash routing and the command palette all read NAV. Icons are
// attached in components/navIcons.tsx so this file stays DOM-free (and
// therefore trivially unit-testable).

export type Tone = 'sky' | 'violet' | 'emerald' | 'amber' | 'rose' | 'graphite';

export interface NavItem {
  id: string;
  label: string;
  blurb: string;
}

export interface NavGroup {
  group: string;
  tone: Tone;
  items: NavItem[];
}

export const NAV = [
  {
    group: 'Overview',
    tone: 'sky',
    items: [{ id: 'overview', label: 'Overview', blurb: 'Fleet health at a glance' }],
  },
  {
    group: 'Compute',
    tone: 'violet',
    items: [
      { id: 'machines', label: 'Machines', blurb: 'Create, power and inspect VMs' },
      { id: 'machinesets', label: 'Machine sets', blurb: 'Scale groups of identical VMs' },
      { id: 'instancetypes', label: 'Instance types', blurb: 'Reusable CPU and memory shapes' },
      { id: 'nodes', label: 'Nodes', blurb: 'FluxVM hosts, capacity and usage' },
      { id: 'storage', label: 'Storage', blurb: 'Atlas volumes, pools, backups and health' },
      { id: 'pools', label: 'Pools & claims', blurb: 'Warm machine pools and bound claims' },
      { id: 'images', label: 'Images', blurb: 'Upload and serve VM images to nodes' },
    ],
  },
  {
    group: 'Lifecycle',
    tone: 'emerald',
    items: [
      { id: 'migrations', label: 'Migrations', blurb: 'Move VMs between nodes, with recovery' },
      { id: 'migration-policies', label: 'Migration policies', blurb: 'Rules for when and how VMs move' },
      { id: 'snapshots', label: 'Snapshots', blurb: 'Point-in-time machine snapshots' },
      { id: 'snapshot-schedules', label: 'Snapshot schedules', blurb: 'Recurring snapshots and retention' },
      { id: 'restores', label: 'Restores', blurb: 'Restore a snapshot into a new claim' },
      { id: 'backups', label: 'Backups', blurb: 'Off-cluster machine backups and restores' },
    ],
  },
  {
    group: 'Policy',
    tone: 'rose',
    items: [
      { id: 'quotas', label: 'Quotas', blurb: 'Namespace CPU, memory and count limits' },
      { id: 'disruption-budgets', label: 'Disruption budgets', blurb: 'Limit concurrent voluntary disruptions' },
      { id: 'network-policies', label: 'Network policies', blurb: 'Per-machine ingress and egress rules' },
      { id: 'security-groups', label: 'Security groups', blurb: 'Reusable network rule sets' },
      { id: 'network', label: 'Network overview', blurb: 'Dataplane, policy sync and edge state per VM' },
    ],
  },
  {
    group: 'Operations',
    tone: 'amber',
    items: [
      { id: 'fleet', label: 'Fleet operations', blurb: 'Bulk actions across many machines' },
      { id: 'system', label: 'System', blurb: 'Kairon components and pods' },
    ],
  },
  {
    group: 'AI',
    tone: 'violet',
    items: [{ id: 'assistant', label: 'Assistant', blurb: 'Ask about and diagnose your fleet' }],
  },
] as const satisfies readonly NavGroup[];

type Items = (typeof NAV)[number]['items'][number];
export type Page = Items['id'] | 'account';

export const PAGES: readonly Page[] = [...NAV.flatMap((g) => g.items.map((i) => i.id as Page)), 'account'];

export function isPage(v: string): v is Page {
  return (PAGES as readonly string[]).includes(v);
}

export function groupOf(page: Page): (typeof NAV)[number] | undefined {
  return NAV.find((g) => (g.items as readonly NavItem[]).some((i) => i.id === page));
}

export function labelOf(page: Page): string {
  if (page === 'account') return 'Account';
  for (const g of NAV) for (const i of g.items as readonly NavItem[]) if (i.id === page) return i.label;
  return page;
}
