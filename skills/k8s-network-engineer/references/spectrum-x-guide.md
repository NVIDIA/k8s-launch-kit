# Spectrum-X Configuration Guide

## Overview

The Spectrum-X profile provides optimized multi-rail networking with OVS hardware
offload, DOCA acceleration, and advanced NIC firmware configuration for AI workloads.
It always requires `fabric=ethernet`, `deployment=sriov`, and `multirail=true`.

## Multiplane Modes

Spectrum-X supports three multiplane modes that determine how network planes are
organized and how resources are named:

### none (Single Plane)

- Single plane, no multiplane support
- BlueField-3 SuperNIC (`a2dc`), ConnectX-7 (`1021`), or ConnectX-8
  (`1023`) on H100/H200/B200/GB200
- Number of planes: 1 (fixed)
- Resources are named per-rail only
- Simplest Spectrum-X deployment

### swplb (Software Plane Load Balancing)

- ConnectX-8 (deviceID `1023`) or ConnectX-9 (deviceID `1025`)
- Software-based load balancing across planes
- Number of planes: 2 or 4 (B300/GB300 default: 2)
- Resources are named per-rail AND per-plane (finest granularity)
- SpectrumXRailPoolConfig emits one `railTopology[]` entry per rail-plane,
  each with its own `cidrPoolRef: rail-{i}-plane-{p}`
- Best for small-to-medium Spectrum-X clusters

### hwplb (Hardware Plane Load Balancing)

- ConnectX-8 (deviceID `1023`) or ConnectX-9 (deviceID `1025`)
- Hardware-based load balancing across planes
- Number of planes: 2 or 4 (explicit site-topology choice)
- Resources are named per-rail only (hardware handles plane distribution)
- Better for large-scale 2-tier and 3-tier network topologies

Both B300 and GB300 support `swplb` and `hwplb`, so the GPU platform cannot
identify which load-balancing mechanism the fabric uses. Launch Kit defaults
these platforms to the GA `swplb` path with 2 planes. Select `hwplb`
explicitly, and pass 4 explicitly for a quad-plane B300 topology.

All three modes are supported by the Spectrum-X profiles. Pick the profile
by the value of `--spectrum-x` (the legacy `--spcx-version` has been folded
into `--spectrum-x`):

- `profiles/spectrum-x/` — RA2.2 on Network Operator 26.4+. Uses the
  consolidated v1alpha2 `SpectrumXRailPoolConfig` (`railTopology[]`) for
  rail wiring and omits the removed `spec.withBCM` field.
- `profiles/spectrum-x-ra2.1/` — RA2.1 on Network Operator 26.1 only.
  Renders the full SR-IOV operator chain (`SriovNetworkPoolConfig` +
  `SriovNetworkNodePolicy` + `OVSNetwork` + `CIDRPool`) plus a v1alpha1
  glue `SpectrumXRailPoolConfig`. The 26.1 NCP is also leaner — no
  `nicFirmwareStorage`, no `spectrumXOperator.xPlane`.

## OVS Bridge Configuration

Spectrum-X uses DOCA-accelerated Open vSwitch with hardware offloading:

- **Datapath type**: `netdev` (DPDK datapath for hardware offload)
- **Fail mode**: `secure` (drops traffic if controller is unreachable)
- **Uplink interface type**: `dpdk`
- **Bridge grouping policy**: Configurable (`perPF`, `perNIC`, or `all`)
- **docaEswitchMax**: Number of OVS bridges
  - hwplb: number of rails (e.g., 4 for a 4-rail config)
  - swplb: planes x number of rails
  - BF3: number of rails (e.g., 8 for Hopper)

The SriovNetworkPoolConfig CR configures OVS bridge settings and enables RDMA
exclusive mode for each rail.

## RDMA Configuration

- **Mode**: `exclusive` -- each workload gets dedicated RDMA resources (not shared)
- **rdmaPrefix**: Selected from the mode's `spectrumX` block
  - hwplb/none: `"roce_r%rail_id%"`
  - swplb: `"roce_r%rail_id%_p%plane_id%"`
- **netdevPrefix**: Selected from the mode's `spectrumX` block
  - hwplb/swplb: `"eth_r%rail_id%_p%plane_id%"`
  - none: `"eth_r%rail_id%"`
- **eSwitchMultiport**: `"true"` for Spectrum-X configurations

Note: RDMA exclusive mode requires a node reboot and cannot be set when namespaces
are already configured. This is a tech-preview feature for non-BCM workflows.

## Resource Naming Patterns

Resources are named based on rail and (optionally) plane indices:

### Per-Rail Resources (hwplb, none)

```
sriov-network-node-policy-rail-0
sriov-network-node-policy-rail-1
ovs-network-rail-0
ovs-network-rail-1
cidr-pool-rail-0
cidr-pool-rail-1
spectrum-x-rail-pool-config-rail-0
spectrum-x-rail-pool-config-rail-1
```

### Per-Rail-Per-Plane Resources (swplb)

```
sriov-network-node-policy-rail-0-plane-0
sriov-network-node-policy-rail-0-plane-1
sriov-network-node-policy-rail-1-plane-0
sriov-network-node-policy-rail-1-plane-1
ovs-network-rail-0-plane-0
ovs-network-rail-0-plane-1
```

## CIDRPool Configuration

Each rail (or rail-plane combination for swplb) gets its own CIDRPool:

- **IPv4**: Per-node `/31`, gateway index `0`, exclusion index `1`
- **IPv6**: Per-node `/64`, leaf gateway `::2`, gateway index and exclusion
  index `2`, and a `/40` pool per rail or rail-plane
- **IPv6 routes**: `/32` for a single plane; `/24` for dual- or quad-plane
  deployments
- **Size limits**: Each CIDRPool CRD has a 1.5 MB etcd limit
  - `kubectl apply`: ~6,424 nodes per pool
  - `kubectl apply --server-side`: ~10,105 nodes per pool (recommended)

IPv6 uses `fd02:00PP:RRDD:SSHH::peer/64`, where the byte-sized fields encode
plane, rail, pod, SU, and host. The host candidate is `::1` and the leaf is
`::2`. `swplb` encodes the plane and creates per-rail-plane pools; `none` and
`hwplb` use address plane zero and create per-rail pools.

## Generated CRDs

The Spectrum-X profile generates these Kubernetes Custom Resources:

| Order | CRD                        | File                              | Purpose                                       |
|-------|----------------------------|-----------------------------------|-----------------------------------------------|
| 1     | NicClusterPolicy           | 10-nicclusterpolicy.yaml          | Network Operator, NV-IPAM, Spectrum-X Operator|
| 2     | NicConfigurationTemplate   | 30-nicconfigurationtemplate.yaml  | Per-source Spectrum-X settings for east-west NIC PCIs |
| 3     | NICInterfaceNameTemplate   | 35-nicinterfacenametemplate.yaml  | Interface naming for multi-rail topology       |
| 4     | SriovNetworkPoolConfig     | 40-sriovnetworkpoolconfig.yaml    | RDMA mode and OVS hardware offload per rail    |
| 5     | SriovNetworkNodePolicy     | 50-sriovnetworknodepolicy.yaml    | SR-IOV VF policies per rail (per plane for swplb)|
| 6     | OVSNetwork                 | 70-ovsnetwork.yaml                | OVS network attachment definitions per rail    |
| 7     | SpectrumXRailPoolConfig    | 80-spectrumxrailpoolconfig.yaml   | Links SR-IOV policies to CIDR pools per rail   |
| 8     | Test Pod                   | 90-pod.yaml                       | Example pod for validation                     |

Additional CRDs may be generated depending on configuration:
- NicFirmwareSource / NicFirmwareTemplate for firmware management
- CIDRPool for IP address allocation

## Prerequisites

1. Kubernetes cluster with SR-IOV capable nodes
2. NVIDIA Network Operator v26.1.0 or later
3. ConnectX-8, ConnectX-9, or BlueField-3 SuperNIC adapters
4. Firmware compatible with Spectrum-X RA2.2 (on 26.4+) or RA2.1 (on 26.1)
5. Helm values enabling `sriovNetworkOperator`, `maintenanceOperator`, and
   feature gate `manageSoftwareBridges: true`

## Quick Reference

```bash
# BF3 SuperNIC deployment
l8k --user-config config.yaml \
  --fabric ethernet --deployment-type sriov \
  --multirail --spectrum-x \
  --multiplane-mode none --number-of-planes 1

# B300/GB300 with software plane load balancing (default)
l8k --user-config config.yaml \
  --fabric ethernet --deployment-type sriov \
  --multirail --spectrum-x \
  --multiplane-mode swplb --number-of-planes 2

# CX8 with hardware plane load balancing (large scale)
l8k --user-config config.yaml \
  --fabric ethernet --deployment-type sriov \
  --multirail --spectrum-x \
  --multiplane-mode hwplb --number-of-planes 4

```
