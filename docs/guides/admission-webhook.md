# Guarding the fleet (admission webhook)

Also see [machine-quotas.md](machine-quotas.md) and [machine-disruption-budgets.md](machine-disruption-budgets.md).


`MachineQuota` and `MachineDisruptionBudget` have always existed as reconcile-loop/CLI-level checks — a Machine over quota just stayed `Pending`; a `MachineMigration` created directly through the API skipped `kaironctl evacuate`'s budget check entirely. Both gaps are now closeable with a real `ValidatingWebhookConfiguration`:

```bash
# You bring the certificate -- this chart doesn't mint one for you,
# same posture as migration.tlsSecretName/console.tls.secretName.
# For evaluation, the repository script writes a throwaway CA and serving certificate
# (ca.pem, cert.pem, key.pem), a ready-made Secret manifest and ca-bundle.b64:
scripts/gen-webhook-tls-certs.sh ./webhook-certs
kubectl create namespace kairon-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f ./webhook-certs/kairon-webhook-tls.secret.yaml
# (or, with your own files: kubectl -n kairon-system create secret tls kairon-webhook-tls --cert=tls.crt --key=tls.key)

helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set webhook.enabled=true \
  --set webhook.tlsSecretName=kairon-webhook-tls \
  --set webhook.caBundle="$(cat ./webhook-certs/ca-bundle.b64)"
```

The chart itself creates the `kairon-controller-webhook` Service (port 443 to the controller's webhook port) and the
`ValidatingWebhookConfiguration` of the same name, but only when `webhook.enabled`, `webhook.tlsSecretName` and
`webhook.caBundle` are all set. The generator script's closing message still says to create that Service and
configuration yourself; ignore that step, the chart does it. The script's certificate names
`kairon-controller-webhook.kairon-system.svc`, so for a release namespace other than `kairon-system` run
`scripts/gen-webhook-tls-certs.sh OUT_DIR kairon-controller-webhook YOUR_NAMESPACE`.

`webhook.enabled=true` without both `webhook.tlsSecretName` and `webhook.caBundle` makes the chart stop
with a message (v0.10.2 rendered an invalid volume instead), and `values-production.yaml` turns the webhook on.
The script's certificates are valid for 30 days (`CERT_DAYS=N` changes it) and its CA is a throwaway one: with
the default `failurePolicy: Fail`, an expired serving certificate blocks every Machine and MachineMigration write,
so in production use cert-manager or your own CA with a rotation process, and renew before expiry.
The webhook has been exercised by unit tests only, not against a real API server in the project's own CI
([`TEST_REPORT.md`](../../TEST_REPORT.md)).

**CPU and memory quantities.** Since v0.10.4 the quantity parser (`model.ParseVCPUs`/`ParseMemoryMiB`/`ParseBytes`)
returns an error for `NaN`, `Inf` and out-of-range values (for example `NaN`, `1e30Gi`, more than 2^20 vCPUs or 2^40 MiB
of memory), and the Machine check rejects them at admission through that parser. Before, such a value parsed to an
arbitrary number (usually 0), which read as "no request" and skipped the scheduler's capacity check. The webhook is
the only place that rejects the Machine up front: with it off (the default), the apply still succeeds, and the
scheduler, which has always treated a quantity it cannot parse as no request, does not enforce capacity for it.
Existing Machines that relied on such a value need a real quantity.

**Warning on create-only fields.** `spec.cloudInit` and `spec.network.forwards`
are read once, when the VM is created. With the webhook enabled, an edit to
either on a Machine whose VM already exists is still accepted but returns an
admission warning (`kubectl` prints it): "spec.cloudInit is only applied when
the VM is created; this change to an existing Machine has no effect. Delete and
recreate the Machine to apply it." Edits before the VM exists, and unrelated
edits, produce no warning.

It reuses the exact decision functions the reconcile loop and `kaironctl evacuate` already had (`internal/controller/quota.go`, `internal/controller/disruption.go`) — not a second implementation to drift out of sync. `MachineDisruptionBudget` and Machine `CREATE` quota checks only ever evaluate `CREATE`, matching each check's pre-existing scope in the reconcile loop, so the webhook can't become *stricter* than what it backstops there. `MachineQuota` is the one exception: the webhook also evaluates `UPDATE`, denying a hotplug resize (`spec.resources` growing on an already-scheduled Machine) that would push its namespace over quota — closing a real gap CREATE alone leaves open, since `kairon-node`'s hotplug has no cluster-wide quota visibility of its own to backstop with. `webhook.failurePolicy` defaults to `Fail` — an outage blocks every `Machine`/`MachineMigration` write cluster-wide rather than silently letting the old bypass back in; flip it to `Ignore` if you'd rather trade that guarantee for availability.

---


