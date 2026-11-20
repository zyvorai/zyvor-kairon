# User guide: guest agent (real guest IP reporting)

How to opt a Machine into FluxVM's real `qemu-guest-agent` channel, and why
you'd want to.

## The problem this solves

`status.guestIP` normally comes from parsing a dnsmasq DHCP lease file --
but that only exists for `spec.network.mode: tap` with `netns: true`. Every
other network mode (in particular `user`, SLIRP -- Kairon's default) has no
lease file at all, so `status.guestIP` just stays empty forever, even
though the guest genuinely has a real, working IP address.

## How to enable it

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web
spec:
  guestAgent:
    enabled: true
  cloudInit:
    packages: ["qemu-guest-agent"]   # the guest has to actually run it
  # ... unchanged otherwise
```

Two things have to both be true: `spec.guestAgent.enabled: true` (tells
FluxVM to add the virtio-serial channel to the VM) and the guest actually
running `qemu-guest-agent` (most cloud images don't ship it by default --
`spec.cloudInit.packages` is the easiest way to get it installed and
started at first boot).

Once both are true, `kairon-node` asks the real guest agent for its network
interfaces (`guest-network-get-interfaces`) whenever it doesn't already have
a `status.guestIP` from a DHCP lease -- the guest's own kernel-reported
address, not a host-side guess.

## How Kairon picks an address

`status.guestIP` stays a single string for backward compatibility (it's
what `kubectl get machine`'s IP column and every existing consumer already
expect), but `status.guestIPs` (and `status.network.guestIPs`) now report
every address the guest agent sees, across every non-loopback interface --
IPv4 addresses first, then IPv6, in interface order. `status.guestIP` is
`status.guestIPs[0]` -- the first IPv4 address found anywhere, falling
back to the first IPv6 address only if the guest has no IPv4 address at
all -- except that the interface matching the Machine's own primary
virtio-net MAC is preferred when it reports an address (so an SR-IOV
passthrough NIC can't displace it). If your guest has more than one NIC, `status.guestIP` alone still
can't tell you which one it picked -- read `status.guestIPs` (or
`status.network.guestIPs`) for the full picture.

## Real limits today (v1 of this feature)

- Re-resolved periodically, not forever-sticky: once an address is first
  resolved, Kairon re-verifies with the guest agent every 5 minutes (not
  every reconcile tick) and updates `status.guestIP`/`status.guestIPs` if
  it changed. Before the first successful resolution, it's retried every
  reconcile tick instead (the guest agent may simply not have booted yet).
  A `kairon-node` restart resets the 5-minute clock and triggers one
  immediate re-check, which is expected, not a bug.
- A guest agent that isn't installed or hasn't started yet just means
  `status.guestIP`/`status.guestIPs` stay empty and resolution is retried
  on the next reconcile tick -- not a hard error.
- This is unrelated to FluxVM's own bespoke vsock-based agent protocol
  (requiring its own FluxVM-specific guest binary,
  `fluxvm-guest-agent`) -- exposed as the separate, independent
  `spec.guestAgent.console` flag, powering an interactive text console
  ([guide](machine-text-console.md)). `spec.guestAgent.enabled` here is
  specifically the real, upstream `qemu-guest-agent` over virtio-serial.
  This same `enabled` flag also gates two other, separately
  documented capabilities built on that same channel: application-
  consistent `MachineSnapshot` quiesce
  ([guide](machine-snapshot-quiesce.md)) and running a one-shot command
  inside the guest ([guide](machine-guest-exec.md)).
