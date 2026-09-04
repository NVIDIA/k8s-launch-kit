<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Cluster Discovery

`l8k discover` inventories NVIDIA NICs and GPUs, groups nodes with compatible hardware, and writes an editable `cluster-config.yaml`. Fresh discovery resolves a profile; a `--user-config` refresh preserves its non-hardware settings.

```bash
l8k discover \
  --kubeconfig "$KUBECONFIG" \
  --save-cluster-config ./cluster-config.yaml
```

Discovery is self-contained. It does not require Node Feature Discovery (NFD) or a pre-installed Network Operator.

## How Discovery Works

1. Selects Kubernetes nodes that are Ready and schedulable.
2. Prepares the private `nvidia-k8s-launch-kit` namespace.
3. Creates the NIC Configuration Operator CRDs only when they are missing.
4. Runs a temporary NIC Configuration Daemon on the eligible nodes.
5. Detects nodes with NVIDIA NICs through a PCI vendor `0x15b3` sysfs probe and excludes BlueFields whose host trust level is `restricted`.
6. Reads the published `NicDevice` resources and probes GPU, NIC, fabric, module, NUMA, and rail data.
7. Groups nodes, writes Launch Kit node labels, and saves the result. Fresh discovery resolves profile settings; with `--user-config`, only `clusterConfig` is replaced.
8. Deletes the temporary namespace and cluster-scoped bootstrap RBAC. The CRDs remain installed.

The bootstrap namespace is fixed. `--network-operator-namespace` is accepted for compatibility but ignored by discovery.

## Requirements

- Kubernetes access through `--kubeconfig`, `$KUBECONFIG`, or `~/.kube/config`.
- At least one Ready, schedulable node with a discoverable NVIDIA or Mellanox NIC. BlueFields in zero-trust (`restricted`) mode do not count because NIC Configuration Operator does not publish `NicDevice` resources for them.
- Image pull access on eligible nodes to the configured NIC Configuration Daemon image.

For a private registry, forward one or more pull secrets:

```bash
l8k discover \
  --image-pull-secrets registry-credentials \
  --save-cluster-config ./cluster-config.yaml
```

The default `--node-selector` value is written to the saved configuration for deployment-time resource selection. It does not limit which nodes run the discovery daemon.

Before waiting for `NicDevice` resources, Launch Kit checks BlueField trust
mode with `mlxprivhost`. A restricted BlueField is excluded from the wait set.
If the same node has another non-restricted NVIDIA NIC, that node remains in
the wait set and its published devices are discovered normally. A failed or
unrecognized trust query is treated as non-restricted, matching NIC
Configuration Operator behavior.

## Hardware And GPU Topology

For each NIC, discovery records the device ID, PCI address, RDMA device,
network interface, VPD identifiers, model, traffic direction, and rail. GPU and
host details use layered sources:

1. Existing `nvidia.com/gpu.machine` and `nvidia.com/gpu.product` node labels.
2. DMI data and `nvidia-smi` from the temporary daemon pod.
3. Sysfs PCI data and the embedded NVIDIA subset of `pci.ids` when `nvidia-smi` is unavailable.

The same batched sysfs probe adds the NUMA node and reads current PF MAC
addresses transiently. On every worker, discovery compares those addresses
with host netplan device stanzas that contain both `match.macaddress` and a
non-empty `set-name`. If any NVIDIA PF matches on any worker in a hardware
group, the saved group has `netplanManaged: true`; otherwise it is `false`.
This flags a potential conflict between netplan naming and udev rules deployed
by NIC Configuration Operator without storing host-unique MAC addresses in the
shared group configuration. If `l8k generate` would emit a
`NicInterfaceNameTemplate` for an affected group, it returns a validation error
instead of generating conflicting NCO naming rules. Remove the affected
`set-name` stanzas from the host netplan configuration and re-run discovery to
clear the saved group state before retrying generation.

GPU topology probing adds connected GPU, GPU PCI address, and proximity data
per PF. A PIX-proximate NIC-to-GPU path can override heuristic traffic
classification and trigger rail reassignment. Probe failures are non-fatal;
discovery keeps the hardware data it can confirm.

## Profile Resolution

Fresh discovery persists a resolved `profile` block in `cluster-config.yaml`.
Values are resolved in this order:

1. Hardware-derived values and Launch Kit defaults.
2. Explicit CLI flags.

Fresh discovery fills missing profile fields:

- `deployment` defaults to `sriov`.
- `multirail` defaults to `true`.
- `routing` defaults to `destination-based`.
- `fabric` is persisted only when every discovered group has the same confirmed fabric.

When fabric probes disagree or cannot confirm a fabric, the saved `profile.fabric` remains empty. Set it explicitly during discovery or generation.

```bash
l8k discover \
  --user-config ./site-config.yaml \
  --fabric infiniband \
  --deployment-type rdma_shared \
  --multirail=false \
  --save-cluster-config ./cluster-config.yaml
```

On a fresh run without `--user-config`, the example profile in the installed
reference configuration is ignored. With `--user-config`, discovery replaces
only `clusterConfig`. It preserves every other loaded section and value,
including explicit `false` values, and then applies explicit CLI overrides.
Missing profile values are not filled during this refresh; supply them in the
file or through CLI flags if they are needed.

## Groups And Node Labels

Nodes with the same discovered PF fingerprint form a source group. Each group records its worker nodes, machine and GPU types, PFs, capabilities, dependent kernel modules, and deployment-time selector.

Discovery also writes up to two Launch Kit-owned labels:

| Label | Purpose |
| --- | --- |
| `nvidia.kubernetes-launch-kit.machine: <identifier>` | Selects one source hardware group using the same value persisted in `clusterConfig[].identifier`. |
| `nvidia.kubernetes-launch-kit.gpu: <gpuType>` | Selects compatible source groups that share a GPU type. |

The machine label and group `identifier` share one lowercase, vendor-free value
bounded to 30 bytes. The same normalization shortens common machine segments:
`ThinkSystem` becomes `ts` and `PowerEdge` becomes `pe`. Long identities keep
balanced machine/GPU prefixes plus a 6-character deterministic hash; unused
prefix space moves to the longer component. GPU-only labels preserve the
discovered case and `NVIDIA` segment, using the Kubernetes 63-byte limit and the
existing 8-character shortening hash. If the machine or GPU type cannot be
resolved, discovery uses a fallback `group-N` identifier and writes only the
labels it can construct.

See [Heterogeneous Clusters](heterogeneous-clusters.md) for group merging and targeted generation.

## Fabric Detection

For east-west PFs with an RDMA device, discovery reads:

```text
/sys/class/infiniband/<rdma-device>/ports/1/link_layer
```

It falls back to the equivalent path under `/host/sys`. The configured `link_layer` is used even when the port is down, so a new cluster can be discovered before switch links are active.

A group receives `linkType: Ethernet` or `linkType: InfiniBand` when its contributing PFs agree. Failed or unavailable PF probes do not contribute. Conflicting recognized values leave the group unresolved.

The top-level profile fabric is defaulted only when every source group has a non-empty, unanimous `linkType`.

## East-West And North-South NICs

Launch Kit generates networking resources from east-west NICs:

- A group containing only north-south NICs is omitted from `cluster-config.yaml`.
- In a mixed group, north-south PFs remain visible in the inventory but are not rendered into networking manifests.

This prevents out-of-band or DPU-facing NICs from consuming rail and NV-IPAM allocations intended for the workload fabric.

## Rail Collapsing

By default, `--collapse-nic-rails=true` records one rail per physical NIC. Multi-plane PFs on one physical port collapse to the master PF, while a model explicitly identified as dual-port keeps one rail per port.

Use one rail per PF for development or legacy configurations:

```bash
l8k discover \
  --collapse-nic-rails=false \
  --save-cluster-config ./cluster-config.yaml
```

A collapsed live topology might no longer exactly match a preset that describes every PF. In that case, discovery keeps the live classification and reports the preset deviation.

## Driver Module Safety

Launch Kit no longer walks `/sys/module/*/holders/` during discovery. That probe could consume excessive memory on large nodes, and the generated driver policy now enables both unload controls by default:

```yaml
docaDriver:
  unloadStorageModules: true
  unloadThirdPartyRDMAModules: true
  skipPreflightChecks: false
```

The per-group `storageModules` and `thirdPartyRDMAModules` fields remain available for explicit site input and safety warnings. Review running storage and RDMA workloads before allowing the driver container to unload dependent modules.

Set `skipPreflightChecks: true` only when the module state is managed and verified outside Launch Kit.

## Topology Presets

After resolving each group's `machineType` and `gpuType`, discovery searches the local preset catalog for an exact match. A match can replace heuristic traffic, rail, NUMA, and GPU-affinity assignments with the certified topology.

There is no machine-only or any-GPU fallback. If no exact pair matches, discovery keeps the hardware-derived topology.

## Preserve Bootstrap Resources

Discovery normally waits up to five minutes for daemon pods. It can continue with the Ready pods if some eligible nodes fail, but it stops if no daemon pod becomes Ready.

Keep the namespace when investigating image pulls, scheduling, or daemon startup:

```bash
l8k discover \
  --keep-namespace \
  --save-cluster-config ./cluster-config.yaml

kubectl get pods,events -n nvidia-k8s-launch-kit
kubectl describe pod -n nvidia-k8s-launch-kit <pod-name>
kubectl logs -n nvidia-k8s-launch-kit <pod-name>
```

Before each run, Launch Kit removes stale bootstrap workloads and `NicDevice` resources from an interrupted prior run.

## Debug Logging

```bash
l8k discover \
  --log-level debug \
  --log-file ./discovery.log \
  --save-cluster-config ./cluster-config.yaml
```

Debug logs include node eligibility, sysfs NIC and BlueField trust probing, machine/GPU source selection, GPU topology, fabric reads, PF traffic classification, preset matching, rail collapsing, hardware fingerprints, and label writes. Summary counts remain visible at info level.

## Saved Configuration

If `--save-cluster-config` is omitted, discovery rewrites the `--user-config`
file or creates `cluster-config.yaml` in the current directory. For a user
config refresh, only `clusterConfig` and explicit CLI overrides change. Source
comments are retained where possible, and a generated-file banner identifies
the discovery output.

Review the saved `profile` and `clusterConfig` sections before generation:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment
```
