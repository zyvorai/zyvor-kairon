# Cross-cluster cold DR (runbook)

Live migration across clusters is **not** supported. This runbook covers cold disaster recovery only: stop the guest on the source cluster, copy the disk, and start it on a second Kairon cluster.

## Current limits

- No live or post-copy migration between clusters.
- No shared control plane or federation.
- Disk copy is operator-driven (rsync, scp, or object storage). FluxVM does not yet expose a multi-cluster disk transport.
- Network identity (IPs, MAC, security groups) must be re-created on the target.

## Prerequisites

- Two independent Kairon clusters (source and target), each with its own FluxVM nodes.
- Matching image format (qcow2) and enough free space on the target node.
- SSH or object-storage access between the clusters for the disk copy.
- The Machine YAML (or `kaironctl get machine -o yaml`) from the source.

## Steps

1. **Quiesce on source**
   ```bash
   kaironctl stop machine <name> --namespace <ns>
   # wait until status.phase is Stopped
   ```

2. **Locate the disk**
   On the source node, the disk is under the FluxVM storage path recorded in `status.vmID` / the node’s `state_dir`. Copy it:
   ```bash
   rsync -a --progress <source-node>:/var/lib/kairon/vms/<id>.qcow2 ./
   ```

3. **Copy to target**
   ```bash
   rsync -a --progress <id>.qcow2 <target-node>:/var/lib/kairon/vms/
   ```

4. **Recreate the Machine on the target cluster**
   Edit the YAML: remove `status`, `resourceVersion`, `uid`, and any node-specific annotations. Point `spec.image.path` at the copied file. Apply:
   ```bash
   kubectl apply -f machine.yaml --context target
   ```

5. **Start and verify**
   ```bash
   kaironctl start machine <name> --namespace <ns>
   # confirm Ready and that the guest boots
   ```

## What is still missing

- Automated disk transport between clusters.
- Network/IP migration or dual-homing.
- Orchestrated failover (today this is a manual runbook).
- Consistency guarantees beyond a clean stop (no guest-agent freeze yet).

See also `docs/runbook-migration-matrix.md` for single-cluster live migration.
