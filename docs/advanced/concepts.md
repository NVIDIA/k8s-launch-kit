<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Concepts

Launch Kit translates discovered server hardware and deployment intent into Network Operator resources. These terms describe the data model used across discovery, generation, deployment, and validation.

## Hardware

| Term | Meaning |
| --- | --- |
| Physical function (PF) | A physical PCI function exposed by a NIC. Launch Kit records its PCI address, device ID, network interface, RDMA device, NUMA node, traffic role, and rail. |
| Virtual function (VF) | A virtual PCI function created from an SR-IOV PF and assigned to a workload. |
| East-west | The workload data-plane path between cluster nodes. East-west PFs determine rails and generated secondary networks. |
| North-south | The path from a node toward management, storage, or external networks. These PFs remain in discovery data but do not create workload rails. |
| Rail | One independent east-west network path. Multirail profiles render resources per rail. |
| Plane | A Spectrum-X subdivision across rails. Plane membership comes from Spectrum-X topology data rather than standard-profile rail discovery. |
| Fabric | The configured link layer, either Ethernet or InfiniBand. Fabric does not describe the Kubernetes device-exposure model. |

## Groups And Buckets

A **source group** is one `clusterConfig` entry produced by discovery. It represents nodes with the same resolved machine type, GPU type, and NIC layout. Its selector uses the `nvidia.kubernetes-launch-kit.machine` label.

A **render bucket** is a generation-time grouping of compatible source groups. Groups with the same GPU type and east-west rail count can share bucket-scoped resources. The merged selector uses the `nvidia.kubernetes-launch-kit.gpu` label.

The distinction matters in heterogeneous clusters: interface naming can remain source-specific while networks, pools, and workloads are shared by a render bucket.

## Deployment Types

| Deployment type | Workload access |
| --- | --- |
| `sriov` | Creates VFs and exposes dedicated devices through SR-IOV networks and device-plugin resources. |
| `host_device` | Moves an entire physical interface into a workload network namespace. |
| `rdma_shared` | Shares the host RDMA device while a Macvlan or IPoIB interface provides workload connectivity. |

Fabric and deployment type are independent inputs. For example, `sriov` can use Ethernet or InfiniBand, while `rdma_shared` maps to Macvlan on Ethernet and IPoIB on InfiniBand.

## Profiles And Presets

A **deployment profile** selects the manifest family from fabric, deployment type, multirail, routing, and optional Spectrum-X settings.

A **topology preset** is a certified hardware description for a known `(machineType, gpuType)` pair. A matching preset can replace discovery heuristics for traffic, rail, NUMA, and GPU affinity only when the discovered PF topology matches completely. A deviation remains live-discovered state and blocks validation acceptance.

## Workflow State

| Artifact | Owner | Role |
| --- | --- | --- |
| `cluster-config.yaml` | Discovery and user | Hardware inventory plus resolved deployment intent. |
| `deployment/network-operator/values.yaml` | Generation | Helm values for the Network Operator release. |
| Generated YAML manifests | Generation | Declarative resources applied in dependency order. |
| Validation HTML report | Validation | Acceptance evidence for release, live resource state, topology, and data-plane checks. |

For the detailed behavior behind these concepts, continue with [Cluster Discovery](../user/discovery.md), [Manifest Generation](generation.md), and [Heterogeneous Clusters](../user/heterogeneous-clusters.md).
