---
name: k8s-launch-kit-generate
version: 1.1.0
description: "Use this skill when the user wants to generate Kubernetes YAML manifests for NVIDIA networking deployment using k8s-launch-kit (l8k). Activate for: manifest generation, profile selection, choosing between SR-IOV/host-device/RDMA-shared/IPoIB/MacVLAN/Spectrum-X, creating deployment files, or when the user asks 'which profile should I use' or needs help choosing a network configuration."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Manifest Generation

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Generate Kubernetes YAML manifests for NVIDIA networking from a cluster config and profile selection.

## Usage

```bash
l8k generate --user-config <CONFIG> --fabric <FABRIC> --deployment-type <TYPE> \
  --save-deployment-files <OUTPUT_DIR>
```

## Profile Selection Flags

| Flag | Required | Values | Description |
|------|----------|--------|-------------|
| `--fabric` | Yes* | `ethernet`, `infiniband` | Network fabric type |
| `--deployment-type` | Yes* | `sriov`, `rdma_shared`, `host_device` | Deployment type |
| `--spectrum-x` | — | — | Enable Spectrum-X profile (replaces `--fabric` + `--deployment-type`) |
| `--spcx-version` | — | `RA2.1` | Spectrum-X reference architecture version |
| `--multiplane-mode` | — | `none`, `swplb`, `hwplb`, `uniplane` | Multiplane mode (requires `--spectrum-x`) |
| `--multirail` | — | — | Enable multirail mode |
| `--save-deployment-files` | Yes | — | Output directory for generated YAMLs |
| `--group` | — | `group-0` | Filter to a specific hardware group |

*Not required when `--spectrum-x` is used.

## Examples

```bash
# SR-IOV Ethernet RDMA (most common for GPU clusters)
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output

# Spectrum-X with hardware plane load balancing
l8k generate --user-config cluster-config.yaml \
  --spectrum-x --spcx-version RA2.1 --multiplane-mode hwplb \
  --save-deployment-files ./output

# Host device RDMA
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type host_device \
  --save-deployment-files ./output

# IPoIB RDMA shared (InfiniBand)
l8k generate --user-config cluster-config.yaml \
  --fabric infiniband --deployment-type rdma_shared \
  --save-deployment-files ./output

# Agent mode
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output \
  --output json 2>/dev/null
```

## Profile Quick Reference

| Profile | Flags | Use Case |
|---------|-------|----------|
| SR-IOV Ethernet RDMA | `--fabric ethernet --deployment-type sriov` | GPU clusters, ML training, HPC |
| Host Device RDMA | `--fabric ethernet --deployment-type host_device` | Legacy HPC, DPDK, full NIC access |
| MacVLAN RDMA Shared | `--fabric ethernet --deployment-type rdma_shared` | Multi-tenant Ethernet environments |
| IPoIB RDMA Shared | `--fabric infiniband --deployment-type rdma_shared` | InfiniBand shared workloads |
| SR-IOV InfiniBand | `--fabric infiniband --deployment-type sriov` | InfiniBand SR-IOV |
| Spectrum-X | `--spectrum-x` | AI cloud, multi-tenant GPU networking |

For detailed profile selection guidance (NIC constraints, multiplane modes, when to use each),
read `references/profile-decision-tree.md`.

## Output

Generated YAMLs are written to the output directory, organized by group:

```
output/
├── group-0/
│   ├── nicclusterpolicy.yaml
│   ├── ippool.yaml
│   ├── sriovnetworknodepolicy.yaml
│   ├── sriovnetwork.yaml
│   └── test-pod.yaml
```

## Auto-Detecting Multirail

After discovery, check if any group in cluster-config.yaml has `railNumber > 0` in its `physicalFunctions`. If so, add `--multirail` to the generate command.

## Common Mistakes

- **There is no `--profile` flag.** Profiles are selected via `--fabric` + `--deployment-type` (or `--spectrum-x`). Do NOT invent flags.
- **The multiplane flag is `--multiplane-mode`**, not `--spcx-multiplane` or `--multiplane`.

## Tips

- Default to SR-IOV Ethernet for new GPU cluster deployments unless told otherwise.
- For Spectrum-X, NIC type determines available multiplane modes — read `references/spectrum-x-guide.md`.
- Use `--group` to generate manifests for a single hardware group in multi-group clusters.

> [!CAUTION]
> Generation does not apply anything to the cluster. Use `--deploy` or `k8s-launch-kit-deploy` to apply.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-discover](../k8s-launch-kit-discover/SKILL.md) — Produce the cluster config needed for generation
- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — Apply generated manifests
- [k8s-launch-kit-dryrun](../k8s-launch-kit-dryrun/SKILL.md) — Preview before applying
