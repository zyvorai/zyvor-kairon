# Kairon Python SDK

A synchronous, dependency-free Python 3.11+ client for Kairon's namespaced
Kubernetes CRDs and optional guest-operation API. Package name: `zyvor-kairon`;
import name: `kairon`. This source release is not published to PyPI.

```bash
python -m pip install ./sdk/python
```

```python
import json, os
from kairon import Client, UIClient

client = Client(os.environ["KAIRON_KUBE_URL"], os.environ["KAIRON_KUBE_TOKEN"],
                namespace="development", ca_file=os.environ.get("KAIRON_KUBE_CA_FILE"))
ui = UIClient(os.environ["KAIRON_UI_URL"], os.environ["KAIRON_UI_TOKEN"],
              namespace="development", ca_file=os.environ.get("KAIRON_UI_CA_FILE"))
manifest = json.load(open("machine.json"))
with client.temporary_machine(manifest, timeout=180) as machine:
    result = ui.exec(machine["metadata"]["name"], ["/usr/bin/python3", "--version"])
    if result["exitCode"]:
        raise RuntimeError("guest command failed")
```

`machines.create/get/list/patch/power/wait/delete/fork`, `migrations.migrate/progress` (including `strategy: adaptive`, experimental), `pools`, `claims`,
and `resource(name, fleet=True)` use Kubernetes credentials directly. Readiness
requires `Running`, `Ready=True`, and an observed generation at least as new as
the object's current generation. Set `ready=False` when waiting for `Stopped`.
Delete requires `uid=` and uses Kubernetes `DeleteOptions.preconditions.uid`.
A context manager requests deletion on success, user exceptions and readiness
failure. It does not wait for finalizers to complete.

`UIClient.exec` takes an argv array for qemu-guest-agent. `agent_exec` takes a
shell command string for FluxVM's guest agent, and `put_file` uses that same
agent. `capabilities(node)` and `usage_csv()` are read operations. UI authorization,
admin requirements, namespace restrictions and console allowlists remain
server-enforced. A Kubernetes service-account token is **not** a UI token.

Transport verifies TLS (custom CA supported), refuses credential-bearing URLs,
rejects remote HTTP unless explicitly allowed, never follows redirects, bounds
responses to 8 MiB, and never retries writes. Error strings exclude response
bodies and tokens. A network exception after a POST may represent a successful
create with a lost response: inspect the named object instead of blindly retrying
or deleting by name. The context manager cannot clean up an object whose UID was
never returned. For CI, use the expiry sweeper described in the ecosystem guide.

`backups.create(machine=..., mode=..., parent=..., policy=..., target=...)` makes
an S3 MachineBackup (incremental, experimental) and `backups.chain(machine)`
returns a Machine's chains, newest first; `backup_policies` is the
`MachineBackupPolicy` resource. See `docs/guides/machine-backup-incremental.md`.

```python
client.backups.create(machine="web", policy="web-hourly", mode="incremental")
for chain in client.backups.chain("web"):
    print(chain["id"], [p["name"] for p in chain["points"]])
```

The SDK does not load kubeconfig, refresh OIDC tokens, provide watch streams, or
expose cluster-scoped `MachineImage` resources. Supply current credentials and
explicit endpoints. Generic CRD operations still pass admission and caller RBAC;
they do not grant access or turn on experimental fleet features.

```bash
python -m unittest discover -s sdk/python/tests -v
```
