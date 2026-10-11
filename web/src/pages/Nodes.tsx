// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { KaironNode, NodeUsage } from '../types';
import { badgeClass, formatBytes, formatCPUPercent, nodeReadyStatus, nodeTaintsSummary } from '../lib/phase';
import ResourceTable from '../components/ResourceTable';
import Inspector, { KV } from '../components/Inspector';
import NodeTools from '../components/NodeTools';
import EmptyHint from '../components/EmptyHint';

function formatAddresses(node: KaironNode): string {
  const addrs = node.status?.addresses || [];
  if (addrs.length === 0) return '-';
  return addrs.map((a) => `${a.type}=${a.address}`).join(', ');
}

// Nodes is a read-only list page for the real Kubernetes Node object --
// not a kairon.zyvor.dev CRD, so it deliberately lives outside the
// "MachineQuota/.../SecurityGroups" family of five/seven list-only policy
// pages above it in Nav, even though it shares their ResourceTable/list
// shape exactly. GET /api/v1/nodes already existed (internal/uiapi/
// overview.go's handleListNodes, added for the Overview tile's node
// count) but until now had no dashboard page consuming it directly --
// an operator using only the dashboard, not kubectl or kaironctl, had no
// way to see which nodes exist, whether each is Ready, or what it's
// tainted with, even though `kaironctl get nodes` has answered exactly
// that since Node taints started actually gating scheduling (see
// docs/guides/machine-placement.md's "Taints and tolerations" section).
// Deliberately no create/edit/delete here, matching kaironctl's own node
// support: Node is a real cluster resource kubectl already owns the
// lifecycle of, not something Kairon ever creates or removes.
export default function Nodes() {
  const [items, setItems] = useState<KaironNode[]>([]);
  const [usage, setUsage] = useState<Record<string, NodeUsage>>({});
  const [msg, setMsg] = useState('');
  const [selected, setSelected] = useState<string | null>(null);

  const refresh = () => {
    api<KaironNode[]>('/api/v1/nodes').then(setItems).catch((e) => setMsg(String(e)));
    // A dedicated GET request, not derived from `items` above: usage is
    // rolled up cluster-wide from every Machine's own Status.ResourceUsage
    // (internal/uiapi's handleNodeUsage), an entirely different resource
    // than the Node list itself, so it can 404/500 independently -- a
    // usage fetch failure here is swallowed (falling back to "-" cells
    // below) rather than clobbering `msg` and hiding an otherwise-healthy
    // Node list behind an error banner.
    api<NodeUsage[]>('/api/v1/nodes/usage')
      .then((rows) => setUsage(Object.fromEntries(rows.map((r) => [r.node, r]))))
      .catch(() => setUsage({}));
  };

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">NODES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(n) => n.metadata.name}
          onRowClick={(n) => setSelected(n.metadata.name)}
          selectedKey={selected ?? undefined}
          emptyText={<EmptyHint what="nodes visible to this cluster" namespaced={false} hint="Label a node to make it eligible: kubectl label node NAME kairon.zyvor.dev/capable=true. kaironctl doctor lists what is missing." />}
          columns={[
            { header: 'Name', render: (n) => n.metadata.name },
            { header: 'Ready', render: (n) => <span className={badgeClass(nodeReadyStatus(n))}>{nodeReadyStatus(n)}</span> },
            { header: 'Unschedulable', render: (n) => (n.spec?.unschedulable ? 'true' : 'false') },
            { header: 'Taints', render: (n) => nodeTaintsSummary(n) },
            { header: 'Addresses', render: (n) => formatAddresses(n) },
            { header: 'Machines', render: (n) => usage[n.metadata.name]?.machines ?? 0 },
            { header: 'CPU', render: (n) => (usage[n.metadata.name] ? formatCPUPercent(usage[n.metadata.name].cpuPercent) : '-') },
            { header: 'Memory', render: (n) => (usage[n.metadata.name] ? formatBytes(usage[n.metadata.name].memoryBytes) : '-') },
          ]}
        />
      </div>
      {selected && (
        <Inspector
          title={selected}
          subtitle="FluxVM node tools"
          status={(() => {
            const n = items.find((x) => x.metadata.name === selected);
            return n ? <span className={badgeClass(nodeReadyStatus(n))}>{nodeReadyStatus(n)}</span> : undefined;
          })()}
          onClose={() => setSelected(null)}
        >
          <KV
            rows={[
              ['Machines', usage[selected]?.machines ?? 0],
              ['CPU', usage[selected] ? formatCPUPercent(usage[selected].cpuPercent) : undefined],
              ['Memory', usage[selected] ? formatBytes(usage[selected].memoryBytes) : undefined],
            ]}
          />
          <NodeTools node={selected} />
        </Inspector>
      )}
    </div>
  );
}
