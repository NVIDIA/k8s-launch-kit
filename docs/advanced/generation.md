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

1. Canonical Launch Kit defaults.
2. Hardware-derived defaults.
3. Values supplied in the configuration file.
4. Explicit CLI flags.

Explicit false, zero and empty values override defaults; YAML `null` means
unset. The selected release supplies catalog-managed coordinates after merging.
See [Configuration](../reference/configuration.md) for the full contract.

Generation leaves the input file unchanged and records the effective result at
`<deployment-dir>/.l8k/resolved-config.yaml`. This includes defaults and explicit
CLI overrides. Retain this metadata with the manifests and omit `--user-config`
from later deploy and validate commands to reuse the rendered configuration.
Passing `--user-config` again intentionally replaces that metadata; the original
file does not contain generation-only CLI overrides. See
[configuration lookup](../reference/configuration.md#deploy-and-validate-config-lookup).
Review both the generated artifacts and effective configuration before deployment.

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

## Review the bundle

Before deploying, inspect both the generated resource directory and
`<deployment-dir>/.l8k/resolved-config.yaml`. Compare them with the approved
site intent:

| Review | Confirm |
| --- | --- |
| Target and profile | Selected operator release, platform flavor, fabric, deployment type, groups, workers, NIC PCI selectors, rails and namespaces. |
| Network | IP pool subnet/gateway/exclusions, MTU, VF count, device resource names, and routing against other site networks and switch configuration. |
| Driver and maintenance | Enabled driver and module-unload controls, current storage/RDMA users, maintenance concurrency and unavailable-node budget. |
| Ownership | `values.yaml` is present only when Launch Kit owns Helm; rendered resource identities match the intended cluster inventory. |
| Validation | Example DaemonSets and selected test families cover the intended workers and rails. |

Generation validates artifact structure; it does not reserve addresses outside
the bundle, ask the Kubernetes API to admit resources, or establish live
readiness. See [Deployment](deployment.md#server-side-dry-run) for the preview
and [Configuration](../reference/configuration.md) for field semantics. Keep
the complete bundle together for later validation. A regeneration replaces its
output directory, so use a new directory when comparing a proposed change
with a deployed baseline.

## Bundle Layout

Launch Kit renders and checks the complete artifact set before replacing the
selected plugin output directory.

Rendered files are structurally checked after ownership annotations and
before the output directory is cleaned. If this check fails, the previous
output remains in place. Successful output keeps the rendered bytes, and a
combined generate/deploy run uses the same checked snapshot. Network Operator
files use this layout:

```text
deployment/
|-- .l8k/
|   `-- resolved-config.yaml
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

The exact files depend on the profile and Helm-management setting:

| Order | Content |
| --- | --- |
| `values.yaml` | Network Operator Helm values consumed by deployment phase 0. Omitted when `networkOperator.skipHelmChart` or `--skip-network-operator-helm` is enabled. |
| `10` | Cluster-wide `NicClusterPolicy`. |
| `11` | Per-group `NicNodePolicy` resources where the release/profile uses them. |
| `20` | NV-IPAM `IPPool` resources. |
| `25` through `40` | NIC naming/configuration templates and SR-IOV pool or node policies. |
| `50` through `80` | Secondary networks, Spectrum-X `CIDRPool`, and rail-pool resources. |
| `85` | Optional Spectrum-X DRA `ResourceClaimTemplate` resources. |
| `40`, `60`, or `90` example | Temporary workload consumed by validation, depending on profile. |

Group and namespace suffixes are added when one render produces multiple copies.

## Skip Network Operator Helm Artifacts

When another system owns the Network Operator Helm release, omit Launch Kit's
Helm input while retaining the generated custom resources:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --skip-network-operator-helm \
  --save-deployment-files ./deployment
```

The equivalent persistent setting is
`networkOperator.skipHelmChart: true`. The plugin output directory is replaced
after successful rendering and structural validation, so a `values.yaml` from an earlier run cannot remain in the
new bundle.

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
  --groups pe-xe9680-h200,ts-sr680a-v3-h200 \
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

Render an application workload instead of the profile's example DaemonSet:

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

The replacement is written as `90-workload-<group>.yaml`, so `l8k deploy`
applies it as an operational resource. It is not a temporary validation
fixture. Inspect the patched manifest before deployment.

On Kubernetes, custom workloads render per source group into the first network
namespace only. Standard secondary networks still fan out to all requested
namespaces. On OpenShift, custom workloads render per shared network bucket
and per network namespace.

Replacing the example removes the default connectivity DaemonSet. Even a custom
DaemonSet in a `90-workload-*.yaml` file is outside connectivity selection.
For connectivity, provide a separate `*example*.yaml` `apps/v1` DaemonSet with
the required route/RDMA containers, namespace, and network resources described
in [Validation](../user/validation.md#connectivity-only-validation). Keep those
fixtures outside generated output and copy them into the manifest directory
after each successful generation, which replaces that directory. Deployment
skips example files; validation temporarily applies their test DaemonSets.
Without a test DaemonSet, default connectivity validation returns a validation
error. `--connectivity=false` runs static validation only and supplies no
data-plane acceptance evidence.

## Generate And Deploy

The separate commands provide the clearest review boundary:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment

l8k deploy \
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
