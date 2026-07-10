---
name: k8s-launch-kit-generate
version: 1.2.0
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
l8k generate --user-config <CONFIG> \
  --save-deployment-files <OUTPUT_DIR>
```

Configs produced by `l8k discover` already contain the resolved profile.
Profile flags remain available as generation-time overrides. When generation
uses a file-backed config, resolved defaults and CLI overrides are written back
to that source file; embedded `--for` generation does not write a config.

## Profile Selection Flags

| Flag | Required | Values | Description |
|------|----------|--------|-------------|
| `--fabric` | Auto-defaulted | `ethernet`, `infiniband` | Network fabric. Auto-defaults from the cluster's unanimous `linkType` when omitted (Unit 5 fabric probe); skipped+warned when groups disagree or any has unverified linkType. |
| `--deployment-type` | Auto-defaulted | `sriov`, `rdma_shared`, `host_device` | Deployment type. Auto-defaults to `sriov`. |
| `--spectrum-x` | — | `RA2.1`, `RA2.2` | Enable Spectrum-X profile by passing the SPC-X RA version. Implies ethernet fabric, sriov deployment, and multirail. |
| `--multiplane-mode` | Auto-defaulted with `--spectrum-x` | `none`, `swplb`, `hwplb`, `uniplane` | Auto-defaults from east-west PF deviceID: CX7 / BF3 SuperNIC → `uniplane`, CX8 → `swplb`, CX9 → `hwplb`. Skipped+warned when groups have mixed deviceIDs. |
| `--number-of-planes` | Auto-defaulted with `--spectrum-x` | `1`, `2`, `4` | Auto-defaults from deviceID: CX7 / BF3 → 1, CX8 → 2, CX9 → 4. |
| `--multirail` | Auto-defaulted | — | Auto-defaults to `true`. Explicit `multirail: false` in YAML and `--multirail=false` on the CLI are both preserved. |
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

- **`l8k discover` then `l8k generate`** — default flow. Discovery learns the hardware and persists the resolved profile, so generation needs only the resulting `cluster-config.yaml` unless an override is desired.
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

Generated YAMLs are written to the output directory under `network-operator/`. Each profile also emits a `values.yaml` (Helm values for the `nvidia/network-operator` chart) alongside the CR manifests:

```
output/
└── network-operator/
    ├── values.yaml                       # Phase 0 helm-install input for `l8k deploy`
    ├── 10-nicclusterpolicy.yaml
    ├── 11-nicnodepolicy-<group>.yaml
    ├── 20-ippool-<group>.yaml
    ├── 40-sriovnetworknodepolicy-<group>.yaml
    └── 50-sriovnetwork-<group>.yaml
```

`values.yaml` is rendered from the profile's `00-values.yaml` template. `--network-operator-release <MAJOR.MINOR>` populates the chart repository URL and image tag from the embedded catalog. To install or upgrade the chart alongside the CRs, pass `--deploy` (and `--overwrite-existing` when the release already exists with different values).

For Network Operator 26.1 and newer, the rendered values enable Maintenance
Operator requestor mode. Profiles with DOCA/OFED enable
`operator.maintenanceOperator.useRequestor`. Profiles that deploy the SR-IOV
Operator enable both the Network Operator drain requestor and
`sriov-network-operator.operator.externalDrainer`; the two SR-IOV switches are
a coordinated handoff and must not be separated. The generated
`MaintenanceOperatorConfig` gets the global limits from the config's
`maintenance` section.

Before release 26.1, OFED uses `maintenance.maxParallelUpgrades` and the SR-IOV
internal drainer uses `maintenance.maxUnavailable` through
`SriovNetworkPoolConfig`. Starting with 26.1, the global Maintenance Operator
limits are effective for both flows; the legacy OFED and SR-IOV pool limits do
not control requestor-mode concurrency.

## Reusing the Discovered Profile

`l8k discover` writes the final `profile.fabric`, `profile.deployment`,
`profile.multirail`, and any enabled Spectrum-X settings to the config. Run
`generate` without profile flags to reuse them; explicit generate flags still
win when a one-off override is needed.

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
