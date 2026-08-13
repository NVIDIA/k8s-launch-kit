# l8k-config.yaml Schema Reference

This document describes every section and field in the l8k configuration file.
The config file is YAML format and is passed via `--user-config <path>`.

## networkOperator

Controls the NVIDIA Network Operator deployment settings.

| Field              | Type   | Default                              | Description                                    |
|--------------------|--------|--------------------------------------|------------------------------------------------|
| `version`          | string | `v26.1.0`                            | Network Operator Helm chart version            |
| `componentVersion` | string | `network-operator-v26.1.0`           | Component image tag for operator containers    |
| `repository`       | string | `nvcr.io/nvidia/mellanox`            | Container image registry/repository            |
| `namespace`        | string | `nvidia-network-operator`            | Kubernetes namespace for operator resources     |

## networkNamespaces

| Field               | Type     | Default       | Description                                       |
|---------------------|----------|---------------|---------------------------------------------------|
| `networkNamespaces` | []string | `["default"]` | Namespaces the secondary-network CRs (SriovNetwork, SriovIBNetwork, HostDeviceNetwork, MacvlanNetwork, IPoIBNetwork) and their example test DaemonSets are rendered into — one independent copy per namespace. Shared resources (IPPool, NicNodePolicy, SriovNetworkNodePolicy, NicClusterPolicy) are NOT duplicated. With >1 namespace, per-namespace CR copies get a `-<namespace>` name suffix. CLI: `--network-namespaces ns1,ns2`. |

## docaDriver

Controls DOCA/OFED driver deployment in the NicClusterPolicy.

| Field                      | Type   | Default                          | Description                                              |
|----------------------------|--------|----------------------------------|----------------------------------------------------------|
| `enable`                   | bool   | `true`                           | Deploy DOCA driver DaemonSet                             |
| `version`                  | string | `doca3.3.0-26.01-1.0.0.0-0`     | DOCA driver image tag                                    |
| `unloadStorageModules`     | bool   | `true`                           | Unload storage kernel modules before driver load         |
| `enableNFSRDMA`            | bool   | `false`                          | Enable NFS over RDMA support                             |
| `unloadThirdPartyRDMAModules`   | bool   | `true`                           | Unload kernel modules that depend on MLX/OFED drivers    |

When `unloadThirdPartyRDMAModules` is true and dependent modules are discovered,
the generated NicClusterPolicy sets the `UNLOAD_THIRD_PARTY_RDMA_MODULES` env var
to `"true"` (a boolean flag) in the ofedDriver section.

## maintenance

Controls disruptive node-operation concurrency. Defaults are chosen for larger
clusters; tune them to the cluster's actual availability budget.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `maxParallelOperations` | int or percent | `4` | Global Maintenance Operator work limit. Positive integer or `"1%"`-`"100%"`; integer `0` is rejected because the current scheduler yields zero slots. Percentages use all cluster nodes and round up. |
| `maxUnavailable` | int or percent | `4` | Global unavailable-node budget in requestor mode; legacy SR-IOV pool limit before 26.1. Non-negative integer or `"1%"`-`"100%"`; integer `0` pauses new work. In requestor mode, already cordoned or NotReady nodes count toward it. Requestor-mode percentages use all cluster nodes; legacy percentages use the selected pool. Both round down. |
| `maxNodeMaintenanceTimeSeconds` | int | `3600` | Non-negative cleanup delay after a NodeMaintenance request reaches Ready. `0` means immediately garbage-collection eligible, not disabled. Keep below the cluster-autoscaler idle interval. |
| `maxParallelUpgrades` | int | `4` | OFED concurrency before Network Operator 26.1. Non-negative; `0` means unlimited on the legacy path. Ignored by OFED requestor mode in 26.1+. |

Releases 26.1 and newer render these requestor settings in Helm values:

- OFED: `operator.maintenanceOperator.useRequestor: true`.
- SR-IOV: both
  `operator.maintenanceOperator.useDrainControllerRequestor: true` and
  `sriov-network-operator.operator.externalDrainer.enabled: true`.

The two SR-IOV switches are required together. In requestor mode,
`MaintenanceOperatorConfig.spec.maxParallelOperations` and
`spec.maxUnavailable` are global and authoritative;
`SriovNetworkPoolConfig.spec.maxUnavailable` no longer controls draining.
Before 26.1, the SR-IOV internal drainer uses that pool field and OFED uses
`maxParallelUpgrades`.

These requestor switches become Deployment environment variables. Applying
only generated CRs cannot enable them; use `--overwrite-existing` to upgrade an
installed Helm release to changed generated values.

## nvIpam

Controls NV-IPAM (NVIDIA IP Address Management) pool generation.

| Field            | Type     | Default          | Description                                          |
|------------------|----------|------------------|------------------------------------------------------|
| `poolName`       | string   | `nv-ipam-pool`   | Name of the IPPool CR                                |
| `startingSubnet` | string   | `192.168.0.0`    | Base subnet for auto-generation                      |
| `mask`           | int      | `22`             | Subnet mask length for auto-generated subnets        |
| `offset`         | int      | `1`              | Offset from network address for gateway (gateway = network + offset) |
| `reserveFirstIPs`| int      | `0`              | Exclude the first N host addresses of every subnet (network upward) |
| `reserveLastIPs` | int      | `0`              | Exclude the last N host addresses of every subnet (broadcast downward) |
| `subnets`        | list     | `[]` (empty)     | Manual subnet list; takes precedence over auto-generation |

### Auto-Generation (Option 1)

When `subnets` is empty, l8k auto-generates a unique subnet slice for each node
group using `startingSubnet`, `mask`, and `offset`. The gateway is calculated as
the network address plus the offset value.

### Manual Subnets (Option 2)

When `subnets` is non-empty, each entry specifies an explicit subnet and gateway:

```yaml
subnets:
  - subnet: 192.168.2.0/24
    gateway: 192.168.2.1
  - subnet: 192.168.3.0/24
    gateway: 192.168.3.1
```

Manual subnets take precedence over auto-generation.

### IP Exclusions

l8k can populate the IPPool's `spec.exclusions` so reserved addresses (floating
gateway, EVPN endpoints, etc.) are never allocated to pods:

- `reserveFirstIPs` / `reserveLastIPs` — a mask-agnostic pattern applied to
  **every** subnet (auto-generated and manual). On a `/24`, `reserveFirstIPs: 10`
  and `reserveLastIPs: 6` reserve `.0–.9` and `.250–.255`, leaving `.10–.249`.
- Each manual subnet may also carry explicit `exclusions` (`startIP`/`endIP`)
  ranges; the computed reserve ranges are prepended to them.

The gateway is not excluded automatically — it falls inside the low reserve
block. Each auto-generated per-rail subnet gets the reserve ranges computed
relative to its own network address.

```yaml
nvIpam:
  poolName: nv-ipam-pool
  startingSubnet: "192.168.0.0"
  mask: 24
  offset: 1
  reserveFirstIPs: 10
  reserveLastIPs: 6
```

## sriov

SR-IOV device plugin and network policy settings.

| Field          | Type   | Default           | Description                                      |
|----------------|--------|-------------------|--------------------------------------------------|
| `ethernetMtu`  | int    | `9000`            | MTU for Ethernet SR-IOV VFs (jumbo frames)       |
| `infinibandMtu`| int    | `4000`            | MTU for InfiniBand SR-IOV VFs                    |
| `numVfs`       | int    | `8`               | Number of Virtual Functions to create per PF     |
| `priority`     | int    | `90`              | SriovNetworkNodePolicy priority (higher = more specific) |
| `resourceName` | string | `sriov_resource`  | SR-IOV device plugin resource name               |
| `networkName`  | string | `sriov-network`   | SriovNetwork CR name                             |

## hostdev

Host device network settings.

| Field          | Type   | Default             | Description                              |
|----------------|--------|---------------------|------------------------------------------|
| `resourceName` | string | `hostdev_resource`  | Host device plugin resource name         |
| `networkName`  | string | `hostdev-network`   | HostDeviceNetwork CR name                |

## rdmaShared

RDMA shared device plugin settings.

| Field          | Type   | Default                | Description                                          |
|----------------|--------|------------------------|------------------------------------------------------|
| `resourceName` | string | `rdma_shared_resource` | Base resource name; `_rail_0`, `_rail_1` suffixes added for multi-rail |
| `hcaMax`       | int    | `63`                   | Maximum number of pods sharing a single HCA          |

## ipoib

IPoIB network settings.

| Field         | Type   | Default          | Description                                              |
|---------------|--------|------------------|----------------------------------------------------------|
| `networkName` | string | `ipoib-network`  | Base IPoIBNetwork CR name; `-rail-0`, `-rail-1` suffixes for multi-rail |

## macvlan

MacVLAN network settings.

| Field         | Type   | Default           | Description                                               |
|---------------|--------|-------------------|-----------------------------------------------------------|
| `networkName` | string | `macvlan-network`  | Base MacvlanNetwork CR name; `-rail-0`, `-rail-1` suffixes for multi-rail |

## nicConfigurationOperator

NIC Configuration Operator settings for interface naming.

| Field                            | Type   | Default                  | Description                                           |
|----------------------------------|--------|--------------------------|-------------------------------------------------------|
| `deployNicInterfaceNameTemplate` | bool   | `true`                   | Enable NIC interface renaming (see conditions below)  |
| `rdmaPrefix`                     | string | `rdma_r%rail_id%`        | Template for RDMA device names; `%rail_id%` is replaced |
| `netdevPrefix`                   | string | `eth_r%rail_id%`         | Template for network interface names                  |

NIC renaming is only activated when needed:
1. Merged groups have cross-rail PCI address conflicts, OR
2. Deployment is `rdma_shared` and PFs have empty `NetworkInterface` fields

## spectrumX

Spectrum-X specific NIC and overlay configuration.

| Field         | Type   | Default                          | Description                                    |
|---------------|--------|----------------------------------|------------------------------------------------|
| `overlay`     | string | `none`                           | Overlay network type                           |
| `singlePlane` | object | rail-only prefixes                | Prefix block selected by `none` |
| `hwplb`       | object | rail-only RDMA, rail-plane NET    | Prefix block selected by `hwplb` |
| `swplb`       | object | rail-plane RDMA and NET           | Prefix block selected by `swplb` |

Each mode object contains user-overridable `rdmaPrefix` and `netdevPrefix`
strings. Launch Kit renders one `NicConfigurationTemplate` per source group;
its `spec.nicSelector.nicType` and `pciAddresses` come from that group's
east-west PF inventory. North-south PFs do not participate, so a DPU with the
same device ID as a SuperNIC is excluded by the PCI selector.

## profile

Top-level profile selection settings. These can be overridden by CLI flags.

| Field        | Type   | Default    | CLI Override           | Description                        |
|--------------|--------|------------|------------------------|------------------------------------|
| `fabric`     | string | unanimous discovered link type | `--fabric` | `ethernet` or `infiniband`         |
| `deployment` | string | `sriov`    | `--deployment-type`    | `sriov`, `rdma_shared`, `host_device` |
| `multirail`  | bool   | `true` when absent | `--multirail`    | Enable multi-rail networking; explicit false is preserved |

### profile.spectrumX

Spectrum-X sub-section within the profile block. `--spectrum-x <RA-version>` is
the single CLI gateway: a non-empty value sets `enable: true` AND populates
`spcxVersion` with the value passed.

| Field            | Type   | Default  | CLI Override                                         | Description                            |
|------------------|--------|----------|------------------------------------------------------|----------------------------------------|
| `enable`         | bool   | `false`  | derived from `--spectrum-x` (true when value is set) | Enable Spectrum-X profile              |
| `spcxVersion`    | string | `RA2.2`  | value of `--spectrum-x`                              | Spectrum-X RA version. `RA2.2` (Network Operator 26.4+) or `RA2.1` (26.1 only). |
| `multiplaneMode` | string | platform/NIC-derived | `--multiplane-mode`                         | H100/H200/B200/GB200 use `none`; B300/GB300 use GA `swplb`; select `hwplb` explicitly |
| `numberOfPlanes` | int    | platform/NIC-derived | `--number-of-planes`                         | Single-plane platforms use 1; B300/GB300 use 2; pass 4 explicitly for quad-plane B300 |

CLI flags always override config file values for all profile fields.

## clusterConfig

Array of cluster node group configurations. Each entry describes a group of
homogeneous worker nodes with their NIC hardware.

### Group-Level Fields

| Field                      | Type     | Default | Description                                    |
|----------------------------|----------|---------|------------------------------------------------|
| `identifier`               | string   | `""`    | Unique group name (auto-generated during discovery, max 40 bytes) |
| `capabilities.nodes.sriov` | bool     | `true`  | Nodes have SR-IOV capable NICs                 |
| `capabilities.nodes.rdma`  | bool     | `true`  | Nodes have RDMA capable NICs                   |
| `capabilities.nodes.ib`    | bool     | `false` | Nodes have InfiniBand capable NICs             |
| `workerNodes`              | []string | `[]`    | List of node names in this group               |
| `nodeSelector`             | map      | `{}`    | Kubernetes label selector for this group       |

### pfs[] (Physical Functions)

Each PF entry describes one physical NIC port.

| Field              | Type   | Default | Description                                         |
|--------------------|--------|---------|-----------------------------------------------------|
| `deviceID`         | string | --      | PCI device ID (e.g., `1023` for CX8, `a2dc` for BF3) |
| `pciAddress`       | string | --      | PCI bus address (e.g., `0000:05:00.0`)              |
| `rdmaDevice`       | string | `""`    | RDMA device name (e.g., `mlx5_0`); only set for single-node groups |
| `networkInterface` | string | `""`    | Network interface name (e.g., `net1`); only set for single-node groups |
| `traffic`          | string | --      | `east-west` (GPU interconnect) or `north-south` (DPU management) |
| `rail`             | int    | --      | Rail index (0-based); only for east-west PFs        |

**Important notes:**
- `rdmaDevice` and `networkInterface` are only populated for single-node groups;
  multi-node groups leave them empty for safety
- North-south PFs are excluded from rail count and manifest generation
- Rail indices must be contiguous starting from 0
- The `traffic` field is how l8k distinguishes GPU interconnect NICs from DPU
  management NICs

### Example

```yaml
clusterConfig:
  - identifier: "gpu-workers"
    capabilities:
      nodes:
        sriov: true
        rdma: true
        ib: false
    workerNodes:
      - worker-0
      - worker-1
    pfs:
      - deviceID: 1023
        pciAddress: "0000:05:00.0"
        rdmaDevice: ""
        networkInterface: ""
        traffic: east-west
        rail: 0
      - deviceID: 1023
        pciAddress: "0000:75:00.0"
        rdmaDevice: ""
        networkInterface: ""
        traffic: east-west
        rail: 1
      - deviceID: 1023
        pciAddress: "0000:6a:00.0"
        rdmaDevice: ""
        networkInterface: ""
        traffic: north-south
    nodeSelector:
      feature.node.kubernetes.io/pci-15b3.present: "true"
```
