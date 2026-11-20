# User guide: guest file access (read/write a file inside a running guest)

A real, runtime, bidirectional file copy into or out of a `Running`
guest -- no SSH key, no shared volume, no reboot. Complements
[guest exec](machine-guest-exec.md) (run a command) and
[`spec.cloudInit.writeFiles`](machine-network.md) (drop files in at
*boot* time, one-way only): this reads or writes a file at any point
while the Machine is already running.

## What this needs

Unlike guest exec (standard `qemu-guest-agent`, `spec.guestAgent.enabled`),
guest file access rides FluxVM's own proprietary vsock guest agent --
the exact same `fluxvm-guest-agent` binary/systemd-service dependency the
[interactive text console](machine-text-console.md) needs. If you've
already enabled `spec.guestAgent.console` for the text console, file
access works with no further guest-image changes.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  guestAgent:
    console: true   # required -- FluxVM's proprietary vsock agent, not qemu-guest-agent
  # ... unchanged otherwise
```

## Who can use it

- The console relay must be configured (`console.enabled`) -- the same
  deployment-level gate the text console and guest exec both share.
- The calling operator must be an admin identity -- a
  `ui.auth.users[].admin` account, or an OIDC session in
  `ui.oidc.adminGroups` (opt-in, empty by default; see
  [`kairon-ui-oidc.md`](kairon-ui-oidc.md)) -- stricter than the text
  console's any-authenticated-operator default, the same posture guest
  exec already has. See [SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md)'s "Guest file
  access" section for why.
- The target Machine must be `Running` with `spec.guestAgent.console: true`.

## Using it

Click **Files** on an eligible Machine in the dashboard. Choose **Read
file** (enter a path, get its content back) or **Write file** (enter a
path and content, optionally an octal mode like `644`). The dashboard's
own panel handles text content only -- it encodes/decodes as UTF-8
client-side, so a genuinely binary file only round-trips correctly if you
work with its base64 payload directly against the API
(`POST .../agent-file/put` / `.../agent-file/get`) rather than through the
text-only dashboard panel.

## Real limits today (first cut)

- **No path restriction.** An admin who can use this can read or write
  any path the guest agent process itself can reach inside the guest --
  there is no allowlist/denylist of sensitive files. See SECURITY.md.
- **~3MB effective file-size ceiling on reads.** Responses are capped by
  `internal/fluxvm.Client`'s own 4MiB HTTP response limit; since content
  travels base64-encoded (~4/3 expansion), a file larger than roughly 3MB
  fails to decode with a clear error rather than returning truncated
  content. A write is now capped too, symmetrically: `kairon-ui`'s own
  request body decoder (`decodeJSON`, `internal/uiapi/server.go`) rejects
  any JSON body over 1MiB by default, and this route in particular
  (`internal/uiapi/agentfile.go`) raises that to 6MiB, sized to comfortably
  fit the same ~3MB-of-real-content ceiling once base64-encoded. Before
  this, a write's `contentBase64` had no size cap enforced on Kairon's own
  side at all -- every other JSON-accepting route in `kairon-ui` had the
  same gap, not just this one.
- **Text-only in the dashboard UI**, as above -- the API itself is
  binary-safe (it's just base64 either way), only the dashboard's own
  panel assumes UTF-8 text.
- **One-shot, not a mount.** There's no live, ongoing sync between guest
  and host -- each read or write is a single, independent round trip.

## Related: guest exec over the same vsock agent (API-only)

**`POST /api/v1/machines/{ns}/{name}/agent-exec`** (body:
`{"command": "...", "timeoutSeconds": 30}`) runs a shell command over this
exact same vsock channel -- same `spec.guestAgent.console` requirement,
same admin-only posture, same `internal/consoleproxy` relay shape as file
access above. This is a genuinely **different** mechanism from
[guest exec](machine-guest-exec.md)'s `qemu-guest-agent`-based
`POST .../exec`, despite the similar name: this one is **backend-agnostic**
(works on Cloud Hypervisor/Firecracker, and FluxVm-backend sandboxes, not
just QEMU) since it doesn't depend on QEMU's own virtio-serial guest-agent
device at all. Response shape: `{"exitCode", "stdout", "stderr"}`, the
same as `.../exec`. Takes a single shell command string, not a real argv
(no PowerShell mode -- that's specific to `qemu-guest-agent`'s own
Windows-guest support). No dedicated dashboard button yet -- API-only,
matching how `qga/fsfreeze-status`/`qga/firewall` first shipped.
