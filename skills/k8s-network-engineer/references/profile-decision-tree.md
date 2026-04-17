# Profile Decision Tree

## Core Profiles

l8k ships with 7 profile definitions. Each profile is matched by comparing the
user's fabric, deployment type, multirail, and Spectrum-X flags against the
profile's `profileRequirements`.

### 1. SR-IOV Ethernet RDMA

- **Directory**: `profiles/sriov-ethernet-rdma/`
- **Requirements**: `fabric=ethernet`, `deployment=sriov`
- **Node capabilities**: `rdma: true`
- **Use cases**: HPC workloads, distributed ML training, low-latency GPU-to-GPU
  communication over Ethernet
- **Performance**: >10 Gbps per VF, hardware-offloaded packet processing
- **Templates**: NicClusterPolicy, IPPool, SriovNetworkNodePolicy, SriovNetwork,
  NicInterfaceNameTemplate, test Pod
- **Keywords**: GPU, ML, AI, SR-IOV, Ethernet, RDMA, HPC, distributed training

### 2. Host Device RDMA

- **Directory**: `profiles/host-device-rdma/`
- **Requirements**: `deployment=host_device`
- **Node capabilities**: `rdma: true`
- **Use cases**: Legacy HPC, DPDK applications, direct PCI device access, workloads
  that need full NIC control
- **Performance**: Full line rate, no virtualization overhead
- **Templates**: NicClusterPolicy, IPPool, HostDeviceNetwork,
  NicInterfaceNameTemplate, test Pod
- **Keywords**: host device, DPDK, direct access, PCI passthrough, legacy

### 3. MacVLAN RDMA Shared

- **Directory**: `profiles/macvlan-rdma-shared/`
- **Requirements**: `fabric=ethernet`, `deployment=rdma_shared`
- **Node capabilities**: `rdma: true`
- **Use cases**: Multi-tenant Ethernet clusters, 10+ pods per node sharing RDMA
  resources, workloads needing network isolation without SR-IOV overhead
- **Performance**: Good throughput with shared RDMA HCA (up to `hcaMax` pods)
- **Templates**: NicClusterPolicy, IPPool, MacvlanNetwork,
  NicInterfaceNameTemplate, test Pod
- **Keywords**: macvlan, shared, multi-tenant, Ethernet, many pods

### 4. IPoIB RDMA Shared

- **Directory**: `profiles/ipoib-rdma-shared/`
- **Requirements**: `fabric=infiniband`, `deployment=rdma_shared`
- **Node capabilities**: `ib: true`, `rdma: true`
- **Use cases**: InfiniBand clusters with shared RDMA, distributed storage over
  IB, multi-pod IB workloads
- **Performance**: >50 Gbps, InfiniBand native performance with sharing
- **Templates**: NicClusterPolicy, IPPool, IPoIBNetwork,
  NicInterfaceNameTemplate, test Pod
- **Keywords**: InfiniBand, IB, IPoIB, shared RDMA, storage

### 5. SR-IOV InfiniBand RDMA

- **Directory**: `profiles/sriov-ib-rdma/`
- **Requirements**: `fabric=infiniband`, `deployment=sriov`
- **Node capabilities**: `ib: true`, `rdma: true`
- **Use cases**: Large-scale HPC, AI/ML training on InfiniBand fabric, highest
  performance IB workloads
- **Performance**: >100 Gbps, hardware-virtualized IB with dedicated VFs
- **Templates**: NicClusterPolicy, IPPool, SriovNetworkNodePolicy,
  SriovIBNetwork, NicInterfaceNameTemplate, test Pod
- **Keywords**: InfiniBand, IB, SR-IOV, HPC, AI training, large-scale

### 6. Spectrum-X Multi-Rail

- **Directory**: `profiles/spectrum-x/`
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.multiplaneMode` in `[swplb, hwplb, uniplane, none]`
- **Node capabilities**: `sriov: true`, `rdma: true`
- **Use cases**: Multi-tenant AI cloud, Spectrum-X ethernet fabric with OVS
  hardware offload, BF3 SuperNIC deployments, CX8 with any multiplane mode
- **Templates**: NicClusterPolicy (with `nicFirmwareStorage` and
  `spectrumXOperator.xPlane`), NicConfigurationTemplate,
  NicInterfaceNameTemplate, CIDRPool (one per rail or per rail-plane in swplb
  with IP placeholders), SpectrumXRailPoolConfig (`v1alpha2`, single resource
  with `railTopology[]` — per-plane entries in swplb, per-rail entries
  otherwise), example DaemonSet
- **Keywords**: Spectrum-X, SPCX, multi-rail, AI cloud, DOCA, swplb, hwplb, uniplane

## Spectrum-X NIC Type Rules

### BlueField-3 SuperNIC (deviceID: a2dc)

- Multiplane mode: **must be `none`**
- Number of planes: **must be 1**
- Single-plane operation only; no multiplane support in BF3 hardware
- Version: always `RA2.2`

### ConnectX-8 (deviceID: 1023) / ConnectX-9 (deviceID: 1025)

- Multiplane modes: `swplb`, `hwplb`, or `uniplane`
- Number of planes:
  - `uniplane`: always 1
  - `swplb`: 2 or 4 (default: 4)
  - `hwplb`: 2 or 4 (default: 4)
- `swplb` is the default mode for CX8 when no explicit mode is given
- Version: always `RA2.2`

### Multiplane Mode Selection Guide

| Mode     | NIC  | Scale          | Resources                | Use When                          |
|----------|------|----------------|--------------------------|-----------------------------------|
| `none`   | BF3  | Any            | Per-rail                 | BF3 SuperNIC deployments          |
| `swplb`  | CX8  | Small-medium   | Per-rail-per-plane       | Default for CX8, finer granularity|
| `hwplb`  | CX8  | Large (2/3-tier)| Per-rail only           | Large-scale multi-tier topologies |
| `uniplane`| CX8 | Simple         | Per-rail                 | Simplest CX8 topology             |

### Number of Planes Rules

| Mode      | Valid Values | Default | Notes                          |
|-----------|-------------|---------|--------------------------------|
| `none`    | 1           | 1       | BF3 only, no planes            |
| `uniplane`| 1           | 1       | Single logical plane            |
| `swplb`   | 2, 4        | 4       | More planes = more granularity  |
| `hwplb`   | 2, 4        | 4       | More planes = more capacity     |

## Keyword Matching Heuristics

When the user describes their workload or environment, use these heuristics to
guide profile selection:

| User Mentions                          | Inferred Setting              |
|---------------------------------------|-------------------------------|
| GPU, ML, AI, training, distributed    | `deployment=sriov`            |
| InfiniBand, IB, IPoIB                 | `fabric=infiniband`           |
| Ethernet, RoCE                        | `fabric=ethernet`             |
| multi-rail, multiple NICs per node    | `multirail=true`              |
| Spectrum-X, SPCX, AI cloud           | `spectrumX=true`              |
| host device, DPDK, passthrough        | `deployment=host_device`      |
| shared, multi-tenant, many pods       | `deployment=rdma_shared`      |
| BlueField, BF3, SuperNIC, DPU        | `spectrumX.nicType=a2dc`      |
| ConnectX-8, CX8                       | `spectrumX.nicType=1023`      |
| ConnectX-9, CX9                       | `spectrumX.nicType=1025`      |

## Decision Flow

1. Is Spectrum-X hardware detected (BF3 SuperNIC or CX8 with multi-rail)?
   - **Before recommending Spectrum-X**: Ask the user if they have a Spectrum-X switch fabric configured (Spectrum-4 switches with appropriate topology). Spectrum-X profiles require specific switch-side configuration that l8k does not manage.
   - If user confirms Spectrum-X fabric → check NIC type → select multiplane mode → Spectrum-X profile
   - If user says no or is unsure → recommend `sriov-ethernet-rdma` as a simpler starting point
2. What is the fabric? `ethernet` or `infiniband`
3. What is the deployment type? `sriov`, `rdma_shared`, or `host_device`
4. Match against profile requirements
5. If no exact match, suggest the closest profile and explain the gap
