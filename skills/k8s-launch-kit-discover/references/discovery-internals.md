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
`/sys/bus/pci/devices/*/vendor` for `0x15b3`. For BlueField-2, BlueField-3,
BlueField-3 Lx, and BlueField-4 devices, the same probe runs
`mlxprivhost -d <pci> q`. Devices that successfully report
`level: restricted` are excluded, mirroring NIC Configuration Operator's
zero-trust handling; failed or unrecognized trust queries remain discoverable,
also matching the operator. A node enters the wait set when at least one
discoverable NIC remains. A restricted-only node is reported separately and
excluded so discovery does not wait for a `NicDevice` that can never be
published. The `--node-selector` flag is **not** used for scheduling or the
wait — it only populates the **saved** `cluster-config.yaml` `nodeSelector` for
deploy time. (Fallback: when no REST config is available for pod exec, the
legacy `--node-selector` label filter is used instead.)

Discovery reports each confirmed host-restricted BlueField PCI function with its
node and PCI address in normal CLI output, before deleting the bootstrap namespace.
The NIC-probe summary logs `restrictedDevicesSkipped`; this counts PCI functions,
not physical cards, separately from `northSouthPFsFiltered` (published devices
removed during group classification). Mixed nodes are scanned fully, so a usable
ConnectX does not hide a restricted BlueField later in PCI order. Restricted devices
remain absent from the saved configuration because the daemon does not publish them.
A failed or unrecognized trust query is not evidence of restriction and is not counted.
If the scan fails after verifying a usable NIC, the node remains eligible and a
warning marks exclusion reporting as incomplete. Read-only publisher eligibility
probes retain their early-exit behavior and do not collect this full inventory.

### Leftover pre-clean

Teardown runs in a `defer`, so a crashed/killed run can leave the namespace
behind. Before bootstrapping, discovery calls `Cleanup` + `WaitForNamespaceDeleted`
(2-min bound) to fully clear `nvidia-k8s-launch-kit` (cascades DaemonSet/pods/SA/
ConfigMap/stale `NicDevice` CRs; CRDs preserved), then `Ensure` deploys fresh.

---

## Per-PF Discovery Integration Point

`pkg/networkoperatorplugin/discovery/gputopology.go` owns the extension rails
for host attributes that `NicDevice` does not publish:

1. `pfDiscoveryProbeCmd` walks `/sys/bus/pci/devices/*` once, filters vendor
   `0x15b3` and virtual functions, and emits one PCI-keyed record per PF:
   `<pci>|<attribute>=<value>|...`.
2. `parsePFDiscoveryBlock` normalizes the PCI address and retains every
   non-empty key/value pair, including unknown keys. This makes the transport
   format forward-compatible.
3. Consumers choose the correct lifetime for each attribute:
   `applyPFDiscoveryAttributes` validates stable values such as NUMA and maps
   them onto `config.PFConfig`; node-specific values such as MAC addresses stay
   transient and feed group-level decisions.

NUMA and current netdevice MAC address use this path today. MACs are compared
with a sanitized projection of host netplan device stanzas; a
`match.macaddress` plus non-empty `set-name` match sets the group's
`netplanManaged` flag. Every worker is probed because MACs and netplan files are
node-local, and the group flag is true if any worker matches. These PF/netplan
reads stay in the NIC daemon, independently of GPU source selection.

To integrate a new per-PF field, add its best-effort read to
`pfDiscoveryProbeCmd`, decide whether its lifetime permits persistence in
`PFConfig` or requires transient aggregation, and extend parser/consumer tests.
Reuse this walk for related host attributes. The GPU physical-device denominator
is a separate complete inventory check, not a best-effort attribute. One missing
optional attribute must not suppress other attributes or PF records.

### GPU source ownership and completeness

`gpuprobe.go` acquires GPU product/query/matrix observations first from the
NIC daemon (local injected executable, then legacy host path), with an existing
same-node GPU Operator GFD main container as fallback. It checks Ready state,
DaemonSet owner UID, real host `/sys` backing, pod identity and container lifetime.
It never modifies pod runtime classes or schedules GPU workloads. Normal lookup
is across namespaces; read-only callers provide the GPU Operator namespace via
`WithReadOnlyGPUTopology` plus REST credentials. GFD fallback requires pod
read/exec and DaemonSet read permissions. No-option/REST-only library calls keep
their previous behavior; consume the context UI sink for optional-failure warnings.

`gpuobservation.go` validates the GPU query against independently enumerated
physical host GPU PCIs, unique indices and full matrix coverage. Missing GPU
labels remain unknown, and only a complete empty host inventory without
conflicting labels allows skipping GPU execution. Restricted/MIG/vGPU visibility
that cannot be reconciled with physical devices is unavailable topology.
`gpuenrichment.go` joins node-local PCI/RDMA identities and requires agreement
for every worker/raw PF in the actual rail-collapse projection. Incomplete matrix
coverage cannot demote unobserved NICs through the PIX gate. Aliases and source
details stay transient. It intersects siblings' nearest-GPU candidates at a
common proximity before balancing once per retained rail on each node; it does
not balance individual collapsed planes. The per-node collector also owns the
sysfs/pci.ids product fallback. Missing or conflicting worker products remain
unknown before preset lookup; no later representative product fallback can
overwrite that evidence. Preset precedence and final VR rail assignment remain
unchanged; stale measured GPU PCI addresses are cleared/reconciled after presets.

The collector bounds concurrency to four and each node to 30 seconds, with one
bounded GFD lookup and no retries. Optional failures retain hardware/preset
fallback with warnings; parent cancellation is fatal. Hardware/runtime/image
qualification remains separate from parser and fake-client validation.

---

## Fabric observations

`discovery/fabric.go` collects configured `link_layer` independently of link-up
state. Grouping preserves the actual retained-to-raw PF collapse projection in
transient `groupPFMembers`, associated with the sorted worker set so group
filtering, identifiers or ordering cannot misassociate it. `buildNodePFIdentities`
joins raw NicDevice records by node/normalized PCI; aliases stay transient.

The probe resolves RDMA mappings through each PCI sysfs subtree, completes the
port manifest for every expected PCI, then returns one result per manifest entry
and terminal counts. Missing/duplicate/truncated/failed records make coverage
unknown. Every selected worker, projected sibling PF and enumerated port must
agree before group LinkType is saved. No default port-1 or representative-only
success is used. An observed IB PF without a netdev emits a host-provisioning
warning without pretending the RDMA shared plugin is ready.

Normal discovery enables this collector; read-only discovery requires explicit
`WithReadOnlyFabricDiscovery()` and REST credentials. No-option and REST-only
read-only calls gain no fabric execs. Collection uses at most four goroutines,
ten seconds per worker, no timeout retries and fatal caller cancellation.

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

North-south classification uses the operator's `NicDevice.status.dpu` flag, a
built-in positive-only DPU product-ID list, or a BlueField-3 device ID (`a2dc` /
`a2d9`) with a VPD model identifying two physical ports (`dual-port` / `2-port`).
The list is not exhaustive. The physical dual-port rule applies even when the
operator reports `dpu=false` in NIC mode and is preserved during GPU PIX
reclassification. Multiple PCI functions do not prove physical port count.

Devices classified as north-south:

- Are excluded from generated manifests (no SriovNetworkNodePolicy, no
  MacvlanNetwork, etc.).
- Remain in a mixed group's `pfs` with `traffic: north-south` and no rail.
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

## Driver Module Safety

Discovery does not walk `/sys/module/*/holders/`. That probe was removed
because it could consume excessive memory on large nodes. The generated
driver policy enables both storage and third-party RDMA unload controls by
default. The per-group `storageModules` and `thirdPartyRDMAModules` fields
remain available for explicit site input and safety warnings; their absence
does not mean that no dependent modules exist. Review running storage and RDMA
workloads before deployment. The canonical behavior is in
`docs/user/discovery.md#driver-module-safety`.

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
- Node selectors are updated to cover merged nodes; generated hardware resources also retain source-group IDs. Deploy checks exact membership against effective worker inventory.
- Explicitly supplied `thirdPartyRDMAModules` values are merged as a **union**.

### Selected-source ownership

Buckets retain their original source groups. A strict subset keeps shared bucket
resources but emits flat-selector policies per selected source. Source identifiers
must be unique. Generated SR-IOV policies and IPPools record those source IDs;
readiness evaluates live selectors against the intended worker union. Empty preset
worker lists are explicitly selector-only and unverified.

---

## Discovery Output Lifecycle

1. Discovery writes the cluster config to the path specified by `--save-cluster-config`.
2. If `--save-cluster-config` is omitted, the config is held in memory and passed
   directly to the generate phase (when used in a pipeline).
3. The saved file can be edited by hand before passing it to `--user-config` for
   generation. This allows operators to remove groups, adjust rail numbers, or add
   custom node selectors.
