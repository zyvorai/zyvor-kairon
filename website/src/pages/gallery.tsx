// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';
import ThemedImage from '@theme/ThemedImage';
import useBaseUrl from '@docusaurus/useBaseUrl';
import styles from './gallery.module.css';

type Shot = {stem: string; caption: string};

// day-<stem>.png / night-<stem>.png in docs/ux, served from the site root.
const TOUR: Shot[] = [
  {stem: '00-overview', caption: 'Overview: fleet health, phases and node usage'},
  {stem: '01-machines', caption: 'Machines, with the inspector open'},
  {stem: '09-machines-all-namespaces', caption: 'All namespaces, with bulk Start and Stop'},
  {stem: '02-migrations', caption: 'Migrations: strategy, nodes and NeedsRecovery'},
  {stem: '03-snapshots', caption: 'Snapshots'},
  {stem: '04-nodes', caption: 'Nodes: capacity and usage'},
  {stem: '05-network-policies', caption: 'Network policies'},
  {stem: '06-pools', caption: 'Warm pools and claims'},
  {stem: '07-backups', caption: 'Backups'},
  {stem: '08-quotas', caption: 'Quotas'},
];

const CLIPS = [
  {name: 'bulk-stop', caption: 'Select three Machines and stop them together'},
  {name: 'namespaces', caption: 'Switch namespace, then filter the table'},
];

const DIAGRAMS = [
  {name: 'how-it-works', alt: 'How Kairon works: clients, the Kubernetes API, controller, node and FluxVM'},
  {name: 'machine-lifecycle', alt: 'Machine lifecycle: Pending, Running, Paused, Halted, Stopped, Failed'},
  {name: 'live-migration', alt: 'Live migration between two nodes over a mutually authenticated TLS link (Preview)'},
  {name: 'no-pod-per-vm', alt: 'Idle control plane memory and time to SSH, Kairon against KubeVirt v1.9.0'},
  {name: 'agent-session', alt: 'Agent sessions: commands pass the policy, git push waits for an admin, egress is allowed or refused, and the session checkpoints and forks (Experimental)'},
  {name: 'agent-sandbox', alt: 'Agent sandboxes: a Machine goes idle, pauses itself and wakes on the next connection (Experimental)'},
  {name: 'cross-cluster-dr', alt: 'Cross-cluster DR: replicate, lose a site, start Machines in order on the other site, move DNS and fence the old site (Experimental)'},
  {name: 'node-maintenance', alt: 'Node maintenance and DRS: drain a node, hibernate what cannot move, rebalance on pressure (Experimental)'},
  {name: 'elastic-memory', alt: 'Elastic memory: balloons give memory back under host pressure, never below each Machine’s minimum (Experimental)'},
  {name: 'incremental-backup', alt: 'Incremental backups: one full, then only changed blocks, merged into a synthetic full in S3 (Experimental)'},
  {name: 'mac-fabric', alt: 'Mac Fabric: capability scheduling across Macs and a private network over Thunderbolt (Experimental)'},
];

function ShotCard({shot}: {shot: Shot}) {
  const day = useBaseUrl(`/day-${shot.stem}.png`);
  const night = useBaseUrl(`/night-${shot.stem}.png`);
  return (
    <figure className={styles.shot}>
      <ThemedImage alt={shot.caption} sources={{light: day, dark: night}} loading="lazy" />
      <figcaption>{shot.caption}</figcaption>
    </figure>
  );
}

function Clip({name, caption}: {name: string; caption: string}) {
  const webm = useBaseUrl(`/clips/${name}.webm`);
  const gif = useBaseUrl(`/clips/${name}.gif`);
  return (
    <figure className={styles.shot}>
      {/* A GIF poster-less fallback keeps this working where <video> is blocked. */}
      <video className={styles.diagram} autoPlay loop muted playsInline preload="metadata" aria-label={caption}>
        <source src={webm} type="video/webm" />
        <img src={gif} alt={caption} />
      </video>
      <figcaption>{caption}</figcaption>
    </figure>
  );
}

function Diagram({name, alt}: {name: string; alt: string}) {
  const src = useBaseUrl(`/anim/${name}.svg`);
  return <img className={styles.diagram} src={src} alt={alt} loading="lazy" />;
}

export default function Gallery(): ReactNode {
  return (
    <Layout
      title="Gallery"
      description="The Kairon dashboard, interaction clips and animated diagrams of how Kairon works.">
      <header className={styles.header}>
        <div className="container">
          <p className={styles.eyebrow}>Product tour</p>
          <Heading as="h1">The Kairon dashboard</Heading>
          <p className={styles.lede}>
            Screenshots and clips are generated from the real dashboard build
            against a sample-data stack, not a production cluster. They follow
            the site theme.
          </p>
        </div>
      </header>
      <main className={styles.main}>
        <section>
          <Heading as="h2">How it works</Heading>
          <p className={styles.note}>
            Kubernetes is the source of truth; FluxVM owns the VMM lifecycle. Live migration is Preview until
            the multi-host lab matrix is green.
          </p>
          <div className={styles.stack}>
            {DIAGRAMS.map((d) => (
              <Diagram key={d.name} {...d} />
            ))}
          </div>
        </section>
        <section>
          <Heading as="h2">In motion</Heading>
          <div className={styles.pair}>
            {CLIPS.map((c) => (
              <Clip key={c.name} {...c} />
            ))}
          </div>
        </section>
        <section>
          <Heading as="h2">Pages</Heading>
          <div className={styles.stack}>
            {TOUR.map((s) => (
              <ShotCard key={s.stem} shot={s} />
            ))}
          </div>
        </section>
      </main>
    </Layout>
  );
}
