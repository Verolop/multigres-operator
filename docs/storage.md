# Storage Management

## PVC Deletion Policy

The operator supports fine-grained control over **Persistent Volume Claim (PVC) lifecycle management** for stateful components (TopoServers and Shard Pools). This allows you to decide whether PVCs should be automatically deleted or retained when resources are deleted or scaled down.

### Policy Options

The `pvcDeletionPolicy` field has two settings:

- **`whenDeleted`**: Controls what happens to PVCs when the entire MultigresCluster (or a component like a TopoServer) is deleted.
  - `Delete` (default): PVCs are automatically deleted along with the cluster
  - `Retain`: PVCs are preserved for manual review and potential data recovery

- **`whenScaled`**: Controls what happens to PVCs when reducing the number of replicas (e.g., scaling from 3 pods down to 1 pod).
  - `Delete` (default): PVCs are automatically deleted when pods are removed
  - `Retain`: PVCs from scaled-down pods are kept for manual recovery

### Defaults

**By default, the operator uses `Delete/Delete`**. This means:
- Deleting a cluster **will** delete its data volumes (`Delete`)
- Scaling down **will** delete the PVCs from removed pods (`Delete`)

The `Delete` default for `whenScaled` is deliberate: pgbackrest is the source of truth for data recovery, so retaining PVCs from scaled-down pods adds storage cost without benefit. If you need to retain scaled-down PVCs for manual recovery, set `whenScaled: Retain` explicitly.

### Where to Set the Policy

The `pvcDeletionPolicy` can be set at multiple levels in the hierarchy, with more specific settings overriding general ones:

```yaml
apiVersion: multigres.com/v1alpha1
kind: MultigresCluster
metadata:
  name: my-cluster
spec:
  # Cluster-level policy (applies to all components unless overridden)
  pvcDeletionPolicy:
    whenDeleted: Retain  # Safe: keep data when cluster is deleted
    whenScaled: Delete   # Aggressive: auto-cleanup when scaling down

  globalTopoServer:
    # Override for GlobalTopoServer specifically
    pvcDeletionPolicy:
      whenDeleted: Delete  # Different policy for topo server
      whenScaled: Retain

  databases:
    - name: postgres
      tablegroups:
        - name: default
          # Override for this specific TableGroup
          pvcDeletionPolicy:
            whenDeleted: Retain
            whenScaled: Retain
          shards:
            - name: "0-inf"
              # Override for this specific shard
              spec:
                pvcDeletionPolicy:
                  whenDeleted: Delete
```

The policy is merged hierarchically:
1. **Shard-level** policy (most specific)
2. **TableGroup-level** policy
3. **Cluster-level** policy
4. **Template defaults** (CoreTemplate, ShardTemplate)
5. **Operator defaults** (Delete/Delete)

**Note**: If a child policy specifies only `whenDeleted`, it will inherit `whenScaled` from its parent, and vice versa.

### Moving the Global Topology to an External Server

When a cluster's `globalTopoServer` changes from a managed `etcd` to an `external` server, the operator keeps the managed TopoServer running until the move has finished: the external topology has every cell and database registered, the cluster is `Healthy`, no running or pending pod of the cluster is still started against the managed address, and the external server is shown to be a different etcd (below). Only then does it delete the managed TopoServer. While it waits, the retiring TopoServer does not count toward the cluster's phase, and the managed etcd itself is never consulted to decide, so an unhealthy or completely dead old etcd cannot hold back its own cleanup once everything has moved. Pods that have terminated (`Succeeded` or `Failed`, such as evicted ones) do not count as still using the managed address; a pod stuck in `Unknown` on a lost node does, until it is removed.

The external server has to be shown to be a different etcd because an endpoint can reach the managed etcd under another name (its fully qualified Service name, an ExternalName Service, its ClusterIP, a LoadBalancer in front of its pods), and retiring would then delete the etcd the cluster runs on. The operator asks the external server for its membership and retires only when none of the members it lists is one of the managed etcd's: those are named after their pods (`<cluster>-global-topo-<n>`) and advertise URLs under the managed Services or at the managed pods' IPs, so any alias lists them, and so does an external cluster that still depends on them (a migration by `member add` whose old members have not been removed yet). The membership is believed only if it is a real one: the member that answers a `Status` call on the same connection must be one of those listed, which an etcd grpc-proxy or similar front end answering for the managed etcd is not. If the managed etcd is readable, its cluster ID must also differ from the external one's. Endpoints that fail these checks, and some etcd-API front ends that never pass them, keep the managed TopoServer indefinitely.

What retirement is waiting for is raised as an event on the cluster, which otherwise stays Healthy: `GlobalTopoRetireWaiting` (Normal) for the expected steps of a move, `GlobalTopoRetireBlocked` (Warning) when the external server's identity is the problem and a person has to act. The changing detail, such as which pod or which error, is in the operator's log.

The etcd PVCs are deleted with the TopoServer only if both the global topology's current policy and the TopoServer's own policy allow it. The current policy is `spec.globalTopoServer.pvcDeletionPolicy` if that object is set, otherwise the cluster's `pvcDeletionPolicy`; note that the API server fills in `whenDeleted: Retain` inside any policy object that leaves it out. It is read at the moment of retirement, so a `Retain` set in the same change as the move is honoured. The TopoServer's own policy is the one it was created with, including an `etcd`-level policy that the move removed from the spec, and nothing changes it after the move: it is also the way to keep the volumes of one retirement without changing the cluster's policy, by editing it on the TopoServer. If either says `Retain`, the PVCs are kept and a `GlobalTopoPVCsKept` warning names them. Just before deleting anything the operator re-reads the cluster, its policy and the TopoServer from the API server, and does nothing that pass if the cluster changed meanwhile.

With `Retain` the PVCs are kept, and moving back to a managed topology later recreates etcd on them with the data from before the move, including pooler registrations under the names of the pods now running. The operator cannot tell those from live ones, so before moving back onto retained data, reset it (delete the PVCs) or expect the return to need manual attention.

A move back to a managed topology that lands after the PVCs were deleted but before the TopoServer was (which needs a failed delete or an operator restart between the two, and the change in that window) leaves the recreated etcd on claims that are already being deleted; they go when its pods next restart. Check for terminating `data-<cluster>-global-topo-*` claims after moving back quickly from an external server.

The TopoServer's own StatefulSet always retains its PVCs, whatever the policy, so deleting or recreating just the StatefulSet never drops etcd data.

### Templates and PVC Policy

You can define PVC policies in templates for reuse:

```yaml
apiVersion: multigres.com/v1alpha1
kind: ShardTemplate
metadata:
  name: production-shard
spec:
  pvcDeletionPolicy:
    whenDeleted: Retain
    whenScaled: Retain
  # ... other shard config
---
apiVersion: multigres.com/v1alpha1
kind: CoreTemplate
metadata:
  name: ephemeral-topo
spec:
  globalTopoServer:
    pvcDeletionPolicy:
      whenDeleted: Delete
      whenScaled: Delete
```

### Important Caveats

⚠️ **Data Loss Risk**: Setting `whenDeleted: Delete` means **permanent data loss** when the cluster is deleted. Use this only for:
- Development/testing environments
- Ephemeral clusters
- Scenarios where data is backed up externally

⚠️ **Replica Scale-Down Behavior**: The default `whenScaled: Delete` will **immediately delete PVCs** when the operator removes pods during scale-down. If you scale the replica count back up, new pods will start with **empty volumes** and will need to restore from backup. Set `whenScaled: Retain` if you want to preserve PVCs for faster scale-up without backup restore.

**Note**: This does NOT affect storage size. Changing PVC storage capacity is handled separately by the **PVC Volume Expansion** feature (see below).

✅ **Production Recommendation**: For production clusters, ensure proper backup/restore procedures are in place. The default `Delete/Delete` policy relies on pgbackrest for recovery; set `whenDeleted: Retain` where volumes must outlive the cluster.

---

## PVC Volume Expansion

The operator supports **in-place PVC volume expansion**. When you increase `storage.size` on a pool (or backup filesystem storage), the operator patches the existing PVC spec and Kubernetes handles the underlying volume expansion.

```yaml
spec:
  databases:
    - name: postgres
      tablegroups:
        - name: default
          shards:
            - name: "0-inf"
              spec:
                pools:
                  main-app:
                    storage:
                      size: "200Gi"  # ← Increase from 100Gi to 200Gi
```

**Requirements:**
- The `StorageClass` must have `allowVolumeExpansion: true`
- Volume expansion is **grow-only** — decreasing `storage.size` is rejected at admission

**Behavior:**
- Most modern CSI drivers (EBS CSI ≥ v1.5, GCE PD CSI) expand the filesystem **online without pod restart**
- For drivers that require restart, the operator detects the `FileSystemResizePending` PVC condition and drains the affected pod automatically

> [!IMPORTANT]
> If your `StorageClass` does not have `allowVolumeExpansion: true`, the Kubernetes API will reject the PVC update and the operator will emit a warning event. Check your StorageClass before changing storage sizes.
