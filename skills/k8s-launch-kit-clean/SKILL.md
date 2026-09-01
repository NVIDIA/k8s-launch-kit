---
name: k8s-launch-kit-clean
description: "Remove an NVIDIA Network Operator deployment from a Kubernetes cluster with l8k clean. Use when the user explicitly asks to uninstall, tear down, remove, reset, or clean a Network Operator installation, delete its custom resources, or keep the Helm chart while clearing Network Operator CRs."
---

# Clean a Network Operator Deployment

Read `k8s-launch-kit-shared` first for binary discovery, JSON output, and error
handling.

## Safety Boundary

Treat cleanup as an irreversible live-cluster mutation. Require explicit user
authority to remove the deployment. Do not infer cleanup authority from a
request to inspect, validate, troubleshoot, or explain the command.

Before running it:

1. Resolve the intended kubeconfig and show its current context.
2. Determine the intended Network Operator namespace.
3. Explain that all namespaced custom resources in that namespace and every
   known cluster-scoped Network Operator CR will be deleted.
4. Confirm whether the Helm release should be uninstalled or retained.

Use read-only checks when the target is not already explicit:

```bash
kubectl --kubeconfig <PATH> config current-context
helm --kubeconfig <PATH> list --all-namespaces --filter '^network-operator$'
```

Stop and ask for direction if the kubeconfig, cluster, namespace, or Helm
retention choice remains ambiguous.

## Run Cleanup

An AI agent uses JSON mode after the user authorizes the exact target:

```bash
l8k clean \
  --kubeconfig <PATH> \
  --network-operator-namespace <NAMESPACE> \
  --output json 2>/dev/null | jq .
```

Omit `--network-operator-namespace` only when the user expects l8k to resolve
it from `--user-config`, `./cluster-config.yaml`, an explicit `--config-dir`,
or the `nvidia-network-operator` default. l8k does not trust in-cluster objects
to select a destructive cleanup target. The same trusted local config supplies
`networkOperator.skipHelmChart`; when true, l8k treats the Helm release as
externally owned and retains it.

To retain the installed release and its chart-managed resources:

```bash
l8k clean \
  --kubeconfig <PATH> \
  --network-operator-namespace <NAMESPACE> \
  --keep-helm-chart \
  --output json 2>/dev/null | jq .
```

JSON mode auto-confirms. Do not invoke it until the read-only target checks and
authorization are complete. `l8k clean` has no dry-run mode.

## Deletion Semantics

The command:

- Deletes every namespaced custom-resource instance in the resolved operator
  namespace.
- Deletes all `HostDeviceNetwork`, `IPoIBNetwork`, `MacvlanNetwork`,
  `NicNodePolicy`, and `NicClusterPolicy` instances cluster-wide.
- Sends deletion requests to the complete namespaced and cluster-scoped set
  before monitoring any CR for finalizer completion, then uses the same
  delete-all-then-wait ordering for re-sweeps.
- Keeps controllers installed until CR deletion and finalizers complete.
- Uninstalls the `network-operator` Helm release last unless
  `networkOperator.skipHelmChart` or `--keep-helm-chart` retains it.
- Preserves the namespace, CRDs, Secrets not owned by Helm, local files, and
  namespaced custom resources elsewhere. Helm metadata and chart-managed
  resources are removed with the release.
- Treats already-missing resources and Helm releases as successful no-ops.

Do not manually strip finalizers as part of the normal workflow. If cleanup
waits indefinitely, inspect the affected CR, operator pods, and events; request
separate authority before forcing finalizer removal.

## Verify the Result

Require `.success == true`, then report:

- `.cleanup.namespace`
- `.cleanup.customResourcesDeleted`
- `.cleanup.helmReleaseRemoved`
- `.cleanup.keepHelmChart`

`cleanup.keepHelmChart` is the effective policy from config plus the explicit
flag, not merely whether `--keep-helm-chart` appeared on the command line.

If the Helm release was meant to be removed, verify it is absent with the same
read-only `helm list` command. If it was kept, verify it remains deployed.
