# User guide: Machine logs (`kubectl logs` equivalent)

View a Machine's captured serial console output, with a live-tailing
**Follow** toggle -- Kairon's `kubectl logs`/`kubectl logs -f` equivalent.

## What this is

Click **Logs** on an eligible Machine in the dashboard: the last 200
lines load immediately, and the **Follow** checkbox (on by default)
keeps streaming new output as it arrives, auto-scrolling to the bottom.

Under the hood: `browser -> kairon-ui -> kairon-node
(internal/consoleproxy) -> FluxVM's real GET /v1/vms/{id}/logs` -- a
plain chunked `text/plain` HTTP stream, not a WebSocket. FluxVM captures
this from the VM's own serial console device, the same output you'd see
attaching a real serial terminal to the VMM process; it has no
`journalctl`-style structure (no per-line priority/unit), just raw text.

## Who can use it

Unlike guest exec/file access/the QGA diagnostics (all admin-only),
viewing logs uses the same authorization model as the VNC/text console --
any authenticated operator by default, restrictable per-Machine via the
`kairon.zyvor.dev/console-allowed-users` annotation. Reading a VM's own
console output is closer in sensitivity to viewing its display than to
running arbitrary code or reading/writing arbitrary guest files.

- **The console relay must be configured** (`console.enabled`) -- the
  same deployment-level gate the console/exec/text-console/file-access
  features all share.
- **The Machine needs a runtime, but doesn't need to be `Running`
  specifically** -- a `Paused` Machine's log file is still there on the
  node (unlike exec/console, which do require `Running`).

## Real limits today (first cut)

- **Unstructured text, not journald-style entries.** No per-line
  timestamp, priority, or source Kairon adds -- exactly what FluxVM
  itself captured from the guest's serial device.
- **No log rotation/retention policy from Kairon's side.** How much
  history is available, and for how long, is entirely FluxVM's own
  capture file's concern, not something Kairon configures or bounds.
- **`Follow` has no reconnect-on-drop.** If the stream disconnects
  (network blip, kairon-node restart), toggle `Follow` off and back on
  to reconnect -- there's no automatic retry loop yet.
- **No download/export button.** Copy from the panel directly if you
  need the text elsewhere; there's no "save as file" affordance yet.
