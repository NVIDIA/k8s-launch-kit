# Discovery Internals

This document covers the internal mechanics of l8k cluster discovery. It is intended as
a reference for agents and advanced users who need to understand how discovery decisions
are made.

---

## Self-Contained Bootstrap (no Network Operator required)

Discovery does not depend on a pre-installed Network Operator. On every `l8k discover`
run, the CLI bootstraps just the NIC Configuration Daemon (and its CRDs, if missing)
into a private namespace, reads the resulting `NicDevice` CRs, then tears the
namespace down.

The objects created by `Ensure(ctx, c, opts)` (`pkg/nicconfigdaemon/`) on every run:

| Kind | Name | Scope | Lifecycle |
|------|------|-------|-----------|
| `Namespace` | `nvidia-k8s-launch-kit` | cluster | created on `Ensure`, deleted on `Cleanup` (cascade) |
| `CustomResourceDefinition` | `nicdevices.configuration.net.nvidia.com` and 4 others | cluster | applied **only when absent** — never overwritten; persist after cleanup |
| `ServiceAccount` | `k8s-launch-kit-nic-config-daemon` | namespaced | created on `Ensure`, deleted by namespace cascade |
| `ClusterRole` | `k8s-launch-kit-nic-config-daemon` | cluster | created on `Ensure`, deleted explicitly on `Cleanup` |
| `ClusterRoleBinding` | `k8s-launch-kit-nic-config-daemon` | cluster | created on `Ensure`, deleted explicitly on `Cleanup` |
| `ConfigMap` | `nic-configuration-operator-supported-nic-firmware` (empty `data: {}`) | namespaced | empty stand-in so the daemon's startup Get succeeds; deleted by namespace cascade |
| `DaemonSet` | `nic-configuration-daemon` | namespaced | image = `<networkOperator.repository>/nic-configuration-operator-daemon:<networkOperator.componentVersion>`; **no NFD `nodeSelector`** + tolerates all taints + node-name affinity for nodes that are Ready and schedulable; deleted by namespace cascade |

### Why renamed RBAC

The upstream nic-configuration-operator Helm chart uses cluster-scoped names
`controller-manager`, `manager-role`, `manager-rolebinding`. Discovery uses
`k8s-launch-kit-nic-config-daemon` for all three so it can coexist with a Network
Operator install without collision.

### Why CRDs persist

Cleanup deletes the namespace and the cluster RBAC but **not** the CRDs. If the
discover run was the thing that installed them, removing them would orphan any
`NicDevice` CRs other tools or future runs may rely on — and CRD deletion would
require also removing every CR. The cost of leaving them: 5 schema definitions in
the API server, no controllers, no consumed resources.

### `--keep-namespace`

Adds a single skip to the `defer Cleanup(...)` call so the bootstrap survives the
exit. Use for `kubectl describe pod -n nvidia-k8s-launch-kit` post-mortems when
daemon pods don't go Ready (ImagePullBackOff, missing pull secret).

### Node scheduling + NIC wait set (NFD-independent)

Before bootstrapping, discovery lists Nodes and builds a deterministic allow-list
of nodes with `status.conditions[Ready] == True` and `spec.unschedulable ==
false`. The DaemonSet has **no NFD `nodeSelector`** and tolerates all taints
(`tolerations: [{operator: Exists}]`), but it also renders required node-name
affinity with one `metadata.name In [node]` selector term per allowed node. This
keeps discovery NFD-independent while avoiding DaemonSet pods on NotReady or
cordoned nodes. Eligible control-plane/tainted nodes are still included, which
matters on small or single-node clusters that carry data-plane NICs there.
`waitForDaemonSetPods`
proceeds when all targeted pods are Ready, or when every not-Ready targeted pod
is *stuck* (`ImagePullBackOff` / `CrashLoopBackOff` / etc., via `podStuck`) and
at least one pod is Ready; on timeout it still proceeds with the Ready set, and
only hard-fails when no pod ever became Ready.

To pick which nodes to wait on for `NicDevice` CRs, `filterNodesWithNICs` execs a
sysfs probe (`sysfsMellanoxNICPresentCmd`) into each daemon pod that scans
`/sys/bus/pci/devices/*/vendor` for `0x15b3`; only NIC-bearing nodes enter the
wait set. The `--node-selector` flag is **not** used for scheduling or the wait —
it only populates the **saved** `cluster-config.yaml` `nodeSelector` for deploy
time. (Fallback: when no REST config is available for pod exec, the legacy
`--node-selector` label filter is used instead.)

### Leftover pre-clean

Teardown runs in a `defer`, so a crashed/killed run can leave the namespace
behind. Before bootstrapping, discovery calls `Cleanup` + `WaitForNamespaceDeleted`
(2-min bound) to fully clear `nvidia-k8s-launch-kit` (cascades DaemonSet/pods/SA/
ConfigMap/stale `NicDevice` CRs; CRDs preserved), then `Ensure` deploys fresh.

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

The discovered dependents are saved per group as `thirdPartyRDMAModules` for visibility
and warnings. When `unloadThirdPartyRDMAModules` is true, manifest generation sets the
`UNLOAD_THIRD_PARTY_RDMA_MODULES` environment variable to `"true"` (a boolean flag) in
the NicClusterPolicy's `ofedDriver` section; the module names themselves are not passed
to that variable.

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
- `thirdPartyRDMAModules` are merged as a **union** (all unique modules from both groups).

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
