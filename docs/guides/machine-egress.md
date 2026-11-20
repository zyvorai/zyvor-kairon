# Machine egress policy (`spec.network.egress`)

A guest on **user-mode (slirp) networking** has no interface on the host, so FluxVM's tc dataplane
(`MachineNetworkPolicy`) cannot govern it. Its traffic is re-originated by the VM's **QEMU process**,
and `spec.network.egress` sets the policy for that process. Without it a user-mode guest reaches
nothing outbound: no guest-agent download, no package mirror, no control plane.

Needs a FluxVM that knows the field (the QEMU egress policy build) and the QEMU backend. A VM that
started before that FluxVM build must be restarted to take a policy.

```yaml
apiVersion: kairon.zyvor.dev/v1beta1
kind: Machine
metadata: {name: web, namespace: prod}
spec:
  network:
    mode: user
    egress:
      mode: allowlist              # loopback | allowlist | audit | open
      rules:
      - {cidr: 10.0.0.0/8, proto: tcp, port: 5151, note: Veyron API}
      - {cidr: 91.189.88.0/21, proto: tcp, port: 80}
      - {cidr: 0.0.0.0/0, proto: udp, port: 53}
```

| mode | effect |
|---|---|
| `loopback` | nothing leaves (also what an unset field means when no rules are given) |
| `allowlist` | only the rules (the default when rules are given) |
| `audit` | everything allowed; what an allow-list would have dropped is recorded, see below |
| `open` | everything allowed |

Rules: IPv4 CIDR or bare address; `port` needs `proto: tcp|udp`; at most 256 rules; at most 4
distinct `(proto, port)` pairs per network once the ports of covering networks are included. The
admission webhook rejects a policy FluxVM would refuse.

**Cilium rewrites service addresses before the filter runs.** A guest dialling a ClusterIP or
NodePort is seen as the backend `pod-ip:port`; allow the backend (or its CIDR). A `hostPort` on the
node IP is not rewritten.

## Reconciliation

* Create: the policy rides in the FluxVM create request.
* Running Machine: kairon-node reads FluxVM's policy for the VM and writes `PUT
  /v1/vms/{id}/network/egress` only when it differs. A running VM takes it at once.
* Removing the field clears the VM's own policy again (it then follows the node default), but only
  when kairon-node set one (annotation `kairon.zyvor.dev/egress-managed`). A policy set directly on
  FluxVM, or through the console relay, on a Machine that never had the field is left alone; on a
  Machine that has the field, the spec wins on the next reconcile.

## Console relay routes

Bearer-authenticated like the other relay routes; bodies are capped at 64 KiB.

| route | FluxVM |
|---|---|
| `GET/PUT/DELETE /vm-egress/{runtimeID}` | `/v1/vms/{id}/network/egress` |
| `GET /vm-egress-observed/{runtimeID}` | `/v1/vms/{id}/network/egress/observed` (refused, or in audit mode would-be-refused, destinations) |
| `POST /vm-egress-suggest/{runtimeID}[?apply=true]` | `/v1/vms/{id}/network/egress/suggest` (rules that would allow what was observed) |
| `GET/PUT /egress-defaults[?apply_running=true]` | `/v1/network/egress/defaults` (node default) |

The per-VM routes end in the runtime id so callers that append it (Veyron) need no special case.
