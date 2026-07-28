<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Spectrum-X

Spectrum-X profiles render multi-rail AI interconnect manifests. The selected RA version and Network Operator release determine the manifest shape.

## Version Matrix

| RA version | Network Operator release | Profile path | Notes |
| --- | --- | --- | --- |
| RA2.1 | `26.1` | `spectrum-x-ra2.1` | Uses the RA2.1 SR-IOV operator chain and v1alpha1 glue CRs. |
| RA2.2 | `26.4` | `spectrum-x-ra2.2` | Uses v1alpha2 `SpectrumXRailPoolConfig`. |
| RA2.3 | `26.7` | `spectrum-x` | Uses v1alpha2 `SpectrumXRailPoolConfig` and a ConfigMap-backed Spectrum-X profile. |

Select the release line explicitly:

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3
```

## Multiplane Modes

| Mode | Use |
| --- | --- |
| `none` | No plane separation. Common for ConnectX-7 and BlueField-3 SuperNIC topologies. |
| `swplb` | Software plane load balancing. Renders per-rail, per-plane resources. |
| `hwplb` | Hardware plane load balancing for larger topologies. |
| `uniplane` | Single unified plane. |

```bash
l8k discover \
  --spectrum-x RA2.3 \
  --multiplane-mode hwplb \
  --number-of-planes 4
```

## RA2.3 Profile ConfigMap

RA2.3 requires the Spectrum-X profile data as either a full ConfigMap YAML or raw `data.profile` YAML.

The full ConfigMap must use the NIC Configuration Operator discovery label and store the profile as a YAML string:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: site-ra23-profile
  namespace: nvidia-network-operator
  labels:
    network.nvidia.com/operator.nic-configuration.spectrum-x-profile: ""
data:
  profile: |
    useSoftwareCCAlgorithm: true
    docaCCVersion: "<validated-version>"
    mlxConfig:
      none:
        "1023":
          postBreakout:
            EXAMPLE_NVCONFIG_PARAMETER: "example-value"
    runtimeConfig:
      roce:
        - name: <parameter-name>
          value: "<validated-value>"
          valueType: string
          dmsPath: "<validated-dms-path>"
```

Replace the placeholders with a validated profile for the target hardware and RA release. The NIC Configuration Operator repository provides a complete [example Spectrum-X profile ConfigMap](https://raw.githubusercontent.com/Mellanox/nic-configuration-operator/refs/heads/main/docs/examples/spectrum-x/example-spectrum-x-profile-configmap.yaml) covering `mlxConfig`, RoCE, adaptive routing, congestion control, and inter-packet gap sections.

Generate from the full ConfigMap:

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3 \
  --spectrum-x-config ./spectrum-x-profile-configmap.yaml
```

Raw profile data:

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3 \
  --spectrum-x-config ./profile.yaml \
  --spectrum-x-configmap-name site-ra23-profile
```

The generated ConfigMap uses the label expected by the NIC Configuration Operator and stores the profile under `data.profile`.

## Topology-Driven CIDRPools

Spectrum-X CIDRPools can be generated from an `spcx-gen` format `topology.json`. This replaces placeholder pool entries with per-host static allocations derived from the resolved topology.

The file contains a `nodes` inventory and two-endpoint entries under `links`. This minimal 2-tier example connects one Kubernetes worker to one leaf:

```json
{
  "nodes": [
    {
      "name": "compute-a",
      "role": "host",
      "type": "default"
    },
    {
      "name": "leaf-p0-r0",
      "role": "leaf",
      "type": "cumulus"
    }
  ],
  "links": [
    [
      {
        "node": "leaf-p0-r0",
        "interface": "swp1s0",
        "attributes": {
          "role": "leaf",
          "plane": 0,
          "pod": 0,
          "su": 0,
          "rail_group": [0]
        }
      },
      {
        "node": "compute-a",
        "interface": "eth_p0_r0",
        "attributes": {
          "role": "host",
          "rail": 0,
          "pod": 0,
          "su": 0
        }
      }
    ]
  ]
}
```

Add one host-to-leaf link for every selected worker rail. Host `node` values must match Kubernetes node names in the selected `clusterConfig` group. Every host endpoint requires `attributes.rail`, every leaf endpoint requires `attributes.plane`, and 3-tier allocation also requires host `attributes.pod`.

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3 \
  --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
  --topology-scheme 2-tier \
  --ip-version ipv4 \
  --topology-file ./topology.json
```

IPv4 CIDRPool rendering is supported. IPv6 is accepted in config for forward compatibility but is not rendered into CIDRPools yet.

## DRA Workload Allocation

For RA2.2 and RA2.3, set `profile.spectrumX.useDRA: true` to render ResourceClaimTemplate-based workload allocation.

```yaml
profile:
  spectrumX:
    enable: true
    spcxVersion: RA2.3
    useDRA: true
```

## Deployment Notes

- Discovery defaults to one rail per physical NIC and collapses multi-plane PFs to the master PF unless the NIC model is genuinely dual-port.
- North-south-only groups are not written to `cluster-config.yaml`; they do not produce Spectrum-X manifests.
- Spectrum-X rendering participates in heterogeneous group merging when source groups share GPU type and rail count.
- `--network-namespaces` does not fan out Spectrum-X resources; Spectrum-X renders into the first configured namespace.
