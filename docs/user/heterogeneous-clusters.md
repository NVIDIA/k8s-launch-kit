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

Fresh discovery derives `identifier` from the machine/GPU identity and bounds
it to 40 bytes with a deterministic hash suffix when the natural value is
longer. Use the persisted identifier shown in `cluster-config.yaml` with
`--groups`.

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

## Example Cluster Shapes

### Two GPU Types

`--gpu-type NVIDIA-H200` selects every H200 source group and excludes the H100 group. The generated deployment still has one cluster-wide `NicClusterPolicy`; bucket-scoped resources target only the selected H200 cohort.

<figure class="hetero-figure">
  <figcaption>Filter generation to one GPU type</figcaption>
  <div class="hetero-flow">
    <div class="hetero-stack">
      <span class="hetero-label">Discovered source groups</span>
      <div class="hetero-node hetero-node--excluded">
        <strong>Group A</strong>
        <span>DGX-B200 + H100</span>
        <small>Filtered out</small>
      </div>
      <div class="hetero-node hetero-node--selected">
        <strong>Group B</strong>
        <span>PowerEdge-XE9680 + H200</span>
        <small>Selected by GPU type</small>
      </div>
    </div>
    <span class="hetero-arrow" aria-hidden="true">&#8594;</span>
    <div class="hetero-stack">
      <span class="hetero-label">Generated scope</span>
      <div class="hetero-node hetero-node--selected">
        <strong>NVIDIA-H200 bucket</strong>
        <span>H200 node-selecting policies</span>
        <span>H200 network and IP pool</span>
        <small>NicClusterPolicy remains cluster-wide</small>
      </div>
    </div>
  </div>
</figure>

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --gpu-type NVIDIA-H200 \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./deployment-h200
```

### Same GPU, Different Servers

Three machine types with the same GPU type and east-west rail count combine into one render bucket. An unfiltered run uses the shared GPU label. A strict-subset `--groups` rollout keeps bucket-shared resources together but renders flat-selector policies for only the selected sources.

<figure class="hetero-figure">
  <figcaption>Combine compatible source groups into one render bucket</figcaption>
  <div class="hetero-flow">
    <div class="hetero-stack">
      <span class="hetero-label">H200 source groups</span>
      <div class="hetero-node">
        <strong>Source A</strong>
        <span>DGX-B200</span>
        <small>machine label A</small>
      </div>
      <div class="hetero-node">
        <strong>Source B</strong>
        <span>ThinkSystem-SR680a-V3</span>
        <small>machine label B</small>
      </div>
      <div class="hetero-node">
        <strong>Source C</strong>
        <span>PowerEdge-XE9680</span>
        <small>machine label C</small>
      </div>
    </div>
    <span class="hetero-arrow" aria-hidden="true">&#8594;</span>
    <div class="hetero-stack">
      <span class="hetero-label">One compatible bucket</span>
      <div class="hetero-node hetero-node--selected">
        <strong>NVIDIA-H200</strong>
        <span>Same east-west rail count</span>
        <span>Shared network, resource name, and IP pool</span>
        <small>GPU selector for the full bucket</small>
      </div>
    </div>
  </div>
</figure>

```bash
# Full H200 bucket
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment

# Stage two of the three source groups
l8k generate \
  --user-config ./cluster-config.yaml \
  --groups dgx-b200-nvidia-h200,thinksystem-sr680a-v3-nvidia-h200 \
  --save-deployment-files ./deployment-stage1
```

### Mixed GPU And Server Matrix

In a four-group cluster, the default run renders an H100 bucket and an H200 bucket. A GPU filter selects a row of the matrix; an explicit group list can select a vendor-specific column across GPU types.

<figure class="hetero-figure">
  <figcaption>Choose a cohort from a mixed cluster</figcaption>
  <div class="hetero-mixed">
    <div>
      <span class="hetero-label">Four discovered source groups</span>
      <div class="hetero-matrix">
        <div class="hetero-node hetero-node--group-filter">
          <strong>G1</strong>
          <span>DGX-B200 + H100</span>
        </div>
        <div class="hetero-node">
          <strong>G2</strong>
          <span>ThinkSystem + H100</span>
        </div>
        <div class="hetero-node hetero-node--selected hetero-node--group-filter">
          <strong>G3</strong>
          <span>DGX-B200 + H200</span>
        </div>
        <div class="hetero-node hetero-node--selected">
          <strong>G4</strong>
          <span>PowerEdge-XE9680 + H200</span>
        </div>
      </div>
      <div class="hetero-legend">
        <span><i class="hetero-swatch hetero-swatch--gpu"></i> <code>--gpu-type NVIDIA-H200</code>: G3 + G4</span>
        <span><i class="hetero-swatch hetero-swatch--group"></i> <code>--groups</code> DGX sources: G1 + G3</span>
      </div>
    </div>
    <div class="hetero-scope">
      <span class="hetero-label">Resulting buckets</span>
      <div><code>default</code><span>H100: G1 + G2<br>H200: G3 + G4</span></div>
      <div><code>--gpu-type</code><span>H200: G3 + G4</span></div>
      <div><code>--groups</code><span>H100: G1<br>H200: G3</span></div>
    </div>
  </div>
</figure>

Use `--gpu-type` when all nodes of one GPU type are the deployment unit. Use `--groups` for a staged rollout, vendor-specific cohort, or CI matrix with an explicit list of source identifiers.

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

## East-West And North-South NICs

Only east-west PFs produce rails and workload networking resources. North-south PFs remain visible in a mixed source group's inventory but are excluded from generated manifests. A source group containing only north-south PFs is omitted from `cluster-config.yaml`.

<figure class="hetero-figure">
  <figcaption>Traffic direction determines whether a PF is rendered</figcaption>
  <div class="hetero-traffic">
    <div class="hetero-traffic__row">
      <div class="hetero-endpoint">Management / OOB network</div>
      <span class="hetero-arrow" aria-hidden="true">&#8596;</span>
      <div class="hetero-node hetero-node--north-south">
        <strong>BlueField DPU</strong>
        <span>North-south PF</span>
        <small>Inventory only; not rendered</small>
      </div>
    </div>
    <div class="hetero-traffic__row">
      <div class="hetero-endpoint">GPU mesh and cluster nodes</div>
      <span class="hetero-arrow" aria-hidden="true">&#8596;</span>
      <div class="hetero-node hetero-node--selected">
        <strong>ConnectX NIC or BlueField SuperNIC</strong>
        <span>East-west PF</span>
        <small>Assigned a rail and rendered</small>
      </div>
    </div>
  </div>
</figure>

## Operational Guidance

- Re-run discovery after adding a new server model or changing NIC mode.
- Keep the Launch Kit machine and GPU labels; generated selectors depend on them.
- Prefer `--gpu-type` when all matching source groups should share one deployment.
- Prefer `--groups` for staged rollouts or machine-specific targeting.
- Run `l8k deploy --dry-run` before applying a newly merged topology.
- Complete the rollout with `l8k validate` and review the node-groups section of the acceptance report.
