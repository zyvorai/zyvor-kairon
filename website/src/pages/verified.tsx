// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import {
  Actions,
  Band,
  Btn,
  CtaBand,
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
  {to: '#results', label: 'Results'},
  {to: '#numbers', label: 'Numbers'},
  {to: '#how', label: 'How it was run'},
  {to: '#not-run', label: 'Not run yet'},
];

type Row = {name: string; how: ReactNode; result: ReactNode; link: string};

// Every figure here comes from docs/COMPATIBILITY.md ("Lab pair re-run") and
// the MachineMigration objects of 2026-10-15. Change the doc first.
const ROWS: Row[] = [
  {
    name: 'Smoke',
    how: 'Machine hw-live checked Running and Ready. Health only: no create or delete.',
    result: 'pass',
    link: '/docs/COMPATIBILITY#lab-pair-re-run-2026-10-15',
  },
  {
    name: 'Cold migration',
    how: 'nldw3-4-32-26 → nldw4-4-04-32, about 38 s.',
    result: 'pass',
    link: '/docs/guides/relocating-a-machine',
  },
  {
    name: 'Live migration, nldw4 → nldw3',
    how: 'Four runs: 4.1–4.4 s transfer, 148–242 ms downtime, data plane encrypted.',
    result: 'pass',
    link: '/docs/guides/relocating-a-machine',
  },
  {
    name: 'Live migration, nldw3 → nldw4',
    how: 'Two runs: 4.2–4.6 s transfer, 64–262 ms downtime, data plane encrypted.',
    result: 'pass',
    link: '/docs/guides/relocating-a-machine',
  },
  {
    name: 'Source failure mid-transfer',
    how: 'kairon-node-lab stopped on the source for 20 s during the transfer, then restarted. Migration Succeeded.',
    result: 'pass',
    link: '/docs/runbook-migration-failures',
  },
  {
    name: 'Controller failover mid-migration',
    how: 'kairon-controller restarted while the migration ran. Migration Succeeded.',
    result: 'pass',
    link: '/docs/runbook-migration-failures',
  },
  {
    name: 'Ambiguous commit → NeedsRecovery',
    how: 'Destination port blocked until the migration parked in NeedsRecovery; ConfirmDestinationNotCommitted completed the cutover; block removed.',
    result: 'pass',
    link: '/docs/runbook-recovery-drill',
  },
  {
    name: 'Dead node: restart on the other node',
    how: 'Agent stopped and VM killed: NodeUnreachable after 51 s, restarted 13 s after fencing attestation. Machines not opted in were left alone. Not a shared-disk split-brain test.',
    result: 'pass',
    link: '/docs/COMPATIBILITY#repeatability-dead-node-rotation-and-scale-2026-10-1516',
  },
  {
    name: 'Migration CA rotation',
    how: 'New 10-year CA, mTLS verified both ways, two live migrations afterwards.',
    result: 'pass',
    link: '/docs/runbook-migration-cert-rotation',
  },
  {
    name: 'Scale smoke: 8 small Machines',
    how: 'Ramped in batches of 2, all Running in 97 s, lowest free memory 8.7 GiB, then deleted. 8 was the cap; no limit was found.',
    result: 'pass',
    link: '/docs/COMPATIBILITY#repeatability-dead-node-rotation-and-scale-2026-10-1516',
  },
  {
    name: 'Repeated live migration',
    how: '13 of 15 succeeded in early repeat runs; soaks stopped after 8 and 10 clean hops. Causes found and fixed or fixed upstream; the FluxVM fix is not yet deployed to the lab.',
    result: 'partial',
    link: '/docs/COMPATIBILITY#repeatability-dead-node-rotation-and-scale-2026-10-1516',
  },
  {
    name: 'Container sandbox on a Mac',
    how: 'alpine:3.22 run through FluxVM, then created through Kairon’s client: exit code 3 and console output read back in about 4 s.',
    result: 'pass',
    link: '/docs/guides/container-sandboxes',
  },
];

function Hero() {
  return (
    <Band tone="black" glow center>
      <Headline
        size="display"
        eyebrow="Verified on real hardware · 15 October 2026"
        title={
          <>
            What the lab has <Grad>actually run.</Grad>
          </>
        }
        lede="Two real hosts, real QEMU, a re-keyed migration CA and a shared NFS root. Everything below was run; what was not run is listed beside it."
      />
      <Actions>
        <Btn to="#results">See the results</Btn>
        <Btn to="/docs/COMPATIBILITY" variant="ghost">
          Full compatibility record
        </Btn>
      </Actions>
      <p className="zv-stat__note" style={{marginTop: '1.4rem'}}>
        One run per case, one Machine, an idle guest. This shows the control plane, certificates and failure
        handling work on this pair. It says nothing about convergence under load.
      </p>
    </Band>
  );
}

function Results() {
  return (
    <Band tone="light" id="results">
      <Headline
        eyebrow="Results"
        title="Eight cases, eight passes."
        lede="Seven are the migration matrix on the two-host lab; the eighth is a container sandbox on a Mac."
      />
      <div className="zv-scroll">
        <table className="zv-table">
          <thead>
            <tr>
              <th>Case</th>
              <th>What was done</th>
              <th>Result</th>
              <th>Read more</th>
            </tr>
          </thead>
          <tbody>
            {ROWS.map((r) => (
              <tr key={r.name}>
                <td>
                  <strong>{r.name}</strong>
                </td>
                <td>{r.how}</td>
                <td>
                  <Pill ok={r.result === 'pass'}>{r.result}</Pill>
                </td>
                <td>
                  <TextLink to={r.link}>Guide ›</TextLink>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Band>
  );
}

function Numbers() {
  return (
    <Band tone="soft" id="numbers" center>
      <Headline
        eyebrow="Numbers"
        title={
          <>
            Six live migrations, <Grad>all encrypted.</Grad>
          </>
        }
        lede="Measured from the MachineMigration objects of that day: a 2 vCPU, 2 GiB guest, idle."
      />
      <StatBar
        stats={[
          {value: 6, label: 'live migrations', note: 'Four one way, two back. All Succeeded.'},
          {value: 262, suffix: ' ms', label: 'worst downtime', note: 'Best was 64 ms; five of six were 148–262 ms.'},
          {value: 4.6, decimals: 1, suffix: ' s', label: 'slowest transfer', note: 'Fastest was 4.1 s, about 0.5 GB of the guest’s 2 GiB.'},
          {value: 6, label: 'of 6 encrypted', note: 'Data plane TLS on the freshly generated CA.'},
        ]}
      />
    </Band>
  );
}

function How() {
  return (
    <Band tone="black" id="how" glow>
      <Headline
        eyebrow="How it was run"
        title="The repo’s own scripts, from a workstation."
        lede="Nothing here is special to the lab beyond the topology. The matrix script drives the cluster with a short-lived token for a dedicated service account."
      />
      <div className="zv-two">
        <Terminal
          title="hardware-migration-matrix.sh"
          lines={[
            {text: '$ KAIRON_HW_LAB=1 scripts/hardware-migration-matrix.sh \\', kind: 'cmd'},
            {text: '    --case=smoke --case=cold', kind: 'cmd'},
            {text: '==> Cold migration: pass', kind: 'ok'},
            {text: '$ scripts/hardware-migration-matrix.sh \\', kind: 'cmd'},
            {text: '    --case=source-failure --case=controller-failover', kind: 'cmd'},
            {text: '[14:15:25] stopping kairon-node-lab for 20s', kind: 'dim'},
            {text: 'PASS: migration Succeeded; healthy on target', kind: 'ok'},
            {text: 'PASS: migration Succeeded across a controller restart', kind: 'ok'},
            {text: '$ scripts/hardware-migration-matrix.sh --case=needs-recovery', kind: 'cmd'},
            {text: 'NeedsRecovery reached; lifting the block', kind: 'info'},
            {text: 'PASS: ConfirmDestinationNotCommitted completed cutover', kind: 'ok'},
          ]}
        />
        <Steps
          steps={[
            {title: 'Re-key the pair', body: 'A new migration CA and per-host certificates; each node gets its own migration adapter.'},
            {title: 'Share the disk root', body: 'One NFS export mounted on both hosts, so a live move transfers memory only.'},
            {title: 'Run the matrix', body: 'Smoke, cold and both live directions, then the three failure cases.'},
            {title: 'Check nothing is left', body: 'No firewall rule, no stuck migration, every node service active.'},
          ]}
        />
      </div>
    </Band>
  );
}

function NotRun() {
  return (
    <Band tone="light" id="not-run">
      <Headline
        eyebrow="Not run yet"
        title="What this page does not claim."
        lede="These stay marked Experimental or unverified until a run is recorded."
      />
      <List
        limits
        items={[
          'Live migration under guest load, and the three adaptive cases (auto-converge, post-copy switch, zstd): they need a dashboard URL and token to generate load.',
          'Guest-agent exec after migration, and post-copy source failure (needs a disposable Machine).',
          'Live migration with the eBPF dataplane: no eBPF Machine in this lab.',
          'The create, stop, start, delete smoke with an image: only health was checked.',
          'Container sandboxes through kairon-node and the controller on a Mac, Rosetta images, published ports, restart policy and health checks. Only the FluxVM call path was run.',
          'More than 8 Machines, migration under many-Machine load, a shared-disk split-brain, and anything with a GPU.',
          'Reliable repeated live migration: the receiver race fix (FluxVM #417) is merged but not yet running in the lab.',
        ]}
      />
      <p>
        <TextLink to="/docs/STATUS">Read status and production gaps</TextLink>
      </p>
    </Band>
  );
}

export default function Verified(): ReactNode {
  return (
    <Layout
      title="Verified on real hardware"
      description="What Kairon's lab has actually run: live and cold migration both directions, failure drills and container sandboxes, with the figures and what was not run.">
      <SubNav title="Verified" links={SUB_LINKS} />
      <main>
        <Hero />
        <Results />
        <Numbers />
        <How />
        <NotRun />
        <CtaBand title="Read the record." lede="Dates, hosts and caveats for every row.">
          <Btn to="/docs/COMPATIBILITY">Compatibility record</Btn>
          <Btn to="/" variant="ghost">
            Back to home
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
