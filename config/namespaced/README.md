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
