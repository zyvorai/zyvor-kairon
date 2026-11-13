# User guide: guest exec (run a command inside a Machine)

How to run a one-shot command inside a Machine's guest -- no SSH key, no
console, no network path into the guest required -- and what it can't do.

## What this is

`kairon-ui`'s dashboard gets an **Exec** button for an eligible, `Running`
Machine: type a command (or, for Windows guests, a PowerShell one-liner),
run it, and see its exit code, stdout, and stderr once it finishes. Under
the hood: `browser -> kairon-ui -> kairon-node -> FluxVM's real
qemu-guest-agent guest-exec` -- the exact same virtio-serial channel
[`spec.guestAgent`](machine-guest-agent.md) already uses for guest-IP
resolution and [`MachineSnapshot` quiesce](machine-snapshot-quiesce.md).
There's no new spec field: any Machine with `spec.guestAgent.enabled: true`
(and a guest actually running `qemu-guest-agent`) is eligible.

This is genuinely synchronous, not a fire-and-forget job: FluxVM itself
polls QEMU's own `guest-exec`/`guest-exec-status` commands internally and
only answers once the command has finished (or its own timeout elapses) --
Kairon never sees, and doesn't need to implement, that polling loop itself.

## Who can use it

Running arbitrary code inside a guest is a meaningfully bigger capability
than viewing its console, so this is gated more strictly than the VNC
console:

- **The deployment needs the console relay configured** (`console.enabled`
  -- exec rides the exact same `kairon-ui` &harr; `kairon-node` relay
  infrastructure, see [SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md)'s "VNC console"
  section for that trust chain).
- **The calling operator must be an admin identity** -- a `ui.auth.users[].admin`
  account, or an OIDC session in `ui.oidc.adminGroups` (opt-in, empty by
  default; see [`kairon-ui-oidc.md`](kairon-ui-oidc.md)). Unlike the
  console (any authenticated operator by default), there is no per-Machine
  allowlist opt-out for exec today -- see SECURITY.md's "Guest exec"
  section for exactly why, and its real limits.
- The target Machine must be `Running`, have `spec.guestAgent.enabled:
  true`, and actually be running `qemu-guest-agent` in the guest.

## Using it

In the dashboard, click **Exec** on an eligible Machine, choose **Command**
(a real argv -- `path` plus space-separated `args`, no shell involved, so
there's no shell-injection surface from what you type) or **PowerShell**
(a convenience for Windows guests: `powershell.exe -Command <your text>`),
and click **Run**. The result panel shows the exit code and any
stdout/stderr once the command completes.

## Related, smaller API-only endpoints

Two more `qemu-guest-agent`-backed calls ride the exact same authorization
model as exec (admin account, `spec.guestAgent.enabled`, `Running`) but
have no dedicated dashboard button yet -- reachable via the REST API only:

- **`GET /api/v1/machines/{ns}/{name}/qga/fsfreeze-status`** -- a
  read-only check of what qemu-guest-agent itself currently reports for
  the guest's filesystem freeze state, independent of Kairon's own
  quiesce-request/-status annotations
  ([guide](machine-snapshot-quiesce.md)). Useful for confirming directly
  whether a guest is actually frozen, rather than inferring it from
  `MachineSnapshot`'s own phase.
- **`POST /api/v1/machines/{ns}/{name}/qga/firewall/open`** /
  **`.../qga/firewall/close`** (body: `{"name", "port", "protocol"}` /
  `{"name"}`) -- toggles a named firewall rule inside the guest. FluxVM
  implements both as a guest-side command run over the same channel
  `exec` uses, so the result has the same shape (`exitCode`/`stdout`/
  `stderr`).

## Real limits today (first cut)

- **One-shot, not interactive.** There's no shell session, no stdin, no
  streaming output as it happens -- you get a complete result only once the
  command has finished. For an interactive shell, see
  [`machine-text-console.md`](machine-text-console.md) -- a separate
  opt-in (`spec.guestAgent.console`) with its own, bigger guest-image
  dependency (FluxVM's proprietary `fluxvm-guest-agent`, not the standard
  `qemu-guest-agent` this feature uses). That same `spec.guestAgent.console`
  opt-in also backs [guest file access](machine-guest-agent-files.md) --
  reading or writing a file inside the guest directly, rather than via
  `--command "cat ..."`/shell redirection through exec.
- **60-second default timeout** (FluxVM's own), overridable per call with
  `timeoutSeconds` (kairon-node's relay itself gives up after 5 minutes, so
  anything longer fails regardless) -- a command that runs
  longer than its timeout returns an error, with no way to reconnect to it
  or see partial output afterward.
- **No finer-grained permission than "admin."** There's no way today to
  let a non-admin operator exec into Machines they otherwise have full
  console/manage access to -- a real, documented gap, not an oversight.
- **No command-line audit trail.** kairon-ui logs who ran something, on
  which Machine, and when -- not the command or its arguments, since those
  can carry secrets. If your compliance posture needs a full transcript,
  this doesn't provide one yet.
- **Effectively QEMU-only**, same as the rest of `spec.guestAgent` --
  Cloud Hypervisor/Firecracker Machines have no working `qemu-guest-agent`
  channel today.
