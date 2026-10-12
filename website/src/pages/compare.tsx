// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import {Band, Btn, CtaBand, Frame, Grad, Headline, StatBar, SubNav, TextLink} from '@site/src/components/zv';

const ROWS: [string, string, string][] = [
  ['A VM is', 'A Machine: its own CRD and lifecycle', 'A Pod in disguise (virt-launcher)'],
  ['Pods per running VM', '0', '1 virt-launcher pod each'],
  ['Path to KVM', 'kairon-node → FluxVM REST → KVM', 'virt-handler → virt-launcher → libvirt → QEMU'],
  ['Hypervisors', 'QEMU, Cloud Hypervisor, Firecracker, FluxVM', 'QEMU via libvirt'],
  ['Control plane code', 'Go standard library only (controller, node)', 'Large client-go / controller-runtime surface'],
  ['VM network', 'eBPF edge: anti-spoof, DNS/SNI policy, rate limit, flows, drop reasons, pcap', 'Pod CNI, masquerade or bridge binding, Multus'],
  ['Ambiguous migration commit', 'NeedsRecovery: no silent split-brain', 'Stack-dependent recovery'],
  ['AI agents', 'MCP server built in', 'Not included'],
  ['Install', 'One Helm chart from OCI, or kaironctl install', 'Operator plus CR, CDI for images'],
  ['Choose it when', 'You want a small control plane and VMs as first-class resources', 'You need its years of ecosystem integrations (OpenShift Virtualization, Harvester) today'],
];

export default function Compare(): ReactNode {
  return (
    <Layout
      title="Kairon vs KubeVirt"
      description="Kairon compared with KubeVirt v1.9.0 on the same node: idle control plane memory, time to SSH, architecture and when to choose each.">
      <SubNav title="Compare" links={[{to: '#numbers', label: 'Numbers'}, {to: '#table', label: 'Table'}, {to: '/use-cases/replace-kubevirt', label: 'Replace KubeVirt'}]} />
      <main>
        <Band tone="black" glow center>
          <Headline
            size="display"
            eyebrow="Kairon vs KubeVirt"
            title={
              <>
                Every layer you remove is <Grad>one less thing to patch.</Grad>
              </>
            }
            lede="Same kubectl, same KVM, half the stack in between. The VM costs the same either way; what you stop paying for is everything around it."
          />
        </Band>

        <Band tone="light" id="numbers" center>
          <Headline eyebrow="Measured" title="Same node. Same guest." lede="k3s, Ubuntu 24.04, 1 vCPU / 512 MiB, one script driving both, ready meaning the guest’s sshd answers." />
          <StatBar
            stats={[
              {value: 14, suffix: 'x', label: 'less idle memory', note: '63 MiB against 905 MiB.'},
              {value: 2.9, decimals: 1, suffix: 'x', label: 'faster, one VM to SSH', note: '23.7 s against 67.6 s.'},
              {value: 7.4, decimals: 1, suffix: 'x', label: 'faster, five VMs to SSH', note: '24.8 s against 184.7 s, median.'},
              {value: 660, label: 'MiB per VM, even', note: 'Kairon 660, KubeVirt 661: same QEMU.'},
            ]}
          />
          <p className="zv-stat__note" style={{marginTop: '2rem'}}>
            KubeVirt v1.9.0. <TextLink to="/docs/benchmarks/kairon-vs-kubevirt">Method, raw JSON and caveats</TextLink>, including the 10-VM run.
          </p>
        </Band>

        <Band tone="soft" id="table">
          <Headline eyebrow="Side by side" title="What differs." />
          <div className="zv-scroll">
            <table className="zv-table">
              <thead>
                <tr>
                  <th />
                  <th>Kairon</th>
                  <th>KubeVirt</th>
                </tr>
              </thead>
              <tbody>
                {ROWS.map(([k, a, b]) => (
                  <tr key={k}>
                    <td>
                      <strong>{k}</strong>
                    </td>
                    <td>{a}</td>
                    <td>{b}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Band>

        <Band tone="light" center>
          <Headline eyebrow="The stack" title="Half the layers." />
          <Frame
            mermaid={`flowchart LR
  subgraph KubeVirt
    a1["kubectl"] --> a2["virt-handler"] --> a3["virt-launcher pod"] --> a4["libvirt"] --> a5["QEMU"] --> a6["KVM"]
  end
  subgraph Kairon
    b1["kubectl"] --> b2["kairon-node"] --> b3["FluxVM REST"] --> b4["KVM"]
  end`}
            caption="Both end at the same hypervisor and the same guest."
          />
        </Band>

        <CtaBand title="See it on your own hardware." lede="One command, then a self-test that boots a real VM.">
          <Btn to="/deploy">Deploy Kairon</Btn>
          <Btn to="/use-cases/replace-kubevirt" variant="ghost">
            The migration story
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
