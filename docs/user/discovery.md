<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Cluster Discovery

`l8k discover` inventories NVIDIA NICs and GPUs, groups nodes with compatible hardware, and writes an editable `cluster-config.yaml`. Fresh discovery resolves a profile; a `--user-config` refresh preserves its non-hardware settings.

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

The `--node-selector` value seeds deployment selection; it does not limit which nodes run the discovery daemon. Discovery derives an exact selector for each discovered worker group. An explicit CLI override is still user intent and is checked against saved worker membership at deployment.

Before waiting for `NicDevice` resources, Launch Kit checks BlueField trust
mode with `mlxprivhost`. A restricted BlueField is excluded from the wait set.
If the same node has another non-restricted NVIDIA NIC, that node remains in
the wait set and its published devices are discovered normally. A failed or
unrecognized trust query is treated as non-restricted, matching NIC
Configuration Operator behavior.

## Run discovery

```bash
l8k discover \
  --kubeconfig "$KUBECONFIG" \
  --save-cluster-config ./cluster-config.yaml
```

Discovery is self-contained. It does not require Node Feature Discovery (NFD) or a pre-installed Network Operator.

The temporary discovery daemon requests 64 MiB of memory and has a 512 MiB
limit to accommodate hardware probes on nodes with large PCI inventories.

For Spectrum-X, discovery also saves the platform- and mode-specific desired
interface prefixes in `clusterConfig[].spectrumX.interfaceNames`. These do not
replace observed PF names. See [Spectrum-X naming](spectrum-x.md#vr-dual-nic-rails)
for VR differences, custom settings and regeneration behavior.

## Review saved inventory

Open `cluster-config.yaml` before generation. Confirm the intended workers and
source groups, east-west PF PCI addresses and rails, any north-south devices,
the chosen profile and release, and the node labels used for later selection.
Check the proposed network and driver settings against the site's approved
inputs in [Plan your deployment](profiles.md#check-prerequisites-and-site-inputs).
Discovery does not prove that a subnet or maintenance budget is safe for the
site. Correct the editable source config before generation if needed.

If a worker is missing, check its readiness, image pull, discovery pod,
`NicDevice` publication, and zero-trust status. See [Troubleshooting](troubleshooting.md#discovery-failures).

## How Discovery Works

1. Selects Kubernetes nodes that are Ready and schedulable.
2. Prepares the private `nvidia-k8s-launch-kit` namespace.
3. Creates the NIC Configuration Operator CRDs only when they are missing.
4. Runs a temporary NIC Configuration Daemon on the eligible nodes.
5. Detects nodes with NVIDIA NICs through a PCI vendor `0x15b3` sysfs probe and excludes BlueFields whose host trust level is `restricted`.
6. Reads the published `NicDevice` resources and probes GPU, NIC, fabric, NUMA, and rail data. It does not walk kernel-module holder graphs.
7. Groups nodes, writes Launch Kit node labels, and saves the result. Fresh discovery resolves profile settings; with `--user-config`, `clusterConfig` and `discoveredSecondaryNetwork` are replaced.
8. Deletes the temporary namespace and cluster-scoped bootstrap RBAC. The CRDs remain installed.

The bootstrap namespace is fixed. `--network-operator-namespace` is accepted for compatibility but ignored by discovery.

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

## Hardware And GPU Topology

For each NIC, discovery records the device ID, PCI address, RDMA device,
network interface, VPD identifiers, model, traffic direction, and rail. GPU and
host details use layered sources:

1. Existing `nvidia.com/gpu.machine` and `nvidia.com/gpu.product` node labels.
2. DMI data from the temporary daemon and GPU product observations from the
   same-node GPU sources described below.
3. Sysfs PCI data and the embedded NVIDIA subset of `pci.ids` when `nvidia-smi` is unavailable.

When GPU product labels are absent, each worker resolves its own `nvidia-smi`
or sysfs product before group aggregation. A product is saved only when all
workers agree. Missing or conflicting products leave `gpuType` unset with a
warning, so exact GPU-specific preset lookup is skipped; a representative
worker's fallback never replaces a known disagreement.

The same batched sysfs probe adds the NUMA node and reads current PF MAC
addresses transiently. Discovery mounts the host's `/etc` read-only and checks
worker nodes until it finds a match or has a conclusive observation for each.
If any NVIDIA PF matches a netplan
`match.macaddress` plus non-empty `set-name` rule, the group has
`netplanManaged: true`. If no match is found but a worker's observation is
unavailable or incomplete, the group has `netplanManagementUnknown: true` and
discovery warns. A readable host `/etc` with no netplan directory, or a complete
readable configuration with no MAC-based `set-name` rules, is a verified
negative and does not require PF MACs. When a MAC-based `set-name` rule exists,
an unreadable expected PF MAC also leaves the result unknown. Neither flag
persists host-unique MAC addresses. If `l8k generate` would emit a
`NicInterfaceNameTemplate` for a group with `netplanManaged: true`, it returns a
validation error because the confirmed netplan naming rule can conflict with
NCO naming. Remove the affected `set-name` stanzas and re-run discovery before
retrying generation. For `netplanManagementUnknown: true` without a confirmed
match, generation displays a warning in text and JSON output and continues.
Review the host netplan configuration and re-run discovery to resolve the
warning; do not clear the field manually. Re-run discovery for
configs created before `netplanManagementUnknown` was available.

GPU product and topology probing first tries a runtime-injected `nvidia-smi`
in the NIC daemon, then its legacy `/host/usr/bin/nvidia-smi` path when the
local observation is unavailable. If both observations are unavailable, discovery
selects the Ready main container of a same-node GPU Operator
`gpu-feature-discovery` DaemonSet, checking its owner UID and actual host `/sys`
mount. It rejects ambiguous sources and observations spanning a pod replacement
or container restart. Normal discovery looks up GFD across namespaces; that
fallback needs pod-list/read, DaemonSet-read, and `pods/exec` permissions.
It never injects a runtime class, allocates GPUs, or creates GPU probe pods.
Existing GPU runtime setup remains the cluster owner's responsibility.

The GPU query must cover an independently enumerated physical GPU PCI set from
the NIC daemon's host sysfs, and its indices must match the complete matrix.
Restricted or unresolvable MIG/vGPU views are unavailable topology. Missing
GPU labels are not proof of a CPU-only node: unlabeled host-driver nodes retain
the local product probe. Only a complete zero-GPU host enumeration without
conflicting GPU labels permits skipping GPU probing.

Connected GPU, GPU PCI address and proximity are saved only when every worker
and raw sibling PF represented by a retained PF agrees. NIC names are resolved
by node and PCI; representative RDMA aliases are not reused across workers.
Collapsed siblings must share a nearest GPU at the same proximity; overlapping
nearest-GPU ties are compatible. GPU balancing then chooses once per retained
rail on each node, so sibling planes do not consume extra assignments.
Only complete relevant PF coverage permits a PIX-based traffic override and
rail reassignment; physically dual-port BlueField-3 NICs stay north-south.
Incomplete or conflicting results emit warnings, leave unavailable fields
unset, and preserve hardware/preset fallback. Presets remain authoritative;
their GPU index retains a measured PCI address only when all workers confirm it.
VR NetworkBay rails are finalized afterward.

Collection uses up to four workers, with a 30-second budget per node and one
bounded GFD lookup per invocation. Source failures do not retry. NIC PF/NUMA
and netplan reads run in the NIC daemon. Check the selected GPU driver, toolkit
and container image against your workload requirements. An accepted topology
observation does not establish GPU or network deployment readiness.

### VR NetworkBay rails

For platform `vr`, discovery requires eight east-west ASIC NICs in four
complete NetworkBay pairs on each worker. A compatible NIC Configuration
Daemon and served `NicDevice` CRD must expose `status.networkBay.asic` and
`status.networkBay.peerPci`. Each pair must have reciprocal peer PCI addresses
and one first ASIC (`asic: 1`) and one second ASIC (`asic: 0`).
Discovery creates missing CRDs but does not upgrade an installed CRD. If an
existing operator manages `NicDevice`, its owner must update the daemon and
CRD to versions that publish and retain NetworkBay status before VR discovery.

NetworkBays are numbered 1–4 by the ascending PCI address of their first ASIC.
Discovery assigns the following zero-based rails:

| Rail | Members |
| --- | --- |
| 0 | NetworkBay 1 first ASIC + NetworkBay 2 second ASIC |
| 1 | NetworkBay 2 first ASIC + NetworkBay 1 second ASIC |
| 2 | NetworkBay 3 first ASIC + NetworkBay 4 second ASIC |
| 3 | NetworkBay 4 first ASIC + NetworkBay 3 second ASIC |

The mapping is finalized after GPU topology and preset enrichment and is also
used by read-only discovery. Existing PF order is preserved; this mapping
does not require a particular ASIC order within a rail. Missing, malformed or
incomplete NetworkBay status, or differing mappings among workers in a group,
stops discovery before inventory is saved. Non-VR rail assignment is unchanged.

With RA2.4/RA3.1 and ConnectX-9, fresh profile resolution defaults to `hwplb`
and eight planes. Explicit YAML/CLI settings take precedence; an existing-config
refresh preserves its profile settings as described below.

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
`clusterConfig` and `discoveredSecondaryNetwork`. It preserves every other loaded section and value,
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

The machine label and group `identifier` use the same lowercase value, shortened
when necessary. The GPU label preserves the discovered GPU name. Use the saved
`clusterConfig[].identifier` with `--groups` rather than constructing it yourself.
If the machine or GPU type cannot be resolved, a single group can have an empty
identifier; multiple groups use `group-N`. Every selected group still needs a
node selector that matches only its intended workers.

Before writing labels, discovery checks all proposed source identities and worker
memberships for collisions. It publishes selectors only after labeling succeeds
and a fresh list of every Node proves exact membership. Unresolved hardware
identities require another proven selector, such as the hostname of a singleton;
a broad PCI-vendor label is insufficient. Failed label writes or stale Launch Kit
machine/GPU labels on excluded nodes stop discovery. Inspect and correct those
labels deliberately before retrying; discovery does not remove unrelated labels.
Completed patches may remain after an error, so retries revalidate live labels.

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

- BlueField-3 NICs whose VPD model identifies two physical ports (`dual-port` or
  `2-port`) are classified as north-south, including when `NicDevice.status.dpu`
  is false or their SKU is absent from the built-in DPU list. Multiple PCI
  functions alone do not establish physical port count; single-port multi-plane
  SuperNICs continue to use the existing classification rules.
- A group containing only north-south NICs is omitted from `cluster-config.yaml`.
- In a mixed group, north-south PFs remain visible in the inventory but are not rendered into networking manifests.

This prevents out-of-band or DPU-facing NICs from consuming rail and NV-IPAM allocations intended for the workload fabric.

## Rail Collapsing

By default, `--collapse-nic-rails=true` records one rail per physical NIC. Multi-plane PFs on one physical port collapse to the master PF, while a model explicitly identified as dual-port keeps one rail per port.

Use one rail per PF when your deployment requires that layout:

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
  # Advanced users only. Values override generated driver env by name.
  # env:
  #   - name: THIRD_PARTY_RDMA_MODULES
  #     value: "nvidia_peermem"
```

The per-group `storageModules` and `thirdPartyRDMAModules` fields remain available for explicit site input and safety warnings. Review running storage and RDMA workloads before allowing the driver container to unload dependent modules.

Set `skipPreflightChecks: true` only when the module state is managed and verified outside Launch Kit.

Use `docaDriver.env` only for low-level driver settings that Launch Kit does
not model. Entries are forwarded through the generated NicClusterPolicy or
NicNodePolicy, override generated environment variables with the same name,
and can prevent the driver from reloading if misconfigured.

## Topology Presets

After resolving each group's `machineType` and `gpuType`, discovery searches the local preset catalog for an exact match. A match can replace heuristic traffic, rail, NUMA, and GPU-affinity assignments with the certified topology.

There is no machine-only or any-GPU fallback. If no exact pair matches, discovery keeps the hardware-derived topology.

## Preserve Bootstrap Resources

Discovery waits up to five minutes for daemon pods. It can proceed with the Ready
pods when other daemon pods are on nodes that are not Ready, or when the wait
timeout expires while the command is still active. Cancellation or a command
deadline stops discovery and reports the reason. Discovery stops if
no daemon pod becomes Ready.

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
config refresh, only `clusterConfig`, `discoveredSecondaryNetwork`, and explicit CLI overrides change. Source
comments are retained where possible, and a generated-file banner identifies
the discovery output.

Review the saved `profile` and `clusterConfig` sections before generation:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment
```

## Platform networking observations

Both ordinary and read-only discovery inspect known API-visible Multus and
reference CNI plugin installers and save `discoveredSecondaryNetwork`. This
adds cluster-wide DaemonSet list reads and NCP GETs for candidate controller
references, without pods/exec or resource mutation. If optional inspection is
denied or unavailable, hardware inventory is still saved with `unavailable`
evidence; cancellation remains an error. Fresh and user-config refresh preserve
desired component policies separately. Auto generation rejects ambiguous or
unavailable evidence rather than treating failed inspection as permission to
install. Review [secondary-network ownership](../reference/configuration.md#secondary-network-ownership)
before selecting explicit managed/external policy. Unsupported or host-only
installers can remain undetected; Multus detection does not imply plugin-suite
presence.

## Independent DPF inventory

For management-cluster and BMC discovery of the separate DPU estate, use the
[DPF discovery preview](dpf-discovery.md). Its configuration, artifacts and
qualification gates are independent of the Host workflow above.
