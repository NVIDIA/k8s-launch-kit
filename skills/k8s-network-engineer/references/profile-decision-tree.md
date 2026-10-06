# Profile Decision Tree

## Core Profiles

l8k ships with 8 profile definitions. Each profile is matched by comparing the
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
  NicInterfaceNameTemplate, test DaemonSet
- **Keywords**: GPU, ML, AI, SR-IOV, Ethernet, RDMA, HPC, distributed training

### 2. Host Device RDMA

- **Directory**: `profiles/host-device-rdma/`
- **Requirements**: `deployment=host_device`
- **Node capabilities**: `rdma: true`
- **Use cases**: Legacy HPC, DPDK applications, direct PCI device access, workloads
  that need full NIC control
- **Performance**: Full line rate, no virtualization overhead
- **Templates**: NicClusterPolicy, IPPool, HostDeviceNetwork,
  NicInterfaceNameTemplate, test DaemonSet
- **Keywords**: host device, DPDK, direct access, PCI passthrough, legacy

### 3. MacVLAN RDMA Shared

- **Directory**: `profiles/macvlan-rdma-shared/`
- **Requirements**: `fabric=ethernet`, `deployment=rdma_shared`
- **Node capabilities**: `rdma: true`
- **Use cases**: Multi-tenant Ethernet clusters, 10+ pods per node sharing RDMA
  resources, workloads needing network isolation without SR-IOV overhead
- **Performance**: Good throughput with shared RDMA HCA (up to `hcaMax` pods)
- **Templates**: NicClusterPolicy, IPPool, MacvlanNetwork,
  NicInterfaceNameTemplate, test DaemonSet
- **Keywords**: macvlan, shared, multi-tenant, Ethernet, many pods

### 4. IPoIB RDMA Shared

- **Directory**: `profiles/ipoib-rdma-shared/`
- **Requirements**: `fabric=infiniband`, `deployment=rdma_shared`
- **Node capabilities**: `ib: true`, `rdma: true`
- **Use cases**: InfiniBand clusters with shared RDMA, distributed storage over
  IB, multi-pod IB workloads
- **Performance**: >50 Gbps, InfiniBand native performance with sharing
- **Templates**: NicClusterPolicy, IPPool, IPoIBNetwork,
  NicInterfaceNameTemplate, test DaemonSet
- **Keywords**: InfiniBand, IB, IPoIB, shared RDMA, storage

### 5. SR-IOV InfiniBand RDMA

- **Directory**: `profiles/sriov-ib-rdma/`
- **Requirements**: `fabric=infiniband`, `deployment=sriov`
- **Node capabilities**: `ib: true`, `rdma: true`
- **Use cases**: Large-scale HPC, AI/ML training on InfiniBand fabric, highest
  performance IB workloads
- **Performance**: >100 Gbps, hardware-virtualized IB with dedicated VFs
- **Templates**: NicClusterPolicy, IPPool, SriovNetworkNodePolicy,
  SriovIBNetwork, NicInterfaceNameTemplate, test DaemonSet
- **Keywords**: InfiniBand, IB, SR-IOV, HPC, AI training, large-scale

### 6. Spectrum-X Multi-Rail (RA2.3 and RA2.4)

- **Directory**: `profiles/spectrum-x/` for RA2.3, `profiles/spectrum-x-ra2.4/` for RA2.4
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.spcxVersion` in `[RA2.3, RA2.4]`, `spectrumX.multiplaneMode` in
  `[swplb, hwplb, none]`. RA2.3 requires Network Operator 26.7;
  RA2.4 requires 26.10 or a newer catalogued release and a full doSPCX bundle.
- **Node capabilities**: `sriov: true`, `rdma: true`
- **Use cases**: Multi-tenant AI cloud, Spectrum-X ethernet fabric with OVS
  hardware offload, BF3 SuperNIC deployments, CX8 with any multiplane mode
- **Templates**: NicClusterPolicy (with `nicFirmwareStorage` and
  `spectrumXOperator.xPlane`), NicInterfaceNameTemplate (applied first),
  profile ConfigMap, NicConfigurationTemplate, topology-derived IPv4 or IPv6
  CIDRPool (one per rail or per rail-plane in swplb),
  SpectrumXRailPoolConfig (`v1alpha2`, single resource with `railTopology[]`
  and removed `spec.withBCM` omitted), optional ResourceClaimTemplate,
  example DaemonSet
- **Keywords**: Spectrum-X, SPCX, RA2.3, multi-rail, AI cloud, DOCA, swplb, hwplb

### 7. Spectrum-X Multi-Rail (RA2.2, Network Operator 26.4)

- **Directory**: `profiles/spectrum-x-ra2.2/`
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.spcxVersion=RA2.2`, `spectrumX.multiplaneMode` in
  `[swplb, hwplb, none]`, `minNetworkOperatorRelease=26.4`,
  `maxNetworkOperatorRelease=26.4`
- **Node capabilities**: `sriov: true`, `rdma: true`
- **Use cases**: Spectrum-X RA2.2 deployments on Network Operator 26.4
- **Templates**: NicClusterPolicy, NicInterfaceNameTemplate,
  NicConfigurationTemplate, topology-derived IPv4 or IPv6 CIDRPool,
  v1alpha2 SpectrumXRailPoolConfig, optional ResourceClaimTemplate,
  example DaemonSet
- **Keywords**: Spectrum-X, SPCX, RA2.2, network-operator-26.4, multi-rail, DOCA

### 8. Spectrum-X Multi-Rail (RA2.1, Network Operator 26.1)

- **Directory**: `profiles/spectrum-x-ra2.1/`
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.spcxVersion=RA2.1`, `spectrumX.multiplaneMode` in
  `[swplb, hwplb, none]`, `minNetworkOperatorRelease=26.1`,
  `maxNetworkOperatorRelease=26.1` (pinned to exactly 26.1)
- **Node capabilities**: `sriov: true`, `rdma: true`
- **Use cases**: Spectrum-X deployments on Network Operator 26.1 (where
  the v1alpha2 SpectrumXRailPoolConfig is not yet available). Same use
  cases as the RA2.2 profile but uses the SR-IOV operator's CRD chain
  instead.
- **Templates**: NicClusterPolicy (no `nicFirmwareStorage`, no `xPlane`),
  NicInterfaceNameTemplate (applied first), NicConfigurationTemplate
  (RA2.1), cluster-scoped SriovNetworkPoolConfig (DOCA OVS otherConfig),
  per-rail SriovNetworkNodePolicy (groupingPolicy: `perPF` for swplb
  with no devlinkParams; `all` for hwplb/none with
  `esw_multiport: "true"`), OVSNetwork with `rdma`+`rail` meta-plugins,
  CIDRPool, v1alpha1 glue SpectrumXRailPoolConfig referencing the
  SR-IOV node policy and CIDR pool, example DaemonSet
- **Keywords**: Spectrum-X, SPCX, RA2.1, network-operator-26.1, multi-rail, AI cloud, DOCA, swplb, hwplb

## Spectrum-X NIC Type Rules

### BlueField-3 SuperNIC (deviceID: a2dc)

- Multiplane mode: **must be `none`**
- Number of planes: **must be 1**
- Single-plane operation only; no multiplane support in BF3 hardware
- Version: `RA2.1` on Network Operator 26.1, `RA2.2` on 26.4, `RA2.3` on 26.7, or `RA2.4` on 26.10 and newer catalogued releases

### ConnectX-8 (deviceID: 1023) / ConnectX-9 (deviceID: 1025)

- Multiplane modes: `swplb` or `hwplb`
- Number of planes:
  - `swplb`: 2 or 4 (B300/GB300 default: 2)
  - `hwplb`: 2 or 4 (explicit site-topology choice)
- H100/H200/B200/GB200 default to single-plane `none` / 1 even when the
  east-west NIC is CX8.
- B300/GB300 default to `swplb` / 2. Pass 4 explicitly for quad-plane B300.
- Platform type cannot distinguish `swplb` from `hwplb`; both are supported on
  B300 and GB300, so `hwplb` must be selected explicitly.
- Version: `RA2.1` on Network Operator 26.1, `RA2.2` on 26.4, `RA2.3` on 26.7, or `RA2.4` on 26.10 and newer catalogued releases

### Multiplane Mode Selection Guide

| Mode     | NIC  | Scale          | Resources                | Use When                          |
|----------|------|----------------|--------------------------|-----------------------------------|
| `none`   | BF3/CX7/CX8 | Any      | Per-rail                 | Single-plane GPU platforms        |
| `swplb`  | CX8  | Small-medium   | Per-rail-per-plane       | GA default for B300/GB300         |
| `hwplb`  | CX8  | Large (2/3-tier)| Per-rail only           | Large-scale multi-tier topologies |

### Number of Planes Rules

| Mode      | Valid Values | Default | Notes                          |
|-----------|-------------|---------|--------------------------------|
| `none`    | 1           | 1       | CX7/BF3, no planes             |
| `swplb`   | 2, 4        | 2 on B300/GB300 | Pass 4 explicitly for quad-plane B300 |
| `hwplb`   | 2, 4        | explicit | Select from site topology, not platform type |

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
| BlueField, BF3, SuperNIC, DPU        | east-west PF `deviceID=a2dc`  |
| ConnectX-8, CX8                       | east-west PF `deviceID=1023`  |
| ConnectX-9, CX9                       | east-west PF `deviceID=1025`  |

## Decision Flow

1. Is Spectrum-X hardware detected (BF3 SuperNIC or CX8 with multi-rail)?
   - **Before recommending Spectrum-X**: Ask the user if they have a Spectrum-X switch fabric configured (Spectrum-4 switches with appropriate topology). Spectrum-X profiles require specific switch-side configuration that l8k does not manage.
   - If user confirms Spectrum-X fabric → check NIC type → select multiplane mode → Spectrum-X profile
   - If user says no or is unsure → recommend `sriov-ethernet-rdma` as a simpler starting point
2. What is the fabric? `ethernet` or `infiniband`
3. What is the deployment type? `sriov`, `rdma_shared`, or `host_device`
4. Match against profile requirements
5. If no exact match, suggest the closest profile and explain the gap

## RA2.4 integration

RA2.3 is limited to Network Operator 26.7; RA2.4 defaults to 26.10 and
accepts newer catalogued releases. RA2.4 needs a full doSPCX ConfigMap, preserving
binaryData and annotations and rendering in the resolved operator namespace.
26.10 beta.2 lacks the packaged NCO platformType CRD: verify a compatible NCO
implementation and matching CRDs before deployment. Deploy checks the served
NCT schema after policy bootstrap and blocks additional resource apply when
platformType support is absent or cannot be inspected. External Helm, overwrite,
and dry-run do not bypass this check; dry-run needs APIs installed already.

Derived clusterConfig[].spectrumX.platformType uses the longest case-insensitive
substring from the internal doSPCX platform list in gpuType. No preset or mapping
setting is needed. Discovery saves empty values and warns; generation rejects
unresolved selected RA2.4 groups. RTX/CX8 defaults to none/one plane. Mapping does
not itself qualify hardware. Optional spectrumX.ovsConfig is a string map;
clusterConfig[].spectrumX.swPlaneByRail assigns existing HWPLB rails, defaults to
zero, and must agree across merged groups. Refresh preserves explicit plane
assignments only for unchanged source/rail hardware and verified unchanged worker
membership (ordering does not matter). Changed or unknown membership drops prior
assignments with a review/reapply warning. Every selected RA2.4 source needs complete
dense rail IDs, uniform NICs per rail, and a divisible planes/NIC ratio; merged
sources must have the same effective layout. Merged GPU selectors must not include
excluded same-GPU sources in other buckets; select one source in that case.
One physical NIC cannot span rails
(the current ThinkSystem-SR650-V4-RTX-PRO-6000 preset has this unsupported layout). Multiple hardware partitions
inside one logical rail are outside this layout. Feature gates are not added.
