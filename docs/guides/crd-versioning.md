# User guide: CRD versioning and the conversion webhook scaffold

Every `kairon.zyvor.dev` CRD serves three versions with an identical schema:

| Version | served | storage | status |
| --- | --- | --- | --- |
| `v1` | yes | **yes** | current; all Kairon binaries, examples and charts use it |
| `v1beta1` | yes | no | deprecated (API server prints a warning) |
| `v1alpha1` | yes | no | deprecated (API server prints a warning) |

Because the schemas are the same (one YAML anchor in each
`charts/kairon/crds/*.yaml`, checked by `scripts/validate.py`), the API
server converts between them with the default `None` strategy: it only
rewrites `apiVersion`. Existing `v1alpha1`/`v1beta1` manifests and GitOps
repos keep working; new and rewritten objects are stored as `v1`. The
`fleet.kairon.zyvor.dev` group is still `v1alpha1`.

## Upgrading an existing cluster

**Apply the CRDs before the new binaries**: Kairon's clients now call
`/apis/kairon.zyvor.dev/v1/...`, which a cluster that has not yet seen the
`v1` CRDs does not serve. Helm installs `crds/` only on first install and
never upgrades them, so apply them yourself. `kaironctl upgrade` (embedded Helm
SDK, not `--helm-cli`) and the host deploy script do it for you:

```sh
kubectl apply --server-side -f deploy/crd.yaml
# or: ./scripts/deploy-remote.sh HOST USER . --apply-crds --apply-rbac ...
```

Objects written before this stay stored as `v1alpha1`/`v1beta1` until
something writes them again; that is harmless while those versions are
served. To rewrite them all now (needed only before a future release stops
serving the old versions), re-store each one and then drop the old versions
from `status.storedVersions`:

```sh
for r in $(kubectl api-resources --api-group=kairon.zyvor.dev -o name); do
  kubectl get "$r" -A -o json | kubectl replace -f -
  kubectl patch crd "$r" --subresource=status --type=merge \
    -p '{"status":{"storedVersions":["v1"]}}'
done
```

The conversion webhook scaffold below is a worked example for a *future*
version with a different schema; it is not wired into any CRD today.

## Why this needed real work ahead of time

A Kubernetes CRD can only ever have one storage version at a time, but can
serve more than one at once -- `kubectl get machinequota.v1beta1...` and
`...v1alpha1...` both working against the same stored object, converted on
the fly. That conversion has to happen somewhere: either a declarative
field-rename-only strategy (`None`, limited to trivial renames the API
server can do itself) or a real webhook the API server calls per request
(`Webhook`, for anything more involved -- restructuring a field, splitting
one into several, changing a type). This project had never built the
latter, and the existing production-readiness review flagged that
explicitly: "the first `v1beta1` bump will need this built from scratch."

Building it speculatively, with no real second version to prove it
against, would have meant either shipping untested plumbing or inventing a
throwaway schema change with no real justification just to exercise it.
Instead, this scaffold does the former with a **real, tested worked
example** (`MachineQuota`) that never goes live -- the machinery is proven
against real conversion logic and a real HTTP contract, without committing
this project to an actual API version bump nobody's asked for yet.

## What exists today

- **`internal/conversion`**: hand-rolls the `apiextensions.k8s.io/v1`
  `ConversionReview` wire format, the same kind of deliberate,
  documented exception `internal/admission` already makes for
  `AdmissionReview` -- see that package's doc comment for why this
  project doesn't pull in `client-go`/`k8s.io/api` for one small, stable
  JSON schema. `Handler(log, kind, convert)` returns an `http.HandlerFunc`
  implementing the webhook contract for one kind: decode the incoming
  `Review`, run every object in `request.objects` through a `Converter`,
  encode the outgoing `Review`. A decode or conversion failure always gets
  a well-formed `Failure` response, never a bare HTTP error -- the API
  server surfaces `response.result.message` straight back to whoever's
  `kubectl apply`/`get` triggered the conversion, which is far more useful
  than an opaque transport error.
- **`ConvertMachineQuota`** (`internal/conversion/machinequota.go`): the
  worked example. It converts a `MachineQuota` between a hypothetical
  renamed-field `v1` schema and the current one, renaming
  `spec.maxTotalCpu`/`maxTotalMemory` to `spec.maxCpu`/`maxMemory` and the
  matching `status` fields -- dropping the redundant "Total" qualifier, a
  plausible real API cleanup. `spec.maxMachines`/`status.usedMachines`
  pass through unrenamed on purpose: not every field needs to move just
  because the version does, and a converter that only touches what
  actually changed is the realistic shape a real one will take too.
  Operates on generic `map[string]any` JSON, not a typed Go struct --
  there's no Go type for a version that isn't wired into the rest of
  Kairon's still-single-version reconcile paths, and inventing one only
  for this would be speculative in the same way the rest of this scaffold
  deliberately isn't.
- **`kairon-controller`'s webhook server already exposes
  `POST /convert/machinequotas`** (`internal/controller/webhook.go`,
  `WebhookHandler`), on the exact same TLS listener, certificate, and
  `Service` as the existing validating admission webhook
  (`webhook.enabled`, see [SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md)'s "MachineQuota
  / MachineDisruptionBudget admission" section). No new listener, no new
  certificate to provision, no new trust boundary to reason about --
  cutting a real version reuses infrastructure this project already
  operates.
- Both are covered by real tests: `internal/conversion`'s own unit tests
  (including a same-version identity case, a full round trip, an
  unsupported-version-pair error, and confirming the converter never
  mutates its input) and `internal/controller`'s
  `TestWebhookHandlerConvertMachineQuotaEndToEnd`, which drives the route
  through the real HTTP `ConversionReview` envelope, not just the
  converter function in isolation.

## What doesn't exist yet, on purpose

No CRD declares a `spec.conversion` webhook: `v1`, `v1beta1` and `v1alpha1`
share a schema, so `None` is enough. `/convert/machinequotas` (which converts
between a *hypothetical* renamed-field MachineQuota schema and the
current one) is live, tested code the API server never calls; it is the
template for the first version whose schema actually differs.

## What cutting a schema-changing version requires

When a version with a different schema is needed:

1. **Add the new version to the CRD's `spec.versions`** (`served: true`,
   `storage: false` initially -- flip `storage` to the new version only
   once every component that writes the CRD directly, if any, is updated
   to tolerate it; `scripts/validate.py` expects the storage version
   first in the array and today requires identical schemas, so update it
   too).
2. **Add `spec.conversion`** to that same CRD object:
   ```yaml
   spec:
     conversion:
       strategy: Webhook
       webhook:
         conversionReviewVersions: ["v1"]
         clientConfig:
           service:
             name: kairon-controller-webhook
             namespace: <release namespace>
             path: /convert/machinequotas
             port: 443
           caBundle: <base64 PEM, same value as webhook.caBundle>
   ```
3. **Register or extend a `Converter`** in `internal/conversion` for the
   real field changes (`ConvertMachineQuota` is the template to copy), and
   wire it into `WebhookHandler` (`internal/controller/webhook.go`) the
   same way `/convert/machinequotas` already is.
4. **Solve the one real gap this scaffold deliberately leaves open**:
   `charts/kairon/crds/*.yaml` lives in Helm's special `crds/` directory,
   which Helm never templates (no `.Values`, no `.Release` access, by
   Helm's own design -- CRDs must be installable before any values are
   known) and never updates on `helm upgrade`. `webhook.caBundle` above
   needs a live value from `values.yaml`, exactly like the existing
   `ValidatingWebhookConfiguration` in `templates/webhook.yaml` already
   gets -- but that file lives in `templates/`, not `crds/`, which is
   how it gets away with templating `.Values.webhook.caBundle` at all.
   Cutting a real version means either moving that one CRD's management
   into `templates/` (gaining real `helm upgrade` semantics for it, at
   the cost of diverging from how the other sixteen `kairon.zyvor.dev` CRDs are managed) or
   applying `spec.conversion` via a separate, explicitly-documented step
   (a small patch script, following the same "we don't auto-mint your
   TLS material" posture this project already takes for
   `webhook.tlsSecretName`/`console.tls.secretName`/
   `migration.dataplaneTlsSecretName`). Pick one deliberately when the
   time comes -- don't let it default to whichever seems less work in
   the moment.
5. **Verify against a real cluster**: register the version, `kubectl
   apply` an object using the new version's field names, read it back
   under the old version's, and vice versa -- confirming the real API
   server is actually invoking the webhook, not just that the converter
   function is correct in isolation (the same distinction this project's
   admission webhook work already draws between a `Validator` and its
   HTTP envelope).

## Real limits today

- **The conversion webhook is not live.** `v1`, `v1beta1` and `v1alpha1`
  convert through the API server's `None` strategy; the webhook is infrastructure for a
  future schema change.
- **Only `MachineQuota` has a worked converter.** The other CRDs
  have no `Converter` implementation yet -- `ConvertMachineQuota` is the
  template, not a generic field-rename engine every kind gets for free.
- **The `crds/`-directory templating gap (step 4 above) is unsolved.**
  This is the one piece of "built from scratch" this scaffold doesn't
  remove -- it names the problem precisely instead.
