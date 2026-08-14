# Namespaced development operators

This installation mode runs one independently versioned Multigres Operator per
developer namespace. The controller cache and namespaced RBAC prevent an
operator from observing or changing another developer's Multigres resources.

Copy `config/namespaced-example` into a small developer overlay and set:

- `namespace` to the developer namespace;
- `namePrefix` to a unique DNS-safe prefix;
- `images` so `controller:latest` resolves to the developer's operator image.

The base deliberately excludes CRDs and admission webhooks because those are
cluster-scoped. Install CRDs once centrally and run admission as a separately
managed shared service. Branch controllers run with `--webhook-enable=false`.

Each installation creates a uniquely named read-only ClusterRole and
ClusterRoleBinding for Node and StorageClass discovery. All writes are granted
through Roles confined to the developer namespace.

## Migration order

Do not run a cluster-wide controller and a namespaced controller over the same
Multigres resources. For a zero-collision cutover:

1. Render and apply the developer installation with its Deployment scaled to zero.
2. Scale the old cluster-wide controller to zero during a coordinated handoff.
3. Scale each developer controller to one and verify leader election and RBAC.
4. Create new Multigres clusters only after the cluster-wide controller is stopped.

Existing database pods continue serving during this controller handoff; only
reconciliation pauses between steps 2 and 3.
