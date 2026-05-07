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
| `--spectrum-x` | — | `RA2.1`, `RA2.2` | Enable Spectrum-X profile by passing the SPC-X RA version (replaces `--fabric` + `--deployment-type`; also folds in the legacy `--spcx-version`) |
| `--multiplane-mode` | Required with `--spectrum-x` | `none`, `swplb`, `hwplb`, `uniplane` | Multiplane mode |
| `--number-of-planes` | Required with `--spectrum-x` | `1`, `2`, `4` | Number of planes |
| `--multirail` | — | — | Enable multirail mode |
| `--save-deployment-files` | Yes | — | Output directory for generated YAMLs |
| `--groups` | — | `dgx-b200-nvidia-h100-nvl,poweredge-xe9680-nvidia-h200` | Restrict output to the named source groups (comma-separated). Mutually exclusive with `--gpu-type`. |
| `--gpu-type` | — | `NVIDIA-H200` | Restrict output to source groups whose `gpuType` matches (case-insensitive). Mutually exclusive with `--groups`. |
| `--for` | — | preset directory name | Skip discovery: synthesize `clusterConfig` from a topology preset. Requires `--node-selector`. List options with `l8k preset list`. |
| `--node-selector` | Required with `--for` | `key=val,key2=val2` | Identifies which nodes the synthesized clusterConfig targets at apply time. |

*Not required when `--spectrum-x` is used.

## Examples

```bash
# SR-IOV Ethernet RDMA (most common for GPU clusters)
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output

# Spectrum-X with hardware plane load balancing
l8k generate --user-config cluster-config.yaml \
  --spectrum-x RA2.2 --multiplane-mode hwplb --number-of-planes 4 \
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

# Generate from a known server SKU (no cluster discovery required)
l8k preset list   # see available presets
l8k generate --user-config cluster-config.yaml \
  --for ThinkSystem-SR680a-V3 \
  --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output
```

## Choosing `l8k discover` vs `--for`

- **`l8k discover` then `l8k generate`** — default flow. Run discovery against a live cluster to learn machine type, GPU type, and NIC topology, then generate from the resulting `cluster-config.yaml`.
- **`l8k generate --for <preset>`** — skip discovery entirely when the SKU is already known and there is a preset for it. Useful for ahead-of-time generation (CI scaffolding, lab runbooks, demos), or when you don't have `kubectl` access yet. Requires `--node-selector` to identify the target nodes at apply time.

A preset used with `--for` must declare `capabilities.nodes.{sriov,rdma,ib}` in its `topology.yaml`. All bundled presets do.

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
- Use `--groups <a,b,...>` (case-sensitive identifier list) or `--gpu-type <X>` (case-insensitive) to scope a generate to a subset of source groups in heterogeneous clusters. Mutually exclusive. Empty match is a validation error. Strict-subset filters split per-source rendering: NodePolicies emit one CR per source (each with its own machine-label nodeSelector but a shared bucket-level resourceName); IPPool/example DaemonSet emit one CR per bucket with an `In` list of source machine labels.

> [!CAUTION]
> Generation does not apply anything to the cluster. Use `--deploy` or `k8s-launch-kit-deploy` to apply.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-discover](../k8s-launch-kit-discover/SKILL.md) — Produce the cluster config needed for generation
- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — Apply generated manifests
- [k8s-launch-kit-dryrun](../k8s-launch-kit-dryrun/SKILL.md) — Preview before applying
