// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import useBaseUrl from '@docusaurus/useBaseUrl';
import Layout from '@theme/Layout';
import {
  Actions,
  Band,
  Bento,
  Btn,
  Cards,
  CtaBand,
  Frame,
  Grad,
  Headline,
  List,
  Pill,
  StatBar,
  Steps,
  SubNav,
  Terminal,
  TextLink,
} from '@site/src/components/zv';

const SUB_LINKS = [
  {to: '#numbers', label: 'Numbers'},
  {to: '#capabilities', label: 'Capabilities'},
  {to: '#deploy', label: 'One command'},
  {to: '#dashboard', label: 'Dashboard'},
  {to: '#honest', label: 'Status'},
];

function Hero() {
  return (
    <Band tone="black" glow center>
      <Headline
        size="display"
        eyebrow="Kairon · VM orchestration on Kubernetes"
        title={
          <>
            VMs that don&apos;t
            <br />
            pretend to be <Grad>Pods.</Grad>
          </>
        }
        lede="Real virtual machines, scheduled by Kubernetes and run straight on KVM. No virt-launcher pod per VM, no libvirt in the hot path, no operator zoo."
      />
      <Actions>
        <Btn to="/deploy">Deploy in one command</Btn>
        <Btn to="#capabilities" variant="ghost">
          See how it works
        </Btn>
      </Actions>
      <Terminal
        title="deploy-remote.sh · lab host"
        lines={[
          {kind: 'cmd', text: './scripts/deploy-remote.sh user@host --remote-build \\'},
          {kind: 'out', text: '    --with-controller --with-ui --with-console --self-test=quick'},
          {kind: 'dim', text: '[*] compiling on the host, nothing is built on this machine'},
          {kind: 'dim', text: '[*] installing kairon-node, kairon-controller, kairon-ui (HTTPS)'},
          {kind: 'info', text: '[*] kaironctl selftest --profile quick'},
          {kind: 'ok', text: '  ✓ control-plane   ✓ boot   ✓ console-log   ✓ power'},
          {kind: 'ok', text: '[+] PASS: a CirrOS VM booted, stopped and started'},
        ]}
      />
      <p className="zv-stat__note" style={{marginTop: '0.9rem'}}>
        Abridged and illustrative; the real run prints live, coloured logs.
      </p>
    </Band>
  );
}

function Numbers() {
  return (
    <Band tone="light" id="numbers" center>
      <Headline
        eyebrow="Measured, not marketed"
        title={
          <>
            Half the stack. <Grad>Same VM.</Grad>
          </>
        }
        lede="Same k3s node, same Ubuntu 24.04 guest, same 1 vCPU and 512 MiB, one script driving both. What you stop paying for is everything KubeVirt puts around the VM."
      />
      <StatBar
        stats={[
          {value: 14, suffix: 'x', label: 'less idle control plane', note: '63 MiB against 905 MiB of memory.'},
          {value: 7.4, decimals: 1, suffix: 'x', label: 'faster to SSH, five VMs', note: '24.8 s against 184.7 s, median.'},
          {value: 0, label: 'pods per running VM', note: 'The VM is the only thing running.'},
          {value: 4, label: 'hypervisors, one CRD', note: 'QEMU, Cloud Hypervisor, Firecracker, FluxVM.'},
        ]}
      />
      <p className="zv-stat__note" style={{marginTop: '2.5rem'}}>
        KubeVirt v1.9.0. Method, raw JSON and caveats:{' '}
        <TextLink to="/docs/benchmarks/kairon-vs-kubevirt">read the benchmark</TextLink>
      </p>
    </Band>
  );
}

function HowItWorks() {
  const how = useBaseUrl('/anim/how-it-works.svg');
  return (
    <Band tone="soft" center>
      <Headline
        eyebrow="Architecture"
        title="Two Go binaries and your KVM hosts."
        lede="Kubernetes is the source of truth. kairon-controller places a Machine; kairon-node on the host turns it into FluxVM calls. Both are Go standard library only, small enough to read in an afternoon."
      />
      <Frame
        src={how}
        alt="Clients talk to the Kubernetes API, kairon-controller places Machines and kairon-node reconciles them into FluxVM"
        caption="A Machine is desired state in the Kubernetes API. Nothing else holds the truth."
      />
      <p>
        <TextLink to="/docs/architecture">Read the architecture</TextLink>
      </p>
    </Band>
  );
}

function Capabilities() {
  return (
    <Band tone="light" id="capabilities" center>
      <Headline
        eyebrow="Everything a VM platform needs"
        title="Run. Move. Protect. Automate."
        lede="One Machine resource covers the lifecycle. Features marked Experimental are real and tested in the lab; their limits are written down."
      />
      <Bento
        tiles={[
          {title: 'Migration that refuses to guess', body: 'Cold, node evacuation and secure live migration over mutual TLS. An ambiguous commit stops in NeedsRecovery and asks a human: never two running copies.', to: '/docs/guides/relocating-a-machine', glyph: '⇄', variant: 'wide', more: 'How migration is kept safe ›'},
          {title: 'An eBPF edge on every VM', body: 'Anti-spoofing, DNS and SNI allowlists, rate limits, drops with reasons, and packet capture of one VM in one command.', to: '/docs/network-fabric', glyph: '◈', variant: 'dark'},
          {title: 'Sandboxes in milliseconds', body: 'Firecracker and FluxVM microVMs, warm pools and claims, fork a running VM.', to: '/docs/guides/machine-pools', glyph: '⚡'},
          {title: 'Incremental S3 backups', body: 'Changed-block tracking uploads only what the guest wrote, with schedules and GFS retention.', to: '/docs/guides/machine-backup-incremental', glyph: '☁', experimental: true},
          {title: 'Hibernate', body: 'Save RAM and device state to disk, free the node, and resume the same boot.', to: '/docs/guides/machine-hibernate', glyph: '☾', experimental: true},
          {title: 'Elastic memory', body: 'Run more VM memory than the host has: guaranteed minimums, a balloon reclaim loop and a pressure signal the scheduler honours.', to: '/docs/guides/machine-elastic-memory', glyph: '◐', experimental: true},
          {title: 'Virtual networks', body: 'Isolated IPv4 networks routed in eBPF with a Geneve overlay, egress SNAT and floating IPs.', to: '/docs/guides/virtual-networks', glyph: '⌘', experimental: true},
          {title: 'Topology-aware placement', body: 'NUMA, GPU, NIC and NVMe locality decided together with the node.', to: '/docs/guides/machine-topology', glyph: '▦', experimental: true},
          {title: 'AI agents, safely', body: 'A built-in MCP server. Writes are gated, approved and written to a hash-chained audit log. No model in the reconcile loop.', to: '/docs/ai-agents', glyph: '✦', variant: 'blue'},
          {title: 'Every Mac a node', body: 'Register an Apple silicon Mac as a Node and run vz Machines through FluxVM. Verified on an M4.', to: '/docs/macos', glyph: '⬢', variant: 'wide', more: 'Kairon on macOS ›'},
        ]}
      />
    </Band>
  );
}

function Deploy() {
  return (
    <Band tone="black" id="deploy" glow center>
      <Headline
        eyebrow="Easy deployment"
        title={
          <>
            From a bare host to a booted VM in <Grad>one command.</Grad>
          </>
        }
        lede="--remote-build compiles on the host, so your laptop needs no Go and no Node. Everything is HTTPS by default. The self-test boots a tiny CirrOS VM to prove it all works."
      />
      <Steps
        steps={[
          {title: 'Label a node', body: 'kubectl label node worker-1 kairon.zyvor.dev/capable=true'},
          {title: 'Deploy', body: 'deploy-remote.sh user@host --remote-build --with-ui, or one Helm command from GHCR.'},
          {title: 'Prove it', body: 'kaironctl selftest boots CirrOS and checks power, network, hotplug and snapshots.'},
          {title: 'Open the dashboard', body: 'HTTPS out of the box, with a Self-test page and live updates.'},
        ]}
      />
      <Actions>
        <Btn to="/deploy">Choose your deployment path</Btn>
        <Btn to="/docs/guides/selftest" variant="ghost">
          What the self-test checks
        </Btn>
      </Actions>
    </Band>
  );
}

function Dashboard() {
  const day = useBaseUrl('/day-01-machines.png');
  const night = useBaseUrl('/night-01-machines.png');
  return (
    <Band tone="light" id="dashboard" center>
      <Headline
        eyebrow="Dashboard"
        title="Watch it live. Fix it fast."
        lede="Machines stream over SSE with a per-VM timeline and metrics, fork and hotplug actions, a node capacity what-if, and a Self-test page with a terminal-style log. It is just another Kubernetes API client."
      />
      <Frame
        src={day}
        darkSrc={night}
        alt="The Kairon dashboard: the Machines table with the inspector open"
        caption="Captured against a sample-data stack, not a production cluster."
      />
      <p>
        <TextLink to="/gallery">Open the gallery</TextLink> ·{' '}
        <TextLink to="/docs/guides/kairon-ui-dashboard">Dashboard guide</TextLink>
      </p>
    </Band>
  );
}

function UseCases() {
  return (
    <Band tone="soft" center>
      <Headline eyebrow="Who it is for" title="One platform, many shapes." />
      <Cards
        cards={[
          {eyebrow: 'CI and AI agents', title: 'Throwaway sandboxes', body: 'Warm pools, fork and TTLs for ephemeral VMs.', to: '/docs/guides/machine-pools', more: 'Warm pools ›'},
          {eyebrow: 'Platform teams', title: 'Replace KubeVirt, keep Kubernetes', body: 'One CRD, zero pods per VM, a control plane you can read.', to: '/docs/benchmarks/kairon-vs-kubevirt', more: 'See the benchmark ›'},
          {eyebrow: 'Migration', title: 'Move off VMware', body: 'kaironctl import ova streams an OVA and FluxVM repairs the guest.', to: '/docs/guides/migrate-from-vmware', more: 'Migration guide ›'},
        ]}
      />
    </Band>
  );
}

function Honest() {
  return (
    <Band tone="light" id="honest">
      <Headline
        eyebrow="Honest status"
        title="What is verified, and what is not."
        lede="Production gaps stay listed, not glossed over."
      />
      <div className="zv-two">
        <div>
          <h3>
            Verified <Pill ok>Lab</Pill>
          </h3>
          <List
            items={[
              'Boot, power, network, vCPU hotplug and snapshot-restore on a real FluxVM host (kaironctl selftest).',
              'Cold and live migration between two real hosts over mTLS, including adaptive auto-converge, post-copy switch and zstd, and the source-failure, NeedsRecovery and controller-failover drills (both directions on matched QEMU, lab run of 2026-10-14).',
              'Node maintenance draining Machines to another node by live migration.',
              'Snapshots and DRA bridging.',
              'Native macOS node on an Apple M4 with a real k3s cluster.',
              'Controller and node are Go standard library only (enforced in CI).',
            ]}
          />
        </div>
        <div>
          <h3>Not yet</h3>
          <List
            limits
            items={[
              'The NeedsRecovery drill passed 5 of 6 lab runs and failed once unexplained, live migration with the eBPF dataplane has not run, and none of it runs in this repo’s CI yet.',
              'Plain pre-copy under a heavy write load did not converge on a 1 Gbps link; adaptive migration is the answer.',
              'Incremental S3 backups, hibernate resume on another node, cross-host virtual networks and multi-Mac clusters are not verified.',
              'Experimental features are labelled; read their “Real limits today”.',
            ]}
          />
        </div>
      </div>
      <p>
        <TextLink to="/docs/STATUS">Read status and production gaps</TextLink>
      </p>
    </Band>
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="Real VMs on Kubernetes, without KubeVirt"
      description="Kairon is Zyvor's Apache-2.0 control plane for running virtual machines on Kubernetes through FluxVM: no virt-launcher pod, no libvirt. One-command deploy, HTTPS by default, a built-in self-test.">
      <SubNav title="Kairon" links={SUB_LINKS} />
      <main>
        <Hero />
        <Numbers />
        <HowItWorks />
        <Capabilities />
        <Deploy />
        <Dashboard />
        <UseCases />
        <Honest />
        <CtaBand
          title="Run your first VM today."
          lede="Apache-2.0, free to run in production. Zyvor Enterprise adds support and SLAs.">
          <Btn to="/deploy">Deploy Kairon</Btn>
          <Btn to="https://github.com/zyvorai/zyvor-kairon" variant="ghost">
            View on GitHub
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
