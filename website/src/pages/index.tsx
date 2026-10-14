// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import {useState} from 'react';
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

const RELEASE_NOTES = 'https://github.com/zyvorai/zyvor-kairon/blob/v0.10.0/RELEASE_NOTES.md';

const SUB_LINKS = [
  {to: '#new', label: 'New in v0.10'},
  {to: '#see-it-move', label: 'See it move'},
  {to: '#numbers', label: 'Numbers'},
  {to: '#capabilities', label: 'Capabilities'},
  {to: '#deploy', label: 'One command'},
  {to: '#dashboard', label: 'Dashboard'},
  {to: '#honest', label: 'Status'},
];

function Hero() {
  return (
    <Band tone="black" glow center>
      <p>
        <TextLink to="#new">v0.10.0 is out: agent sessions, cross-cluster DR, Mac Fabric and virtctl parity</TextLink>
      </p>
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

function WhatsNew() {
  return (
    <Band tone="soft" id="new" center>
      <Headline
        eyebrow="New in v0.10.0 · October 2026"
        title={
          <>
            Agents, DR and <Grad>the data center.</Grad>
          </>
        }
        lede="The biggest Kairon release yet: sandboxes for AI coding agents, failover to a second cluster, GPUs and Macs as schedulable pools, and every virtctl verb in kaironctl. New features are opt-in and marked Experimental until their lab rows pass."
      />
      <Cards
        cards={[
          {eyebrow: 'Experimental', title: 'Agent sessions', body: 'A FluxVM agent session as an AgentSession: a sandbox VM with a git workspace, an egress and process policy, checkpoints, live forks, admin approvals, credential grants the guest never sees, and the Mac guest’s screen.', to: '/docs/guides/agent-sessions', more: 'Agent sessions guide ›'},
          {eyebrow: 'Experimental', title: 'Agent sandboxes', body: 'Machines that pause, hibernate or delete themselves when idle and wake on the next console, exec or port-forward. Paused warm pools hold memory, not CPU; claims report what they used.', to: '/docs/guides/agent-sandboxes', more: 'Agent sandboxes guide ›'},
          {eyebrow: 'Experimental', title: 'Cross-cluster DR', body: 'MachineProtectionPlan replicates Machines to a second cluster with RPO and RTO objectives, ordered failover and failback with epoch fencing, kept addresses, DNS that follows the active site, and scheduled DR tests.', to: '/docs/guides/cross-cluster-dr', more: 'DR guide ›'},
          {eyebrow: 'Experimental', title: 'Mac Fabric', body: 'Several Apple silicon Macs as one pool: chip, macOS and Rosetta labels, spec.apple scheduling, and MacNetwork private networks across Macs over Thunderbolt.', to: '/docs/guides/mac-fabric', more: 'Mac Fabric guide ›'},
          {eyebrow: 'Experimental', title: 'GPUs and accelerators', body: 'vGPU and MIG profiles scheduled by free instances and GPU memory, GPU health in NodeTopology, and named accelerator profiles that expand into devices with per-node capacity.', to: '/docs/guides/accelerator-profiles', more: 'Accelerator profiles ›'},
          {eyebrow: 'kaironctl', title: 'Coming from virtctl', body: 'console, vnc and screenshots, restart and soft-reboot, exports and memory dumps, guest info, SSH keys, expose, freeze, guestfs, cdrom, create clone, --dry-run -o yaml and wait.', to: '/docs/CLI#coming-from-virtctl', more: 'virtctl to kaironctl ›'},
        ]}
      />
      <div className="zv-two" style={{marginTop: '3rem', textAlign: 'left'}}>
        <div>
          <h3>Also in v0.10.0</h3>
          <List
            items={[
              'DRS on measured pressure (PSI), and node maintenance mode that migrates, hibernates or parks Machines.',
              'NVMe-oF volumes with native multipath, raw Block boot, and per-disk IOPS and bandwidth limits changed live.',
              'Adaptive live migration: auto-converge, then post-copy, inside a downtime and completion budget.',
              'Incremental S3 backups, topology-aware placement, elastic memory, virtual networks and hibernate.',
              'Incident bundles: one redacted tar.gz with cluster state and the node’s QMP, cgroup, console and pcap.',
              'kaironctl selftest, a live dashboard over server-sent events, and HTTPS everywhere by default.',
            ]}
          />
        </div>
        <div>
          <h3>Upgrading from v0.9</h3>
          <p className="zv-note">
            The dashboard now serves HTTPS and the console relay uses TLS, so upgrade kairon-ui and kairon-node
            together. Apply the CRDs and RBAC from the v0.10.0 tag before the new binaries: eleven CRDs are new.
            FluxVM 0.4.8 has every route this release uses.
          </p>
          <Actions>
            <Btn to={RELEASE_NOTES}>Read the release notes</Btn>
            <Btn to="/docs/COMPATIBILITY" variant="ghost">
              What the lab has run
            </Btn>
          </Actions>
        </div>
      </div>
    </Band>
  );
}

type Diagram = {id: string; tab: string; doc: string; alt: string; caption: string};

const DIAGRAMS: Diagram[] = [
  {id: 'agent-session', tab: 'Agent sessions', doc: '/docs/guides/agent-sessions', alt: 'An agent’s commands pass through the AgentSession into the sandbox VM; git push waits for an admin, egress to an allowed host passes and an unknown host is refused, and the session checkpoints and forks', caption: 'Commands pass the policy, risky ones wait for an admin, and forks branch from a checkpoint.'},
  {id: 'agent-sandbox', tab: 'Idle sandboxes', doc: '/docs/guides/agent-sandboxes', alt: 'A sandbox Machine is busy, goes idle, pauses itself when the idle timer runs out, and wakes when a client connects', caption: 'Busy, idle, paused with no CPU, then woken by the next connection.'},
  {id: 'cross-cluster-dr', tab: 'Cross-cluster DR', doc: '/docs/guides/cross-cluster-dr', alt: 'Site A replicates restore points to site B; site A is lost; db then web start on site B, DNS moves and site A is fenced', caption: 'Replicate, lose a site, start Machines in order on the other one, move DNS and fence the old site.'},
  {id: 'node-maintenance', tab: 'Maintenance and DRS', doc: '/docs/guides/node-maintenance-drs', alt: 'node-1 enters maintenance, two Machines migrate and a GPU Machine hibernates; DRS moves a Machine off a node under pressure; maintenance ends', caption: 'Drain a node, hibernate what cannot move, rebalance on pressure, and resume on exit.'},
  {id: 'elastic-memory', tab: 'Elastic memory', doc: '/docs/guides/machine-elastic-memory', alt: 'Balloons inflate in three Machines down to their minimums under host pressure, the node reports pressure, then the balloons deflate', caption: 'Balloons give memory back under pressure, never below each Machine’s minimum.'},
  {id: 'incremental-backup', tab: 'Incremental backups', doc: '/docs/guides/machine-backup-incremental', alt: 'A full backup uploads every block, incrementals upload only changed blocks, and a synthetic full merges the chain in S3', caption: 'One full, then only the changed blocks, merged into a synthetic full in S3.'},
  {id: 'mac-fabric', tab: 'Mac Fabric', doc: '/docs/guides/mac-fabric', alt: 'A Machine asking for an M4 lands on mac-a, a Machine on a MacNetwork lands on mac-b, and the VMs talk over Thunderbolt', caption: 'Capability scheduling across Macs, and a private network over Thunderbolt.'},
  {id: 'live-migration', tab: 'Live migration', doc: '/docs/guides/relocating-a-machine', alt: 'Live migration between two nodes over a mutually authenticated TLS link', caption: 'The source stays authoritative until the target has committed.'},
];

function SeeItMove() {
  const [active, setActive] = useState(DIAGRAMS[0].id);
  const base = useBaseUrl('/anim/');
  const d = DIAGRAMS.find((x) => x.id === active) ?? DIAGRAMS[0];
  return (
    <Band tone="black" id="see-it-move" glow center>
      <Headline
        eyebrow="See it move"
        title={
          <>
            How it works, <Grad>in motion.</Grad>
          </>
        }
        lede="Each diagram loops through one feature from start to finish. Pick one; the guide has the details and the real limits."
      />
      <div className="zv-tabs" role="tablist" aria-label="Animated diagrams">
        {DIAGRAMS.map((x) => (
          <button
            key={x.id}
            id={`anim-tab-${x.id}`}
            role="tab"
            type="button"
            className="zv-tab"
            aria-selected={x.id === active}
            aria-controls="anim-panel"
            onClick={() => setActive(x.id)}>
            {x.tab}
          </button>
        ))}
      </div>
      <div id="anim-panel" role="tabpanel" aria-labelledby={`anim-tab-${d.id}`}>
        <Frame key={d.id} src={`${base}${d.id}.svg`} alt={`Animated: ${d.alt}`} caption={d.caption} />
        <p>
          <TextLink to={d.doc}>{d.tab} guide</TextLink>
        </p>
      </div>
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
        lede="One Machine resource covers the lifecycle. Features marked Experimental are opt-in; what has run in the lab, and what has not, is written down."
      />
      <Bento
        tiles={[
          {title: 'Migration that refuses to guess', body: 'Cold, node evacuation and secure live migration over mutual TLS, now with an adaptive engine that auto-converges and switches to post-copy. An ambiguous commit stops in NeedsRecovery and asks a human: never two running copies.', to: '/docs/guides/relocating-a-machine', glyph: '⇄', variant: 'wide', more: 'How migration is kept safe ›'},
          {title: 'An eBPF edge on every VM', body: 'Anti-spoofing, DNS and SNI allowlists, rate limits, drops with reasons, and packet capture of one VM in one command.', to: '/docs/network-fabric', glyph: '◈', variant: 'dark'},
          {title: 'Agent sessions', body: 'Sandbox VMs for AI coding agents: git workspace, egress policy, checkpoints, live forks, approvals and an event ledger.', to: '/docs/guides/agent-sessions', glyph: '✳', variant: 'blue', experimental: true},
          {title: 'Sandboxes in milliseconds', body: 'Firecracker and FluxVM microVMs, warm pools that can sit paused, idle suspend with wake on connect, and fork a running VM.', to: '/docs/guides/agent-sandboxes', glyph: '⚡'},
          {title: 'AI agents, safely', body: 'A built-in MCP server. Writes are gated, approved and written to a hash-chained audit log. No model in the reconcile loop.', to: '/docs/ai-agents', glyph: '✦'},
          {title: 'Cross-cluster DR', body: 'Replicate Machines to a second cluster with RPO and RTO objectives. Ordered failover and failback with epoch fencing, addresses and DNS that follow the active site, and DR tests in an isolated network on a schedule.', to: '/docs/guides/cross-cluster-dr', glyph: '⟲', variant: 'wide', experimental: true, more: 'Plan a failover ›'},
          {title: 'Incremental S3 backups', body: 'Changed-block tracking uploads only what the guest wrote, with schedules, GFS retention, encryption and Object Lock.', to: '/docs/guides/machine-backup-incremental', glyph: '☁', experimental: true},
          {title: 'Hibernate', body: 'Save RAM and device state to disk, free the node, and resume the same boot.', to: '/docs/guides/machine-hibernate', glyph: '☾', experimental: true},
          {title: 'Elastic memory', body: 'Run more VM memory than the host has: guaranteed minimums, a balloon reclaim loop and a pressure signal the scheduler honours.', to: '/docs/guides/machine-elastic-memory', glyph: '◐', experimental: true},
          {title: 'Virtual networks', body: 'Isolated IPv4 networks routed in eBPF with a Geneve overlay, egress SNAT, floating IPs and BGP announcements.', to: '/docs/guides/virtual-networks', glyph: '⌘', experimental: true},
          {title: 'Topology-aware placement', body: 'NUMA, GPU, NIC and NVMe locality decided together with the node.', to: '/docs/guides/machine-topology', glyph: '▦', experimental: true},
          {title: 'GPUs, vGPU and accelerators', body: 'vGPU and MIG profiles scheduled by free instances and GPU memory, GPU health, and named accelerator profiles.', to: '/docs/guides/gpu-vgpu', glyph: '◆', experimental: true},
          {title: 'DRS and maintenance', body: 'Rebalance on reserved resources or measured pressure, and drain a node by migrating, hibernating or parking its Machines.', to: '/docs/guides/node-maintenance-drs', glyph: '⚖', experimental: true},
          {title: 'NVMe-oF and disk QoS', body: 'nvme-tcp and nvme-rdma volumes with native multipath and raw Block boot, plus per-disk IOPS and bandwidth limits changed live.', to: '/docs/guides/machine-storage-nvmeof', glyph: '⛁', experimental: true},
          {title: 'Incident bundles', body: 'One command collects a redacted tar.gz: cluster state plus the node’s QMP, cgroup, console log, dataplane and pcap.', to: '/docs/guides/machine-incident-bundle', glyph: '⚑', experimental: true},
          {title: 'Every virtctl verb', body: 'console, vnc, restart, soft-reboot, export, memory-dump, guest info, credentials, expose, freeze, guestfs, cdrom and wait in kaironctl.', to: '/docs/CLI#coming-from-virtctl', glyph: '›_', variant: 'dark'},
          {title: 'Every Mac a node, every Mac a pool', body: 'Register an Apple silicon Mac as a Node and run vz Machines through FluxVM, verified on an M4. Mac Fabric pools several Macs with capability scheduling and private networks over Thunderbolt.', to: '/docs/guides/mac-fabric', glyph: '⬢', variant: 'wide', more: 'Mac Fabric ›'},
          {title: 'Small enough to audit', body: 'kairon-controller and kairon-node use only the Go standard library, enforced in CI.', to: '/docs/DEPENDENCIES', glyph: '❏'},
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
        lede="Machines stream over SSE with a per-VM timeline and metrics, fork and hotplug actions, a node capacity what-if, Topology, System and Network pages, a Self-test page, and an Agent sessions page with approvals and the Mac guest’s screen. It is just another Kubernetes API client."
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
          {eyebrow: 'CI', title: 'Throwaway sandboxes', body: 'Warm pools that can sit paused, fork, idle suspend and maximum lifetimes for ephemeral VMs.', to: '/docs/guides/agent-sandboxes', more: 'Agent sandboxes ›'},
          {eyebrow: 'AI coding agents', title: 'A VM per agent, on a leash', body: 'Agent sessions with a git workspace, an egress policy, budgets, checkpoints and forks, and commands that wait for a human.', to: '/docs/guides/agent-sessions', more: 'Agent sessions ›'},
          {eyebrow: 'Platform teams', title: 'Replace KubeVirt, keep Kubernetes', body: 'One CRD, zero pods per VM, a control plane you can read, and the virtctl verbs you already know.', to: '/docs/benchmarks/kairon-vs-kubevirt', more: 'See the benchmark ›'},
          {eyebrow: 'Migration', title: 'Move off VMware', body: 'kaironctl import ova streams an OVA and FluxVM repairs the guest.', to: '/docs/guides/migrate-from-vmware', more: 'Migration guide ›'},
          {eyebrow: 'Business continuity', title: 'Survive losing a site', body: 'Incremental backups to S3 and failover to a second cluster, with tests you can schedule.', to: '/docs/guides/cross-cluster-dr', more: 'Cross-cluster DR ›'},
          {eyebrow: 'GPU workloads', title: 'Share and place GPUs', body: 'vGPU and MIG slices, NUMA-aware placement and named accelerator shapes for inference fleets.', to: '/docs/guides/accelerator-profiles', more: 'Accelerator profiles ›'},
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
              'Single-node lab runs of hibernate, elastic memory, virtual networks, NVMe-oF, disk QoS, vGPU, accelerator profiles, node maintenance, DRS, incident bundles and agent sandboxes.',
              'Cross-cluster DR replication, failover, failback and DR tests, with both sites on one lab node.',
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
              'Incremental S3 backups, hibernate resume on another node and cross-host virtual networks are not verified.',
              'Agent sessions have not run on a real Mac yet.',
              'Mac Fabric has only run against simulated Mac nodes; multi-Mac clusters are not verified.',
              'DR between two real clusters, and anything needing an NVIDIA GPU or a BGP router.',
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
      description="Kairon is Zyvor's Apache-2.0 control plane for running virtual machines on Kubernetes through FluxVM: no virt-launcher pod, no libvirt. v0.10.0 adds agent sessions for AI coding agents, cross-cluster DR, Mac Fabric, GPU profiles and every virtctl verb.">
      <SubNav title="Kairon" links={SUB_LINKS} />
      <main>
        <Hero />
        <WhatsNew />
        <SeeItMove />
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
