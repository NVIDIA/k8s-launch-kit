# Spectrum-X Multiplane Modes

## Overview

Spectrum-X supports three multiplane modes that determine how network planes are
organized, how resources are named, and which NIC types are supported. The mode
is selected via `--multiplane-mode` or `profile.spectrumX.multiplaneMode` in the
config file.

All Spectrum-X deployments require:
- `fabric=ethernet`
- `deployment=sriov`
- `multirail=true`
- `spectrumX.enable=true`

## Mode: none

- **NIC type**: BlueField-3 SuperNIC only (deviceID `a2dc`)
- **Number of planes**: 1 (fixed, no other value allowed)
- **Profile**: `spectrum-x` (the base Spectrum-X profile)
- **Resource naming**: Per-rail only

```
sriov-network-node-policy-rail-0
sriov-network-node-policy-rail-1
ovs-network-rail-0
ovs-network-rail-1
```

- **Description**: Single-plane operation for BF3 SuperNICs. No multiplane support
  because BF3 hardware does not implement plane load balancing. This is the only
  valid mode for BF3 deployments.

## Mode: swplb (Software Plane Load Balancing)

- **NIC type**: ConnectX-8 (deviceID `1023`) or ConnectX-9 (deviceID `1025`)
- **Number of planes**: 2 or 4 (default: 4)
- **Profile**: `spectrum-x` (unified profile, branches on swplb internally)
- **Resource naming**: Per-rail AND per-plane (finest granularity)

```
sriov-network-node-policy-plane-0-rail-0
sriov-network-node-policy-plane-0-rail-1
sriov-network-node-policy-plane-1-rail-0
sriov-network-node-policy-plane-1-rail-1
ovs-network-plane-0-rail-0
ovs-network-plane-1-rail-0
```

- **Description**: Software-based distribution of traffic across multiple planes.
  Each rail-plane combination gets its own SR-IOV policy, OVS network, and CIDR
  pool. This provides the finest resource granularity and is the default mode for
  CX8 deployments. Best for small-to-medium Spectrum-X clusters.
- **docaEswitchMax**: planes x number of rails

## Mode: hwplb (Hardware Plane Load Balancing)

- **NIC type**: ConnectX-8 (deviceID `1023`) or ConnectX-9 (deviceID `1025`)
- **Number of planes**: 2 or 4 (default: 4)
- **Profile**: `spectrum-x` (base Spectrum-X profile)
- **Resource naming**: Per-rail only (hardware handles plane distribution)

```
sriov-network-node-policy-rail-0
sriov-network-node-policy-rail-1
ovs-network-rail-0
ovs-network-rail-1
```

- **Description**: Hardware-based distribution of traffic across planes. The NIC
  firmware handles plane selection, so resources are only per-rail. Better for
  large-scale 2-tier and 3-tier network topologies where per-plane granularity
  is not needed and hardware efficiency matters.
- **docaEswitchMax**: number of rails

## NIC Type Constraint Summary

| NIC Type        | deviceID | Allowed Modes       | Default Mode |
|-----------------|----------|---------------------|--------------|
| BlueField-3     | `a2dc`   | `none`              | `none`       |
| ConnectX-7      | `1021`   | `none`              | `none`       |
| ConnectX-8      | `1023`   | `swplb`, `hwplb`    | `swplb`      |
| ConnectX-9      | `1025`   | `swplb`, `hwplb`    | `hwplb`      |

## Number of Planes Rules

| Mode      | Valid Values | Default | Notes                                  |
|-----------|-------------|---------|----------------------------------------|
| `none`    | 1           | 1       | CX7/BF3, single plane                  |
| `swplb`   | 2, 4        | 4       | More planes = finer resource granularity|
| `hwplb`   | 2, 4        | 4       | More planes = more hardware capacity   |

## Version

Two RA versions are supported, picked by the value of `--spectrum-x` together with
`--network-operator-release`:

| Version | Network Operator | Profile               | Rail wiring                                                      |
|---------|------------------|-----------------------|------------------------------------------------------------------|
| `RA2.2` | 26.4+            | `spectrum-x`          | Single v1alpha2 `SpectrumXRailPoolConfig` with `railTopology[]`  |
| `RA2.1` | 26.1 only        | `spectrum-x-ra2.1`    | Full SR-IOV operator chain + v1alpha1 `SpectrumXRailPoolConfig`  |

Both profiles support three multiplane modes (`none`, `swplb`, `hwplb`).
Selecting a mismatched `(spcxVersion, network-operator-release)`
pair (e.g. `RA2.1` with `26.4`) causes the matcher to skip both profiles and
fall through to a non-Spectrum-X profile or error out.

The v1alpha2 rail-pool template omits the removed `spec.withBCM` field.
Current `SpectrumXRailPoolConfig` CRDs reject that field during strict
decoding.

## Mode Selection Guide

| Scenario                                  | Recommended Mode |
|-------------------------------------------|------------------|
| BF3 SuperNIC deployment                   | `none`           |
| CX7 deployment                            | `none`           |
| CX8, small cluster, fine-grained control  | `swplb`          |
| CX8, large multi-tier topology            | `hwplb`          |
| Not sure (CX8)                            | `swplb` (default)|

## Validation Rules

l8k validates the mode and planes combination at startup:

1. Mode must be `none`, `swplb`, or `hwplb`
2. If mode is `none`, planes must be 1
3. Number of planes must be 1, 2, or 4
4. Version must be `RA2.1` (with `--network-operator-release 26.1`) or
   `RA2.2` (with `--network-operator-release 26.4` or higher / no release pinned)

Validation failures produce exit code 2 (validation error) with a descriptive
error message.
