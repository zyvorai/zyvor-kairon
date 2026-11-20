# Kairon developer ecosystem

This kit turns Kairon's existing VM, pool, claim and fleet APIs into reusable
interfaces for developers, CI systems and integration partners. It adds SDKs,
a catalog compiler, a GitHub Action, Terraform composition, orphan cleanup and
qualification tooling without adding controller/node runtime dependencies.

## What is implemented

| Component | Path | Behaviour |
|---|---|---|
| Python SDK | `sdk/python` | Kubernetes CRD CRUD, power/readiness/fork, pools/claims/fleet resources; optional UI guest exec/files/capabilities/usage |
| TypeScript SDK | `sdk/typescript` | Typed CRD client, abortable readiness, scoped cleanup; optional UI guest exec/capabilities/usage |
| Recipe catalog | `ecosystem/catalog.py` | Render reviewed recipes as Machine, MachinePool or MachineTemplateVersion JSON with a pinned image |
| CI runner | `ecosystem/ci.py` | Create a unique VM, wait for readiness, execute JSON argv, request UID-scoped deletion |
| GitHub Action | `.github/actions/kairon-machine` | Composite action using the same tested CI runner |
| Orphan sweeper | `ecosystem/cleanup.py` | Dry-run or UID-scoped deletion of explicitly marked, expired temporary VMs |
| Terraform module | `ecosystem/terraform` | Manage rendered Machine manifests using the upstream Kubernetes provider |
| Partner contract | `ecosystem/extensions.schema.json` | Validate extension metadata and local lifecycle reports; does not load/execute extensions |
| Qualification runner | `ecosystem/compatibility.py` | Opt-in create/ready/stop/restart/delete checks; optional guest execution |

This is the first ecosystem implementation. A hosted marketplace, proprietary
Terraform provider, managed billing service, enterprise support programme and
cross-product adapters are not implemented here. Existing fleet features remain
experimental and must be enabled/configured separately.

## 1. Render a template

Supply a real reviewed Ubuntu 24.04 cloud image with cloud-init and a preinstalled,
working qemu-guest-agent. The examples deliberately do not invent image URLs or
digests. Catalog recipes install packages from your configured Ubuntu mirrors;
they are deployment recipes, not reproducible image builds or certified images.

```bash
python ecosystem/catalog.py --list
python ecosystem/catalog.py python-dev --name workspace --namespace development \
  --image "$APPROVED_DISK_URL" --digest "$APPROVED_DISK_DIGEST" > machine.json
kubectl apply --dry-run=server -f machine.json
```

`--image` accepts an HTTPS disk URL or `oci://registry/repository` containing a
containerDisk disk payload, not an ordinary application container. For HTTP,
`--digest` is the downloaded disk's SHA-256; for OCI it is the manifest/index
SHA-256. Add `--format raw|qcow2|ova|vmdk|vhd|vhdx` as appropriate. Imported
OVA/VMDK/VHD/VHDX sources request Kairon's existing repair pipeline.

Recipes: `python-dev`, `ci-linux`, `postgres-dev`. PostgreSQL is a disposable
development database, not a production persistent database. User networking is
used for these initial QEMU recipes; it does not provide the eBPF tenant isolation
of a properly configured tap network. Use operator-authored tap/eBPF templates
and default-deny policies for untrusted multi-tenant workloads.

Run `cloud-init status --wait` inside a VM before using recipe-installed packages.
Machine `Ready` means Kairon's VM lifecycle is ready, not that all application
initialization or guest agents have finished. CI prepared images should already
contain the executable and working QGA required by the command.

## 2. Use SDKs and warm pools

Install/build clients following their SDK READMEs. Namespaced operations use your
own Kubernetes bearer token and RBAC. UI guest operations require an independent
UI token and the existing admin/console authorization gates. Use HTTPS and CA
files/trust-store configuration for production.

```python
from kairon import API_VERSION
claim = client.claims.create({
    "apiVersion": API_VERSION, "kind": "MachineClaim",
    "metadata": {"name": "agent-job"},
    "spec": {"poolName": "python-warm", "ttlSeconds": 900,
             "reclaimPolicy": "Delete"},
})
# Poll claim.status.phase == "Bound"; then wait on claim.status.machineName.
# Release the claim by UID; its existing controller reclaims the Machine.
client.claims.delete(claim["metadata"]["name"], uid=claim["metadata"]["uid"])
```

Render `--kind MachinePool --replicas 2` to define a warm pool. Pool members are
not assigned a short runtime TTL. Put the lease TTL on the MachineClaim so
Kairon's controller reclaims the leased VM. Render
`--kind MachineTemplateVersion --name python-dev-v1` for the experimental fleet
catalog. It requires a pinned image source and sets `maxTTLSeconds` from `--ttl`.
Generic SDK fleet resource calls use `fleet.kairon.zyvor.dev/v1alpha1` directly;
they do not bypass Kubernetes approvals, identity checks or feature flags.

The full Machine manifest API also allows prebuilt FluxVM sandbox configurations
(`spec.sandbox` plus `spec.guestAgent.console`) and guest-agent execution through
`UIClient.agent_exec` / `agentExec`; this kit does not build FluxVM sandbox images.
See Kairon's existing sandbox guide for those deployment prerequisites.

## 3. Run CI inside a VM

Provision a dedicated namespace with quotas/admission enabled and a credential
allowed to get/list/create/delete Machines there. Supply endpoints and tokens via
environment variables, never the manifest or action inputs:

- `KAIRON_KUBE_URL`, `KAIRON_KUBE_TOKEN`, optional `KAIRON_KUBE_CA_FILE`
- `KAIRON_UI_URL`, `KAIRON_UI_TOKEN`, optional `KAIRON_UI_CA_FILE`

```bash
python ecosystem/ci.py --manifest machine.json --namespace development \
  --command '["/usr/bin/python3","--version"]'
```

A GitHub Actions job, after checking out the source:

```yaml
- uses: zyvorai/zyvor-kairon/.github/actions/kairon-machine@YOUR_REVIEWED_COMMIT_SHA
  with:
    manifest: machine.json
    namespace: development
    command: '["/usr/bin/python3","--version"]'
  env:
    KAIRON_KUBE_URL: ${{ secrets.KAIRON_KUBE_URL }}
    KAIRON_KUBE_TOKEN: ${{ secrets.KAIRON_KUBE_TOKEN }}
    KAIRON_UI_URL: ${{ secrets.KAIRON_UI_URL }}
    KAIRON_UI_TOKEN: ${{ secrets.KAIRON_UI_TOKEN }}
```

The runner generates a unique name, marks temporary objects with an expiry,
returns failure for a nonzero guest exit code, and requests UID-scoped deletion
on success, command errors, readiness errors and SIGTERM. Finalizer completion
is asynchronous. It cannot clean up after SIGKILL, host loss or a create response
that never returns; schedule the sweeper in that same namespace:

```bash
python ecosystem/cleanup.py --namespace development          # dry-run
python ecosystem/cleanup.py --namespace development --delete # delete expired temporary objects
```

The sweeper requires the explicit temporary label, a valid timezone-aware expiry
annotation and a UID. It leaves ordinary VMs and deleting objects alone. The
runtime TTL is not a guarantee that the Kubernetes object disappears; orphan
cleanup uses the expiry annotation. A guest command timeout can leave that command
running until VM cleanup finishes. Do not run privileged CI against untrusted
pull-request code with production credentials.

## 4. Terraform and partner qualification

In a Terraform root with an authenticated Kubernetes provider:

```hcl
module "workspace" {
  source   = "./ecosystem/terraform"
  manifest = jsondecode(file("machine.json"))
}
```

Kairon CRDs and the namespace must exist before planning. This is composition
using `hashicorp/kubernetes`, not a new provider. Run Terraform's normal init,
validate, plan and apply checks in the target environment.

Qualification creates a real VM and requests power changes/deletion; it requires
an explicit `--run` flag and target credentials:

```bash
python ecosystem/compatibility.py --run --manifest machine.json \
  --namespace development --guest-exec --report compatibility.result.json
python -m pip install -r ecosystem/requirements-test.txt
python ecosystem/validate_extension.py ecosystem/examples/ci-extension.json
```

Reports distinguish passed cases and `not-run` guest execution. Qualification
scope is only one VM's lifecycle. Migration, storage durability, confidentiality,
network isolation and large-scale behaviour require separate hardware testing.
Extension metadata starts `unqualified`. Changing it to `tested` requires a local
passing report with create/ready/stop/restart/delete evidence. This validates the
report's structure and recorded cases; it does not cryptographically authenticate
reports or award a Zyvor certification. Partner maintainers must publish reviewed
version, hardware and dependency evidence before customer-facing certification.

## Validation

```bash
python -m pip install -r ecosystem/requirements-test.txt
python -m unittest discover -s sdk/python/tests -v
python -m unittest discover -s ecosystem/tests -v
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run typecheck
npm --prefix sdk/typescript test
```

SDK transport tests use a local HTTP fixture; catalog tests validate all outputs
against this repository's actual CRD schemas. They do not claim real KVM or
multi-host hardware results. `.github/workflows/ecosystem.yml` runs these checks
without cluster credentials on pushes and pull requests.
