<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Configuration File

`cluster-config.yaml` is both an input and an output. Discovery writes it, generation reads and updates it, and deploy/validate use it to resolve release and namespace context.

## Top-Level Sections

| Section | Purpose |
| --- | --- |
| `networkOperator` | Release line, image repositories, Helm repository, namespace, and image pull secrets. |
| `networkNamespaces` | Namespaces that receive secondary network CRs and example DaemonSets. |
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

- `25.10`
- `26.1`
- `26.4`
- `26.7`

The release line fills Network Operator versions, component image tags, DOCA driver version, repositories, Helm repository URL, and version-gated template behavior.

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
