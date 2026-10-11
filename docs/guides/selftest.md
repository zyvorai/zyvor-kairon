---
title: Self-test
---

# Self-test: is it all working?

`kaironctl selftest` answers the question every install ends with. It runs the
control-plane checks of `kaironctl doctor`, boots a tiny **CirrOS** VM, and
exercises power, pause/resume, networking, CPU hotplug and observability against
it, printing a timestamped, coloured log. Every Machine it creates is deleted
afterwards, including when you press Ctrl-C.

```bash
kaironctl selftest --profile quick      # about a minute
kaironctl selftest                      # standard profile
kaironctl selftest --list               # every check and what it needs
```

## Profiles

| Profile | Adds | Typical time |
|---|---|---|
| `quick` | control plane, boot, serial console, stop/start | ~1 min |
| `standard` (default) | forwarded-port network, pause/resume, vCPU hotplug, usage and conditions | ~3 min |
| `full` | NIC hotplug (`--bridge`), disk hotplug (`--pvc`), fork, cold migration | ~8 min |

A check whose prerequisite is missing is **skipped with the reason and the flag
that enables it**, never dropped silently. `--strict` makes a skip a failure.

## What each check proves

| Check | Proves | Skipped when |
|---|---|---|
| `control-plane` | API server, capable nodes, CRDs, controller/node/dashboard health | never |
| `boot` | the scheduler places the VM, the node pulls and verifies the image, and the guest boots far enough to lease an IP over DHCP or print `login:` | never |
| `console-log` | the serial console relay works and the guest did not crash | no dashboard API (`--ui-url`, `--ui-token`) |
| `power` | stop then start | after a failed `boot` |
| `network` | the guest answers on a forwarded port (SSH banner) | the node address is unreachable from where you run it, or the guest's SSH server is not up yet (CirrOS starts it only after cloud-init stops waiting for a metadata service) |
| `pause-resume` | `Paused` then `Running` | after a failed `boot` |
| `hotplug-cpu` | a running VM gains a vCPU (`appliedVCPUs`) | after a failed `boot` |
| `observability` | `status.resourceUsage`, the `Ready` condition, events, dashboard samples | after a failed `boot` |
| `nic-hotplug` | a tap-networked VM gets a live NIC | `--bridge` not given |
| `disk-hotplug` | a PVC attaches live | `--pvc` not given |
| `fork` | a `flux-vm` VM forks into a running child | the backend is not accepted or does not start |
| `cold-migration` | a VM moves to a second node | fewer than two capable Ready nodes |

`boot` does not depend on `control-plane`: a doctor warning, or a 403 for the
identity you run as, never hides whether a VM really boots. A 403 is reported as
"not checked" rather than a failure.

## Why not wait for `login:`?

CirrOS prints its login prompt only after cloud-init has stopped looking for a
metadata service, which can take many minutes when none is configured. The DHCP
lease it logs earlier already proves the kernel booted, the guest's init ran and
its NIC works, so the self-test accepts either.

## The image

kairon-node downloads CirrOS 0.6.3 from `download.cirros-cloud.net` and verifies a
pinned sha256 for the node's architecture (amd64 or arm64, detected from a
capable node). Nothing is booted if the digest does not match.

| Need | Flag |
|---|---|
| An air-gapped cluster | `--image /path/on/the/node.qcow2` |
| A mirror | `--image-url URL --image-digest sha256:...` |
| A specific architecture | `--arch amd64` or `--arch arm64` |

## Serial console and usage samples

Reading the guest's serial console and the dashboard's usage history goes
through kairon-ui. Set `KAIRON_UI_URL` and `KAIRON_UI_TOKEN` (or `--ui-url`,
`--ui-token`). Without them the run still proves the VM is `Running`; it just
cannot confirm the login prompt.

## When something fails

The run ends with a result table and an **Evidence** block: controller and node
log lines that mention the test Machines, the guest's last console lines, and the
Machine's events. `--log-file run.log` keeps a plain-text copy. Leftovers from a
killed run are removed with:

```bash
kaironctl selftest --cleanup
```

Common results:

- `no image cache configured`: kairon-node needs a directory to download the
  image into. Helm: `--set node.imageCacheDir=/var/lib/fluxvm/images/cache`.
  systemd hosts: `deploy-remote.sh HOST USER --image-cache-dir=/var/lib/kairon-node/image-cache`.
  Or skip the download with `--image PATH` for a file already on the node.

- `no node carries kairon.zyvor.dev/capable=true`: label a node
  (`kubectl label node NAME kairon.zyvor.dev/capable=true`). The self-test stops
  waiting after 45 seconds in this case.
- Boot times out: read the scheduler's message in the failure reason, then
  `kaironctl events -n default --warnings` and `kaironctl logs node`.

## Output and automation

| Flag | Effect |
|---|---|
| `-o json` | a machine-readable report instead of the log |
| `--no-vm` | control-plane checks only; creates nothing (for CI against a cluster with no FluxVM) |
| `--keep` | leave the test Machines for debugging |
| `--color never`, `NO_COLOR=1` | plain text with the same layout |

Exit status is 0 when nothing failed and 1 otherwise.

## From the deploy script

`scripts/deploy-remote.sh HOST USER --with-ui --self-test[=quick|standard|full]`
runs the self-test on the host over an SSH terminal after the deploy, using the
host's admin kubeconfig (k3s) or the node's own connection, and the dashboard
when it was deployed. The result becomes the script's exit code.

## From the dashboard

**Operations › Self-test** runs the same checks server-side and streams the log
into a terminal panel. It needs a named administrator account (it creates and
deletes real Machines), runs one test at a time, and can be cancelled; the
Machines are still cleaned up. The test Machines go in the `default` namespace.
`GET/POST/DELETE /api/v1/selftest` is documented in the
[kairon-ui API reference](kairon-ui-api.md).
