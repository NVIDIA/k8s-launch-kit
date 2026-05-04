---
name: k8s-launch-kit-discover
version: 1.1.0
description: "Use this skill when the user wants to discover their Kubernetes cluster's network hardware capabilities using k8s-launch-kit (l8k). Activate for: cluster discovery, hardware detection, NIC detection, finding what GPUs or NICs are in a cluster, creating a cluster config file, or when the user says 'discover' in the context of l8k or NVIDIA networking."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Cluster Discovery

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Discover cluster hardware and produce a `cluster-config.yaml` describing NICs, GPUs, rails, and node groups.

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
| `--network-operator-namespace` | — | `nvidia-network-operator` | Override operator namespace |
| `--user-config` | — | — | Base config to merge with discovered hardware |
| `--node-selector` | — | — | Restrict to matching nodes |
| `--image-pull-secrets` | — | — | Image pull secret names for NicClusterPolicy (comma-separated) |

## Examples

```bash
# Basic discovery
l8k discover \
  --kubeconfig ~/.kube/config \
  --save-cluster-config ./cluster-config.yaml

# Using $KUBECONFIG env var (no --kubeconfig needed)
l8k discover --save-cluster-config ./cluster-config.yaml

# Non-default operator namespace
l8k discover \
  --kubeconfig ~/.kube/config \
  --network-operator-namespace network-operator \
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
  - groupName: "group-0"
    capabilities: [sriov, multirail]
    physicalFunctions:
      - deviceID: "101e"
        pfName: "eth0"
        railNumber: 0
    workerNodes: [node-01, node-02]
    nodeSelector:
      feature.node.kubernetes.io/pci-15b3.present: "true"
    thirdPartyRDMAModules: [nv_peer_mem]
```

## Prerequisites

- NVIDIA Network Operator Helm chart installed in the cluster
- Node Feature Discovery (NFD) active with `NodeFeature` CRDs populated
- Worker nodes with label `feature.node.kubernetes.io/pci-15b3.present=true`

## Tips

- If discovery fails with "no pods found for DaemonSet", the error will suggest using `--network-operator-namespace`. Common namespaces are `nvidia-network-operator` and `network-operator`.
- Discovery uses server-side apply (field owner `l8k-discovery`) — it won't conflict with an existing NicClusterPolicy.
- After determining each group's `(machineType, gpuType)`, discovery looks up a topology preset under `presets/` using **exact-match** lookup on that pair. A matching preset overrides heuristic-derived topology fields (traffic class, rail, NUMA, GPU affinity). There is no any-GPU fallback — a preset with empty `gpuType:` is rejected at load time. If no preset matches, discovery proceeds with heuristic classification.
- If you already know the SKU and want to skip cluster discovery entirely, use `l8k generate --for <preset>` (see `k8s-launch-kit-generate`).

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-config](../k8s-launch-kit-config/SKILL.md) — Understand the config file discovery produces
- [k8s-launch-kit-generate](../k8s-launch-kit-generate/SKILL.md) — Generate manifests from cluster config
