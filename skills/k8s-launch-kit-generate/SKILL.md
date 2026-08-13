---
name: k8s-launch-kit-generate
version: 1.2.6
description: "Use this skill when the user wants to generate Kubernetes YAML manifests for NVIDIA networking deployment using k8s-launch-kit (l8k). Activate for: manifest generation, profile selection, choosing between SR-IOV/host-device/RDMA-shared/IPoIB/MacVLAN/Spectrum-X, creating deployment files, or when the user asks 'which profile should I use' or needs help choosing a network configuration."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Manifest Generation

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

This workflow uses the default `host` target; `--target host` is equivalent.
The CLI snapshots generation arguments and runs the concrete Host generation
operation through the target registry; command syntax and artifacts are
unchanged.

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
| `--spectrum-x` | — | `RA2.1`, `RA2.2`, `RA2.3` | Enable Spectrum-X profile by passing the SPC-X RA version. Implies ethernet fabric, sriov deployment, and multirail. |
| `--multiplane-mode` | Auto-defaulted with `--spectrum-x` | `none`, `swplb`, `hwplb` | Auto-defaults from GPU platform plus east-west PF deviceID: H100/H200/B200/GB200 → `none`; B300/GB300 → the GA `swplb` path. Platform cannot identify `hwplb`; select it explicitly. Unknown platforms fall back to NIC family. |
| `--number-of-planes` | Auto-defaulted with `--spectrum-x` | `1`, `2`, `4` | Single-plane platforms → 1; B300/GB300 → 2. Pass 4 explicitly for quad-plane B300. An explicit `none` also implies 1, and an explicit 1 implies `none`. |
| `--topology-scheme` | Required with `--spectrum-x` | `2-tier`, `3-tier` | Selects the Spectrum-X topology addressing scheme. |
| `--ip-version` | Required with `--spectrum-x` | `ipv4`, `ipv6` | Selects per-node IPv4 `/31` or IPv6 `/64` CIDRPool allocation. |
| `--topology-file` | Required with `--spectrum-x` | path | spcx-gen/reference-generator or contract-compliant NVIDIA AIR topology JSON. The format is detected from the JSON structure. |
| `--multirail` | Auto-defaulted | — | Auto-defaults to `true`. Explicit `multirail: false` in YAML and `--multirail=false` on the CLI are both preserved. |
| `--save-deployment-files` | Yes | — | Output directory for generated YAMLs |
| `--groups` | — | `dgx-b200-nvidia-h100-nvl,poweredge-xe9680-nvidia-h200` | Restrict output to the named source groups (comma-separated). Mutually exclusive with `--gpu-type`. |
| `--gpu-type` | — | `NVIDIA-H200` | Restrict output to source groups whose `gpuType` matches (case-insensitive). Mutually exclusive with `--groups`. |
| `--for` | — | preset directory name | Skip discovery: synthesize `clusterConfig` from a topology preset. Requires `--node-selector`. List options with `l8k preset list`. |
| `--node-selector` | Required with `--for` | `key=val,key2=val2` | Identifies which nodes the synthesized clusterConfig targets at apply time. |

**Note:** `--fabric` and `--deployment-type` are not required when `--spectrum-x` is used (it implies `ethernet` fabric and `sriov` deployment).

## Examples

```bash
# SR-IOV Ethernet RDMA (most common for GPU clusters)
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output

# Spectrum-X with hardware plane load balancing
l8k generate --user-config cluster-config.yaml \
  --spectrum-x RA2.2 --multiplane-mode hwplb --number-of-planes 4 \
  --topology-scheme 2-tier --ip-version ipv6 \
  --topology-file ./topology.json \
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

When `validation.gpuDirect.enabled` is true, every generated example
DaemonSet requests `validation.gpuDirect.gpuResourceType` only on its primary
DOCA container. The request exposes the highest `GPU<N>` referenced by PF
topology, the image comes from the selected release's `validation.image`, and
`networkOperator.imagePullSecrets` is copied to the Pod spec. Do not inject
these fields later at validation runtime.

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

`values.yaml` is rendered from the profile's `00-values.yaml` template. `--network-operator-release <MAJOR.MINOR>` populates the chart repository URL and image tag from the embedded catalog. For Spectrum-X profiles, the same catalog entry supplies the independently versioned xPlane repository and tag rather than reusing the generic Network Operator component coordinates. To install or upgrade the chart alongside the CRs, pass `--deploy` (and `--overwrite-existing` when the release already exists with different values).

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
- For Spectrum-X, GPU platform and NIC type determine the safe defaults, but
  B300/GB300 platform type does not distinguish SWPLB from HWPLB. l8k defaults
  to SWPLB; use an explicit HWPLB override when the site topology requires it.
  Read `references/spectrum-x-modes.md`.
- Spectrum-X renders one `NicConfigurationTemplate` per source group and
  derives `spec.nicSelector.nicType` plus `pciAddresses` from that group's
  east-west PFs. The intersection prevents same-device-ID north-south DPUs from
  matching. Do not ask users to configure `spectrumX.nicType`; missing or mixed
  east-west device IDs and missing east-west PCI addresses are generation
  errors.
- NVIDIA AIR topology support requires the documented one-based node/interface
  naming contract (`su<S>`, `h<H>`, `leaf-p<P>`, `r<R>`, `rail<R>p<P>`, and
  `pod<D>` for 3-tier). See `docs/user/spectrum-x.md` in the l8k repository.
- Spectrum-X CIDRPool allocation matches `clusterConfig.workerNodes` to
  topology host endpoint `node` values exactly and case-sensitively. A zero-match
  error usually means the wrong topology file, a case difference, or a
  short-name/FQDN difference. Partial-pool errors report the worker's available
  rail/plane coverage; check host `attributes.rail` and, for `swplb`, leaf
  `attributes.plane`.
- RA2.2 and RA2.3 v1alpha2 `SpectrumXRailPoolConfig` output intentionally
  omits the removed `spec.withBCM` field; current CRDs reject it during strict
  decoding.
- Group identifiers produced by discovery are bounded to 40 bytes with a
  deterministic hash suffix. Use the exact persisted identifier from
  `cluster-config.yaml` with `--groups`; do not reconstruct it from long
  `machineType` and `gpuType` strings.
- Use `--groups <a,b,...>` (case-sensitive identifier list) or `--gpu-type <X>` (case-insensitive) to scope a generate to a subset of source groups in heterogeneous clusters. Mutually exclusive. Empty match is a validation error. Strict-subset filters split per-source rendering: NodePolicies emit one CR per source (each with its own machine-label nodeSelector but a shared bucket-level resourceName); IPPool/example DaemonSet emit one CR per bucket with an `In` list of source machine labels.

> [!CAUTION]
> Generation does not apply anything to the cluster. Use `--deploy` or `k8s-launch-kit-deploy` to apply.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-discover](../k8s-launch-kit-discover/SKILL.md) — Produce the cluster config needed for generation
- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — Apply generated manifests
- [k8s-launch-kit-dryrun](../k8s-launch-kit-dryrun/SKILL.md) — Preview before applying
