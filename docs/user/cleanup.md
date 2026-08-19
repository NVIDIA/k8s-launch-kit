<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Remove a Network Operator Deployment

Use `l8k clean` to tear down the Network Operator deployment on one Kubernetes
cluster. The command removes custom resources first so the installed
controllers can process their finalizers, then uninstalls the Helm release.

```bash
l8k clean --kubeconfig ~/.kube/config
```

The command is destructive and asks for confirmation before changing the
cluster. Verify the kubeconfig and the displayed target namespace before
confirming.

The kubeconfig identity must be able to list CRDs cluster-wide, list, get, and
delete the selected custom resources, and manage the Helm release in the target
namespace. Cluster-administrator access normally satisfies this requirement.

## Namespace Resolution

The first available namespace source wins:

1. `--network-operator-namespace <namespace>`
2. `networkOperator.namespace` in `--user-config` or
   `./cluster-config.yaml`
3. `networkOperator.namespace` in an explicit
   `--config-dir/l8k-config.yaml`
4. `nvidia-network-operator`

Custom installation namespaces must be supplied by flag or trusted local
config. Cleanup deliberately does not infer a destructive target from
user-creatable in-cluster objects such as Helm release Secrets. The config is
read only for the namespace; stale release settings elsewhere in the file do
not block cleanup.

```bash
l8k clean --kubeconfig ~/.kube/config \
  --network-operator-namespace operator-system
```

## Deletion Boundary

Cleanup performs these operations in order:

1. Discover every namespaced custom-resource instance in the resolved operator
   namespace and every instance of the Network Operator's known cluster-scoped
   kinds: `HostDeviceNetwork`, `IPoIBNetwork`, `MacvlanNetwork`,
   `NicNodePolicy`, and `NicClusterPolicy`.
2. Send a background deletion request to every discovered custom resource
   before waiting on any one of them. The `NicClusterPolicy` request is sent
   after the other known cluster-scoped kinds.
3. Monitor the complete deletion set until every custom resource is gone,
   including finalizer processing. Sending all requests first prevents one
   CR's finalizer from blocking on another CR that has not entered deletion.
4. Re-scan both scopes and repeat the delete-all-then-wait sequence for any
   custom resources created or exposed during policy teardown.
5. Uninstall the `network-operator` Helm release and wait for Helm-managed
   resources to be removed.

Because step 1 intentionally covers every custom-resource kind, do not keep
unrelated custom resources in the Network Operator namespace. Cluster-scoped
resources cannot be associated with a namespace, so every live instance of
the five listed kinds is removed.

The command preserves:

- The resolved namespace
- CustomResourceDefinitions
- Secrets not owned by the Helm release, including unrelated registry
  credentials
- Custom resources outside the resolved namespace, except the five explicitly
  listed cluster-scoped kinds
- Generated configuration and deployment files on disk

When Helm is uninstalled, Helm release metadata and any Secret rendered as a
chart-managed resource are removed with the release.

Missing CRDs, custom resources, or the Helm release are treated as successful
no-ops, making the command safe to re-run after a partial cleanup.

## Keep the Helm Release

Use `--keep-helm-chart` to remove the custom resources while leaving the
Network Operator release and its chart-managed resources installed:

```bash
l8k clean --kubeconfig ~/.kube/config --keep-helm-chart
```

This option does not narrow the custom-resource deletion boundary.

## Automation

JSON output is non-interactive and auto-confirms the cleanup. Use it only when
the target has already been reviewed:

```bash
l8k clean --kubeconfig ~/.kube/config --output json 2>/dev/null | jq .
```

A successful result includes the resolved namespace, the number of deleted
custom resources, whether Helm removed a release, and whether
`--keep-helm-chart` was requested:

```json
{
  "success": true,
  "phase": "clean",
  "deployed": false,
  "cleanup": {
    "namespace": "nvidia-network-operator",
    "customResourcesDeleted": 12,
    "helmReleaseRemoved": true,
    "keepHelmChart": false
  },
  "messages": []
}
```
