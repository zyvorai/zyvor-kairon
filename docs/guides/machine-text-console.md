# User guide: interactive text console (`spec.guestAgent.console`)

A real interactive shell inside the guest, rendered in-browser -- Kairon's
equivalent of `virtctl console`. Distinct from the graphical
[VNC console](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md) (a display, not a shell),
[guest exec](machine-guest-exec.md) (one-shot commands, no interactive
session), and [guest file access](machine-guest-agent-files.md) (read/write
a file, no shell) -- the latter shares this feature's own
`spec.guestAgent.console` dependency.

## What this needs that nothing else in Kairon does

Every other guest-agent feature in this project (guest-IP resolution,
[`MachineSnapshot` quiesce](machine-snapshot-quiesce.md),
[guest exec](machine-guest-exec.md)) rides the standard, widely-packaged
`qemu-guest-agent` (`spec.guestAgent.enabled`). The text console is
different: it needs FluxVM's own **proprietary** in-guest agent,
`fluxvm-guest-agent`, compiled from the FluxVM repo and installed as a
systemd service inside the guest image -- something no distro ships by
default, and a real new operational step. See FluxVM's own
`docs/build-image-tutorials.md` for exactly how an image build bakes it
in (copy the compiled binary in, enable its systemd unit). If your image
doesn't have this installed and running, the console dial fails with a
clear FluxVM error, not a silent hang.

## How to enable it

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  guestAgent:
    console: true   # independent of `enabled` above -- set either, both, or neither
  # ... unchanged otherwise
```

`spec.guestAgent.console` and `spec.guestAgent.enabled` are independent:
one opts into FluxVM's proprietary vsock agent (this feature), the other
into standard `qemu-guest-agent` (guest-IP/quiesce/exec). A Machine can set
either, both, or neither.

You also need the console relay itself configured
(`console.enabled`/`KAIRON_NODE_CONSOLE_TOKEN`) -- the same deployment-level
gate the VNC console and guest exec already share. See
[SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md)'s "Text console" section for the full
trust chain and authorization model (same as VNC's: any authenticated
operator by default, restrictable via
`kairon.zyvor.dev/console-allowed-users`).

## Using it

Once both are true and the Machine is `Running`, click **Text console** in
the dashboard. Unlike VNC, this works on **every backend** -- FluxVM's
vsock agent isn't tied to a display device the way QEMU's VNC server is.

## Real limits today (first cut)

- **Requires the proprietary `fluxvm-guest-agent` binary** -- see above.
  This is the single biggest practical barrier to using this feature: it's
  not something most existing cloud images already have, unlike
  `qemu-guest-agent`.
- **No resize-after-connect.** The terminal's cols/rows are sent once, at
  connection time, from the browser's own initial xterm.js dimensions --
  resizing the browser window afterward doesn't propagate a new size to
  the guest's shell.
- **No session persistence or reconnect.** Closing the tab/losing network
  ends the shell session; there's no `tmux`-style "reattach to where you
  left off."
- **No command-line auditing beyond open/close.** kairon-ui logs when a
  text console session opens and closes (username, Machine, duration),
  the same as VNC -- not a transcript of what was typed or seen inside it.
- **The vsock agent's own token is FluxVM's to manage, not Kairon's** --
  Kairon never generates, stores, or displays it; FluxVM auto-generates
  one per VM and injects it into the guest's own disk before boot.
