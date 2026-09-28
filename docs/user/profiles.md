<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Plan your deployment

A profile maps discovered hardware and user intent to a complete set of Kubernetes manifests. Profile selection is driven by `profile.fabric`, `profile.deployment`, `profile.multirail`, and optional Spectrum-X settings.

Fabric describes the physical transport (`ethernet` or `infiniband`). Deployment type describes how Kubernetes exposes that transport to workloads (`sriov`, `rdma_shared`, or `host_device`).

Decide the platform, operator ownership, and site network inputs before using
the [first deployment walkthrough](quick-start.md). Hardware discovery can
describe NICs; it cannot determine whether a subnet is free, a switch fabric is
configured, or a maintenance window is acceptable.

## Choose a platform and operating model

| Environment | Who installs or applies resources | Continue with |
| --- | --- | --- |
| Kubernetes, Launch Kit managed | `l8k deploy` installs or verifies the selected Network Operator Helm release, applies generated custom resources, and waits for reconciliation. | [First deployment](quick-start.md) |
| Kubernetes, external Helm | Your Helm owner installs the Network Operator. Set `networkOperator.skipHelmChart: true`; Launch Kit still generates, applies, and validates custom resources. | [Generation](../advanced/generation.md#skip-network-operator-helm-artifacts) and [Deployment](../advanced/deployment.md#helm-install-or-upgrade) |
| Kubernetes, external resource application | Your controller owns Helm and custom-resource application. Generate and retain the complete bundle; apply only the intended resources in dependency order. | [CI/CD and GitOps](../integrator/automation.md#gitops-pattern) |
| OpenShift | Install the certified NVIDIA Network Operator, Red Hat SR-IOV Network Operator, NFD, and NVIDIA Maintenance Operator separately. Launch Kit configures the installed operators and generates platform-specific resources; it does not install their Helm charts. | [Deploy on OpenShift](openshift.md) |

The site operator owns the Kubernetes cluster and required API permissions.
The network/fabric owner provides routing, unused address ranges, switch
configuration, and MTU. The driver/operator owner approves changes to driver
modules and maintenance budgets. The application owner selects networks,
resource requests, and workload namespaces. Launch Kit resolves those inputs,
renders resources, and reports its observed deployment checks. A site owner
decides which checks and coverage constitute acceptance for the workload.

## Availability and evidence

| Workflow | Documented implementation | Recorded verification |
| --- | --- | --- |
| Kubernetes standard profiles below | Generation, deployment, and validation paths are documented. | Consult the relevant release/hardware qualification record; this guide does not establish live qualification for every combination. |
| OpenShift SR-IOV Ethernet | OpenShift-specific generation, deployment, and validation. | Exercised on a two-node OpenShift 4.22 cluster. |
| Other OpenShift standard profiles | Rendering and server dry-run. | Live integration qualification on suitable hardware remains to be recorded. |
| OpenShift Spectrum-X | No generated OpenShift variant. | Unavailable; see [OpenShift qualification](openshift.md). |
| Kubernetes Spectrum-X RA2.1, RA2.2, RA2.3 | Release-specific profile generation as shown in the [version matrix](spectrum-x.md#version-matrix). | Confirm the exact hardware, RA, operator release, and validated site profile with the responsible owner. |

Rendering, API dry-run, live deployment, and a formal support commitment are
different evidence. The table describes what this repository documents; it
does not extend qualification to an untested platform or hardware combination.
The reserved `dpf` target has no available lifecycle phases.

## Check prerequisites and site inputs

Before discovery, confirm API access to create its temporary namespace,
cluster-scoped bootstrap RBAC and missing NIC CRDs, run a privileged daemon
with host access on eligible workers, and write the Launch Kit node labels.
Eligible nodes need a discoverable NVIDIA NIC and image-pull access. Discovery
does not require a preinstalled Network Operator or NFD; later deployment and
validation have additional permissions. [Discovery](discovery.md#requirements)
and [OpenShift](openshift.md) give phase-specific details.

Record these decisions before generation and check them again in
`cluster-config.yaml` and the generated effective configuration:

| Decision | Site check before deployment |
| --- | --- |
| Target scope | Intended cluster context, workers, east-west PFs, and any excluded or separately managed cohorts. |
| Fabric and addressing | Ethernet/InfiniBand link layer, switch/VLAN/rail setup, non-overlapping subnet and gateway, routing, and MTU. NV-IPAM only prevents overlap **within one generated bundle**; it cannot reserve other site networks. |
| Workload resources | VF count or shared-device capacity, network names, namespaces, image access, and application placement. |
| Driver and disruption | Current storage/RDMA module users, driver changes, maintenance concurrency, and acceptable node unavailability. The default driver unload controls are enabled; review their effect. |
| Lifecycle owner | Who owns the Network Operator release, custom resources, acceptance report, and eventual removal. |

Local installation and version selection are in [Install Launch Kit](installation.md).
For explicit values and precedence, use the [configuration reference](../reference/configuration.md).

## Profile Matrix

| Profile | Fabric | Deployment | Main resources |
| --- | --- | --- | --- |
| SR-IOV Ethernet RDMA | Ethernet | `sriov` | `SriovNetworkPoolConfig`, `SriovNetworkNodePolicy`, `SriovNetwork`, `NicNodePolicy` |
| SR-IOV InfiniBand RDMA | InfiniBand | `sriov` | `SriovNetworkPoolConfig`, `SriovNetworkNodePolicy`, `SriovIBNetwork`, `NicNodePolicy` |
| Host Device RDMA | Ethernet or InfiniBand | `host_device` | `HostDeviceNetwork`; driver and device plugin remain in the singleton `NicClusterPolicy` |
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

The custom manifest renders as `90-workload-*.yaml` and is applied by
`l8k deploy`. It replaces the default example DaemonSet, so it also removes the
automatic connectivity fixture. Validation only consumes `*example*.yaml`
DaemonSets; provide a separate test fixture when replacing the example.
Kubernetes custom workloads use the first network namespace; OpenShift custom
workloads fan out per shared network bucket and namespace.

For bundle layout and custom workload mutation, see [Manifest Generation](../advanced/generation.md).
