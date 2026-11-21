# Kairon vs KubeVirt: what you wait for when you press "create"

You press create. Then you wait. This page is about that wait, and about the
part of the system that sits idle the rest of the time.

We put Kairon and KubeVirt v1.9.0 on the same node, one after the other, and
asked the same question of both: how long until I can SSH into a new VM, and
what does the control plane cost me while nothing is happening?

Measured on 2026-10-04. Same node, same Ubuntu 24.04 image, same guest
(1 vCPU, 512 MiB), same readiness probe (the guest's SSH banner), one script
driving both, run back to back.

## The short version

| | Kairon | KubeVirt v1.9.0 | Difference |
|---|---:|---:|---:|
| Idle control plane memory | **63 MiB** | 905 MiB | 14x lighter |
| Idle control plane CPU | **3.5 m** | 30.2 m | about 9x lighter |
| 1 VM, create to SSH | **23.7 s** | 67.6 s | 2.9x faster |
| 5 VMs at once, create to SSH (p50) | **24.8 s** | 184.7 s | 7.4x faster |
| 10 VMs at once, reached SSH within 600 s | **10 of 10** | 0 of 10 | |

## Chapter 1: the quiet cost

Before a single VM exists, KubeVirt's control plane holds 905 MiB of memory and
30.2 millicores of CPU. Kairon's holds 63 MiB and 3.5 millicores. Nothing is
running. That is the price of having the platform switched on.

On a small edge node or a lab host, that gap is the difference between a
control plane that is a rounding error and one you have to budget for.

## Chapter 2: one VM, one wait

Create one VM and count the seconds until SSH answers. Kairon: 23.7 seconds.
KubeVirt: 67.6 seconds.

The guest is identical. What differs is the path to it. A pod-per-VM design
schedules a pod, pulls an image and starts a launcher container before the
guest boots. Kairon has no pod per VM: the engine starts the VM directly, so
there is less in front of the boot you are waiting for.

## Chapter 3: five at once

Ask for five VMs together. The median VM on Kairon reaches SSH in 24.8 seconds,
almost the same as one VM alone. On KubeVirt the median is 184.7 seconds.

The interesting part is the shape, not the ratio. Kairon barely notices the
extra four. KubeVirt's wait grows with the crowd.

## Chapter 4: ten at once

Ten VMs, a 600-second window. Kairon: all ten reached SSH. KubeVirt: none of
the ten did within the window.

This is the result to read carefully (see the next section). It says the
pod-per-VM path is the first thing to give under pressure on this host. It does
not say KubeVirt can never run ten VMs.

## What this does not say

- **Per-VM memory is the same.** Both run QEMU, so a running guest costs about
  the same either way. The win is the control plane and the start path, which
  is the part you wait on.
- **The ten-VM run was on a shared lab host.** Treat it as "the pod-per-VM path
  breaks first under pressure", not as a density ceiling for either system.
- **One node, one guest size, one image.** Other sizes, images and clusters
  will give other numbers. Run your own before you plan capacity.
- **Raw output is not in this repository yet.** The figures above are the
  published results of that run; the raw JSON and the driver script have not
  been added here, so this page cannot yet be reproduced from the repo alone.

## Where these numbers are used

Veyron runs VMs on the Kairon engine, and its product page quotes this
comparison: <https://zyvor.dev/veyron>.
