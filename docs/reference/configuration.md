<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Configuration File

`cluster-config.yaml` is both an input and an output. Discovery writes it, generation reads and updates it, and deploy/validate use it to resolve release and namespace context.

Configuration source precedence is:

1. Embedded defaults.
2. `--config-dir/l8k-config.yaml`, when selected.
3. `--user-config`.
4. Resolved hardware defaults for missing profile fields.
5. Explicit CLI flags.

`--config-dir/presets/` replaces the embedded preset catalog. It does not merge with it.

## Top-Level Sections

| Section | Purpose |
| --- | --- |
| `networkOperator` | Release line, image repositories, Helm repository, namespace, and image pull secrets. |
| `networkNamespaces` | Namespaces that receive secondary network CRs and example DaemonSets. |
| `workload` | Optional custom workload manifest. |
| `validation` | Connectivity mode, enabled checks, and RDMA parameters. |
| `docaDriver` | DOCA driver version and module unload behavior. |
| `maintenance` | Maintenance Operator and upgrade concurrency limits. |
| `nvIpam` | IPPool subnet generation, manual subnets, and exclusions. |
| `sriov`, `hostdev`, `rdmaShared`, `ipoib`, `macvlan` | Profile-specific resource and network naming. |
| `nicConfigurationOperator` | Interface and RDMA device naming templates. |
| `spectrumX` | Spectrum-X naming defaults. |
| `profile` | Selected fabric, deployment, routing, ARP, and Spectrum-X options. |
| `clusterConfig` | Discovered or preset-provided hardware groups. |

## Release Catalog

```yaml
networkOperator:
  selectedRelease: "26.4"
```

Supported release lines are currently:

- `26.1`
- `26.4`
- `26.7`

The release line fills Network Operator versions, component image tags, DOCA driver version, repositories, Helm repository URL, and version-gated template behavior. Spectrum-X releases also carry a manually maintained xPlane repository and version used in `spectrumXOperator.xPlane` instead of reusing the generic component tag.

## Network Operator

| Field | Meaning |
| --- | --- |
| `selectedRelease` | Catalog release line. Equivalent to `--network-operator-release`. |
| `version` | Network Operator chart/operator version. Filled by the selected release. |
| `componentVersion` | Tag used by managed component images. |
| `repository` | Registry path for component images such as drivers, CNI, IPAM, and device plugins. |
| `operatorRepository` | Registry path for the Network Operator controller image. |
| `helmRepoURL` | Chart repository used by `l8k deploy`. Empty means Helm phase 0 is skipped. |
| `namespace` | Namespace for the Helm release and namespaced Network Operator resources. |
| `imagePullSecrets` | Secret names propagated into `NicClusterPolicy` and per-group `NicNodePolicy` specifications. |

When `selectedRelease` is set, catalog values replace explicit version and repository fields so the cohort remains consistent.

## Network And Workload Namespaces

```yaml
networkNamespaces:
  - default
  - training
  - inference

workload:
  manifest: ./workloads/rdma-test.yaml
```

Secondary-network CRs and example workloads render once per network namespace. Shared policy and pool resources do not. `workload.manifest` is equivalent to `--workload-manifest`.

## Validation

```yaml
validation:
  connectivity: true
  mode: strict
  checks:
    - icmp
    - rping
    - ib_write_bw
  rdma:
    rpingIterations: 5
    ibWriteSize: 65536
    ibWriteMinBandwidthGbps: 100
```

| Field | Meaning |
| --- | --- |
| `connectivity` | Enables the data-plane stage. Static acceptance checks always run. |
| `mode` | `quick`, `full`, or `strict`. |
| `checks` | Any combination of `icmp`, `rping`, and `ib_write_bw`; an empty list disables all connectivity test families. |
| `rdma.rpingIterations` | Client iterations for each `rping` test. |
| `rdma.ibWriteSize` | Message size passed to `ib_write_bw`. |
| `rdma.ibWriteMinBandwidthGbps` | Minimum observed peak bandwidth. Set `0` to disable the bandwidth gate. |

## DOCA Driver

```yaml
docaDriver:
  enable: true
  version: <catalog-version>
  unloadStorageModules: true
  enableNFSRDMA: false
  unloadThirdPartyRDMAModules: true
  skipPreflightChecks: false
```

| Field | Meaning |
| --- | --- |
| `enable` | Include DOCA/OFED driver configuration. |
| `version` | Driver image tag; filled by the selected release catalog. |
| `unloadStorageModules` | Allow unload of storage-over-RDMA dependencies before driver replacement. |
| `unloadThirdPartyRDMAModules` | Allow unload of non-MLX RDMA dependencies before driver replacement. |
| `enableNFSRDMA` | Enable NFS-over-RDMA support. |
| `skipPreflightChecks` | Skip the init-container module dependency check. |

Both unload controls default to `true`. Discovery does not populate the holder-module lists automatically. Confirm that no active workload depends on a module that the driver flow will unload.

## Maintenance

The top-level `maintenance` fields control Maintenance Operator and legacy upgrade concurrency:

```yaml
maintenance:
  maxParallelOperations: 4
  maxUnavailable: 4
  maxNodeMaintenanceTimeSeconds: 3600
  maxParallelUpgrades: 4
```

Values can be integers or percentages where supported. See [Maintenance](../user/maintenance.md) for release-specific requestor behavior.

## NV-IPAM

Auto-generate per-group subnets:

```yaml
nvIpam:
  poolName: nv-ipam-pool
  startingSubnet: "192.168.0.0"
  mask: 22
  offset: 1
  reserveFirstIPs: 10
  reserveLastIPs: 6
```

Or list subnets manually:

```yaml
nvIpam:
  subnets:
    - subnet: 192.168.2.0/24
      gateway: 192.168.2.1
      exclusions:
        - {startIP: 192.168.2.2, endIP: 192.168.2.3}
```

Reserved first/last IPs are merged with explicit exclusions for every subnet.

| Field | Meaning |
| --- | --- |
| `poolName` | Base name for generated `IPPool` resources. |
| `startingSubnet` | Aligned IPv4 network address for automatic allocation. |
| `mask` | Automatic subnet prefix length, from `/1` through `/30`. |
| `offset` | Number of subnet-sized blocks between allocations; minimum `1`. |
| `reserveFirstIPs` / `reserveLastIPs` | Host addresses excluded from every automatic or manual subnet. |
| `subnets` | Explicit `{subnet, gateway, exclusions}` entries. A non-empty list takes precedence over automatic fields. |

Automatic subnet allocation is precomputed across all final heterogeneous render buckets to avoid overlap within the generated bundle.

## Profile Resource Settings

| Section | Fields |
| --- | --- |
| `sriov` | `ethernetMtu`, `infinibandMtu`, `numVfs`, `priority`, `resourceName`, `networkName`. |
| `hostdev` | `resourceName`, `networkName`. |
| `rdmaShared` | `resourceName`, `hcaMax`. |
| `ipoib` | `networkName`. |
| `macvlan` | `networkName`. |

When multirail is enabled, Launch Kit adds rail suffixes to generated resource and network names.

## NIC Naming

```yaml
nicConfigurationOperator:
  deployNicInterfaceNameTemplate: true
  rdmaPrefix: "rdma_r%rail_id%"
  netdevPrefix: "eth_r%rail_id%"
  updateFW: false

spectrumX:
  nicType: "1023"
  overlay: "none"
  rdmaPrefix: "roce_p%plane_id%_r%rail_id%"
  netdevPrefix: "eth_p%plane_id%_r%rail_id%"
```

| Field | Meaning |
| --- | --- |
| `deployNicInterfaceNameTemplate` | Allows per-source interface-name templates when profile or PCI-layout rules require stable names. |
| `rdmaPrefix` / `netdevPrefix` | Standard-profile names. Multirail values require a rail placeholder. |
| `updateFW` | Enables firmware staging storage in generated Network Operator configuration. |
| `spectrumX.nicType` | Device ID: `1021` ConnectX-7, `1023` ConnectX-8, `1025` ConnectX-9, or `a2dc` BlueField-3 SuperNIC. |
| `spectrumX.overlay` | Spectrum-X overlay mode. |
| `spectrumX.rdmaPrefix` / `netdevPrefix` | Spectrum-X names with plane and rail placeholders when the profile requires them. |

## Profile

```yaml
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  routing: destination-based
  ignoreARP: false
  spectrumX:
    enable: false
    spcxVersion: RA2.3
    multiplaneMode: swplb
    numberOfPlanes: 4
    topologyType: 2-tier
    ipVersion: ipv4
    topologyFile: ./topology.json
    configMapName: site-ra23-profile
    useDRA: false
```

| Field | Meaning |
| --- | --- |
| `fabric` | `ethernet` or `infiniband`. |
| `deployment` | `sriov`, `rdma_shared`, or `host_device`. |
| `multirail` | Enable more than one east-west rail. An explicit `false` is preserved. |
| `routing` | `destination-based` or `source-based`; source-based adds the `sbr` CNI plugin outside Spectrum-X. |
| `ignoreARP` | Adds per-interface ARP tuning outside Spectrum-X. |
| `spectrumX.enable` | Select a Spectrum-X profile. |
| `spectrumX.spcxVersion` | `RA2.1`, `RA2.2`, or `RA2.3`. |
| `spectrumX.multiplaneMode` | `none`, `swplb`, or `hwplb`. |
| `spectrumX.numberOfPlanes` | `1`, `2`, or `4`. |
| `spectrumX.topologyType` | `2-tier` or `3-tier`. |
| `spectrumX.ipVersion` | `ipv4` or `ipv6`; CIDRPool rendering currently supports IPv4. |
| `spectrumX.hostFirstOctet` | Config-only first octet for generated IPv4 topology addressing. |
| `spectrumX.topologyFile` | Path to `spcx-gen` format topology JSON. Relative paths resolve from the config file. |
| `spectrumX.configMapName` / `profile` | RA2.3 ConfigMap name and embedded profile data. |
| `spectrumX.useDRA` | Render DRA `ResourceClaimTemplate` workload allocation. |

## Hardware Groups

Each `clusterConfig` entry represents one source group:

```yaml
clusterConfig:
  - identifier: poweredge-xe9680-h200
    machineType: PowerEdge-XE9680
    gpuType: NVIDIA-H200
    capabilities:
      nodes:
        sriov: true
        rdma: true
        ib: false
    nodeSelector:
      nvidia.kubernetes-launch-kit.machine: PowerEdge-XE9680-NVIDIA-H200
    pfs:
      - deviceID: a2dc
        pciAddress: 0000:1a:00.0
        rdmaDevice: rocep26s0f0
        networkInterface: eth2
        traffic: east-west
        rail: 0
```

Fresh discovery may merge compatible source groups during generation, but it keeps source groups explicit in the config for review and filtering.

| Field | Meaning |
| --- | --- |
| `identifier` | Lowercase resource-name form of machine/GPU identity, or `group-N` fallback. |
| `machineType` / `gpuType` | Discovered hardware identity. |
| `linkType` | Per-group `Ethernet` or `InfiniBand` result when fabric probes agree. |
| `presetApplied` | An exact topology preset was applied. |
| `presetDeviation` | PF count, PCI address, or device ID drift from a matched preset. |
| `capabilities.nodes` | Group-level SR-IOV, RDMA, and InfiniBand capability flags. |
| `workerNodes` | Kubernetes node names in the source group. |
| `nodeSelector` | Deployment selector, normally the Launch Kit machine label. |
| `storageModules` / `thirdPartyRDMAModules` | Optional site-supplied dependent module lists. |
| `pfs` | Physical-function inventory. |

Each PF can include `deviceID`, `pciAddress`, `rdmaDevice`, `networkInterface`, `traffic`, `rail`, `psid`, `partNumber`, `model`, `numaNode`, `connectedGPU`, `connectedGPUPCIAddress`, and `gpuProximity`.
