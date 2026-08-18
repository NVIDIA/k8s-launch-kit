# Profiles Summary

Summary of all 8 l8k profile definitions from `profiles/*/profile.yaml`.

Every profile renders the top-level `maintenance` settings into Helm values.
For Network Operator 26.1+, those values enable Maintenance Operator, deploy a
`MaintenanceOperatorConfig`, enable the OFED requestor in profiles with an OFED
driver, and enable both the external drainer and Network Operator drain
requestor in profiles with SR-IOV Operator. Older releases retain the legacy
direct-drain controllers.

## 1. SR-IOV Ethernet RDMA

- **Directory**: `profiles/sriov-ethernet-rdma/`
- **Plugin**: network-operator
- **Requirements**: `fabric=ethernet`, `deployment=sriov`
- **Node Capabilities**: `rdma: true`
- **Description**: High-performance virtualized networking with hardware acceleration
- **Templates**:
  - `10-nicclusterpolicy.yaml` -- NicClusterPolicy with OFED driver, device plugins
  - `20-ippool.yaml` -- NV-IPAM IPPool for subnet allocation
  - `35-nicinterfacenametemplate.yaml` -- NIC interface naming (conditional)
  - `30-sriovnetworknodepolicy.yaml` -- SR-IOV VF creation policy
  - `40-sriovnetwork.yaml` -- SriovNetwork attachment definition
  - `50-pod.yaml` -- Test pod

## 2. Host Device RDMA

- **Directory**: `profiles/host-device-rdma/`
- **Plugin**: network-operator
- **Requirements**: `deployment=host_device`
- **Node Capabilities**: `rdma: true`
- **Description**: Direct hardware access with minimal CPU overhead
- **Templates**:
  - `10-nicclusterpolicy.yaml`
  - `20-ippool.yaml`
  - `35-nicinterfacenametemplate.yaml`
  - `30-hostdevicenetwork.yaml` -- HostDeviceNetwork attachment definition
  - `40-pod.yaml`

## 3. MacVLAN RDMA Shared

- **Directory**: `profiles/macvlan-rdma-shared/`
- **Plugin**: network-operator
- **Requirements**: `fabric=ethernet`, `deployment=rdma_shared`
- **Node Capabilities**: `rdma: true`
- **Description**: Ethernet networking with shared RDMA resources
- **Templates**:
  - `10-nicclusterpolicy.yaml`
  - `20-ippool.yaml`
  - `35-nicinterfacenametemplate.yaml`
  - `30-macvlannetwork.yaml` -- MacvlanNetwork attachment definition
  - `40-pod.yaml`

## 4. IPoIB RDMA Shared

- **Directory**: `profiles/ipoib-rdma-shared/`
- **Plugin**: network-operator
- **Requirements**: `fabric=infiniband`, `deployment=rdma_shared`
- **Node Capabilities**: `ib: true`, `rdma: true`
- **Description**: InfiniBand networking with shared RDMA resources
- **Templates**:
  - `10-nicclusterpolicy.yaml`
  - `20-ippool.yaml`
  - `35-nicinterfacenametemplate.yaml`
  - `30-ipoibnetwork.yaml` -- IPoIBNetwork attachment definition
  - `40-pod.yaml`

## 5. SR-IOV InfiniBand RDMA

- **Directory**: `profiles/sriov-ib-rdma/`
- **Plugin**: network-operator
- **Requirements**: `fabric=infiniband`, `deployment=sriov`
- **Node Capabilities**: `ib: true`, `rdma: true`
- **Description**: High-performance virtualized InfiniBand networking with hardware acceleration
- **Templates**:
  - `10-nicclusterpolicy.yaml`
  - `20-ippool.yaml`
  - `35-nicinterfacenametemplate.yaml`
  - `30-sriovnetworknodepolicy.yaml`
  - `40-sriovibnetwork.yaml` -- SriovIBNetwork attachment definition
  - `50-pod.yaml`

## 6. Spectrum-X Multi-Rail (RA2.3, Network Operator 26.7+)

- **Directory**: `profiles/spectrum-x/`
- **Plugin**: network-operator
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.spcxVersion=RA2.3`,
  `spectrumX.multiplaneMode` in `[swplb, hwplb, none]`,
  `minNetworkOperatorRelease=26.7`
- **Node Capabilities**: `sriov: true`, `rdma: true`
- **Description**: Unified Spectrum-X profile covering all three multiplane
  modes. It deploys the Spectrum-X profile through a ConfigMap and emits one
  `SpectrumXRailPoolConfig` (`v1alpha2`). In `swplb`, `railTopology[]` splits
  each rail into per-plane entries; in other modes one entry per rail groups
  all planes. The operator uses each topology name for both its
  NetworkAttachmentDefinition and device-plugin resource: `rail0` per rail or
  `rail0p0` per rail-plane.
- **Templates**:
  - `10-nicclusterpolicy.yaml` -- NicClusterPolicy (with `nicFirmwareStorage`
    and `spectrumXOperator.xPlane`)
  - `25-nicinterfacenametemplate.yaml` -- Multi-rail interface naming; each
    inner `railPciAddresses` list groups all planes of one rail. Applied
    **before** the NIC config template so firmware settings reference the
    renamed PFs.
  - `28-spectrumxprofile-configmap.yaml` -- ConfigMap-backed RA2.3 profile
  - `30-nicconfigurationtemplate.yaml` -- Spectrum-X firmware settings (RA2.3)
  - `60-cidrpool.yaml` -- One CIDRPool per rail (non-swplb) or per rail-plane
    (swplb), with topology-derived IPv4 or IPv6 static allocations
  - `80-spectrumxrailpoolconfig.yaml` -- Single SpectrumXRailPoolConfig with
    `railTopology[]`; omits the removed `spec.withBCM` field because current
    v1alpha2 strict decoding rejects it
  - `85-resourceclaimtemplate.yaml` -- Optional DRA workload claims
  - `90-example-daemonset.yaml` -- Example workload

## 7. Spectrum-X Multi-Rail (RA2.2, Network Operator 26.4)

- **Directory**: `profiles/spectrum-x-ra2.2/`
- **Plugin**: network-operator
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.spcxVersion=RA2.2`,
  `spectrumX.multiplaneMode` in `[swplb, hwplb, none]`,
  `minNetworkOperatorRelease=26.4`, `maxNetworkOperatorRelease=26.4`
- **Node Capabilities**: `sriov: true`, `rdma: true`
- **Description**: RA2.2 variant of the consolidated v1alpha2 profile. In
  `swplb`, `railTopology[]` splits each rail into per-plane entries; in other
  modes one entry per rail groups all planes. Generated workloads consume the
  topology names directly (`rail0` or `rail0p0`) for both the network
  annotation and `nvidia.com/<name>` resource request.
- **Templates**:
  - `10-nicclusterpolicy.yaml` -- NicClusterPolicy with Spectrum-X Operator
  - `25-nicinterfacenametemplate.yaml` -- Multi-rail interface naming
  - `30-nicconfigurationtemplate.yaml` -- Spectrum-X firmware settings (RA2.2)
  - `60-cidrpool.yaml` -- One CIDRPool per rail (non-swplb) or per rail-plane
    (swplb), with topology-derived IPv4 or IPv6 static allocations
  - `80-spectrumxrailpoolconfig.yaml` -- Single v1alpha2 rail topology resource
  - `85-resourceclaimtemplate.yaml` -- Optional DRA workload claims
  - `90-example-daemonset.yaml` -- Example workload

## 8. Spectrum-X Multi-Rail (RA2.1, Network Operator 26.1)

- **Directory**: `profiles/spectrum-x-ra2.1/`
- **Plugin**: network-operator
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.spcxVersion=RA2.1`,
  `spectrumX.multiplaneMode` in `[swplb, hwplb, none]`,
  `minNetworkOperatorRelease=26.1`, `maxNetworkOperatorRelease=26.1`
  (pinned to exactly 26.1)
- **Node Capabilities**: `sriov: true`, `rdma: true`
- **Description**: Spectrum-X profile for Network Operator 26.1, where the
  consolidated `SpectrumXRailPoolConfig` v1alpha2 CRD does not yet exist.
  Renders the full SR-IOV operator chain plus a v1alpha1 glue resource:
  cluster-scoped `SriovNetworkPoolConfig` (DOCA OVS hardware-offload
  otherConfig), per-rail `SriovNetworkNodePolicy`, `OVSNetwork` with
  `rdma`+`rail` meta-plugins, nv-ipam `CIDRPool`, and v1alpha1
  `SpectrumXRailPoolConfig` referencing the SR-IOV node policy and CIDR
  pool. Same multiplane modes as the RA2.2 profile (swplb, hwplb, none).
  26.1 NCP shape is leaner: no `nicFirmwareStorage`, no
  `spectrumXOperator.xPlane`.
- **Mode-specific shape**:
  - `swplb` -- `bridge.groupingPolicy: perPF`, single PF per
    SriovNetworkNodePolicy, no `devlinkParams`. Per-plane resources named
    `rail-{i}-plane-{p}`.
  - `hwplb`/`none` -- `bridge.groupingPolicy: all`, all of a
    rail's PFs grouped, plus `devlinkParams.params.esw_multiport: "true"`.
    Per-rail resources named `rail-{i}`.
- **Templates**:
  - `10-nicclusterpolicy.yaml` -- NicClusterPolicy (no `nicFirmwareStorage`,
    no `xPlane`)
  - `25-nicinterfacenametemplate.yaml` -- Multi-rail interface naming
    (applied before the NIC config template).
  - `30-nicconfigurationtemplate.yaml` -- Spectrum-X firmware settings (RA2.1)
  - `40-sriovnetworkpoolconfig.yaml` -- Cluster-scoped SR-IOV pool config
    with DOCA OVS otherConfig
  - `50-sriovnetworknodepolicy.yaml` -- Per-rail (or per rail-plane in
    swplb) SriovNetworkNodePolicy with bridge config
  - `55-ovsnetwork.yaml` -- Matching OVSNetwork with rdma+rail meta-plugins
  - `60-cidrpool.yaml` -- One CIDRPool per rail (or per rail-plane in
    swplb), with topology-derived IPv4 or IPv6 static allocations
  - `80-spectrumxrailpoolconfig.yaml` -- v1alpha1 glue resource referencing
    the SR-IOV node policy and CIDR pool
  - `90-example-daemonset.yaml` -- Example workload

## Profile Matching Logic

l8k selects a profile by matching the user's flags against each profile's
`profileRequirements` field:

1. `fabric` must match (if specified in requirements)
2. `deployment` must match
3. `multirail` must match (if specified)
4. `spectrumX.multiplaneMode` must be in the profile's allowed list (if specified)

The first matching profile is selected. If no profile matches, exit code 2 is
returned with a validation error.

## Template File Conventions

- Files prefixed with numbers (10-, 20-, 30-...) indicate apply ordering
- Files containing "nicinterfacenametemplate" are rendered per original (unmerged)
  group to preserve PCI-to-rail mappings
- DaemonSet files (*-example-daemonset.yaml) are test/example workload DaemonSets
- All templates use Go `text/template` syntax with the full config context
