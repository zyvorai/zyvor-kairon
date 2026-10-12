// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import {Band, Bento, Btn, CtaBand, Grad, Headline, SubNav, Tile} from '@site/src/components/zv';

const GROUPS: {id: string; eyebrow: string; title: string; lede: string; tiles: Tile[]}[] = [
  {
    id: 'run',
    eyebrow: 'Run',
    title: 'Machines, not Pods.',
    lede: 'One resource covers the lifecycle from first boot to deletion.',
    tiles: [
      {title: 'Machine and MachineSet', body: 'Instance types, NUMA and CPU pinning, Windows guests, PVC and CSI boot, cloud-init and port forwards.', to: '/docs/WHAT_SHIPS', glyph: '▣'},
      {title: 'Live disks and NICs', body: 'Hot-attach PVC disks and hot-add or remove NICs on running Machines.', to: '/docs/guides/machine-hotplug', glyph: '⇆'},
      {title: 'Hibernate', body: 'Save RAM and device state to disk and resume the same boot.', to: '/docs/guides/machine-hibernate', glyph: '☾', experimental: true},
      {title: 'Elastic memory', body: 'Guaranteed minimums, balloon reclaim and a node pressure signal.', to: '/docs/guides/machine-elastic-memory', glyph: '◐', experimental: true},
      {title: 'Topology-aware placement', body: 'NUMA, GPU, NIC and NVMe locality decided with the node.', to: '/docs/guides/machine-topology', glyph: '▦', experimental: true},
      {title: 'Warm pools and fork', body: 'Claim a booted VM in one reconcile tick, or fork a running one.', to: '/docs/guides/machine-pools', glyph: '⚡'},
    ],
  },
  {
    id: 'move',
    eyebrow: 'Move',
    title: 'Migration that refuses to guess.',
    lede: 'Relocation is a state machine with a named failure state.',
    tiles: [
      {title: 'Cold and live migration', body: 'mTLS prepare, transfer and commit, with an encrypted data plane.', to: '/docs/guides/relocating-a-machine', glyph: '⇄', variant: 'wide'},
      {title: 'NeedsRecovery', body: 'An ambiguous commit waits for an operator’s attested decision.', to: '/docs/runbook-migration-failures', glyph: '⚠', variant: 'dark'},
      {title: 'Node evacuation', body: 'Cordon a node and the controller can evacuate it, throttled by budgets.', to: '/docs/guides/machine-disruption-budgets', glyph: '⇥'},
      {title: 'Import from VMware', body: 'kaironctl import ova streams, converts and repairs.', to: '/docs/guides/migrate-from-vmware', glyph: '⬇'},
    ],
  },
  {
    id: 'protect',
    eyebrow: 'Protect',
    title: 'Snapshots, backups and an edge on every VM.',
    lede: 'Recovery and isolation are part of the platform.',
    tiles: [
      {title: 'Snapshots and schedules', body: 'CSI VolumeSnapshots with guest filesystem freeze and daily schedules.', to: '/docs/guides/machine-snapshot-quiesce', glyph: '◉'},
      {title: 'Incremental S3 backups', body: 'Changed-block tracking, policies and GFS retention.', to: '/docs/guides/machine-backup-incremental', glyph: '☁', experimental: true},
      {title: 'eBPF edge', body: 'Anti-spoofing, DNS and SNI policy, rate limits, flows and pcap.', to: '/docs/network-fabric', glyph: '◈', variant: 'dark'},
      {title: 'Virtual networks', body: 'Isolated IPv4 networks with a Geneve overlay, SNAT and floating IPs.', to: '/docs/guides/virtual-networks', glyph: '⌘', experimental: true},
      {title: 'Tenant fence', body: 'Deny other tenants’ guest addresses within a namespace.', to: '/docs/guides/tenant-fence', glyph: '▤'},
    ],
  },
  {
    id: 'automate',
    eyebrow: 'Automate',
    title: 'Agents, CLI, dashboard.',
    lede: 'The same API everywhere, with the controls you would want on automation.',
    tiles: [
      {title: 'Self-test', body: 'kaironctl selftest boots a CirrOS VM and checks the platform end to end.', to: '/docs/guides/selftest', glyph: '✓', variant: 'blue'},
      {title: 'MCP server', body: 'Gated writes, single-use approvals and a hash-chained audit log.', to: '/docs/ai-agents', glyph: '✦'},
      {title: 'Dashboard', body: 'Live SSE updates, timelines, metrics, fork and hotplug actions, HTTPS by default.', to: '/docs/guides/kairon-ui-dashboard', glyph: '▭'},
      {title: 'Incident bundles', body: 'One redacted tar.gz with cluster state and node-side evidence.', to: '/docs/guides/machine-incident-bundle', glyph: '⛑', experimental: true},
      {title: 'SDKs and CI actions', body: 'Python and TypeScript SDKs, recipes and disposable-VM GitHub Actions.', to: '/docs/guides/developer-ecosystem', glyph: '</>'},
    ],
  },
];

export default function Features(): ReactNode {
  return (
    <Layout
      title="Features"
      description="Everything Kairon ships: run, move, protect and automate VMs on Kubernetes, with experimental features clearly labelled.">
      <SubNav title="Features" links={GROUPS.map((g) => ({to: `#${g.id}`, label: g.eyebrow}))} />
      <main>
        <Band tone="black" glow center>
          <Headline
            size="display"
            eyebrow="Features"
            title={
              <>
                Run. Move. Protect. <Grad>Automate.</Grad>
              </>
            }
            lede="Experimental features are real and exercised in the lab; each one links to its real limits."
          />
        </Band>
        {GROUPS.map((g, i) => (
          <Band key={g.id} id={g.id} tone={i % 2 === 0 ? 'light' : 'soft'} center>
            <Headline eyebrow={g.eyebrow} title={g.title} lede={g.lede} />
            <Bento tiles={g.tiles} />
          </Band>
        ))}
        <CtaBand title="Every feature, in the docs." lede="The full inventory lists what ships today and what does not.">
          <Btn to="/docs/WHAT_SHIPS">What ships today</Btn>
          <Btn to="/docs/STATUS" variant="ghost">
            Status and gaps
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
