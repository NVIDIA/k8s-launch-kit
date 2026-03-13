# Discovery Internals

This document covers the internal mechanics of l8k cluster discovery. It is intended as
a reference for agents and advanced users who need to understand how discovery decisions
are made.

---

## Non-Disruptive Discovery

Discovery must be safe to run on production clusters. To achieve this, l8k never
deletes or recreates the NicClusterPolicy. Instead it uses **server-side apply** with
field owner `l8k-discovery`.

- If no NicClusterPolicy exists, discovery creates a minimal one that enables only the
  NIC configuration daemon.
- If a NicClusterPolicy already exists (e.g., managed by Helm or another controller),
  discovery patches only the fields it owns (`l8k-discovery`). Fields owned by other
  managers are left untouched.
- On conflict (another manager owns a field discovery needs to set), discovery reports
  the conflict and exits with code 3 rather than forcing an overwrite.

This approach ensures discovery never disrupts an existing Network Operator deployment.

---

## Node Grouping Algorithm

Nodes are grouped by their **physical-function layout**. Two nodes belong to the same
group if and only if:

1. They have the same set of east-west PCI device IDs (e.g., both have two `101e`
   devices).
2. They have the same rail count (number of east-west PFs).
3. They share the same GPU product label (if present).

The grouping is deterministic: given the same cluster state, discovery always produces
the same groups in the same order.

### Steps

1. For each node, collect all PCI devices with vendor ID `15b3` (Mellanox/NVIDIA).
2. Filter out north-south devices (see below).
3. Sort remaining PFs by PCI address.
4. Create a fingerprint string: `deviceID_0:deviceID_1:...:gpuProduct`.
5. Nodes with identical fingerprints are placed in the same group.
6. Groups are sorted by fingerprint for stable ordering.

---

## North-South Detection

BlueField DPU devices are identified by matching their **OPN (Orderable Part Number)**
against a built-in product-ID list maintained in the l8k source code. This list covers
all BlueField-2 and BlueField-3 SKUs.

Devices classified as north-south:

- Are excluded from generated manifests (no SriovNetworkNodePolicy, no
  MacvlanNetwork, etc.).
- Are still recorded in the cluster config under a `northSouthDevices` field for
  informational purposes.
- Do not contribute to rail numbering.

If a node has only north-south devices and no east-west devices, that node is excluded
from all hardware groups.

---

## East-West Classification

All non-DPU NICs are classified as east-west. This includes:

- **ConnectX-6 Dx, ConnectX-7** -- standard NICs used for SR-IOV, RoCE, GPUDirect RDMA.
- **SuperNIC (CX-8)** -- high-performance NICs with hardware offloads.

East-west PFs are assigned **sequential rail numbers** starting from 0, ordered by PCI
bus address. Rail numbers are per-group (each group starts at rail 0).

---

## OFED Dependent Module Probing

After the NIC configuration daemon pods are running, discovery execs into each pod to
inspect kernel module dependencies:

```
/sys/module/<module>/holders/
```

For each MLX kernel module (mlx5_core, mlx5_ib, etc.), discovery checks the `holders`
directory to find modules that depend on OFED. Common dependents include:

- `nv_peer_mem` -- legacy GPUDirect RDMA peer memory module.
- `nvidia_peermem` -- modern GPUDirect RDMA peer memory module.
- `mlx5_vdpa` -- vDPA offload module.

The discovered dependents are saved per group as `ofedDependentModules`. These are used
during manifest generation to configure the NicClusterPolicy's `ofedDriver` section
with the correct secondary module list.

---

## Group Merging

After initial grouping, l8k attempts to **merge groups** that are functionally
equivalent to reduce the number of generated manifest sets.

### Merge Criteria

Two groups are eligible for merging if:

1. They have the same **GPU product type** (e.g., both are A100-SXM4-80GB).
2. They have the same **east-west rail count**.
3. Their PF device IDs are identical.

### Merge Behavior

- Worker node lists are concatenated.
- Node selectors are updated to cover all merged nodes.
- `ofedDependentModules` are merged as a **union** (all unique modules from both groups).

### Merge Exceptions

Merging is **skipped** in the following cases:

- **Spectrum-X fabric**: Spectrum-X deployments require per-switch-group policies, so
  groups must remain separate.
- **Single group**: Nothing to merge.
- **`--group` filter active**: When the user targets a specific group by name, merging
  is disabled to preserve the explicit selection.

---

## Discovery Output Lifecycle

1. Discovery writes the cluster config to the path specified by `--save-cluster-config`.
2. If `--save-cluster-config` is omitted, the config is held in memory and passed
   directly to the generate phase (when used in a pipeline).
3. The saved file can be edited by hand before passing it to `--user-config` for
   generation. This allows operators to remove groups, adjust rail numbers, or add
   custom node selectors.
