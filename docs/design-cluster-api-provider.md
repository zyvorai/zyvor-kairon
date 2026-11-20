# Design (not implemented): a Cluster API infrastructure provider

**Status: scoped, not built.** This documents what a real Kairon Cluster
API (CAPI) provider -- "Kubernetes clusters made of Kairon Machines,"
managed the standard `clusterctl`/CAPI way -- would take, and why it's
being named rather than attempted as a quick addition. Read this before
assuming it's close to existing, or trivial to bolt on.

## Why this is a separate project, not a feature

Every other item in Kairon's own feature roadmap has been an incremental
addition *within* the existing `kairon-controller`/`kairon-node`
architecture: a new CRD, a new reconcile step, a new field. A real CAPI
infrastructure provider is different in kind, not just size:

1. **It needs its own CRDs matching CAPI's exact contract** --
   `KaironCluster`/`KaironMachine`/`KaironMachineTemplate` implementing
   the `InfrastructureCluster`/`InfrastructureMachine`/
   `InfrastructureMachineTemplate` interfaces CAPI's own core
   `cluster-controller`/`machine-controller` reconcile against
   (`spec.providerID`, `status.ready`, `status.addresses`, a
   `status.failureReason`/`failureMessage` pair, and more) -- a fixed,
   versioned contract Kairon doesn't get to simplify.
2. **It fundamentally requires `ownerReferences`.** CAPI's whole object
   graph (`Cluster` owns `Machine` owns `KaironMachine`, garbage
   collection cascades through it) is built on Kubernetes'
   `metadata.ownerReferences` -- **confirmed absent everywhere in this
   codebase** (`grep -rn "ownerReferences\|OwnerReference" internal/`
   returns nothing, and `model.ObjectMeta`
   (`internal/model/types.go`) has no such field at all). Every other
   parent-child relationship in Kairon today is a plain name/label
   reference the reconcile loop resolves itself (`MachineMigrationSpec
   .MachineName`, `MachineSet`'s own `kairon.zyvor.dev/machineset`
   label -- see `internal/controller/machineset.go`), a deliberate
   choice consistent with this project's no-client-go, hand-rolled-API
   posture. Supporting real ownerReference-based garbage collection means
   either adopting it project-wide (a cross-cutting model change well
   beyond this one feature) or building a second, CAPI-only convention
   that behaves differently from every other CRD kind Kairon already has
   -- neither is a small addition.
3. **It's a second, separate controller-manager binary and release
   artifact**, not a new file in `internal/controller` -- CAPI providers
   are conventionally their own repository/binary/Helm chart, registered
   with `clusterctl` via a provider manifest (`clusterctl generate
   provider`), versioned and released independently of Kairon's own
   `kairon-controller`.
4. **It needs CAPI's own conformance test suite** (`cluster-api/test/e2e`)
   to be a credible, trustworthy provider at all -- not something a unit
   test in this repo's own `internal/controller` package can stand in
   for.

## What genuinely *does* fit well, if this is built later

Not everything is friction -- two real architectural fits are worth
naming so a future design pass doesn't have to rediscover them:

- **Bootstrap data bridges cleanly onto `spec.cloudInit`.** CAPI's
  bootstrap providers (kubeadm, etc.) hand an infrastructure provider a
  ready-made cloud-init/ignition blob via a Secret
  (`spec.bootstrap.dataSecretName` on the core `Machine`) -- and Kairon's
  `Machine.spec.cloudInit` already forwards operator-supplied
  customization verbatim into FluxVM's own cloud-init NoCloud seed image
  (`internal/model.CloudInitSpec`, see
  [`machine-network.md`](guides/machine-network.md)). `spec.cloudInit`
  is a structured spec (`hostname`, `user`, `sshAuthorizedKeys`,
  `packages`, `runCmd`, `writeFiles`), not a raw user-data field, so a
  `KaironMachine` controller would have to either translate the Secret's
  cloud-config onto those fields or add a raw-payload field first --
  little new plumbing needed on the FluxVM/cloud-init side specifically,
  but not zero.
- **`MachineSet`'s replica-reconciliation shape is close to what CAPI's
  own `MachineDeployment`/`MachineSet` already expect from an infra
  template** -- `internal/controller/machineset.go`'s "create/delete to
  match a template, track a template-hash label" pattern is
  conceptually the same shape CAPI itself uses at its own layer, so the
  *reconciliation logic* wouldn't be unfamiliar to design, even though
  the object graph (CRDs, ownerReferences) would be new.

## Recommended path if this becomes a priority

Not a Kairon feature PR -- a **separate project** (its own repository,
following `cluster-api-provider-<name>` convention), starting from
CAPI's own provider scaffolding (`clusterctl init --infrastructure`
against a bootstrapped skeleton), that calls into Kairon's existing
`internal/kube`-style client (or, more likely, just talks to the same
Kubernetes API server the `Machine` CRD already lives on) to translate
CAPI's `KaironMachine` objects into ordinary Kairon `Machine` objects --
treating Kairon's own API exactly the way a cloud infrastructure provider
treats a cloud API, not reaching into Kairon's internals directly.
