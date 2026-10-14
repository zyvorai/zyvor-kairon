// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import {useState} from 'react';
import type {ReactNode} from 'react';
import clsx from 'clsx';
import Layout from '@theme/Layout';
import {
  Actions,
  Band,
  Btn,
  CopyCommand,
  CtaBand,
  Frame,
  Grad,
  Headline,
  List,
  Pill,
  Steps,
  SubNav,
  TextLink,
} from '@site/src/components/zv';

type Path = {
  id: string;
  tab: string;
  title: string;
  lede: ReactNode;
  commands: string[];
  needs: ReactNode[];
  notes?: ReactNode;
  doc: {to: string; label: string};
  badge?: ReactNode;
};

const PATHS: Path[] = [
  {
    id: 'helm',
    tab: 'Helm (Kubernetes)',
    title: 'One Helm command from GHCR',
    lede: 'Charts and images are published to GitHub Container Registry on every release. kaironctl embeds the same chart, so no helm binary is needed either.',
    commands: [
      `# 1. mark the nodes that can run VMs
kubectl label node worker-1 kairon.zyvor.dev/capable=true

# 2. install the production profile
helm upgrade --install kairon oci://ghcr.io/zyvorai/charts/kairon \\
  --version 0.10.0 -n kairon-system --create-namespace \\
  -f https://raw.githubusercontent.com/zyvorai/kairon/v0.10.0/charts/kairon/values-production.yaml`,
      `# or, without Helm
kaironctl install --profile production`,
      `# 3. check it, then prove it
kaironctl doctor
kaironctl selftest --profile quick`,
    ],
    needs: [
      'A Kubernetes cluster and kubectl.',
      'KVM and a reachable FluxVM service on each VM node.',
      'The CSI snapshot stack if you want MachineSnapshot.',
    ],
    notes: 'Drop the -f overlay for the evaluation profile. Helm applies CRDs only on first install; apply them from the same tag before upgrading.',
    doc: {to: '/docs/deploy/helm', label: 'Helm deployment guide'},
  },
  {
    id: 'ssh',
    tab: 'One command to a host',
    title: 'One command to a bare-metal host',
    lede: 'Copy a small source tarball to the host, compile there, install systemd services, and boot a CirrOS VM to prove it. Your machine builds nothing.',
    commands: [
      `./scripts/deploy-remote.sh user@host --remote-build \\
  --with-controller --with-ui --with-console --self-test=quick`,
      `# dry run: print the plan, no SSH, no build, no writes
./scripts/deploy-remote.sh user@host --remote-build --dry-run`,
    ],
    needs: [
      'SSH access to a Linux host with FluxVM, and a reachable Kubernetes API (k3s works).',
      'On the host: curl, tar and network access to go.dev and the Go module proxy. Downloads are checksum-verified.',
      'Your machine needs ssh and tar. No Go, no Node, no build cache.',
    ],
    notes: 'The dashboard and the VNC console relay are HTTPS by default. A self-signed certificate is generated unless you bring your own.',
    doc: {to: '/docs/deploy/bare-metal', label: 'Bare-metal deployment guide'},
    badge: <Pill ok>Used on the lab host</Pill>,
  },
  {
    id: 'kind',
    tab: 'Laptop with kind',
    title: 'Evaluate the control plane on a laptop',
    lede: 'Install the CRDs and the chart into a throwaway kind cluster and check that everything applies and the control plane reports healthy.',
    commands: [
      `kind create cluster --name kairon
kubectl apply --server-side -f deploy/crd.yaml
helm install kairon charts/kairon -n kairon-system --create-namespace --skip-crds`,
      `# control-plane checks only; creates no VM
kaironctl selftest --no-vm --profile quick`,
    ],
    needs: [
      'Docker, kind, kubectl and helm, and a checkout of the repository.',
    ],
    notes: 'kind has no KVM and no FluxVM, so no VM boots there. Use it to learn the objects and the CLI; boot real VMs on a KVM host.',
    doc: {to: '/docs/deploy/kind', label: 'kind evaluation guide'},
  },
  {
    id: 'mac',
    tab: 'Apple silicon Mac',
    title: 'Turn a Mac into a node',
    lede: 'kairon-node registers an Apple silicon Mac as a Kubernetes Node and runs backend: vz Machines through FluxVM.',
    commands: [
      `# prerequisites: a Kubernetes API, FluxVM with the vz backend, Go, Rust, Xcode CLT
KUBECONFIG=/path/to/kubeconfig scripts/macos-e2e.sh /path/to/arm64-debian.raw`,
    ],
    needs: [
      'An Apple silicon Mac, a Kubernetes API it can reach, and FluxVM built with the vz backend.',
      'Linux-only features (eBPF edge, migration, CPU pinning, VFIO) stay Linux-only.',
    ],
    notes: 'Verified on an Apple M4 against a real k3s cluster. Multi-Mac clusters and macOS guests are not verified.',
    doc: {to: '/docs/macos', label: 'Kairon on macOS'},
    badge: <Pill>Verified on one M4</Pill>,
  },
  {
    id: 'airgap',
    tab: 'Air-gapped',
    title: 'No internet on the hosts',
    lede: 'Point Kairon at images and a CirrOS test image you host yourself, and mirror the container images to your registry.',
    commands: [
      `# host the test image yourself, pinned by digest
kaironctl selftest --image-url https://mirror.internal/cirros.qcow2 \\
  --image-digest sha256:<digest>

# or use a file already on the node
kaironctl selftest --image /var/lib/fluxvm/images/cirros.qcow2`,
      `# mirror the images, then point the chart at them
helm upgrade --install kairon charts/kairon -n kairon-system \\
  --set controller.image.repository=registry.internal/kairon-controller \\
  --set node.image.repository=registry.internal/kairon-node`,
      `# keep downloaded images in a directory you control
./scripts/deploy-remote.sh user@host --image-cache-dir=/var/lib/kairon-node/image-cache`,
    ],
    needs: [
      'A registry and a file server reachable from the cluster.',
      'The Go toolchain cannot be downloaded on the host, so build elsewhere and omit --remote-build.',
    ],
    doc: {to: '/docs/deploy/air-gapped', label: 'Air-gapped guide'},
  },
];

function PathPanel({p}: {p: Path}) {
  return (
    <div className="zv-panel" role="tabpanel" id={`panel-${p.id}`} aria-labelledby={`tab-${p.id}`}>
      <div>
        <h3>
          {p.title} {p.badge}
        </h3>
        <p className="zv-lede" style={{margin: '0 0 1rem'}}>
          {p.lede}
        </p>
        <h4>You need</h4>
        <List items={p.needs} />
        {p.notes && <p className="zv-note">{p.notes}</p>}
        <p>
          <TextLink to={p.doc.to}>{p.doc.label}</TextLink>
        </p>
      </div>
      <div>
        {p.commands.map((c) => (
          <CopyCommand key={c}>{c}</CopyCommand>
        ))}
      </div>
    </div>
  );
}

function Chooser() {
  const [active, setActive] = useState(PATHS[0].id);
  const current = PATHS.find((p) => p.id === active) ?? PATHS[0];
  return (
    <Band tone="light" id="paths" center>
      <Headline eyebrow="Choose your path" title="Five ways in. One way to check." />
      <div className="zv-tabs" role="tablist" aria-label="Deployment paths">
        {PATHS.map((p) => (
          <button
            key={p.id}
            id={`tab-${p.id}`}
            role="tab"
            type="button"
            className={clsx('zv-tab')}
            aria-selected={p.id === active}
            aria-controls={`panel-${p.id}`}
            onClick={() => setActive(p.id)}>
            {p.tab}
          </button>
        ))}
      </div>
      <PathPanel p={current} />
    </Band>
  );
}

function Verify() {
  return (
    <Band tone="black" id="verify" glow center>
      <Headline
        eyebrow="Then prove it"
        title={
          <>
            Don&apos;t trust it. <Grad>Boot a VM.</Grad>
          </>
        }
        lede="kaironctl selftest deploys a tiny CirrOS VM, exercises the platform against it, prints live coloured logs and ends with a PASS, FAIL or SKIP table. A skip always says which flag would enable it."
      />
      <Steps
        steps={[
          {title: 'quick', body: 'Control plane, boot, serial console, power. About a minute.'},
          {title: 'standard', body: 'Adds the forwarded-port network, pause and resume, vCPU hotplug and usage samples.'},
          {title: 'full', body: 'Adds NIC and disk hotplug, snapshot-restore, fork and cold migration where the prerequisites exist.'},
        ]}
      />
      <Actions>
        <Btn to="/docs/guides/selftest">How to read the result</Btn>
        <Btn to="/docs/getting-started" variant="ghost">
          Getting started
        </Btn>
      </Actions>
    </Band>
  );
}

function Flow() {
  return (
    <Band tone="soft" center>
      <Headline eyebrow="What happens" title="Source in, running VM out." />
      <Frame
        mermaid={`flowchart LR
  you["Your machine<br/>ssh + tar"] -->|"source tarball"| host
  subgraph host["Target host: Linux + k3s + FluxVM"]
    build["Go + Node toolchain<br/>cached, checksum-verified"] --> bins["kairon-node, controller,<br/>kairon-ui, kaironctl"]
    bins --> units["systemd units + HTTPS certs"]
    units --> st["kaironctl selftest<br/>boots CirrOS"]
  end
  st --> res["PASS / FAIL / SKIP<br/>script exit code"]
  units --> dash["HTTPS dashboard<br/>Self-test page"]`}
        caption="deploy-remote.sh --remote-build --self-test"
      />
    </Band>
  );
}

function Day2() {
  return (
    <Band tone="light">
      <Headline eyebrow="After day one" title="Upgrade, roll back, diagnose." />
      <div className="zv-two">
        <List
          items={[
            'kaironctl upgrade keeps your values; --check compares with the latest release first.',
            'kaironctl history and rollback list and restore Helm revisions.',
            'kaironctl doctor explains every problem with a hint.',
            'kaironctl sysdump and diagnose machine --deep collect redacted bundles for support.',
          ]}
        />
        <div>
          <p>
            Everything is HTTPS by default: the dashboard serves TLS and the console relay is pinned to a server name.
            Plain HTTP is only ever a named opt-out (<code>ui.insecureHttp</code>, <code>--console-insecure-http</code>).
          </p>
          <p>
            <TextLink to="/docs/CLI">CLI reference</TextLink> · <TextLink to="https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md">Security</TextLink>
          </p>
        </div>
      </div>
    </Band>
  );
}

export default function DeployPage(): ReactNode {
  return (
    <Layout
      title="Deploy Kairon"
      description="Deploy Kairon with one Helm command, one SSH command that builds on the host, kind, an Apple silicon Mac or an air-gapped cluster, then prove it with a built-in self-test.">
      <SubNav
        title="Deploy"
        links={[
          {to: '#paths', label: 'Paths'},
          {to: '#verify', label: 'Verify'},
          {to: '/docs/getting-started', label: 'Getting started'},
        ]}
      />
      <main>
        <Band tone="black" glow center>
          <Headline
            size="display"
            eyebrow="Deploy"
            title={
              <>
                Up and running <Grad>in minutes.</Grad>
              </>
            }
            lede="Pick the path that matches what you have. Every path ends with the same check: a VM boots, and you see it."
          />
          <Actions>
            <Btn to="#paths">Choose a path</Btn>
            <Btn to="#verify" variant="ghost">
              How we verify
            </Btn>
          </Actions>
        </Band>
        <Chooser />
        <Flow />
        <Verify />
        <Day2 />
        <CtaBand title="Stuck? We have a runbook for that." lede="Troubleshooting, migration failures and recovery drills are all written down.">
          <Btn to="/docs/getting-started">Getting started</Btn>
          <Btn to="/docs/runbook-migration-failures" variant="ghost">
            Runbooks
          </Btn>
        </CtaBand>
      </main>
    </Layout>
  );
}
