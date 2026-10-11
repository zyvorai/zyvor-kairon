// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';
import FeatureHighlights from '@site/src/components/FeatureHighlights';
import Reveal from '@site/src/components/Reveal';

import styles from './index.module.css';

function HomepageHeader() {
  return (
    <header className={clsx('hero hero--primary', styles.heroBanner)}>
      <div className="container">
        <div className={clsx(styles.heroGridSingle, 'text--center')}>
          <Heading as="h1" className="hero__title">
            Kubernetes declares.
            <br />
            Kairon orchestrates.
          </Heading>
          <p className="hero__subtitle">
            Real VMs on Kubernetes — without KubeVirt. A{' '}
            <code>Machine</code> is desired state in the API.{' '}
            <code>kairon-controller</code> places it. <code>kairon-node</code>{' '}
            turns that into FluxVM — QEMU, Cloud Hypervisor, Firecracker, or
            the FluxVM hypervisor — on real KVM. No per-VM wrapper Pod, no
            libvirt, no guessed hypervisor migration endpoints.
          </p>
          <div className={styles.buttons}>
            <Link
              className="button button--secondary button--lg"
              to="/docs/getting-started">
              Get Started
            </Link>
            <Link
              className="button button--outline button--lg button--secondary"
              to="https://github.com/zyvorai/kairon">
              View on GitHub
            </Link>
          </div>
        </div>
      </div>
    </header>
  );
}

function ProblemStatement() {
  return (
    <section className={styles.problem}>
      <div className="container">
        <Reveal className="row">
          <div className="col col--8 col--offset-2 text--center">
            <Heading as="h2" className={styles.sectionHeading}>
              Why Kairon
            </Heading>
            <p>
              KubeVirt-style stacks run each VM inside a virt-launcher Pod
              wrapping libvirt — a large Go/operator surface with scheduling
              extras bolted onto the Pod scheduler. Kairon takes a
              different, smaller path: a node agent talks directly to{' '}
              <Link to="https://github.com/zyvorai/fluxvm">FluxVM</Link>'s
              REST API, with Kairon's own least-loaded placement deciding
              where a <code>Machine</code> lands.
            </p>
            <p>
              Runtime code is Go standard library only — no client-go, no
              generated deep call stacks, no vendored operator framework.{' '}
              <code>kairon-controller</code> and <code>kairon-node</code>{' '}
              are each small enough to read in an afternoon. And when a
              live migration commit becomes genuinely ambiguous, Kairon
              names that state (<code>NeedsRecovery</code>) and waits for
              an operator's attested decision, rather than risking
              split-brain.
            </p>
          </div>
        </Reveal>
      </div>
    </section>
  );
}

function MacSection() {
  return (
    <section className={styles.macs}>
      <div className="container">
        <Reveal>
          <div className="text--center">
            <Heading as="h2" className={styles.sectionHeading}>
              Every Mac a Node: Mac mini to Mac Studio cluster
            </Heading>
            <p className={styles.enterpriseCopy}>
              <code>kairon-node</code> registers an Apple silicon Mac as a
              Kubernetes Node with allocatable unified memory, and Kairon
              schedules Machines onto it through FluxVM's <code>vz</code>{' '}
              backend. Next to Velora's private LLM endpoints, a few Macs
              become a quiet, low-power, on-premise cluster.
            </p>
          </div>
          <img
            className={styles.macImg}
            src={useBaseUrl('/img/macos/readme-macs.jpg')}
            alt="Mac mini for home, Mac Studio for a team, MacBook Pro for development"
            loading="lazy"
          />
          <div className={styles.macGrid}>
            <div className={styles.macCard}>
              <Heading as="h3">Home, low cost</Heading>
              <p>A Mac mini as one Node: a VM or two beside a private chat endpoint.</p>
            </div>
            <div className={styles.macCard}>
              <Heading as="h3">Team</Heading>
              <p>A Mac Studio labelled <code>kairon.zyvor.dev/mlx=true</code> with its inference URL.</p>
            </div>
            <div className={styles.macCard}>
              <Heading as="h3">On-premise cluster</Heading>
              <p>Two to four Mac Studios in one pool, Machines placed by unified memory.</p>
            </div>
          </div>
          <img
            className={styles.macImg}
            src={useBaseUrl('/img/macos/readme-home-cluster.jpg')}
            alt="A private LLM cluster made of Mac Studios joined by Thunderbolt 5"
            loading="lazy"
          />
          <p className="text--center">
            Verified on an Apple M4 with macOS 27.2: the Mac registers as a
            Ready Node and a <code>vz</code> Machine boots, gets an address and
            answers SSH. Multi-Mac clusters are not yet verified.{' '}
            <Link to="/docs/macos-cluster">Read the Mac guide →</Link>
          </p>
        </Reveal>
      </div>
    </section>
  );
}

function TrustBand() {
  return (
    <section className={styles.trust}>
      <div className="container">
        <Reveal className={styles.trustGrid}>
          <div>
            <Heading as="h3" className={styles.sectionHeading}>
              Open, and honest about its limits
            </Heading>
            <p>
              Apache-2.0. <strong>v0.9.0</strong> ships admission webhooks,
              dashboard SSO, MachineSet, pause/halt, snapshot schedules, a
              native macOS node, the developer ecosystem kit, and more — cold relocation, snapshots, DRA bridging, the secure
              live control plane, and a real FluxVM migration adapter are
              tested. Real two-host live migration has not yet been
              exercised against real hardware in this repo&apos;s own CI.
              Production gaps stay listed, not glossed over.
            </p>
            <Link to="/docs/STATUS">Read status &amp; production gaps →</Link>
          </div>
          <div className={styles.trustBadges}>
            <img
              src="https://github.com/zyvorai/kairon/actions/workflows/ci.yml/badge.svg"
              alt="CI status"
            />
            <img
              src="https://img.shields.io/github/license/zyvorai/kairon"
              alt="Apache 2.0 license"
            />
            <img
              src="https://img.shields.io/badge/version-v0.9.0-blue"
              alt="v0.9.0"
            />
          </div>
        </Reveal>
      </div>
    </section>
  );
}

function EnterpriseCTA() {
  return (
    <section className={styles.enterprise}>
      <div className="container text--center">
        <Reveal>
          <Heading as="h2" className={styles.sectionHeading}>
            Need production support or SLAs?
          </Heading>
          <p className={styles.enterpriseCopy}>
            Kairon&apos;s core is Apache-2.0 and free to run in personal, lab,
            and commercial production use at no charge. Zyvor Enterprise
            adds production support, SLAs, and additional products for
            teams that need them.
          </p>
          <Link
            className="button button--primary button--lg"
            href="mailto:sales@zyvor.dev">
            Contact sales@zyvor.dev
          </Link>
        </Reveal>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="Real VMs on Kubernetes — without KubeVirt"
      description="Kairon is Zyvor's Apache-2.0 control plane for running virtual machines on Kubernetes through FluxVM — no virt-launcher Pod, no libvirt.">
      <HomepageHeader />
      <main>
        <ProblemStatement />
        <Reveal>
          <FeatureHighlights />
        </Reveal>
        <MacSection />
        <TrustBand />
        <EnterpriseCTA />
      </main>
    </Layout>
  );
}
