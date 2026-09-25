# Topology storage maintenance

Managed global and cell-local topology servers enable etcd MVCC auto-compaction
with a one-hour retention window. The operator compares existing cell and
database metadata before updating it, so an unchanged registration does not
consume a new etcd revision. Changes to owned fields and deleted records are
still reconciled; other writers' fields are preserved.

`spec.topologyPruning` deletes logical topology records that are no longer
desired. It does not compact their revision history or return unused backend
pages to the filesystem. These are separate operations:

| Operation | Effect |
| --- | --- |
| Topology pruning | Removes stale logical records |
| MVCC compaction | Discards superseded history and makes pages reusable |
| Defragmentation | Returns unused backend pages to the filesystem |

Compaction bounds retained history for a bounded workload. The latest revision
number still increases with writes; live data can still grow. Use backend bytes,
bytes in use, memory utilization, and write rate to assess storage health.
[etcd maintenance reference](https://etcd.io/docs/v3.6/op-guide/maintenance/)

## Configuration

Set `maintenance` on the managed `etcd` spec, including a `CoreTemplate`, a
`CellTemplate`'s `localTopoServer`, or a standalone `TopoServer`:

```yaml
spec:
  globalTopoServer:
    etcd:
      maintenance:
        autoCompactionMode: periodic
        autoCompactionRetention: 1h
        quotaBackendBytes: 2147483648
        defragmentationEnabled: false
```

This fragment shows defaults, not a production resource-sizing recommendation.
For cell-local topology, place the same block under
`spec.cells[].spec.localTopoServer.etcd`. External topology is not modified.

| Field | Default | Meaning |
| --- | --- | --- |
| `autoCompactionMode` | `periodic` | `periodic` or `revision` |
| `autoCompactionRetention` | `1h` / `10000` | Positive h/m/s duration in periodic mode; positive revision count in revision mode |
| `quotaBackendBytes` | `2147483648` | Backend quota in bytes, from 1 MiB through 8 GiB |
| `defragmentationEnabled` | `false` | Opt into maintenance of healthy clusters with at least three members |

Global topology template overrides merge explicitly supplied fields. Set both
mode and retention when switching a template's compaction mode or overriding a
revision count. Explicit `false`
disables defragmentation inherited from a template. Local topology follows the
existing whole-block replacement behavior for an inline `localTopoServer`.

## Upgrade and sizing

Applying compaction or quota settings changes the StatefulSet pod template and
causes a rolling update. Apply to healthy topology clusters first. This change
does not implement recovery of an already stalled, fully-down StatefulSet.

The explicit default quota preserves etcd's existing 2 GiB default. It is not
lowered automatically based on memory limits or PVC size: lowering a quota below
an existing database can cause `NOSPACE`. Check current backend size before
choosing a smaller quota. Compaction can reuse pages without reducing the
physical file; defragmentation is needed to reduce that file.

The existing 512 MiB memory limit and 1 GiB resolver storage default are **not a
capacity guarantee for a 2 GiB quota**. Provision disk for backend, WAL, and
maintenance headroom. Size memory using restore-time measurements at the target
backend size; quota is not a reliable memory sizing formula. This change leaves
resource sizing to the separate defaults work.

Watchers resuming before the compaction boundary must re-list current state and
restart their watches. Choose a retention window that accommodates expected
disconnects, and validate custom topology consumers before shortening it. The
Multigres topology watch retry helpers obtain a fresh snapshot on reconnect.

## Automatic defragmentation

When enabled, the TopoServer controller checks for maintenance at least hourly
and defragments at most one member per hour. A candidate must have both at least
100 MiB and 30% reclaimable space. It requires:

- At least three expected voting members, all reporting the same cluster and
  leader, with successful linearizable reads through every member.
- Every owned pod Ready for at least one minute, with no terminating pod or
  pending StatefulSet rollout.
- No member reporting an etcd error, including a backend quota alarm.

The controller transfers leadership before defragmenting a leader and verifies
health again before and after maintenance. It does not delete pods, alter etcd
membership, or delete PVCs. Single-member topology requires planned manual
maintenance because defragmentation blocks the member being processed.

`status.etcdMaintenance` reserves the target using a Kubernetes resource-version
precondition before any maintenance mutation. This reservation and the minimum
interval survive operator restarts. An interrupted or timed-out request remains
in progress: pod rollouts pause until the operation deadline has passed and all
members pass direct health checks. An `EtcdDefragmented` event records success;
`EtcdMaintenanceFailed` records a failed maintenance attempt.

If maintenance cannot recover its reservation, inspect etcd member health and
the operator error. After the two-minute operation deadline, explicitly setting
`defragmentationEnabled: false` releases an abandoned reservation and permits a
corrected workload configuration to roll out. This does not cancel a server-side
defragmentation already underway; verify member health before manual disruption.

Managed topology TLS is supported using the TopoServer's existing certificate
with client-auth usage and its CA. The operator needs network access to each
member's client endpoint. It does not run maintenance against external topology.
