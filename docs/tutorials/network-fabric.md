# Tutorial: Network Fabric on a Machine

This walkthrough creates a TAP+netns Machine, applies a security group and
edge policy, and checks dataplane status. FluxVM must be running with Network
Fabric enabled (`sandbox.dataplane.mode = "ebpf"`). See the reference map in
[network-fabric.md](../network-fabric.md).

## Prerequisites

- Kairon installed (`deploy/crd.yaml` includes `MachineNetworkPolicy` and `NetworkSecurityGroup`)
- At least one node labeled `kairon.zyvor.dev/capable=true`
- FluxVM on that node with eBPF dataplane GA profile
- A bootable qcow2 under the node `--image-root` (default `/var/lib/fluxvm/images`)

## 1. Apply the example stack

```bash
# Edit nodeName / image path if needed
kubectl apply -f examples/network-fabric-machine.yaml
```

That creates:

| Resource | Name | Role |
|---|---|---|
| `Machine` | `web` | TAP + netns, static guest address, dataplane required |
| `NetworkSecurityGroup` | `frontend` | Named group with allow CIDRs/ports |
| `MachineNetworkPolicy` | `web-edge` | Label selector `app=web` → FluxVM VM policy |

## 2. Wait for the Machine

```bash
kubectl get machine web -w
# Expect Phase=Running and a guestIP once FluxVM reports it
kubectl get machine web -o jsonpath='{.status.guestIP}{"\n"}{.status.network.dataplane.attached}{"\n"}'
```

With `dataplaneRequired: true`, a failed eBPF attach leaves the Machine in
`Error` instead of silently running without enforcement.

## 3. Confirm policy apply

```bash
kubectl get machinenetworkpolicy web-edge -o yaml
kubectl get networksecuritygroup frontend -o yaml
```

Expect `status.phase: Applied` on both after the node agent reconciles.
`MachineNetworkPolicy.status.observedMachines` should be `1` when `web` is
Running on a node.

## 4. Inspect FluxVM dataplane (on the node)

```bash
# Replace UUID with Machine.status.runtimeID
curl -sS "http://127.0.0.1:7788/v1/vms/${RUNTIME_ID}/network/status" | jq .
curl -sS "http://127.0.0.1:7788/v1/vms/${RUNTIME_ID}/network/effective" | jq .
```

Through Fabric, the same surface is proxied as
`/api/vms/{name}/dataplane/*` (Dataplane tab / `zyvorctl dataplane`).

## 5. Tighten the policy

```bash
kubectl patch machinenetworkpolicy web-edge --type merge -p '
spec:
  policy:
    defaultAllow: false
    allowPorts: ["tcp/443","udp/53"]
    maxEgressMbps: 100
'
```

The node agent posts the new policy to FluxVM without recreating the VM.

## 6. Optional: Service Fabric membership

If FluxVM already has a service named `web-vip`, the Machine example registers
its guest IP as a backend on port `8080`. Create the VIP in FluxVM/Fabric
first; Kairon only merges membership.

```yaml
spec:
  serviceFabric:
    services:
      - name: web-vip
        port: 8080
        weight: 1
```

## 7. Optional: Cilium cluster network

Skip this on a cluster that is not running Cilium. When it is, enable the
controller flags and attach the Machine:

```bash
# Helm: --set network.ciliumAttach.enabled=true --set network.ciliumPolicySync.enabled=true
kubectl patch machine web --type merge -p '
spec:
  network:
    mode: tap
    netns: true
    dataplaneMode: cilium
    dataplaneRequired: true
    ciliumAttach: true
'
kubectl patch machinenetworkpolicy web-edge --type merge -p '
spec:
  cilium:
    sync: true
'
kaironctl network status web
kubectl get ciliumexternalworkload
kubectl get ciliumnetworkpolicy -n default
```

FluxVM still owns TAP/TC/eBPF. Kairon only reconciles the
`CiliumExternalWorkload` and, when sync is on, the `CiliumNetworkPolicy`.
Node-wide default: `sandbox.dataplane.mode = "cilium"` in `/etc/fluxvm.toml`.

## 8. Cleanup

```bash
kubectl delete -f examples/network-fabric-machine.yaml
```

Deleting `MachineNetworkPolicy` resets matched VMs to `default_allow: true`.
Deleting `NetworkSecurityGroup` removes the FluxVM group on that node.
Deleting the `Machine` (or setting its `spec.powerState` to `Stopped`/
`Halted`) also removes its backend entry from `web-vip` above, so the VIP
never keeps routing traffic at a guest that is no longer there.

## Next

- Field reference: [guides/machine-network.md](../guides/machine-network.md)
- Policy & groups: [guides/network-policy.md](../guides/network-policy.md)
- Architecture boundary: [network-fabric.md](../network-fabric.md)
