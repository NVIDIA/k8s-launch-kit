<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshooting

Validation is the normal deployment acceptance stage. When it does not produce a green-light report, use its failed checks and captured live state as the starting evidence for troubleshooting.

## Start With The Failed Stage

| Failure area | First checks |
| --- | --- |
| Discovery | Bootstrap pod status, events, image pulls, and `NicDevice` publication in `nvidia-k8s-launch-kit`. |
| Helm chart download | For an HTTP 401 or image-pull-Secret error, verify each configured Secret exists in `networkOperator.namespace`, the kubeconfig can read it, and its Docker config has credentials for the Helm host or `nvcr.io` for an NGC Helm repository. |
| Deploy preflight | Existing Helm values, generated values, stray custom resources, and whether overwrite was explicitly intended. |
| Reconciliation | `NicClusterPolicy`, `NicNodePolicy`, and component pod conditions in the Network Operator namespace. |
| SR-IOV | `SriovNetworkNodeState` sync status, VF totals, node selectors, and advertised resources. |
| Secondary network | NetworkAttachmentDefinition namespace, Multus events, CNI binaries, and resource names. |
| Connectivity | Test DaemonSet pods, routes, interface addresses, RDMA devices, and the failed validation matrix cells. |
| IP allocation | `IPPool` or `CIDRPool` capacity, overlap, static allocations, and NV-IPAM logs. |

## Retain Validation Evidence

Allow in-progress manifests to finish before treating them as failures:

```bash
l8k validate \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --wait 10m
```

Keep the connectivity test DaemonSet for inspection:

```bash
l8k validate \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --keep
```

Use JSON output in automation:

```bash
l8k validate \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --output json 2>/dev/null | jq .
```

The HTML report includes manifest classifications, live YAML, release checks, topology comparisons, and connectivity matrices. Preserve it with the generated bundle.

## Discovery Failures

Discovery always uses `nvidia-k8s-launch-kit`; `--network-operator-namespace` does not change its namespace. Keep the bootstrap resources and inspect them:

```bash
l8k discover \
  --keep-namespace \
  --save-cluster-config ./cluster-config.yaml

kubectl get pods -n nvidia-k8s-launch-kit -o wide
kubectl get events -n nvidia-k8s-launch-kit \
  --sort-by=.metadata.creationTimestamp
kubectl describe pod -n nvidia-k8s-launch-kit <pod-name>
kubectl logs -n nvidia-k8s-launch-kit <pod-name>
```

Common causes include an unavailable daemon image, a missing image pull secret, no Ready schedulable nodes, or no node exposing PCI vendor `0x15b3` through sysfs. NFD labels are not required for discovery scheduling.

## Namespace Mismatch

Generation, deployment, and validation must agree on the Network Operator namespace:

```bash
l8k generate \
  --network-operator-namespace nvidia-network-operator \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment

l8k deploy \
  --network-operator-namespace nvidia-network-operator \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment

l8k validate \
  --network-operator-namespace nvidia-network-operator \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment
```

A mismatch can make Launch Kit report missing operator pods or resources even when they exist in another namespace.

## Deploy Preflight

Preview the Kubernetes apply without persisting changes:

```bash
l8k deploy \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --dry-run
```

Preflight reports installed Helm value drift and unmanaged or stray custom resources that could make the apply ambiguous. Review the diff before using `--overwrite-existing`; the flag authorizes a Helm upgrade to the generated values.

## Operator And Data-Plane Checks

```bash
# Main policy and component reconciliation
kubectl get nicclusterpolicy -o yaml
kubectl get pods -n nvidia-network-operator -o wide
kubectl get events -n nvidia-network-operator \
  --sort-by=.metadata.creationTimestamp

# SR-IOV state and node resources
kubectl get sriovnetworknodestates -A -o yaml
kubectl get nodes -o json |
  jq '.items[] | {name: .metadata.name, allocatable: .status.allocatable}'

# Secondary networks and affected workload events
kubectl get network-attachment-definitions -A
kubectl describe pod -n <workload-namespace> <pod-name>
```

Typical failure patterns:

- OFED or DOCA driver pods in `CrashLoopBackOff`: inspect driver logs for kernel-module dependencies and compare them with `thirdPartyRDMAModules` and unload settings.
- VFs missing or out of sync: compare requested `numVfs` with `SriovNetworkNodeState.status.interfaces[].totalvfs`, then verify policy selectors.
- RDMA resources missing: inspect device-plugin pods and node allocatable resources, then compare the workload request with the generated resource name.
- Secondary network not found: verify the NetworkAttachmentDefinition exists in the workload namespace and references the expected resource.
- IP allocation failure: check pool capacity and overlap with pod, service, management, and external CIDRs.

## Collect A Sosreport

Collect cluster state, CRDs, operator component specifications and logs, events, and node information:

```bash
l8k sosreport \
  --kubeconfig "$KUBECONFIG" \
  --output-dir ./sosreport
```

The kubeconfig can also come from `$KUBECONFIG` or `~/.kube/config`. If the collection script is missing from the source tree or installed share directory, install it with:

```bash
make download-sosreport
```

Start with the diagnostic summary and collection errors, then correlate:

1. Policy and custom-resource conditions.
2. Component pod state and restart counts.
3. Operator and daemon logs.
4. Node labels and allocatable device resources.
5. Namespace events near the failure time.

Share the sosreport only through an approved channel and review it for environment-specific metadata before distribution.

## AI-Assisted Investigation

The repository's `k8s-launch-kit-troubleshoot` skill gives compatible AI agents the same stage-based triage workflow. Provide the validation report, generated manifests, and an approved sosreport rather than only the final error line.

See [AI Skills](../integrator/ai-skills.md) for setup and safety boundaries. The skill does not grant cluster access or authorize changes; credentials and deployment approval remain external.
