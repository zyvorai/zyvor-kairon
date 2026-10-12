// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import type {UseCaseData} from '@site/src/components/zv/UseCase';

export const USE_CASES: Record<string, UseCaseData> = {
  sandboxes: {
    slug: 'sandboxes',
    eyebrow: 'CI and AI agents',
    title: 'A fresh VM, the moment you ask.',
    seoTitle: 'Sandboxes for CI and AI agents',
    description: 'Warm pools, claims, fork and TTLs give CI jobs and AI agents disposable virtual machines on Kubernetes in a single reconcile tick.',
    lede: 'Warm pools keep booted Machines ready. A claim binds one in a single reconcile tick, with its own egress allowlist and a time to live.',
    problem: [
      'A cold VM boot is too slow for a job that lives for seconds.',
      'Containers share a kernel; agents that run untrusted code want a real VM boundary.',
      'Every extra pod per VM is overhead you pay thousands of times a day.',
    ],
    how: [
      {title: 'Keep a pool warm', body: 'A MachinePool keeps spec.replicas Machines booted and unclaimed.'},
      {title: 'Claim one', body: 'A MachineClaim is a label change on a Running Machine; the pool boots a replacement.'},
      {title: 'Fork or expire', body: 'Fork a running Machine through FluxVM, or let spec.ttlSeconds expire it.'},
      {title: 'Gate the agent', body: 'The MCP server gates writes behind approval and an audit log.'},
    ],
    commands: [
      `kubectl apply -f examples/machinepool.yaml      # pool "agents"
kaironctl claim agents --label team=ml --ttl 1h`,
      `kaironctl fork demo --count 3`,
      `kaironctl mcp serve --allow-write              # for an MCP client; writes need approval`,
    ],
    mermaid: `flowchart LR
  pool["MachinePool agents<br/>replicas: 5, booted"] -->|"claim, one reconcile tick"| m["Machine bound to job-42"]
  m -->|"ttl expires"| gone["deleted"]
  pool -->|"boots a replacement"| pool
  m -->|"fork"| f["forked children"]`,
    diagramCaption: 'A claim takes over a booted Machine; the pool refills itself.',
    verified: [
      'Warm pool claims bind in one controller tick; the docs show a 412 ms example.',
      'FluxVM warm-pool claims measured at 24 ms in the FluxVM benchmarks.',
      'Fork and sandboxes go through FluxVM’s real endpoints.',
    ],
    limits: [
      'A claim carries the template’s resources and settings; anything not carried is listed in machineWarnings.',
      'On the single-node lab host, FluxVM lacks the fork endpoint, so the self-test skips fork there.',
    ],
    docs: [
      {to: '/docs/guides/machine-pools', label: 'Pools and claims'},
      {to: '/docs/guides/machine-sandboxes', label: 'Sandboxes'},
      {to: '/docs/guides/machine-fork', label: 'Fork'},
      {to: '/docs/guides/agent-plane', label: 'Agent plane'},
    ],
    deploy: {to: '/deploy', label: 'Deploy a sandbox host'},
  },

  'replace-kubevirt': {
    slug: 'replace-kubevirt',
    eyebrow: 'Platform teams',
    title: 'Keep Kubernetes. Drop the wrapper.',
    seoTitle: 'Replace KubeVirt, keep Kubernetes',
    description: 'Kairon runs real VMs on Kubernetes with zero pods per VM and a standard-library-only control plane, measured against KubeVirt on the same node.',
    lede: 'One Machine resource, no virt-launcher pod, no libvirt in the hot path. Measured on the same node with the same guest.',
    problem: [
      'Each VM drags a virt-launcher pod, libvirt and a QEMU wrapper behind it.',
      'The control plane is a large client-go and operator surface to patch and read at 2 a.m.',
      'Idle control-plane memory and time to a reachable guest both cost real money.',
    ],
    how: [
      {title: 'A Machine is a CRD', body: 'Its own lifecycle, not a Pod in disguise.'},
      {title: 'Straight to KVM', body: 'kairon-node talks to FluxVM’s REST API, which talks to KVM.'},
      {title: 'Placement on real capacity', body: 'Capacity-aware scheduling on allocatable, with NUMA and topology.'},
      {title: 'Small enough to read', body: 'Controller and node are Go standard library only.'},
    ],
    commands: [
      `kubectl apply -f examples/linux-machine.yaml
kubectl get machines -A -w`,
      `kaironctl create demo --image /var/lib/fluxvm/images/ubuntu-24.04.qcow2 --cpu 2 --memory 2Gi --backend qemu`,
    ],
    mermaid: `flowchart LR
  subgraph kubevirt["KubeVirt"]
    a["virt-handler"] --> b["virt-launcher pod"] --> c["libvirt"] --> d["QEMU"]
  end
  subgraph kairon["Kairon"]
    e["kairon-node"] --> f["FluxVM REST"] --> g["KVM"]
  end`,
    diagramCaption: 'The path from the API to KVM, with a layer removed at each step.',
    verified: [
      'Idle control plane: 63 MiB against 905 MiB (KubeVirt v1.9.0).',
      'Five VMs to SSH, median: 24.8 s against 184.7 s.',
      'Memory per VM is the same: same QEMU, same guest.',
    ],
    limits: [
      'Choose KubeVirt when you need its years of ecosystem integrations today (OpenShift Virtualization, Harvester).',
      'Two-host live migration has not run on real hardware in this repo’s CI.',
    ],
    docs: [
      {to: '/compare', label: 'Compare in detail'},
      {to: '/docs/benchmarks/kairon-vs-kubevirt', label: 'Benchmark method'},
      {to: '/docs/architecture', label: 'Architecture'},
    ],
    deploy: {to: '/deploy', label: 'Try it on your cluster'},
  },

  'vmware-migration': {
    slug: 'vmware-migration',
    eyebrow: 'Migration',
    title: 'Move VMs off VMware, one OVA at a time.',
    seoTitle: 'Migrate from VMware to Kubernetes',
    description: 'kaironctl import ova streams an OVA once, reads the OVF, and FluxVM converts and repairs the guest so it boots on virtio.',
    lede: 'Export a VM as an OVA, put it on any HTTP server the nodes can reach, and create a Machine with one command.',
    problem: [
      'A VMDK from vSphere does not boot on virtio without driver and fstab repair.',
      'Hand-converting disks and re-typing CPU and memory is slow and error-prone.',
      'You want the result to be a declarative Machine, not a one-off VM.',
    ],
    how: [
      {title: 'Export', body: 'Export the powered-off VM as an OVA (ovftool, govc or the UI) and publish it.'},
      {title: 'Import', body: 'kaironctl streams the OVA once, hashes it for spec.image.digest and reads vCPU, memory and firmware from the OVF.'},
      {title: 'Convert and repair', body: 'kairon-node downloads it; FluxVM converts the VMDK to raw and repairs the guest for virtio.'},
      {title: 'Run', body: 'The Machine starts and is managed like any other.'},
    ],
    commands: [
      `govc export.ovf -vm web01 ./exports
tar -C exports/web01 -cf web01.ova web01.ovf web01-disk1.vmdk`,
      `kaironctl import ova "$URL" --name web01 --network tap
kaironctl get machines`,
      `kaironctl import ova "$URL" --dry-run     # show the Machine without creating it`,
    ],
    mermaid: `flowchart LR
  vs["vSphere VM, powered off"] -->|"export"| ova["OVA on an HTTP server"]
  ova -->|"kaironctl import ova"| m["Machine<br/>image digest, vCPU, memory"]
  m --> n["kairon-node downloads"]
  n --> f["FluxVM: VMDK to raw,<br/>repair for virtio"]
  f --> r["Running Machine"]`,
    diagramCaption: 'One command from an exported OVA to a Machine.',
    verified: ['The import flow, OVF parsing and the repair step are documented in the migration guide with a checklist.'],
    limits: [
      'Windows and exotic guests need their own checks; follow the guide’s checklist.',
      'Export is from a powered-off VM; there is no live V2V.',
    ],
    docs: [
      {to: '/docs/guides/migrate-from-vmware', label: 'Migration guide'},
      {to: '/docs/guides/machine-image-import', label: 'Image import'},
    ],
    deploy: {to: '/deploy', label: 'Set up the target cluster'},
  },

  'private-ai': {
    slug: 'private-ai',
    eyebrow: 'Private AI',
    title: 'Your Macs and GPUs, one cluster.',
    seoTitle: 'Private AI on your own Macs and GPUs',
    description: 'Register Apple silicon Macs as Kubernetes nodes and place VMs by NUMA, GPU, NIC and NVMe locality on your own hardware.',
    lede: 'Run VMs on Apple silicon through FluxVM’s vz backend, and place GPU workloads next to the NIC and NVMe they need.',
    problem: [
      'Private model hosting wants hardware you already own, not another cloud bill.',
      'GPUs, NICs and NVMe sit on specific NUMA nodes; placing them separately costs performance.',
      'A few Macs are quiet, low power and sitting idle.',
    ],
    how: [
      {title: 'Every Mac a node', body: 'kairon-node registers an Apple silicon Mac and runs backend: vz Machines.'},
      {title: 'Topology-aware placement', body: 'The scheduler picks the node, exact CPUs, NUMA memory and PCI devices together.'},
      {title: 'GPU slices', body: 'vGPU and MIG profiles are scheduled against free instances and GPU memory.'},
      {title: 'One API', body: 'The same Machine, the same dashboard, the same CLI.'},
    ],
    commands: [
      `kaironctl get nodetopologies
kaironctl node topology worker-1`,
      `# on a Mac, against a Kubernetes API it can reach
KUBECONFIG=/path/to/kubeconfig scripts/macos-e2e.sh /path/to/arm64-debian.raw`,
    ],
    mermaid: `flowchart TD
  m["Machine<br/>spec.topology, spec.devices"] --> s["Scheduler<br/>NodeTopology + free GPU memory"]
  s --> l["Linux GPU node<br/>NUMA 0: GPU + NIC + CPUs"]
  s --> mac["Apple silicon Mac<br/>vz backend, NAT network"]`,
    diagramCaption: 'Placement considers locality on Linux hosts and registers Macs as ordinary nodes.',
    verified: [
      'Native macOS node: a vz Machine boots, gets an address and answers SSH, on an Apple M4 with a real k3s cluster.',
      'Topology and vGPU placement are documented with their real limits in the guides.',
    ],
    limits: [
      'Experimental: topology-aware placement and vGPU.',
      'Multi-Mac clusters, macOS guests and Intel Macs are not verified; Mac Machines get NAT networking only.',
      'The eBPF edge, migration, CPU pinning and VFIO remain Linux-only.',
    ],
    docs: [
      {to: '/docs/macos', label: 'Kairon on macOS'},
      {to: '/docs/macos-cluster', label: 'Mac clusters'},
      {to: '/docs/guides/machine-topology', label: 'Topology placement'},
      {to: '/docs/guides/gpu-vgpu', label: 'vGPU and MIG'},
    ],
    deploy: {to: '/deploy', label: 'Deploy to a Mac or GPU host'},
  },

  'multi-tenant': {
    slug: 'multi-tenant',
    eyebrow: 'Multi-tenant and regulated',
    title: 'Isolation you can show an auditor.',
    seoTitle: 'Multi-tenant and regulated environments',
    description: 'Tenant fences, an eBPF network edge, admission, audit-logged consoles and hash-chained agent audit give teams verifiable isolation.',
    lede: 'Policy is enforced at each VM’s tap by eBPF, tenants are fenced from each other, and every operator and agent write leaves a record.',
    problem: [
      'Pod network policy does not see a VM’s own traffic.',
      'Tenants in one namespace must not reach each other’s guests.',
      'Auditors ask who did what, and agents must not act unsupervised.',
    ],
    how: [
      {title: 'eBPF edge', body: 'Anti-spoofing, DNS and SNI allowlists, rate limits and drops with reasons on every VM tap.'},
      {title: 'Tenant fence', body: 'spec.tenant plus an annotation produces a deny group of other tenants’ addresses.'},
      {title: 'Gated consoles', body: 'The VNC console uses a single-use ticket bound to the operator, audit-logged, over TLS.'},
      {title: 'Audited agents', body: 'MCP writes need approval and are recorded in a hash-chained log.'},
    ],
    commands: [
      `kubectl annotate machine web kairon.zyvor.dev/tenant-fence=true
kubectl get networksecuritygroups`,
      `kaironctl network drop-reasons web
kaironctl network capture web --seconds 15 --output web.pcap`,
    ],
    mermaid: `flowchart LR
  a["Tenant A Machine"] -. "denied by fence" .-> b["Tenant B Machine"]
  a --> edge["eBPF edge on the tap<br/>anti-spoof, DNS/SNI, rate limit"]
  edge --> net["Network"]
  edge -. "drops with reasons" .-> obs["flows, drop reasons, pcap"]`,
    diagramCaption: 'Policy is applied where the VM attaches, not in the guest.',
    verified: [
      'Tenant fence verified on a live k3s host (2026-10-06): two tenants each got a group denying only the other’s addresses.',
      'The agent plane’s audit chain and approval flow are covered end to end with the official MCP SDK in CI.',
    ],
    limits: [
      'The console’s shared-token model suits a trusted operator team, not a hostile multi-tenant network; read SECURITY.md first.',
      'The eBPF flow, drop and stats relays need FluxVM’s tap dataplane with netns and eBPF mode.',
    ],
    docs: [
      {to: '/docs/guides/tenant-fence', label: 'Tenant fence'},
      {to: '/docs/network-fabric', label: 'Network fabric'},
      {to: '/docs/guides/network-policy', label: 'Network policy'},
      {to: 'https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md', label: 'Security model'},
    ],
    deploy: {to: '/deploy', label: 'Deploy the production profile'},
  },

  'edge-and-lab': {
    slug: 'edge-and-lab',
    eyebrow: 'Edge and lab',
    title: 'One box. Real VMs. Safe to leave on.',
    seoTitle: 'Edge, lab and home lab',
    description: 'Run Kairon on a single k3s node with hibernate, elastic memory and incremental S3 backups for edge sites and labs.',
    lede: 'A single host with k3s is enough. Save a VM to disk, overcommit memory safely and back up only the changed blocks.',
    problem: [
      'A lone host has no spare capacity for a heavy control plane.',
      'Memory is the first thing you run out of.',
      'Full backups of big disks every night are wasteful.',
    ],
    how: [
      {title: 'Small control plane', body: '63 MiB idle on the measured node.'},
      {title: 'Hibernate', body: 'Save RAM and device state, free the node, resume the same boot.'},
      {title: 'Elastic memory', body: 'Guaranteed minimums, a balloon reclaim loop and a pressure signal.'},
      {title: 'Incremental backups', body: 'Changed-block tracking uploads only what the guest wrote to S3.'},
    ],
    commands: [
      `./scripts/deploy-remote.sh user@host --remote-build --with-controller --with-ui --self-test=quick`,
      `kaironctl hibernate db
kaironctl resume db`,
      `kaironctl backuppolicy create nightly --selector app=db --incremental-every 1h \\
  --s3-endpoint https://s3.internal --s3-bucket kairon --s3-credentials s3-creds`,
    ],
    mermaid: `flowchart LR
  run["Running Machine"] -->|"powerState Hibernated"| hib["Hibernated<br/>node capacity freed"]
  hib -->|"powerState Running"| run
  run -->|"hourly incremental, changed blocks only"| s3[("S3 bucket")]`,
    diagramCaption: 'Free the node by hibernating; protect the data with changed-block backups.',
    verified: [
      'Same-node hibernate and resume have run on real hardware.',
      'One-command deploy and the self-test run on a single-node lab host.',
    ],
    limits: [
      'Experimental: hibernate, elastic memory and incremental S3 backups; resume on another node has not been exercised.',
      'Incremental backups need VMs that boot from spec.image on a QEMU 5.1+ node.',
    ],
    docs: [
      {to: '/docs/guides/machine-hibernate', label: 'Hibernate'},
      {to: '/docs/guides/machine-elastic-memory', label: 'Elastic memory'},
      {to: '/docs/guides/machine-backup-incremental', label: 'Incremental backups'},
    ],
    deploy: {to: '/deploy', label: 'One command to a host'},
  },
};

export const USE_CASE_LIST = Object.values(USE_CASES);
