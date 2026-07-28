<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Deployment Profiles

A profile maps discovered hardware and user intent to a complete set of Kubernetes manifests. Profile selection is driven by `profile.fabric`, `profile.deployment`, `profile.multirail`, and optional Spectrum-X settings.

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

## Discovery Defaults

Fresh discovery fills missing profile fields:

- `deployment` defaults to `sriov`.
- `multirail` defaults to `true`.
- `fabric` is persisted only when all discovered groups have the same confirmed fabric.
- Explicit CLI flags override both defaults and values from `--user-config`.

```bash
l8k discover \
  --fabric infiniband \
  --deployment-type rdma_shared \
  --multirail=false
```

## Group Selection

Discovery writes l8k-owned node labels:

| Label | Meaning |
| --- | --- |
| `nvidia.kubernetes-launch-kit.machine` | One source group, value `<machineType>-<gpuType>` |
| `nvidia.kubernetes-launch-kit.gpu` | All source groups sharing the same GPU type |

Use `--groups` when named source groups require different outputs:

```bash
l8k generate --groups poweredge-xe9680-h200,thinksystem-sr680a-v3-h200
```

Use `--gpu-type` when all groups with the same GPU type can share a generated bundle:

```bash
l8k generate --gpu-type NVIDIA-H200
```

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
