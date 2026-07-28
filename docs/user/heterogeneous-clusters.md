<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Heterogeneous Clusters

Launch Kit can render one coherent deployment for clusters containing multiple server models, GPU types, or NIC layouts. Discovery preserves each hardware layout as a source group; generation merges only groups that can safely share network resources.

## Source Groups

Each `clusterConfig` entry is a source group with its own:

- `identifier`, `machineType`, and `gpuType`.
- Worker-node list and `nvidia.kubernetes-launch-kit.machine` selector.
- East-west PF inventory, rail assignments, and capabilities.
- Storage and third-party RDMA kernel modules.

Inspect the available groups before filtering:

```bash
yq '.clusterConfig[] | {
  identifier,
  machineType,
  gpuType,
  workerNodes,
  nodeSelector
}' cluster-config.yaml
```

## Automatic Merging

Without a filter, generation buckets source groups by:

1. GPU type.
2. Number of east-west rails.

Different machine types can therefore share one generated resource bucket when their GPU type and rail count match. The merged bucket uses the GPU label:

```yaml
nodeSelector:
  nvidia.kubernetes-launch-kit.gpu: NVIDIA-H200
```

Groups with different GPU types or east-west rail counts remain separate. A group with an unresolved GPU type is never automatically merged.

North-south PFs do not contribute to the merge rail count.

## Select Groups

Use `--gpu-type` to render every source group with a matching GPU type. Matching is case-insensitive:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --gpu-type NVIDIA-H200 \
  --save-deployment-files ./deployment
```

Use `--groups` for an exact set of source identifiers. Identifier matching is case-sensitive:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --groups poweredge-xe9680-h200,thinksystem-sr680a-v3-h200 \
  --save-deployment-files ./deployment
```

`--groups` and `--gpu-type` are mutually exclusive. Launch Kit reports an error when a requested identifier or GPU type does not match, including the available values.

## Resource Scope

Launch Kit preserves Kubernetes resource ownership while rendering merged groups:

| Resource behavior | Examples | Render result |
| --- | --- | --- |
| Cluster-wide | `NicClusterPolicy`, Spectrum-X profile `ConfigMap` | One per deployment. |
| Shared per bucket | Secondary-network CRs, `CIDRPool`, `IPPool`, example workload | One per compatible GPU/rail bucket. |
| Node-selecting policy | `NicNodePolicy`, `SriovNetworkNodePolicy`, `SpectrumXRailPoolConfig` | One per bucket, or per source group when a strict subset needs flat selectors. |
| Machine-specific | `NicInterfaceNameTemplate` | One per source group. |

When `--groups` selects only part of an otherwise mergeable bucket, shared resources keep one stable bucket identity while node-selecting policies are rendered for each selected source group. Aggregate workloads and IP pools use a selector expression containing the selected machine-label values.

## PCI Layout Differences

Two source groups can use the same rail number with different PCI addresses. Launch Kit aggregates non-conflicting addresses per rail. When addresses conflict across rails, it can render a separate `NicInterfaceNameTemplate` for each source group so policies can use stable interface names instead of ambiguous PCI addresses.

Review generated interface templates whenever a merged bucket spans different machine types:

```bash
find deployment/network-operator \
  -name '*nic-interface-name-template*.yaml' \
  -print
```

## NV-IPAM Allocation

Subnets are allocated across the final render buckets before templates are written. This keeps generated IP pools disjoint when one cluster produces multiple GPU/rail buckets.

For production, inspect every generated `IPPool` or `CIDRPool` and confirm that it does not overlap the cluster pod, service, management, or external network ranges.

## Operational Guidance

- Re-run discovery after adding a new server model or changing NIC mode.
- Keep the Launch Kit machine and GPU labels; generated selectors depend on them.
- Prefer `--gpu-type` when all matching source groups should share one deployment.
- Prefer `--groups` for staged rollouts or machine-specific targeting.
- Run `l8k deploy --dry-run` before applying a newly merged topology.
- Complete the rollout with `l8k validate` and review the node-groups section of the acceptance report.
