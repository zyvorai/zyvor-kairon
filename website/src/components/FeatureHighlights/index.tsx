// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type FeatureItem = {
  title: string;
  description: ReactNode;
  to: string;
};

const FeatureList: FeatureItem[] = [
  {
    title: 'Machine CRD, full lifecycle',
    description:
      'MachineSet, instance types, PVC/CSI boot, image import, DRA→VFIO, hotplug, pause/halt, sandboxes — one CRD family, not a virt-launcher Pod.',
    to: '/docs/WHAT_SHIPS',
  },
  {
    title: 'Real placement, not a stub',
    description:
      'Ready, capable-labeled nodes; least-loaded with deterministic tie-break; hard affinity and topologySpreadConstraints.',
    to: '/docs/guides/machine-placement',
  },
  {
    title: 'Secure live migration handshake',
    description:
      'TLS 1.3 mTLS prepare → transfer → commit, no user-supplied tcp: URIs, and NeedsRecovery when a commit is genuinely ambiguous.',
    to: '/docs/guides/relocating-a-machine',
  },
  {
    title: 'Network Fabric',
    description:
      'Rich spec.network, MachineNetworkPolicy, NetworkSecurityGroup, and Service Fabric VIP membership through to the FluxVM eBPF edge.',
    to: '/docs/network-fabric',
  },
  {
    title: 'kaironctl & dashboard',
    description:
      'Full CLI (and kubectl kairon plugin) plus optional kairon-ui with SSO, VNC, and the NeedsRecovery recovery workflow.',
    to: '/docs/CLI',
  },
  {
    title: 'Deploy in one command',
    description:
      'deploy-remote.sh --remote-build compiles on the host, so a laptop needs no Go or Node. Add --self-test and a CirrOS VM proves it all works.',
    to: '/docs/getting-started#fastest-path',
  },
  {
    title: 'Self-test, HTTPS by default',
    description:
      'kaironctl selftest and a dashboard page boot a tiny VM and check power, network, hotplug and snapshots. TLS is on for the dashboard and console.',
    to: '/docs/guides/selftest',
  },
  {
    title: 'Hibernate, elastic memory, vnets',
    description:
      'Experimental: save a running VM to disk, overcommit memory with a balloon, and route isolated IPv4 networks in eBPF with a Geneve overlay.',
    to: '/docs/guides/machine-hibernate',
  },
  {
    title: 'Go standard library only',
    description:
      'No client-go, no generated deep call stacks — kairon-controller and kairon-node stay small enough to audit. Opt-in OIDC/CSI are the only exceptions.',
    to: 'https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md',
  },
];

function Feature({title, description, to}: FeatureItem) {
  return (
    <div className="col col--4">
      <Link to={to} className={styles.card}>
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </Link>
    </div>
  );
}

export default function FeatureHighlights(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
