<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Manifest Generation

`l8k generate` resolves a profile against `cluster-config.yaml` and renders a reviewable deployment bundle. Generation does not require cluster access unless `--deploy` is also set.

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment
```

## Configuration Resolution

Profile settings use this precedence:

1. Hardware-derived values and Launch Kit defaults.
2. Values persisted in the configuration file.
3. Explicit CLI flags.

When generation uses a file-backed config, resolved defaults and CLI overrides are written back to the same file while comments and file permissions are preserved. This makes the reviewed file the input for deploy and validation.

If hardware groups disagree on fabric, or discovery cannot resolve a configured link layer, generation requires `--fabric`.

## Profile Selection

Use the profile persisted by discovery, or override it:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./deployment
```

See [Deployment Profiles](../user/profiles.md) for the fabric and deployment-type matrix. See [Spectrum-X](../user/spectrum-x.md) for the Spectrum-X cohort flags and required RA2.3 inputs.

## Bundle Layout

Launch Kit cleans the selected plugin output directory before every render and writes Network Operator files under:

```text
deployment/
`-- network-operator/
    |-- values.yaml
    |-- 10-nicclusterpolicy.yaml
    |-- 11-nicnodepolicy-<group>.yaml
    |-- 20-ippool-<group>.yaml
    |-- 30-*.yaml
    |-- 40-*.yaml
    |-- 50-*.yaml
    `-- 60-example-daemonset-<group>.yaml
```

The exact files depend on the profile:

| Order | Content |
| --- | --- |
| `values.yaml` | Network Operator Helm values consumed by deployment phase 0. |
| `10` | Cluster-wide `NicClusterPolicy`. |
| `11` | Per-group `NicNodePolicy` resources where the release/profile uses them. |
| `20` | NV-IPAM `IPPool` resources. |
| `25` through `40` | NIC naming/configuration templates and SR-IOV pool or node policies. |
| `50` through `80` | Secondary networks, Spectrum-X `CIDRPool`, DRA, and rail-pool resources. |
| `40`, `60`, or `90` example | Temporary workload consumed by validation, depending on profile. |

Group and namespace suffixes are added when one render produces multiple copies.

## Generate Without Discovery

Use a known topology preset when cluster access is unavailable:

```bash
l8k generate \
  --for PowerEdge-XE9680-H200 \
  --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
  --fabric ethernet \
  --deployment-type sriov \
  --save-deployment-files ./deployment
```

`--node-selector` is required because a preset has no live worker-node list. See [Topology Presets](../user/presets.md).

## Limit The Hardware Cohort

```bash
# All source groups for one GPU type
l8k generate \
  --user-config ./cluster-config.yaml \
  --gpu-type NVIDIA-H200 \
  --save-deployment-files ./deployment-h200

# An explicit source-group subset
l8k generate \
  --user-config ./cluster-config.yaml \
  --groups poweredge-xe9680-h200,thinksystem-sr680a-v3-h200 \
  --save-deployment-files ./deployment-stage
```

`--gpu-type` is case-insensitive. `--groups` identifiers are case-sensitive. The flags are mutually exclusive and an empty match fails generation. See [Heterogeneous Clusters](../user/heterogeneous-clusters.md) for the render-scope rules.

## Multiple Network Namespaces

Render secondary-network CRs and example test DaemonSets into more than one workload namespace:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --network-namespaces default,training,inference \
  --save-deployment-files ./deployment
```

Launch Kit creates an independent secondary-network and example-workload copy per namespace. Cluster-wide and shared resources such as `NicClusterPolicy`, node policies, `IPPool`, and `CIDRPool` are not duplicated.

Spectrum-X resources render into the first configured network namespace.

## Custom Workload Manifest

Replace the profile's example DaemonSet:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --workload-manifest ./workloads/rdma-test.yaml \
  --save-deployment-files ./deployment
```

Supported workload kinds are `Pod`, `Deployment`, `DaemonSet`, `StatefulSet`, `Job`, and `ReplicaSet`. Launch Kit:

- Sets the selected network namespace.
- Adds a group suffix to the workload name.
- Adds the Multus network annotation.
- Adds network resource requests and limits to the first container.
- Adds required node affinity for the render group.

For example, a two-rail SR-IOV workload receives:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: sriov-network-rail-0,sriov-network-rail-1
spec:
  containers:
    - name: rdma-app
      resources:
        requests:
          nvidia.com/sriov_resource_rail_0: "1"
          nvidia.com/sriov_resource_rail_1: "1"
        limits:
          nvidia.com/sriov_resource_rail_0: "1"
          nvidia.com/sriov_resource_rail_1: "1"
```

Review the rendered manifest after patching. `l8k deploy` skips files with `example` in the filename, while `l8k validate` applies them temporarily for connectivity checks.

## Generate And Deploy

The separate commands provide the clearest review boundary:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment

l8k deploy \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment
```

For a single invocation:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment \
  --deploy \
  --kubeconfig "$KUBECONFIG"
```

Add `--dry-run` for a server-side preview and `--overwrite-existing` only after reviewing preflight drift.
