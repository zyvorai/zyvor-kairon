# Kairon TypeScript SDK

Dependency-free fetch client for Node 22+ or applications with compatible
`fetch`, `AbortSignal.any`, `structuredClone`, and streaming `Response` APIs.
This source release is not published to npm.

```bash
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run build
# In a consuming project:
npm install /absolute/path/to/zyvor-kairon/sdk/typescript
```

```typescript
import {Client, UIClient, type KaironObject} from '@zyvor/kairon';

const client = new Client(kubeURL, kubeToken, {namespace: 'development'});
const ui = new UIClient(uiURL, uiToken, {namespace: 'development'});
await client.withMachine(manifest as KaironObject, async machine => {
  const result = await ui.exec(machine.metadata.name, ['/usr/bin/python3', '--version']);
  if (result.exitCode !== 0) throw new Error('guest command failed');
}, {timeoutMs: 180000});
```

`machines.create/get/list/patch/power/wait/delete/fork`, `migrations.migrate/progress` (including `strategy: adaptive`, experimental), `pools`, `claims`, and
`resource(name, true)` support current namespaced core/fleet CRDs. `wait` checks
current-generation readiness, accepts an abort signal, and can wait for `Stopped`
with `{phase: 'Stopped', ready: false}`. `delete(name, uid)` always uses a UID
precondition. `withMachine` requests cleanup independently of an aborted wait
and preserves both application and cleanup failures via `AggregateError`.
It does not wait for finalizer completion.

`UIClient.exec` takes QGA argv; `agentExec` takes a FluxVM guest-agent command
string. Both preserve the UI's admin and Machine-access checks.
`capabilities(node)` and `usageCSV()` reuse current UI endpoints.
Kubernetes and UI credentials are separate.

`backups.create(machine, {mode, parent, policy, target})` makes an S3
MachineBackup (incremental, experimental) and `backups.chain(machine)` returns
a Machine's chains, newest first; `backupPolicies` is the
`MachineBackupPolicy` resource. See `docs/guides/machine-backup-incremental.md`.

```typescript
await client.backups.create('web', {policy: 'web-hourly', mode: 'incremental'});
for (const chain of await client.backups.chain('web')) console.log(chain.id, chain.points.map(p => p.name));
```

TLS uses the supplied fetch implementation's trust store. For private cluster
CAs, configure a trusted Node dispatcher/fetch implementation; the SDK never
disables TLS verification. HTTP is restricted to localhost/loopback unless
`allowInsecure` is explicitly set. Redirects are not followed. Responses are
bounded to 8 MiB. Writes are not retried. Inspect named objects after ambiguous
create failures; do not retry blindly. Credentials and response bodies are not
included in API error messages. Tokens are held by the client and must stay out
of untrusted browser code.

```bash
npm --prefix sdk/typescript run typecheck
npm --prefix sdk/typescript test
```
