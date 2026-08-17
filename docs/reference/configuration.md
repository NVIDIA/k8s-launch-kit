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

The release line fills Network Operator versions, component image tags, DOCA driver version, full-runtime validation image, repositories, Helm repository URL, and version-gated template behavior. Spectrum-X releases also carry a manually maintained xPlane repository and version used in `spectrumXOperator.xPlane` instead of reusing the generic component tag.

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
| `imagePullSecrets` | Secret names propagated into the discovery daemon, generated policies, and Helm values for the Network Operator and enabled subcharts. During deploy, matching credentials also authenticate the Helm chart download. |

When `selectedRelease` is set, catalog values replace explicit version and repository fields so the cohort remains consistent.

For an authenticated Helm repository, each referenced Secret must already
exist in `networkOperator.namespace` before `l8k deploy` starts, and the
kubeconfig must allow `get` on Secrets there. l8k reads
`kubernetes.io/dockerconfigjson` and legacy `kubernetes.io/dockercfg` data in
memory and never logs or persists the credential. Credentials are sent only
when the Docker registry host exactly matches the Helm repository host. The
one explicit cross-host mapping is NGC: `nvcr.io` credentials use the same
`$oauthtoken` and API key required by `helm.ngc.nvidia.com`. Unrelated registry
credentials are never forwarded to the chart server.

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
  gpuDirect:
    enabled: false
    gpuResourceType: nvidia.com/gpu
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
| `gpuDirect.enabled` | Run a separate CUDA DMA-BUF `ib_write_bw` matrix when `ib_write_bw` is selected. Discovery always writes this boolean and enables it only when every discovered worker can satisfy its render bucket's topology-derived `gpuResourceType` request. |
| `gpuDirect.gpuResourceType` | Qualified Kubernetes extended resource requested by the primary validation container. Defaults to `nvidia.com/gpu`. |
| `connectivity` | Enables the data-plane stage. Static acceptance checks always run. |
| `mode` | `quick`, `full`, or `strict`. |
| `checks` | Any combination of `icmp`, `rping`, and `ib_write_bw`; an empty list disables all connectivity test families. |
| `rdma.rpingIterations` | Client iterations for each `rping` test. |
| `rdma.ibWriteSize` | Message size passed to `ib_write_bw`. |
| `rdma.ibWriteMinBandwidthGbps` | Minimum observed peak bandwidth. Set `0` to disable the bandwidth gate. |

GPU resource counts are not added to `clusterConfig`. Generation derives the
request needed to expose the discovered `GPU<N>` indices from the existing PF
topology. The DMA-BUF runner resolves source and destination indices separately
from `connectedGPU` (and reports `connectedGPUPCIAddress` when available); an
unresolved or ambiguous NIC/GPU association is a failed validation result.
`networkOperator.imagePullSecrets` is copied to the validation Pod spec, so
each named Secret must exist in every generated network namespace.

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
  overlay: "none"
  singlePlane:
    netdevPrefix: "eth_r%rail_id%"
    rdmaPrefix: "roce_r%rail_id%"
  hwplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%"
  swplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%_p%plane_id%"
```

| Field | Meaning |
| --- | --- |
| `deployNicInterfaceNameTemplate` | Allows per-source interface-name templates when profile or PCI-layout rules require stable names. |
| `rdmaPrefix` / `netdevPrefix` | Standard-profile names. Multirail values require a rail placeholder. |
| `updateFW` | Enables firmware staging storage in generated Network Operator configuration. |
| `spectrumX.overlay` | Spectrum-X overlay mode. |
| `spectrumX.singlePlane` | Prefix block selected by `none`; defaults both device types to rail-only names. |
| `spectrumX.hwplb` | Prefix block selected by `hwplb`; defaults RDMA to rail-only and NET to rail-plane names. |
| `spectrumX.swplb` | Prefix block selected by `swplb`; defaults both device types to rail-plane names. |

Spectrum-X `NicConfigurationTemplate.spec.nicSelector` is not a separate
configuration input. Launch Kit renders one template per source hardware group
and derives both `nicType` and `pciAddresses` from `clusterConfig[].pfs[]`
entries whose `traffic` is `east-west`. The NIC Configuration Operator matches
the intersection of those fields, so a north-south DPU with the same device ID
as an east-west SuperNIC is not selected. Every selected east-west PF must have
the same non-empty device ID and a non-empty PCI address or generation fails.

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
| `spectrumX.multiplaneMode` | `none`, `swplb`, or `hwplb`. When absent, H100/H200/B200/GB200 default to `none`; B300/GB300 default to the GA `swplb` path. Platform type cannot distinguish `swplb` from `hwplb`, so `hwplb` must be selected explicitly. |
| `spectrumX.numberOfPlanes` | `1`, `2`, or `4`. Single-plane platforms default to 1; B300/GB300 default to 2. Pass 4 explicitly for quad-plane B300. |
| `spectrumX.topologyType` | `2-tier` or `3-tier`. |
| `spectrumX.ipVersion` | `ipv4` for per-node `/31` allocation or `ipv6` for per-node `/64` allocation. |
| `spectrumX.hostFirstOctet` | Config-only first octet for generated IPv4 topology addressing. |
| `spectrumX.topologyFile` | Path to spcx-gen/reference-generator or contract-compliant NVIDIA AIR topology JSON. The format is detected from its structure; relative paths resolve from the config file. |
| `spectrumX.configMapName` / `profile` | RA2.3 ConfigMap name and embedded profile data. |
| `spectrumX.useDRA` | Render DRA `ResourceClaimTemplate` workload allocation. |

## Hardware Groups

Each `clusterConfig` entry represents one source group:

```yaml
clusterConfig:
  - identifier: pe-xe9680-h200
    machineType: PowerEdge-XE9680
    gpuType: NVIDIA-H200
    capabilities:
      nodes:
        sriov: true
        rdma: true
        ib: false
    nodeSelector:
      nvidia.kubernetes-launch-kit.machine: pe-xe9680-h200
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
| `identifier` | Lowercase resource-name form of machine/GPU identity with complete `NVIDIA` segments removed and common machine segments shortened (`ThinkSystem` → `ts`, `PowerEdge` → `pe`), bounded to 30 bytes with balanced machine/GPU prefixes and a 6-character deterministic hash when needed, or `group-N` fallback. The Launch Kit machine node label uses the same value. |
| `machineType` / `gpuType` | Discovered hardware identity. |
| `linkType` | Per-group `Ethernet` or `InfiniBand` result when fabric probes agree. |
| `presetApplied` | An exact topology preset was applied. |
| `presetDeviation` | PF count, PCI address, or device ID drift from a matched preset. |
| `capabilities.nodes` | Group-level SR-IOV, RDMA, and InfiniBand capability flags. |
| `workerNodes` | Kubernetes node names in the source group. |
| `nodeSelector` | Deployment selector, normally the Launch Kit machine label whose value matches `identifier`. |
| `storageModules` / `thirdPartyRDMAModules` | Optional site-supplied dependent module lists. |
| `pfs` | Physical-function inventory. |

Each PF can include `deviceID`, `pciAddress`, `rdmaDevice`, `networkInterface`, `traffic`, `rail`, `psid`, `partNumber`, `model`, `numaNode`, `connectedGPU`, `connectedGPUPCIAddress`, and `gpuProximity`.
