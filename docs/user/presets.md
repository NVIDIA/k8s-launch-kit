<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Topology Presets

Topology presets describe known server layouts by machine type, GPU type, PF PCI address, traffic direction, rail, NUMA node, and GPU proximity.

## How Presets Are Used

| Flow | Behavior |
| --- | --- |
| Discovery overlay | `l8k discover` matches a preset by exact `(machineType, gpuType)`, validates PF count, PCI addresses, and device IDs, and applies topology fields only when the hardware matches exactly. |
| Offline generation | `l8k generate --for <preset>` replaces `clusterConfig` with the preset topology and renders manifests without cluster access. |

Presets are all-or-nothing. When a matched preset deviates from live hardware, l8k records the deviation and keeps live-discovered topology instead of partially overlaying stale rail data.

## List Presets

```bash
l8k preset list
```

Bundled presets include:

- `GB300-DGX-Station-NVIDIA-GB300`
- `GB300-NVL-NVIDIA-GB300`
- `PowerEdge-R760xa-H100-NVL`
- `PowerEdge-XE7745-RTX-PRO-4500`
- `PowerEdge-XE9680-H200`
- `ThinkSystem-SR650-V4-RTX-PRO-6000`
- `ThinkSystem-SR675-V3-H200-NVL`
- `ThinkSystem-SR675-V3-RTX-PRO-6000`
- `ThinkSystem-SR675-V3-System-Board-RTX-PRO-6000`
- `ThinkSystem-SR680a-V3-H200`
- `UCSC-885A-M8-H22-H200`

## Update Presets

Download a catalog from a Git repository into the active filesystem override:

```bash
l8k preset update
```

Select the source or destination explicitly:

```bash
l8k preset update \
  --repo nvidia/k8s-launch-kit \
  --branch main

l8k preset update \
  --dir /etc/l8k/presets
```

Use `--config-dir /etc/l8k` on later commands to select a catalog stored under `/etc/l8k/presets`.

## Generate From A Preset

```bash
l8k generate \
  --for PowerEdge-XE9680-H200 \
  --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
  --fabric ethernet \
  --deployment-type sriov \
  --save-deployment-files ./deployment
```

`--node-selector` is required because the synthesized cluster config has no live worker-node list.

## Preset YAML

```yaml
machineType: PowerEdge-XE9680
gpuType: NVIDIA-H200
manufacturer: Dell
nicModel: BlueField-3 SuperNIC
capabilities:
  nodes:
    sriov: true
    rdma: true
    ib: false
pfs:
  - deviceID: a2dc
    pciAddress: 0000:1a:00.0
    rdmaDevice: rocep26s0f0
    networkInterface: eth2
    traffic: east-west
    rail: 0
    numaNode: 0
    connectedGPU: GPU0
    gpuProximity: PIX
```

The `capabilities.nodes` block is required for `--for` because profile selection happens without live discovery.

## Author A Preset

1. Collect topology on a representative node for the exact machine/GPU pair.
   Record every PF, including north-south interfaces, and confirm its PCI
   address, device ID, traffic role, rail, NUMA node, and GPU association.
2. Set `machineType` to the existing `nvidia.com/gpu.machine` label, or the
   discovered `/sys/class/dmi/id/product_name` value with spaces normalized to
   dashes. Set `gpuType` to `nvidia.com/gpu.product` or the sanitized GPU product
   name from discovery. Copy the persisted values from `cluster-config.yaml`
   when available; matching is exact and case-sensitive.
3. Create `presets/<unique-machine-gpu-variant>/topology.yaml`. Include both
   matching keys and `capabilities.nodes` for offline `--for` use. Keep rail
   assignments and `connectedGPU: GPU<N>` consistent with the physical layout;
   do not infer a missing association as GPU0.
4. Put the candidate under a dedicated config directory and test it locally:

   ```bash
   l8k preset list --config-dir ./preset-review
   l8k generate --config-dir ./preset-review \
     --for MyServer-H200 --node-selector rack=42 \
     --fabric ethernet --deployment-type sriov \
     --save-deployment-files ./preset-review-output --output json
   ```

   These examples expect `preset-review/presets/MyServer-H200/topology.yaml`.
   The override replaces the catalog, so include every preset needed for the
   review. Inspect networks, resource names, selectors, and GPU requests in
   the rendered output. Rendering does not certify the hardware.
5. Verify the complete PF inventory against representative live discovery and
   inspect any preset deviations. Submit the preset and its evidence through
   the repository contribution process.

## Validation And Deviations

Preset lookup is an exact match on `(machineType, gpuType)`. After lookup, Launch Kit compares:

- PF count.
- The complete PCI-address set.
- Device ID at every shared PCI address.

Part number and PSID differences are not topology deviations because firmware and SKU variants are expected.

When the topology differs, discovery:

1. Records each difference under `clusterConfig[].presetDeviation`.
2. Keeps the live-discovered traffic, rail, NUMA, and GPU-affinity data.
3. Does not partially apply the preset.
4. Repeats the warning whenever the config is loaded.

```yaml
presetDeviation:
  - field: pfCount
    expected: "8"
    got: "6"
    detail: PF count differs from preset
  - field: deviceID
    expected: 1023@0000:5e:00.0
    got: a2dc@0000:5e:00.0
    detail: device ID at PCI address differs from preset
```

`l8k validate` compares the saved group with the current catalog again. A topology mismatch appears as paired actual/expected hardware in the report and prevents the deployment from receiving a green-light result.

## Override The Catalog

Use `--config-dir` to replace the embedded catalog:

```bash
l8k preset list --config-dir /etc/l8k
l8k generate --config-dir /etc/l8k --for MyServer-H200 --node-selector rack=42
```

The override directory must contain `presets/<name>/topology.yaml`.
