<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Configuration File

`cluster-config.yaml` keeps its existing public YAML shape. Discovery writes
it, while generation reads it without modifying it. The repository-root file
is also the canonical lowest-precedence defaults source embedded in the
binary. Generation records the effective result in
`<deployment-dir>/.l8k/resolved-config.yaml`; deploy and validate use that
bundle metadata when no explicit `--user-config` is supplied.

The canonical default release is `26.7`. The `26.10` catalog entry selects
Network Operator `v26.10.0-beta.1` when requested. A compatible Spectrum-X release
selected during fresh discovery takes precedence over that default; the saved
file contains the selected release and its matching catalog versions. An
explicit release in `--user-config` remains pinned unless a CLI release
override is supplied.

For generation, configuration source precedence is:

1. Canonical defaults from the repository-root `cluster-config.yaml`.
2. Resolved hardware defaults for missing profile fields.
3. `--config-dir/l8k-config.yaml` or `--user-config` values.
4. Explicit CLI flags.

Presence is tracked before normalization. An explicit `false`, `0`, empty
string, or empty list therefore overrides lower layers; YAML `null` means the
field is unset. A selected Network Operator release remains authoritative and
replaces the catalog-managed version and repository fields after all layers
are merged.

Fresh discovery treats the selected reference file as a user layer except for
its sample `profile`, which is discarded before hardware defaults are derived.
Explicit non-profile values such as `docaDriver.enable: false` are preserved.

Discovery with `--user-config` replaces only `clusterConfig`, preserves every
other loaded section, and then applies explicit CLI flags. It does not fill
missing profile fields, recompute validation settings, or expand a release
selected only in YAML.
The refresh patches the source YAML, retaining explicit zero values, omitted
fields, and custom keys. YAML aliases and merge keys are expanded when saving
so a CLI override affects only its target field.

Standalone validation applies its validation CLI flags before checking the
resolved configuration. A valid flag value can therefore replace an invalid
value from the YAML file.

`--config-dir/presets/` replaces the embedded preset catalog. It does not merge with it.

Invalid enum values are rejected when CLI inputs are bound. Cross-field
requirements are checked after the effective configuration is resolved.

## Deploy And Validate Config Lookup

Standalone deploy and validate choose the first existing candidate in this order:

1. Explicit `--user-config` (selected even when unreadable; no lower path is tried).
2. `<deployment-files>/.l8k/resolved-config.yaml`.
3. `<deployment-files>/../.l8k/resolved-config.yaml`.
4. `<deployment-files>/../cluster-config.yaml`.
5. `<deployment-files>/cluster-config.yaml`.
6. `./cluster-config.yaml`.
7. With explicit `--config-dir`, only its `l8k-config.yaml` is considered as a
   default fallback; if absent, no local/share default file is selected. With
   no `--config-dir`, try `./l8k-config.yaml`, then the installation
   share-directory `l8k-config.yaml`.

The two sidecar locations support passing either the bundle root or its
`network-operator/` directory. Effective metadata is loaded without applying
defaults or expanding the release catalog again. Explicit deploy/validate CLI
overrides still apply. An explicit `--user-config` bypasses the sidecar and
resolves that file; use it only when intentionally changing the configuration
used to interpret the bundle. Connectivity validation requires a sidecar or
user-owned config from steps 1–6, not the installed/default fallback.

A configuration load error prevents connectivity validation. Static-only
validation can continue with configuration-dependent version checks skipped;
inspect the report before interpreting it as complete acceptance.

Keep `.l8k/resolved-config.yaml` in its reserved path. It is a versioned metadata
envelope, not a Kubernetes manifest or an ordinary flat cluster config.
Generation still takes a flat config; edit that source and regenerate when the
deployment intent changes.

## Top-Level Sections

| Section | Purpose |
| --- | --- |
| `flavor` | `k8s` (default) or `ocp`; selects profile and operator integration. |
| `nfd` | OpenShift NFD configuration object namespace and name. |
| `networkOperator` | Release line, image repositories, Helm repository, namespace, and image pull secrets. |
| `networkNamespaces` | Namespaces that receive secondary network CRs and example DaemonSets. |
| `workload` | Optional custom workload manifest. |
| `validation` | Connectivity mode, enabled checks, and RDMA parameters. |
| `docaDriver` | DOCA driver version and module unload behavior. |
| `maintenance` | Maintenance Operator and upgrade concurrency limits. |
| `nvIpam` | IPPool per-node block allocation, subnet generation, manual subnets, and exclusions. |
| `sriov`, `hostdev`, `rdmaShared`, `ipoib`, `macvlan` | Profile-specific resource and network naming. |
| `nicConfigurationOperator` | Optional NIC firmware/runtime tuning and interface naming templates. |
| `spectrumX` | Spectrum-X naming defaults. |
| `profile` | Selected fabric, deployment, routing, ARP, and Spectrum-X options. |
| `clusterConfig` | Discovered or preset-provided hardware groups. |

## Release Catalog

```yaml
networkOperator:
  selectedRelease: "26.7"
```

Supported release lines are currently:

- `26.1`
- `26.4`
- `26.7`
- `26.10` (`v26.10.0-beta.1`)

The release line fills Network Operator versions, component image tags, DOCA driver version, full-runtime validation image, repositories, Helm repository URL, and version-gated template behavior. Spectrum-X releases also carry a manually maintained xPlane repository and version used in `spectrumXOperator.xPlane` instead of reusing the generic component tag.

Nightly synchronization follows each upstream release branch. Once a catalog
line points to a GA Network Operator release, beta and release-candidate builds
of a later patch do not replace its public artifact set. The line advances
again when that patch is published as GA.

## Network Operator

| Field | Meaning |
| --- | --- |
| `selectedRelease` | Catalog release line. Equivalent to `--network-operator-release`. |
| `version` | Network Operator chart/operator version. Filled by the selected release. |
| `componentVersion` | Tag used by managed component images. |
| `skipHelmChart` | When `true`, generation omits `values.yaml`, deploy skips Network Operator chart installation and Helm preflight checks, validate skips Helm release-version and values checks, and clean retains the externally owned Helm release. Network Operator CR generation, deployment, validation, and cleanup remain enabled. Equivalent to `--skip-network-operator-helm` for generate, deploy, and validate. |
| `repository` | Registry path for component images such as drivers, CNI, IPAM, and device plugins. |
| `operatorRepository` | Registry path for the Network Operator controller image. |
| `helmRepoURL` | Chart repository used by `l8k deploy`. Empty means Helm phase 0 is skipped. |
| `namespace` | Namespace for the Helm release and namespaced Network Operator resources. |
| `imagePullSecrets` | Secret names propagated into the discovery daemon, generated policies, and Helm values for the Network Operator and enabled subcharts. During deploy, matching credentials also authenticate the Helm chart download. |

When `selectedRelease` is set, catalog values replace explicit version and repository fields so the cohort remains consistent.

Use `skipHelmChart: true` when another system owns the Network Operator Helm
release:

```yaml
networkOperator:
  selectedRelease: "26.7"
  skipHelmChart: true
```

The selected release still drives rendered component versions and their
validation; only the Helm artifact/install/check/uninstall boundary is disabled.

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

Secondary-network CRs and default example workloads render once per network
namespace outside Spectrum-X, which uses the first namespace. Shared policy
and pool resources do not fan out. `workload.manifest` is equivalent to
`--workload-manifest`: it replaces the example with an operational
`90-workload-*.yaml` deployment input. Kubernetes custom workloads use the first
namespace only; OpenShift custom workloads fan out per network bucket and
namespace. See [custom workload lifecycle](../advanced/generation.md#custom-workload-manifest)
for the resulting connectivity-fixture requirement.

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
| `gpuDirect.enabled` | Run a separate CUDA DMA-BUF `ib_write_bw` matrix when `ib_write_bw` is selected. Fresh discovery enables it only when every discovered worker can satisfy its render bucket's topology-derived `gpuResourceType` request. Discovery with `--user-config` preserves the supplied value. |
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
  # Advanced users only. Custom values override generated driver env by name.
  # env:
  #   - name: THIRD_PARTY_RDMA_MODULES
  #     value: "nvidia_peermem"
```

| Field | Meaning |
| --- | --- |
| `enable` | Include DOCA/OFED driver configuration. |
| `version` | Driver image tag; filled by the selected release catalog. |
| `unloadStorageModules` | Allow unload of storage-over-RDMA dependencies before driver replacement. |
| `unloadThirdPartyRDMAModules` | Allow unload of non-MLX RDMA dependencies before driver replacement. |
| `enableNFSRDMA` | Enable NFS-over-RDMA support. |
| `skipPreflightChecks` | Skip the init-container module dependency check. |
| `env` | Advanced literal `name`/`value` environment entries forwarded through the generated NicClusterPolicy or NicNodePolicy. Custom values override generated values by name; the last custom duplicate wins. |

Both unload controls default to `true`. Discovery does not populate the holder-module lists automatically. Confirm that no active workload depends on a module that the driver flow will unload.

`env` is an advanced escape hatch for driver settings that Launch Kit does not
model. The generated policy contains no duplicate environment names. Incorrect
values can prevent MOFED from reloading and disrupt node networking.

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
  perNodeBlockSize: 10
  startingSubnet: "192.168.0.0"
  mask: 22
  offset: 1
  reserveFirstIPs: 10
  reserveLastIPs: 6
```

Or list subnets manually:

```yaml
nvIpam:
  perNodeBlockSize: 10
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
| `perNodeBlockSize` | Non-negative number of addresses reserved per node in each generated `IPPool`; `0` or omission defaults to `10`. SR-IOV generation warns when the effective value is smaller than `sriov.numVfs`. |
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

## NIC Configuration And Naming

```yaml
nicConfigurationOperator:
  deployNicConfigurationTemplate: false
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
| `deployNicConfigurationTemplate` | Default `false`; opt into standard-profile NIC tuning independently of interface naming. CLI: `--deploy-nic-configuration-template[=false]` on root, generate and discover. |
| `deployNicInterfaceNameTemplate` | Allows per-source interface-name templates when profile or PCI-layout rules require stable names. |
| `rdmaPrefix` / `netdevPrefix` | Standard-profile names. Multirail values require a rail placeholder. |
| `updateFW` | Enables firmware staging storage in generated Network Operator configuration. |
| `spectrumX.overlay` | Spectrum-X overlay mode. |
| `spectrumX.singlePlane` | Prefix block selected by `none`; defaults both device types to rail-only names. |
| `spectrumX.hwplb` | Prefix block selected by `hwplb`; defaults RDMA to rail-only and NET to rail-plane names. |
| `spectrumX.swplb` | Prefix block selected by `swplb`; defaults both device types to rail-plane names. |

For standard SR-IOV, RDMA-shared (Macvlan/IPoIB), and host-device profiles on
Kubernetes and OpenShift, `deployNicConfigurationTemplate: true` enables NCO
and emits one `NicConfigurationTemplate` per source hardware group. It uses
that group's node selector, east-west NIC type and exact PCI addresses. Missing
selectors or mixed east-west NIC types within one group are rejected. The
setting is independent of `deployNicInterfaceNameTemplate` and `updateFW`;
Spectrum-X already owns its NIC configuration and rejects this new opt-in.

The template routes `numVfs` from `sriov.numVfs` for all these profiles and maps
`profile.fabric` to `Ethernet` or `Infiniband`. It enables PCI performance tuning
with `maxReadRequest: 4096` and `gpuDirectOptimized` for `Baremetal`. Ethernet
also enables `roceOptimized` with DSCP trust and PFC `"0,0,0,1,0,0,0,0"`;
InfiniBand omits that block. NCO settings operate on whole NICs, so confirm that
all ports of each selected NIC are within the intended configuration scope.
Generation rejects inventory that assigns east-west and excluded ports to the
same PCI device (domain:bus:device). This check cannot identify omitted ports or
NICs spanning different PCI devices; inventory and site validation must cover
those cases.

Only SR-IOV profiles disable the SR-IOV operator's `mellanox` plugin when this
option is enabled: through Helm values on Kubernetes, and through
`SriovOperatorConfig` on OpenShift. OCP adds the entry without removing other
disabled plugins. Turning the option off stops rendering its template and
plugin-disable request, but does not restore applied NIC settings. On Kubernetes,
deploy preflight treats omitted templates (including unselected groups in a
subset deployment) as stray resources: deployment blocks without
`--overwrite-existing`, and deletes them with that flag. Review the deletion
scope before an opt-out or subset deployment. OCP preserves existing templates
and `disablePlugins` values; cleanup requires an explicit action. With
`skipHelmChart: true` on Kubernetes, the externally managed SR-IOV operator must
be configured separately to disable `mellanox` before applying the template.
RDMA-shared and host-device profiles do not change SR-IOV operator settings.

Deploy and validate monitor all matched `NicDevice` objects and require current
`ConfigUpdateInProgress` conditions with `reason: UpdateSuccessful` and
`status: "False"`. Missing, stale or partial status remains in progress; runtime
or firmware configuration failures are reported. Confirm NCO/DOCA/RHCOS runtime
RoCE support for the selected release before enabling this option on OCP; offline
rendering and tests do not establish live qualification.

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
```

| Field | Meaning |
| --- | --- |
| `fabric` | `ethernet` or `infiniband`. |
| `deployment` | `sriov`, `rdma_shared`, or `host_device`. |
| `multirail` | Enable more than one east-west rail. An explicit `false` is preserved. |
| `routing` | `destination-based` or `source-based`; source-based adds the `sbr` CNI plugin outside Spectrum-X. |
| `ignoreARP` | Adds `arp_ignore=1`, `arp_announce=2`, and `rp_filter=0` through the `tuning` CNI plugin outside Spectrum-X. Kubernetes renders both `all` and `IFNAME` scopes; OpenShift renders `IFNAME` only and requires those keys in the Multus sysctl allowlist. |
| `spectrumX.enable` | Select a Spectrum-X profile. |
| `spectrumX.spcxVersion` | `RA2.1`, `RA2.2`, or `RA2.3`. |
| `spectrumX.multiplaneMode` | `none`, `swplb`, or `hwplb`. When absent, H100/H200/B200/GB200 default to `none`; B300/GB300 default to the GA `swplb` path. Platform type cannot distinguish `swplb` from `hwplb`, so `hwplb` must be selected explicitly. |
| `spectrumX.numberOfPlanes` | `1`, `2`, or `4`. Single-plane platforms default to 1; B300/GB300 default to 2. Pass 4 explicitly for quad-plane B300. |
| `spectrumX.topologyType` | `2-tier` or `3-tier`. |
| `spectrumX.ipVersion` | `ipv4` for per-node `/31` allocation or `ipv6` for per-node `/64` allocation. |
| `spectrumX.hostFirstOctet` | Config-only first octet for generated IPv4 topology addressing. |
| `spectrumX.topologyFile` | Path to spcx-gen/reference-generator or contract-compliant NVIDIA AIR topology JSON. The format is detected from its structure; relative paths resolve from the config file. |
| `spectrumX.configMapName` / `profile` | RA2.3 ConfigMap name and embedded profile data. |
| `spectrumX.useDRA` | Render DRA `ResourceClaimTemplate` workload allocation; see [prerequisites](../user/spectrum-x.md#dra-workload-allocation). |

The example above is a standard profile. When Spectrum-X is disabled, omit
its RA, mode, plane, topology, and ConfigMap fields; setting them is an error.
For an enabled RA2.3 profile, start with the
[full ConfigMap generation example](../user/spectrum-x.md#ra23-profile-configmap),
which supplies the required profile payload as well as the RA/release pair.

## Hardware Groups

Each `clusterConfig` entry represents one source group:

```yaml
clusterConfig:
  - identifier: pe-xe9680-h200
    machineType: PowerEdge-XE9680
    gpuType: NVIDIA-H200
    netplanManaged: true
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
| `netplanManaged` | `true` when any worker has an NVIDIA PF selected by a netplan `match.macaddress` stanza with a non-empty `set-name`. Generation fails if a `NicInterfaceNameTemplate` would configure this group; remove the affected `set-name` stanzas and re-run discovery before retrying generation. |
| `presetApplied` | An exact topology preset was applied. |
| `presetDeviation` | PF count, PCI address, or device ID drift from a matched preset. |
| `capabilities.nodes` | Group-level SR-IOV, RDMA, and InfiniBand capability flags. |
| `workerNodes` | Kubernetes node names in the source group. |
| `nodeSelector` | Deployment selector, normally the Launch Kit machine label whose value matches `identifier`. |
| `storageModules` / `thirdPartyRDMAModules` | Optional site-supplied dependent module lists. |
| `pfs` | Physical-function inventory. |

Each PF can include `deviceID`, `pciAddress`, `rdmaDevice`, `networkInterface`,
`traffic`, `rail`, `psid`, `partNumber`, `model`, `numaNode`, `connectedGPU`,
`connectedGPUPCIAddress`, and `gpuProximity`. PF MAC addresses are
host-specific and are not part of the saved configuration.

## OpenShift flavor

See [OpenShift host deployments](../user/openshift.md) for operator prerequisites, separate namespace defaults, supported profiles, and validation behavior. `sriov.operatorNamespace`, `nfd.operatorNamespace`, `nfd.configurationName`, and `maintenance.operatorNamespace` can override their OpenShift defaults.
