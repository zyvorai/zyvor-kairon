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

`webhook.enabled=true` without both `webhook.tlsSecretName` and `webhook.caBundle` makes the chart stop
with a message (v0.10.2 rendered an invalid volume instead), and `values-production.yaml` turns the webhook on.
The script's certificates are valid for 30 days (`CERT_DAYS=N` changes it) and its CA is a throwaway one: with
the default `failurePolicy: Fail`, an expired serving certificate blocks every Machine and MachineMigration write,
so in production use cert-manager or your own CA with a rotation process, and renew before expiry.
The webhook has been exercised by unit tests only, not against a real API server in the project's own CI
([`TEST_REPORT.md`](../../TEST_REPORT.md)).

**Warning on create-only fields.** `spec.cloudInit` and `spec.network.forwards`
are read once, when the VM is created. With the webhook enabled, an edit to
either on a Machine whose VM already exists is still accepted but returns an
admission warning (`kubectl` prints it): "spec.cloudInit is only applied when
the VM is created; this change to an existing Machine has no effect. Delete and
recreate the Machine to apply it." Edits before the VM exists, and unrelated
edits, produce no warning.

It reuses the exact decision functions the reconcile loop and `kaironctl evacuate` already had (`internal/controller/quota.go`, `internal/controller/disruption.go`) — not a second implementation to drift out of sync. `MachineDisruptionBudget` and Machine `CREATE` quota checks only ever evaluate `CREATE`, matching each check's pre-existing scope in the reconcile loop, so the webhook can't become *stricter* than what it backstops there. `MachineQuota` is the one exception: the webhook also evaluates `UPDATE`, denying a hotplug resize (`spec.resources` growing on an already-scheduled Machine) that would push its namespace over quota — closing a real gap CREATE alone leaves open, since `kairon-node`'s hotplug has no cluster-wide quota visibility of its own to backstop with. `webhook.failurePolicy` defaults to `Fail` — an outage blocks every `Machine`/`MachineMigration` write cluster-wide rather than silently letting the old bypass back in; flip it to `Ignore` if you'd rather trade that guarantee for availability.

---


