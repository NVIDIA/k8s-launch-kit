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

## podNamespace

| Field          | Type   | Default   | Description                                       |
|----------------|--------|-----------|---------------------------------------------------|
| `podNamespace` | string | `default` | Namespace for generated pods and network resources |

## docaDriver

Controls DOCA/OFED driver deployment in the NicClusterPolicy.

| Field                      | Type   | Default                          | Description                                              |
|----------------------------|--------|----------------------------------|----------------------------------------------------------|
| `enable`                   | bool   | `true`                           | Deploy DOCA driver DaemonSet                             |
| `version`                  | string | `doca3.3.0-26.01-1.0.0.0-0`     | DOCA driver image tag                                    |
| `unloadStorageModules`     | bool   | `true`                           | Unload storage kernel modules before driver load         |
| `enableNFSRDMA`            | bool   | `false`                          | Enable NFS over RDMA support                             |
| `unloadDependentModules`   | bool   | `true`                           | Unload kernel modules that depend on MLX/OFED drivers    |

When `unloadDependentModules` is true and dependent modules are discovered,
the generated NicClusterPolicy includes `UNLOAD_CUSTOM_MODULES` env var
(space-separated module names) in the ofedDriver section.

## nvIpam

Controls NV-IPAM (NVIDIA IP Address Management) pool generation.

| Field            | Type     | Default          | Description                                          |
|------------------|----------|------------------|------------------------------------------------------|
| `poolName`       | string   | `nv-ipam-pool`   | Name of the IPPool CR                                |
| `startingSubnet` | string   | `192.168.0.0`    | Base subnet for auto-generation                      |
| `mask`           | int      | `22`             | Subnet mask length for auto-generated subnets        |
| `offset`         | int      | `1`              | Offset from network address for gateway (gateway = network + offset) |
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
| `nicType`     | string | `1023`                           | NIC device ID: `1023` (CX8) or `a2dc` (BF3)   |
| `overlay`     | string | `none`                           | Overlay network type                           |
| `rdmaPrefix`  | string | `roce_p%plane_id%_r%rail_id%`    | RDMA device name template with plane and rail  |
| `netdevPrefix`| string | `eth_p%plane_id%_r%rail_id%`     | Network interface name template                |

## profile

Top-level profile selection settings. These can be overridden by CLI flags.

| Field        | Type   | Default    | CLI Override           | Description                        |
|--------------|--------|------------|------------------------|------------------------------------|
| `fabric`     | string | `ethernet` | `--fabric`             | `ethernet` or `infiniband`         |
| `deployment` | string | `sriov`    | `--deployment-type`    | `sriov`, `rdma_shared`, `host_device` |
| `multirail`  | bool   | `false`    | `--multirail`          | Enable multi-rail networking       |
| `ai`         | bool   | `false`    | `--ai`                 | AI workload optimizations          |

### profile.spectrumX

Spectrum-X sub-section within the profile block.

| Field            | Type   | Default  | CLI Override            | Description                            |
|------------------|--------|----------|-------------------------|----------------------------------------|
| `enable`         | bool   | `false`  | `--spectrum-x`          | Enable Spectrum-X profile              |
| `spcxVersion`    | string | `RA2.1`  | `--spcx-version`        | Spectrum-X version (always RA2.1)      |
| `multiplaneMode` | string | `swplb`  | `--multiplane-mode`     | `swplb`, `hwplb`, `uniplane`, or `none`|
| `numberOfPlanes` | int    | `4`      | `--number-of-planes`    | 1, 2, or 4 (also used as pfsPerNic)   |

CLI flags always override config file values for all profile fields.

## clusterConfig

Array of cluster node group configurations. Each entry describes a group of
homogeneous worker nodes with their NIC hardware.

### Group-Level Fields

| Field                      | Type     | Default | Description                                    |
|----------------------------|----------|---------|------------------------------------------------|
| `identifier`               | string   | `""`    | Unique group name (auto-generated during discovery) |
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
