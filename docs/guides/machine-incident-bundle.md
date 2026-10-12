# Machine incident bundles

`kaironctl diagnose machine NAME --deep` writes one tar.gz with what you need to
debug a Machine after the fact: its Kubernetes state, and from its node the
VMM's view (QMP), cgroup and pressure files, the serial console, the
dataplane, and optionally a packet capture and a guest memory dump.

```sh
export KAIRON_UI_URL=https://kairon-ui.example:27231 KAIRON_UI_TOKEN=...
kaironctl diagnose machine web                       # ranked causes only (JSON), no bundle
kaironctl diagnose machine web --deep -o web.tar.gz  # cluster + node bundle
kaironctl diagnose machine web --deep --pcap 10s --pcap-filter 'tcp port 443'
kaironctl diagnose machine db --deep --memory-dump   # also dump guest memory on the node
kaironctl diagnose memory-dump db 3f9c0a1b2c3d4e5f -o db.elf
```

## What is in the bundle

```text
manifest.json             kind KaironIncidentBundle, Machine, node, runtime id, what was skipped,
                          the node job (sections, sha256, memory dump path/size/sha256/pause)
README.txt                the layout, what was redacted, what was not collected
cluster/machine.json      the Machine as stored (redacted)
cluster/events.json       Events for the Machine
cluster/migrations.json   MachineMigrations of the Machine
cluster/diagnosis.json    the ranked causes ("kaironctl agent diagnose --no-ai")
cluster/node.json         the Node
cluster/nodetopology.json the NodeTopology
node/...                  FluxVM's bundle: manifest.json, vm/record.json, vm/metrics.json,
                          vm/pressure.json, vm/cgroup/*, logs/console.log, qmp/*.json,
                          network/{status,stats,flows,drops}.json, network/datapath.txt
                          (ip, tc, bpftool), network/capture.pcap, host/*.json
```

The node part is collected by FluxVM as an asynchronous job: kaironctl starts
it through kairon-ui, polls it, downloads the tar.gz, checks its sha256 against
the job and copies it under `node/`. A section FluxVM cannot read (no balloon
device, no eBPF dataplane) is listed in `node/manifest.json` with its error
instead of failing the bundle. A part kaironctl cannot get (no runtime yet,
`KAIRON_UI_URL` unset, node unreachable) is listed under `skipped` and in
`README.txt`; the cluster part is still written.

The node keeps the last 5 bundles per VM under FluxVM's `diagnostics_dir`
(default `<state_dir>/diagnostics`) and removes them when the VM is deleted.

## Memory dumps

`--memory-dump` has QEMU write the guest's memory as an ELF core
(`dump-guest-memory`). **The guest is paused while the dump is written** (about
1 s per GiB on local NVMe in the lab). The dump stays on the node, next to the
bundle; the manifest gives its path, size, sha256 and the pause, and kaironctl
prints the `kaironctl diagnose memory-dump` command that downloads it and checks
the sha256. Open it with `crash` or `gdb` and the guest's `vmlinux` debug
symbols.

FluxVM refuses a dump for non-QEMU runtimes, Machines that are not Running or
Paused, confidential VMs, guests larger than `diagnostics_max_memory_dump_mib`
(default 16384; 0 disables dumps) and when the directory lacks the guest's
memory plus 1 GiB of free space.

## Redaction

By default secrets are replaced with `redacted:sha256:<16 hex>` (the same value
gives the same fingerprint, so bundles can be compared):

- `cluster/`: values under credential-like keys and the Machine's
  `spec.cloudInit`, as in `kaironctl sysdump`.
- `node/`: JSON values under keys such as `password`, `token`, `secret`,
  `user_data`, `content` (cloud-init `write_files`); in every other JSON string
  and in text files, the value after `password=`, `token:`, `Bearer ` and
  similar is replaced with `[redacted]`.

Redaction is pattern-based. A secret the guest prints to its console without
such a key word is kept, and packet captures and memory dumps are never
redacted. Review a bundle before you share it. `--no-redact` keeps everything.

## Access

The kairon-ui diagnostics routes need an administrator account or the shared
`KAIRON_UI_TOKEN` (which is root-equivalent on every route); other accounts get
`403`. They go through the node console relay, so kairon-ui and kairon-node need
the same `KAIRON_NODE_CONSOLE_TOKEN`; without it the routes return `501`.
kairon-ui logs every start and memory dump download, and FluxVM audits starts,
finishes and deletes. Only one bundle runs per VM at a time (`409` otherwise). See
[kairon-ui-api.md](kairon-ui-api.md#incident-bundles).

## Limits

- `--pcap` needs the Machine on FluxVM's eBPF dataplane; otherwise the capture
  section records why there is none. At most 30 s.
- QMP state, cgroup files and memory dumps are QEMU only.
- There is no dashboard button yet.

Lab results: [`COMPATIBILITY.md`](../COMPATIBILITY.md#incident-bundles)
(`scripts/lab-incident-bundle.sh`).
