// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import {readdirSync} from 'node:fs';
import {join} from 'node:path';
import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

type Group = {label: string; description: string; items: string[]};

// Guides grouped by task. A guide added to docs/guides/ but not listed here
// still appears, under "More guides", so nothing falls out of the sidebar.
const GUIDE_GROUPS: Group[] = [
  {
    label: 'AI agents and sandboxes',
    description: 'Agent sessions, idle-suspending sandboxes, warm pools, forks and the MCP server.',
    items: [
      'agent-sessions',
      'agent-sandboxes',
      'machine-sandboxes',
      'container-sandboxes',
      'machine-pools',
      'machine-fork',
      'agent-plane',
      'hermes-mcp',
      'developer-ecosystem',
    ],
  },
  {
    label: 'Machines',
    description: 'Shapes, power states, memory and CPU, images and guests.',
    items: [
      'machine-sets',
      'machine-instance-types',
      'machine-hotplug',
      'machine-pause-resume',
      'machine-halt',
      'machine-hibernate',
      'machine-vm-state-snapshot',
      'machine-elastic-memory',
      'machine-cpu-pinning',
      'machine-cpu-numa',
      'machine-resource-limits',
      'machine-images',
      'machine-image-import',
      'machine-image-catalog',
      'machine-install-media',
      'machine-windows-guests',
    ],
  },
  {
    label: 'Placement, GPUs and Macs',
    description: 'Scheduling, topology, accelerators, maintenance and DRS, and Mac pools.',
    items: [
      'machine-placement',
      'machine-topology',
      'accelerator-profiles',
      'gpu-vgpu',
      'machine-sriov',
      'node-maintenance-drs',
      'preemption',
      'mac-fabric',
    ],
  },
  {
    label: 'Storage',
    description: 'Boot and data disks: PVCs, CSI, Atlas, NVMe-oF and per-disk I/O limits.',
    items: [
      'machine-storage',
      'machine-storage-csi',
      'machine-storage-atlas',
      'machine-storage-thirdparty-csi',
      'machine-storage-nvmeof',
      'disk-qos',
    ],
  },
  {
    label: 'Backup and disaster recovery',
    description: 'Snapshots, full and incremental backups, and failover to a second cluster.',
    items: [
      'machine-backup',
      'machine-backup-incremental',
      'machine-snapshot-schedules',
      'machine-snapshot-quiesce',
      'machine-snapshot-restore',
      'cross-cluster-dr',
    ],
  },
  {
    label: 'Migration',
    description: 'Cold and live migration, policies, TLS, fencing and moving off VMware.',
    items: [
      'relocating-a-machine',
      'migration-policies',
      'machine-migration-tls',
      'machine-fencing',
      'migrate-from-vmware',
    ],
  },
  {
    label: 'Networking',
    description: 'Machine networks, policies, virtual networks, tenant fences and port-forward.',
    items: ['machine-network', 'network-policy', 'virtual-networks', 'tenant-fence', 'machine-port-forward'],
  },
  {
    label: 'Guest access and diagnostics',
    description: 'Guest agent, exec, files, consoles, logs and incident bundles.',
    items: [
      'machine-guest-agent',
      'machine-guest-exec',
      'machine-guest-agent-files',
      'machine-text-console',
      'machine-logs',
      'machine-diagnostics',
      'machine-incident-bundle',
    ],
  },
  {
    label: 'Dashboard and API',
    description: 'The kairon-ui dashboard, its HTTP API, sign-in and high availability.',
    items: ['kairon-ui-dashboard', 'kairon-ui-api', 'kairon-ui-oidc', 'kairon-ui-console-rbac', 'kairon-ui-ha'],
  },
  {
    label: 'Operations',
    description: 'Self-test, metrics, HA, quotas, budgets, admission and fleet automation.',
    items: [
      'selftest',
      'observability',
      'kairon-controller-ha',
      'machine-quotas',
      'machine-disruption-budgets',
      'admission-webhook',
      'crd-versioning',
      'enterprise-fleet',
      'enterprise-fleet-reference',
    ],
  },
  {
    label: 'kaironctl recipes',
    description: 'Acting on many objects at once with label selectors.',
    items: ['kaironctl-get-selector', 'kaironctl-bulk-delete', 'kaironctl-scale-selector'],
  },
];

function guideCategories() {
  const listed = new Set(GUIDE_GROUPS.flatMap((g) => g.items));
  const unlisted = readdirSync(join(__dirname, '../docs/guides'))
    .filter((f) => f.endsWith('.md'))
    .map((f) => f.slice(0, -3))
    .filter((id) => !listed.has(id))
    .sort();
  const groups = unlisted.length
    ? [...GUIDE_GROUPS, {label: 'More guides', description: 'Guides not yet filed under a topic.', items: unlisted}]
    : GUIDE_GROUPS;
  return groups.map((g) => ({
    type: 'category' as const,
    label: g.label,
    collapsed: true,
    link: {type: 'generated-index' as const, description: g.description},
    items: g.items.map((id) => `guides/${id}`),
  }));
}

const sidebars: SidebarsConfig = {
  docsSidebar: [
    'README',
    'getting-started',
    {
      type: 'category',
      label: 'Deploy',
      link: {type: 'generated-index', description: 'Five ways to deploy Kairon, and how to check that it works.'},
      items: [{type: 'autogenerated', dirName: 'deploy'}],
    },
    {
      type: 'category',
      label: 'Product',
      collapsed: false,
      items: ['WHAT_SHIPS', 'STATUS', 'COMPATIBILITY', 'CLI'],
    },
    {
      type: 'category',
      label: 'Tutorials',
      collapsed: true,
      items: [{type: 'autogenerated', dirName: 'tutorials'}],
    },
    {
      type: 'category',
      label: 'Guides',
      collapsed: false,
      link: {type: 'generated-index', slug: '/guides', description: 'Task guides for every Kairon feature, grouped by topic.'},
      items: guideCategories(),
    },
    {
      type: 'category',
      label: 'Concepts',
      collapsed: true,
      items: [
        'architecture',
        'network-fabric',
        'ebpf-edge',
        'ebpf-performance-patch',
        'migration-adapter',
        'ai-agents',
        'macos',
        'macos-cluster',
        'design-cluster-api-provider',
      ],
    },
    {
      type: 'category',
      label: 'Runbooks',
      collapsed: true,
      items: [
        'runbook-migration-failures',
        'runbook-recovery-drill',
        'runbook-multi-host-migration-test',
        'runbook-node-fence',
        'runbook-backup-restore',
        'runbook-velero-backup',
        'runbook-vm-export',
        'runbook-macos-node',
      ],
    },
    {
      type: 'category',
      label: 'Benchmarks',
      collapsed: true,
      items: [{type: 'autogenerated', dirName: 'benchmarks'}],
    },
    {
      type: 'category',
      label: 'Project',
      collapsed: true,
      items: ['DEPENDENCIES', 'OPENSSF_BEST_PRACTICES', 'SUBSCRIPTION-MODEL', 'social/README', 'ux/README'],
    },
  ],
};

export default sidebars;
