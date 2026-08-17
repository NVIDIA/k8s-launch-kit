<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Deployment Profiles

A profile maps discovered hardware and user intent to a complete set of Kubernetes manifests. Profile selection is driven by `profile.fabric`, `profile.deployment`, `profile.multirail`, and optional Spectrum-X settings.

Fabric describes the physical transport (`ethernet` or `infiniband`). Deployment type describes how Kubernetes exposes that transport to workloads (`sriov`, `rdma_shared`, or `host_device`).

## Profile Matrix

| Profile | Fabric | Deployment | Main resources |
| --- | --- | --- | --- |
| SR-IOV Ethernet RDMA | Ethernet | `sriov` | `SriovNetworkPoolConfig`, `SriovNetworkNodePolicy`, `SriovNetwork`, `NicNodePolicy` |
| SR-IOV InfiniBand RDMA | InfiniBand | `sriov` | `SriovNetworkPoolConfig`, `SriovNetworkNodePolicy`, `SriovIBNetwork`, `NicNodePolicy` |
| Host Device RDMA | Ethernet or InfiniBand | `host_device` | `HostDeviceNetwork`, `NicNodePolicy` |
| Macvlan RDMA shared | Ethernet | `rdma_shared` | `MacvlanNetwork`, RDMA shared device plugin, `NicNodePolicy` |
| IPoIB RDMA shared | InfiniBand | `rdma_shared` | `IPoIBNetwork`, RDMA shared device plugin, `NicNodePolicy` |
| Spectrum-X RA2.1 | Ethernet | `sriov` | RA2.1 SR-IOV operator chain plus v1alpha1 `SpectrumXRailPoolConfig` |
| Spectrum-X RA2.2 | Ethernet | `sriov` | v1alpha2 `SpectrumXRailPoolConfig` |
| Spectrum-X RA2.3 | Ethernet | `sriov` | v1alpha2 `SpectrumXRailPoolConfig` plus Spectrum-X profile ConfigMap |

## Selection Guidance

| Profile | Use when |
| --- | --- |
| SR-IOV Ethernet RDMA | Each pod needs a dedicated Ethernet VF, isolated bandwidth, and direct RDMA. Common for distributed training and HPC. |
| SR-IOV InfiniBand RDMA | Each pod needs a dedicated InfiniBand VF and isolated IB connectivity. |
| Host Device RDMA | A pod needs exclusive use of a physical NIC with minimal virtualization overhead. The device must not be required by the host. |
| Macvlan RDMA shared | Many Ethernet workloads need separate MAC/network namespaces while sharing the host RDMA device. |
| IPoIB RDMA shared | InfiniBand workloads can share the host RDMA device and use IP over InfiniBand rather than dedicated VFs. |
| Spectrum-X | The cluster uses a configured Spectrum-X switch fabric and the selected RA release. |

## Generate A Standard Profile

```bash
# SR-IOV Ethernet
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type sriov --multirail \
  --save-deployment-files ./deployment

# SR-IOV InfiniBand
l8k generate --user-config ./cluster-config.yaml \
  --fabric infiniband --deployment-type sriov --multirail \
  --save-deployment-files ./deployment

# Host device; set fabric explicitly when discovery could not resolve it
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type host_device --multirail \
  --save-deployment-files ./deployment

# Macvlan with shared RDMA
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type rdma_shared --multirail \
  --save-deployment-files ./deployment

# IPoIB with shared RDMA
l8k generate --user-config ./cluster-config.yaml \
  --fabric infiniband --deployment-type rdma_shared --multirail \
  --save-deployment-files ./deployment
```

The saved profile from discovery makes these flags optional. Pass them during generation only when intentionally overriding that file.

After deployment, profile-specific resources should be present:

| Profile | Inspect |
| --- | --- |
| SR-IOV Ethernet | `kubectl get sriovnetworknodepolicy,sriovnetwork -A` |
| SR-IOV InfiniBand | `kubectl get sriovnetworknodepolicy,sriovibnetwork -A` |
| Host device | `kubectl get hostdevicenetwork -A` |
| Macvlan RDMA shared | `kubectl get macvlannetwork -A` |
| IPoIB RDMA shared | `kubectl get ipoibnetwork -A` |

Use `l8k validate` for the deployment acceptance verdict; resource presence alone is not a green light.

## Group Selection

Discovery writes l8k-owned node labels:

| Label | Meaning |
| --- | --- |
| `nvidia.kubernetes-launch-kit.machine` | One source group, value matches the generated `clusterConfig[].identifier` |
| `nvidia.kubernetes-launch-kit.gpu` | All source groups sharing the same GPU type |

Use `--groups` when named source groups require different outputs:

```bash
l8k generate --groups pe-xe9680-h200,ts-sr680a-v3-h200
```

Use `--gpu-type` when all groups with the same GPU type can share a generated bundle:

```bash
l8k generate --gpu-type NVIDIA-H200
```

For automatic merging, strict subset behavior, and resource scope, see [Heterogeneous Clusters](heterogeneous-clusters.md).

## Routing And ARP Tuning

For routed multi-rail IPv4/RoCE deployments, `--routing source-based` adds the `sbr` CNI meta-plugin to non-Spectrum-X secondary networks. Traffic sourced from a rail IP exits through that rail's interface and gateway.

```bash
l8k generate \
  --routing source-based \
  --ignore-arp
```

`--ignore-arp` chains the `tuning` CNI meta-plugin and sets interface-local ARP sysctls to prevent ARP flux between rails. These options apply to SR-IOV, SR-IOV IB, host-device, Macvlan RDMA-shared, and IPoIB RDMA-shared profiles. They do not apply to Spectrum-X.

## Network Namespaces

`--network-namespaces` renders secondary-network CRs and example test DaemonSets once per namespace. Shared resources such as IP pools, node policies, and device-plugin resources are not duplicated.

```bash
l8k generate --network-namespaces default,training,inference
```

## Custom Workloads

Replace the default example DaemonSet with a workload manifest:

```bash
l8k generate --workload-manifest ./workloads/rdma-test-daemonset.yaml
```

`l8k validate` consumes generated example workloads for connectivity tests. `l8k deploy` skips example workloads and applies only the operational manifests.

For bundle layout and custom workload mutation, see [Manifest Generation](../advanced/generation.md).
