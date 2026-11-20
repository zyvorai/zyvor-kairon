# User guide: Windows guests

What's actually possible today, and what's genuinely blocked upstream in
FluxVM -- read this before assuming Kairon can't run Windows at all, or
that it can run *any* Windows edition.

## What works today: legacy-BIOS Windows with a pre-built image

The fast path, exactly like a Linux Machine, is to boot a **pre-built
qcow2/raw image** (see [`machine-storage.md`](machine-storage.md)/
[`machine-image-import.md`](machine-image-import.md)); an interactive
install from an ISO is also possible via `spec.cdroms` (see "Real limits
today" below). For a pre-built Windows image, it needs two things baked in
*before* it ever reaches Kairon:

1. **VirtIO drivers already installed** (disk and network) -- Windows has
   no built-in virtio support, and a pre-built image has no install step
   left to inject them in. The standard, well-supported way to get this is to install Windows once
   (on any hypervisor, or using the official
   [`virtio-win`](https://github.com/virtio-win/virtio-win-pkg-scripts)
   driver ISO during that one-time setup), then export the resulting disk
   as your golden qcow2/raw image.
2. **[cloudbase-init](https://cloudbase-init.readthedocs.io/) installed**
   -- the Windows-native equivalent of cloud-init. It consumes the exact
   same NoCloud datasource format FluxVM's own cloud-init support already
   generates (a labeled ISO/vfat volume with `meta-data`/`user-data`
   files), so **`spec.cloudInit` works completely unchanged** for a
   Windows guest with cloudbase-init pre-installed -- no Kairon-side code
   exists or is needed specifically for Windows here.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: win-app-1
spec:
  image:
    path: /var/lib/fluxvm/images/windows-server-2022-cloudbase-init.qcow2
  resources: {cpu: "4", memory: "8Gi"}
  runtime: {backend: qemu}
  cloudInit:
    hostname: win-app-1
    runCmd: ["powershell -Command \"...\""]
  powerState: Running
```

This covers Windows Server (2019/2022) and Windows 10 -- any edition that
doesn't *require* UEFI Secure Boot + TPM 2.0 to boot at all.

## Windows 11: Secure Boot and TPM 2.0

`spec.security.secureBoot`/`spec.security.tpm` now pass through to FluxVM,
which as of its own `ff145b4`/`e2bd218` implements real UEFI Secure Boot
(OVMF pflash, QEMU-backend-only) and a real emulated TPM 2.0 device
(`swtpm`, QEMU or Cloud Hypervisor). Kairon enforces the same backend
restrictions FluxVM's own scheduler enforces server-side, so an
unsupported combination is refused at Kairon with a clear error instead of
a bare HTTP failure relayed from FluxVM:

```yaml
spec:
  runtime: {backend: qemu}
  security: {secureBoot: true, tpm: true}
```

- **`secureBoot` is QEMU-only, permanently.** Cloud Hypervisor's own
  `--firmware` is a single opaque file with no documented separate
  variable store to enroll Secure Boot keys into and no documented
  enforcement mechanism -- claiming support there would be dishonest, not
  just unimplemented, so FluxVM itself rejects it and Kairon refuses it
  first.
- **`tpm` works on QEMU or Cloud Hypervisor** -- both dial a real
  `swtpm`-backed Unix socket. Firecracker and the in-tree `flux-vm`
  sandbox backend have no vTPM device at all.
- **A real Secure Boot chain also needs the FluxVM *node* configured with
  an OVMF vars template** (`Config.qemu_ovmf_vars_template` -- a vars
  store with Microsoft's UEFI CA keys already enrolled) and, unless the
  node also sets `Config.qemu_ovmf_code` as a default, a firmware path
  Kairon has no `Machine`-spec field for yet. This is deliberately a
  FluxVM node-level operator responsibility, not something Kairon
  synthesizes or manages -- see FluxVM's own `docs/secure-boot-tpm.md`.
  Setting `secureBoot: true` against a node that hasn't configured this
  fails closed with FluxVM's own clear error at VM-create time, not a
  silent no-op.
- **Not live-verified end-to-end through Kairon itself.** FluxVM's own
  `e2bd218` verified real `qemu-system-x86_64`/`cloud-hypervisor`/`swtpm`
  argv construction and a real OVMF boot on a remote host directly against
  FluxVM's API -- but no `kairon-node` build has yet made a real
  `POST /v1/vms` call with `secure_boot`/`tpm` set against a live FluxVM
  instance in this repo's own CI. The Go-side request mapping (this
  file's own `buildCreateRequest`) has full unit coverage; the resulting
  real boot has not been separately reconfirmed from the Kairon side.

**Windows 11 hard-requires both** to boot at all -- with a node configured
for Secure Boot per the above, it's now reachable through Kairon rather
than refused outright.

## Real limits today (first cut)

- Secure Boot/vTPM need a FluxVM node-level OVMF vars template configured
  by the operator -- see above. No `Machine`-spec field to override the
  firmware/vars path per-Machine yet, only the node-wide default.
- Interactive Windows Setup from an ISO, with the VirtIO driver ISO as a
  second CD-ROM, works through `spec.cdroms` + `spec.image.blank` -- see
  [`machine-install-media.md`](machine-install-media.md). A pre-built image
  is still the fast path for fleets.
- `spec.guestAgent` (real `qemu-guest-agent`-reported `status.guestIP`,
  see [`machine-guest-agent.md`](machine-guest-agent.md)) needs the
  Windows `qemu-guest-agent` service installed in your image too --
  Kairon's own side is guest-OS-agnostic, but hasn't been verified
  end-to-end against a real Windows guest in this repo's own CI.
- The graphical VNC console (`console.enabled`,
  [SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md)) is QEMU-backend-only and
  guest-OS-agnostic -- it should work against a Windows guest exactly
  like a Linux one, but likewise hasn't been specifically verified here.
