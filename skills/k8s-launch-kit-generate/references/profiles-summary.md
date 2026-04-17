# Profiles Summary

Summary of all 7 l8k profile definitions from `profiles/*/profile.yaml`.

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

## 6. Spectrum-X Multi-Rail

- **Directory**: `profiles/spectrum-x/`
- **Plugin**: network-operator
- **Requirements**: `fabric=ethernet`, `deployment=sriov`, `multirail=true`,
  `spectrumX.multiplaneMode` in `[swplb, hwplb, uniplane, none]`
- **Node Capabilities**: `sriov: true`, `rdma: true`
- **Description**: Unified Spectrum-X profile covering all four multiplane modes.
  Emits a single `SpectrumXRailPoolConfig` (`v1alpha2`) that replaces the
  legacy SriovNetworkPoolConfig + SriovNetworkNodePolicy + OVSNetwork trio.
  In `swplb` the `railTopology[]` splits each rail into per-plane entries; in
  other modes one entry per rail groups all planes. `CIDRPool` manifests are
  generated with IP-address placeholders for the operator to fill in.
- **Templates**:
  - `10-nicclusterpolicy.yaml` -- NicClusterPolicy (with `nicFirmwareStorage`
    and `spectrumXOperator.xPlane`)
  - `30-nicconfigurationtemplate.yaml` -- Spectrum-X firmware settings (RA2.2)
  - `35-nicinterfacenametemplate.yaml` -- Multi-rail interface naming; each
    inner `railPciAddresses` list groups all planes of one rail
  - `60-cidrpool.yaml` -- One CIDRPool per rail (non-swplb) or per rail-plane
    (swplb), with IP placeholders
  - `80-spectrumxrailpoolconfig.yaml` -- Single SpectrumXRailPoolConfig with
    `railTopology[]`
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
