---
name: k8s-launch-kit-discover
version: 1.2.0
description: "Use this skill when the user wants to discover their Kubernetes cluster's network hardware capabilities using k8s-launch-kit (l8k). Activate for: cluster discovery, hardware detection, NIC detection, finding what GPUs or NICs are in a cluster, creating a cluster config file, or when the user says 'discover' in the context of l8k or NVIDIA networking."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Cluster Discovery

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Discover cluster hardware and produce a `cluster-config.yaml` describing NICs, GPUs, rails, and node groups.

**l8k discover is self-contained.** It does NOT require a pre-installed
Network Operator. On every run it bootstraps the NIC Configuration Daemon
(and the 5 nic-configuration-operator CRDs, only if missing) into a private
namespace `nvidia-k8s-launch-kit`, reads `NicDevice` CRs published by the
daemon, then tears the namespace down on exit (unless `--keep-namespace`
is set). The daemon image is pulled from `<networkOperator.repository>/
nic-configuration-operator-daemon:<networkOperator.componentVersion>` (both
fields come from `l8k-config.yaml`).

## Usage (from AI agent)

```bash
l8k discover \
  --kubeconfig ~/.kube/config \
  --save-cluster-config ./cluster-config.yaml \
  --output json 2>/dev/null | jq .
```

## Usage (human-interactive)

```bash
l8k discover --save-cluster-config <OUTPUT> [--kubeconfig <PATH>]
```

## Flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--kubeconfig` | — | `$KUBECONFIG` env var | Path to kubeconfig (optional — falls back to env var) |
| `--save-cluster-config` | Yes | — | Output path for cluster-config.yaml |
| `--keep-namespace` | — | `false` | Skip teardown of the `nvidia-k8s-launch-kit` bootstrap namespace (for debugging) |
| `--network-operator-namespace` | — | — | **Deprecated for `discover`**: accepted but ignored. The daemon always runs in `nvidia-k8s-launch-kit`. Still used by `l8k generate` / `l8k deploy`. |
| `--user-config` | — | — | Base config to merge with discovered hardware |
| `--node-selector` | — | `feature.node.kubernetes.io/pci-15b3.present=true` | Value written into the **saved** `cluster-config.yaml` `nodeSelector` (for deploy time). It does **not** gate discovery scheduling or the NicDevice wait set — the daemon runs on all nodes and NIC-bearing nodes are detected via a sysfs `0x15b3` probe. |
| `--image-pull-secrets` | — | — | Image pull secret names (comma-separated). Forwarded onto the bootstrapped DaemonSet pod spec. |

## Examples

```bash
# Basic discovery
l8k discover \
  --kubeconfig ~/.kube/config \
  --save-cluster-config ./cluster-config.yaml

# Using $KUBECONFIG env var (no --kubeconfig needed)
l8k discover --save-cluster-config ./cluster-config.yaml

# Keep the bootstrap namespace for debugging
l8k discover \
  --kubeconfig ~/.kube/config \
  --keep-namespace \
  --save-cluster-config ./cluster-config.yaml

# Merge with existing config
l8k discover --user-config my-config.yaml \
  --kubeconfig ~/.kube/config \
  --save-cluster-config ./cluster-config.yaml

# Agent mode (JSON output)
l8k discover \
  --kubeconfig ~/.kube/config \
  --save-cluster-config ./cluster-config.yaml \
  --output json 2>/dev/null
```

## Output Format

The generated `cluster-config.yaml` contains a `clusterConfig[]` array. Each element is a hardware group:

```yaml
clusterConfig:
  - identifier: "dgx-b200-nvidia-h100-nvl"
    machineType: DGX-B200
    gpuType: NVIDIA-H100-NVL
    capabilities:
      nodes:
        sriov: true
        rdma: true
    pfs:
      - deviceID: "101e"
        networkInterface: "eth0"
        rail: 0
    workerNodes: [node-01, node-02]
    nodeSelector:
      nvidia.kubernetes-launch-kit.machine: "DGX-B200-NVIDIA-H100-NVL"
    thirdPartyRDMAModules: [nv_peer_mem]
```

Discovery patches every node in the group with two labels:

- `nvidia.kubernetes-launch-kit.machine: <machineType>-<gpuType>` — per-source-group
  identity, used as the source group's `nodeSelector`.
- `nvidia.kubernetes-launch-kit.gpu: <gpuType>` — used as the merged-group
  `nodeSelector` when `l8k generate` auto-merges source groups sharing a GPU type.

Label values keep their original case (matching `nvidia.com/gpu.product` style) since
upstream discovery already trims whitespace and replaces spaces with hyphens. Values
that would exceed the Kubernetes 63-char label-value limit are skipped (logged at
debug). The group's `identifier` is the lowercase resource-name form of the machine
label (RFC 1123 — required for downstream NicNodePolicy / SriovNetworkNodePolicy
naming). When `machineType` or `gpuType` couldn't be resolved (GPU operator labels
absent and hardware probe failed), a fallback `group-N` identifier is used and the
machine label is not written; the GPU label is still written when `gpuType` alone is
resolved.

## Prerequisites

- Node Feature Discovery (NFD) is **not** required. The bootstrap daemon
  runs on every node — no `nodeSelector`, and it tolerates all taints so it
  also lands on control-plane nodes (small/single-node clusters may carry
  NICs there). NIC-bearing nodes are detected by a sysfs probe for PCI vendor
  `0x15b3` rather than the NFD `feature.node.kubernetes.io/pci-15b3.present`
  label.
- Image-pull access from every worker node to
  `<networkOperator.repository>/nic-configuration-operator-daemon:<networkOperator.componentVersion>`.
  Use `--image-pull-secrets <name>` (or `networkOperator.imagePullSecrets`
  in the config file) for private registries.
- Network Operator does **NOT** need to be pre-installed.

## Tips

- The bootstrap namespace is **always** `nvidia-k8s-launch-kit` — not configurable. The
  daemon's SA / ClusterRole / ClusterRoleBinding are renamed to
  `k8s-launch-kit-nic-config-daemon` so they don't collide with the cluster-scoped
  names a coexisting Network Operator install would create.
- CRDs (`nicdevices`, `nicconfigurationtemplates`, etc.) are applied **only when missing**.
  If they already exist (because Network Operator or a prior l8k discover run created
  them), they're left alone — discovery never overwrites a different version.
- After discovery finishes the namespace is torn down (cascade-delete handles SA /
  RoleBinding / DaemonSet / pods / NicDevice CRs); the CRDs intentionally persist so
  any external NicDevice consumers survive. Cluster-scoped ClusterRole /
  ClusterRoleBinding are deleted explicitly.
- Pass `--keep-namespace` to leave the namespace in place — useful when debugging
  daemon pod start-up failures (`kubectl describe pod -n nvidia-k8s-launch-kit`).
- Before bootstrapping, discovery pre-cleans the `nvidia-k8s-launch-kit` namespace
  (deletes any leftover DaemonSet/pods/NicDevice CRs from a crashed prior run and
  waits up to 2 min for it to clear) so a fresh daemon is never layered on stale state.
- Discovery waits up to 5 minutes for daemon pods to be Ready, but tolerates
  stuck pods: if some pods are wedged (e.g. ImagePullBackOff/CrashLoopBackOff on
  an unrelated node) it proceeds with the Ready ones rather than blocking the
  whole window, and only aborts if **no** pod ever becomes Ready. Common causes
  of a total failure: image tag missing in your registry, pull-secret missing.
- If discovery reports "no nodes with an NVIDIA NIC (PCI vendor 15b3) were found",
  the daemon ran but no node's sysfs exposed a `0x15b3` device (no Mellanox/NVIDIA
  NICs, or `/sys` not mounted/readable in the pod).
- After determining each group's `(machineType, gpuType)`, discovery looks up a topology preset under `presets/` using **exact-match** lookup on that pair. A matching preset overrides heuristic-derived topology fields (traffic class, rail, NUMA, GPU affinity). There is no any-GPU fallback — a preset with empty `gpuType:` is rejected at load time. If no preset matches, discovery proceeds with heuristic classification.
- If you already know the SKU and want to skip cluster discovery entirely, use `l8k generate --for <preset>` (see `k8s-launch-kit-generate`).

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-config](../k8s-launch-kit-config/SKILL.md) — Understand the config file discovery produces
- [k8s-launch-kit-generate](../k8s-launch-kit-generate/SKILL.md) — Generate manifests from cluster config
