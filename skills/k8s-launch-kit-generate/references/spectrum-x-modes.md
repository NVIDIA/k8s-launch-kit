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

Launch Kit renders one `NicConfigurationTemplate` per source hardware group and
derives `spec.nicSelector.nicType` plus `pciAddresses` from the selected
east-west PF inventory. The selector is not configured under `spectrumX`;
north-south PFs are ignored, and the east-west IDs must be non-empty and
unanimous while every east-west PF must have a PCI address. Combining type and
PCI selectors prevents a same-device-ID DPU from receiving the SuperNIC config.

## Mode: none

- **NIC type**: BlueField-3 SuperNIC (`a2dc`), ConnectX-7 (`1021`), or
  ConnectX-8 (`1023`) in a single-plane platform
- **Number of planes**: 1 (fixed, no other value allowed)
- **Profile**: `spectrum-x` (the base Spectrum-X profile)
- **Resource naming**: Per-rail only

```
sriov-network-node-policy-rail-0
sriov-network-node-policy-rail-1
ovs-network-rail-0
ovs-network-rail-1
```

- **Description**: Single-plane operation. This is the only valid mode for BF3
  and CX7, and the default for H100/H200/B200/GB200 GPU platforms.

## Mode: swplb (Software Plane Load Balancing)

- **NIC type**: ConnectX-8 (deviceID `1023`) or ConnectX-9 (deviceID `1025`)
- **Number of planes**: 2 or 4; l8k defaults B300/GB300 to 2
- **Profile**: `spectrum-x` (unified profile, branches on swplb internally)
- **Resource naming**: Per-rail AND per-plane (finest granularity)

```
sriov-network-node-policy-rail-0-plane-0
sriov-network-node-policy-rail-0-plane-1
sriov-network-node-policy-rail-1-plane-0
sriov-network-node-policy-rail-1-plane-1
ovs-network-rail-0-plane-0
ovs-network-rail-0-plane-1
```

- **Description**: Software-based distribution of traffic across multiple planes.
  Each rail-plane combination gets its own SR-IOV policy, OVS network, and CIDR
  pool. This provides the finest resource granularity and is the GA default for
  B300/GB300 deployments. Best for small-to-medium Spectrum-X clusters.
- **docaEswitchMax**: planes x number of rails

## Mode: hwplb (Hardware Plane Load Balancing)

- **NIC type**: ConnectX-8 (deviceID `1023`) or ConnectX-9 (deviceID `1025`)
- **Number of planes**: 2 or 4, selected explicitly with `hwplb`
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

NIC type only constrains what is possible. Platform type does not distinguish
`swplb` from `hwplb` on B300 or GB300 because both modes support both
platforms. For the documented CX8/B300/GB300 combinations, Launch Kit uses
`swplb` as the GA default and treats `hwplb` as an explicit site-topology
choice. Unknown platforms retain the NIC-family fallback in the table above.

## Platform Default Summary

| GPU platform | Default mode | Default planes | Notes |
|--------------|--------------|----------------|-------|
| H100/H200/B200/GB200 | `none` | 1 | Single-plane architecture |
| B300 | `swplb` | 2 | Pass 4 explicitly for quad-plane |
| GB300 | `swplb` | 2 | Dual-plane architecture |

## Number of Planes Rules

| Mode      | Valid Values | Default | Notes                                  |
|-----------|-------------|---------|----------------------------------------|
| `none`    | 1           | 1       | CX7/BF3, single plane                  |
| `swplb`   | 2, 4        | 2 on B300/GB300 | Pass 4 explicitly for quad-plane B300 |
| `hwplb`   | 2, 4        | explicit | Platform type cannot select the mode |

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
| Not sure (B300/GB300)                    | `swplb` (GA default) |

## Validation Rules

l8k validates the mode and planes combination at startup:

1. Mode must be `none`, `swplb`, or `hwplb`
2. If mode is `none`, planes must be 1
3. Number of planes must be 1, 2, or 4
4. Version must be `RA2.1` (with `--network-operator-release 26.1`) or
   `RA2.2` (with `--network-operator-release 26.4` or higher / no release pinned)

Validation failures produce exit code 2 (validation error) with a descriptive
error message.
