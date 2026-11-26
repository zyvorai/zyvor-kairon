# User guide: autoscaling a MachineSet

A `MachineSet` can be scaled by anything that speaks the Kubernetes
`/scale` subresource: `kubectl scale`, the built-in
`HorizontalPodAutoscaler` (HPA), KEDA, or your own tooling. Kairon also
ships its own CPU-based `MachineAutoscaler` in the experimental fleet API.
This page covers all three and how to pick one.

| Option | Needs | Metric source | Scale-down guard |
| --- | --- | --- | --- |
| `kubectl scale` / your own tooling | RBAC on `machinesets/scale` | yours | none |
| HPA (`autoscaling/v2`) | Prometheus + prometheus-adapter | `custom.metrics.k8s.io` Object metric | none (see [limits](#real-limits-today)) |
| KEDA `ScaledObject` | Prometheus + KEDA | KEDA's Prometheus trigger | none (see [limits](#real-limits-today)) |
| fleet `MachineAutoscaler` | `fleet.enabled`, Prometheus | `kairon_machine_resource_usage` | `MachineDisruptionBudget` admission ([enterprise fleet](enterprise-fleet.md)) |

Use exactly one of these per `MachineSet`. Two autoscalers writing
`spec.replicas` fight each other; the fleet `MachineAutoscaler` refuses
a second `MachineAutoscaler` on the same target, but it cannot see an HPA
or a `ScaledObject`.

## The scale subresource

`charts/kairon/crds/machinesets.yaml` (and the generated
`deploy/crd.yaml`) declare, on every served version:

```yaml
subresources:
  status: {}
  scale:
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .status.selector
```

`kairon-controller` writes `status.replicas` (every owned, not-deleting
Machine, including surge and outdated replicas during a rollout) and
`status.selector` (`kairon.zyvor.dev/machineset=<name>`, the label on every
replica) on each reconcile tick (`internal/controller/machineset.go`).
`scripts/validate.py` fails CI if the subresource or its paths change.

```console
$ kubectl scale machineset web --replicas=5
$ kubectl get machineset web -o jsonpath='{.status.selector}'
kairon.zyvor.dev/machineset=web
$ kubectl get --raw /apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/web/scale
```

`kaironctl scale machineset` does the same through a merge patch on
`spec.replicas`.

### RBAC

The HPA controller (`system:controller:horizontal-pod-autoscaler`) and the
KEDA operator already hold `get`/`update` on `*/scale` for every API group,
so they need nothing extra. A person, CI job or custom autoscaler that
should only resize MachineSets can be given
[`examples/autoscaling/scale-role.yaml`](../../examples/autoscaling/scale-role.yaml)
(`get`/`update`/`patch` on `machinesets/scale`).

## Metrics: per-VM usage, per MachineSet

Per-VM usage is `kairon_machine_resource_usage{namespace, machine,
resource}` (`cpu_percent`, `memory_bytes`, ...), exported by `kairon-node`
from FluxVM stats. `kairon-node` does not know which MachineSet a Machine
belongs to, so `kairon-controller` also exports
`kairon_machineset_machine_info{namespace, machineset, machine} 1` for
every replica (the `kube_pod_owner` equivalent, see
[observability](observability.md#kairon_machineset_machine_info-kairon-controller-only)).

`charts/kairon/alerts.yaml`'s `kairon-autoscaling` group joins the two
into one series per MachineSet:

| Recording rule | Value |
| --- | --- |
| `kairon:machineset_cpu_percent:sum` | sum of `cpu_percent` over the MachineSet's replicas |
| `kairon:machineset_memory_bytes:sum` | sum of `memory_bytes` over the MachineSet's replicas |

These ship in the chart's `PrometheusRule` when
`metrics.prometheusRule.enabled=true`; otherwise copy the group into your
Prometheus `rule_files`. Every replica in a MachineSet is built from the
same template, so a per-replica `cpu_percent` target means the same thing
for each of them. `cpu_percent` is FluxVM's figure for the whole VM;
the fleet `MachineAutoscaler` divides it by the template's vCPU count, so
check a running replica (`kaironctl top machines`) before picking a
target for a multi-vCPU template.

### Why sums and `AverageValue`

HPA treats `Value` targets and `Resource`/`Pods` metrics as per-Pod
calculations: it lists Pods matching `status.selector` to count ready
replicas. Machines are not Pods, so HPA finds none and stops with
`no pods returned by selector while calculating replica count`. An
`Object` or `External` metric with `target.type: AverageValue` skips the
Pod lookup: HPA divides the metric by `status.replicas` and sets
`replicas = ceil(sum / averageValue)`. That is why the recording rules are
MachineSet-wide sums and every example uses `AverageValue`.

## HPA with prometheus-adapter

1. Install [prometheus-adapter](https://github.com/kubernetes-sigs/prometheus-adapter)
   with the rules in
   [`examples/autoscaling/prometheus-adapter-values.yaml`](../../examples/autoscaling/prometheus-adapter-values.yaml).
   They expose `machineset_cpu_percent` and `machineset_memory_bytes` as
   Object metrics on `machinesets.kairon.zyvor.dev`:

   ```console
   $ kubectl get --raw "/apis/custom.metrics.k8s.io/v1beta1/namespaces/default/machinesets.kairon.zyvor.dev/web/machineset_cpu_percent"
   ```

2. Apply [`examples/autoscaling/hpa-machineset.yaml`](../../examples/autoscaling/hpa-machineset.yaml):

   ```yaml
   scaleTargetRef: {apiVersion: kairon.zyvor.dev/v1, kind: MachineSet, name: web}
   minReplicas: 2
   maxReplicas: 10
   metrics:
   - type: Object
     object:
       describedObject: {apiVersion: kairon.zyvor.dev/v1, kind: MachineSet, name: web}
       metric: {name: machineset_cpu_percent}
       target: {type: AverageValue, averageValue: "60"}
   ```

   The example's `behavior` adds one Machine per two minutes and removes
   one per five, after a ten-minute scale-down window: a VM boots far
   slower than a Pod, and HPA's Pod-tuned defaults would otherwise
   overshoot while new replicas are still booting.

3. `kubectl describe hpa web` shows the current per-replica value and any
   `FailedGetObjectMetric` events.

## KEDA

[`examples/autoscaling/keda-scaledobject.yaml`](../../examples/autoscaling/keda-scaledobject.yaml)
needs no adapter: KEDA queries Prometheus itself and drives the scale
subresource through an HPA it manages, using an `External` metric with
`AverageValue`:

```yaml
scaleTargetRef: {apiVersion: kairon.zyvor.dev/v1, kind: MachineSet, name: web}
triggers:
- type: prometheus
  metricType: AverageValue
  metadata:
    serverAddress: http://prometheus-operated.monitoring.svc:9090
    query: sum(kairon:machineset_cpu_percent:sum{namespace="default",machineset="web"})
    threshold: "60"
```

Leave `minReplicaCount` at 1 or more unless you want KEDA to stop every
VM in the set when the query returns zero.

## fleet MachineAutoscaler

If you already run the fleet API (`fleet.enabled`), a
`MachineAutoscaler` ([`examples/fleet/autoscaler.json`](../../examples/fleet/autoscaler.json))
needs no adapter or KEDA. It reads the same `kairon_machine_resource_usage`
series, normalises by vCPUs, waits for `stabilizationSeconds`, moves at
most `maxStep` replicas at a time, and checks scale-down against
`MachineDisruptionBudget`s. See
[enterprise fleet](enterprise-fleet.md) and its
[reference](enterprise-fleet-reference.md).

## Real limits today

- **A paused MachineSet ignores the autoscaler.** HPA/KEDA can still write
  `spec.replicas` through `/scale` while `spec.paused` is `true`, but no
  replica is created or deleted until it is resumed; the HPA will report
  the new desired count while the actual count stays put. See
  [Pausing a MachineSet](machine-sets.md#pausing-a-machineset).
- **HPA/KEDA scale-down does not check `MachineDisruptionBudget`.** The
  MachineSet controller deletes surplus replicas when `spec.replicas`
  drops, whoever lowered it, and does not pick the least-loaded or
  newest one. Use a conservative `scaleDown` policy, or the fleet
  `MachineAutoscaler` where budgets matter.
- **Scale-down deletes the VM.** There is no drain or guest shutdown hook
  beyond what deleting a Machine already does.
- **Usage is only as fresh as `kairon-node`'s sample.** A replica that
  is not `Running` yet reports no usage, so the sum can dip while new
  replicas boot; the stabilization windows above absorb that.
- **Untested end to end.** The scale subresource, selector, membership
  metric and recording rules are covered by unit tests, `promtool check
  rules` and `scripts/validate.py`. No HPA, prometheus-adapter or KEDA
  install against a live cluster has been recorded yet (see
  [`TEST_REPORT.md`](../../TEST_REPORT.md)).
