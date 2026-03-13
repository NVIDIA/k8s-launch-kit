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
| `--label-selector` | — | — | Restrict to matching nodes |

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
    ofedDependentModules: [nv_peer_mem]
```

## Prerequisites

- NVIDIA Network Operator Helm chart installed in the cluster
- Node Feature Discovery (NFD) active with `NodeFeature` CRDs populated
- Worker nodes with label `feature.node.kubernetes.io/pci-15b3.present=true`

## Tips

- If discovery fails with "no pods found for DaemonSet", the error will suggest using `--network-operator-namespace`. Common namespaces are `nvidia-network-operator` and `network-operator`.
- Discovery uses server-side apply (field owner `l8k-discovery`) — it won't conflict with an existing NicClusterPolicy.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-config](../k8s-launch-kit-config/SKILL.md) — Understand the config file discovery produces
- [k8s-launch-kit-generate](../k8s-launch-kit-generate/SKILL.md) — Generate manifests from cluster config
