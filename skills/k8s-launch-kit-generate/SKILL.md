---
name: k8s-launch-kit-generate
version: 1.2.12
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
Review the effective config and generated resources using
`docs/advanced/generation.md#review-the-bundle` before applying them.

Generation validates all rendered YAML after annotation and before replacing
the output directory. A malformed resource reports its filename/document and
leaves the previous output directory in place.

Multus and CNI plugin ownership are independent YAML controls under
`networkOperator.secondaryNetwork`. Auto uses discovery evidence, external omits
the component, and managed renders it. Automatic generation rejects ambiguous or
unavailable observations. Offline configurations retain managed rendering with
unverified ownership; deployment still performs live checks. See
[ownership rules](../../docs/reference/configuration.md#secondary-network-ownership).

## Usage

```bash
l8k generate --user-config <CONFIG> \
  --save-deployment-files <OUTPUT_DIR>
```

Configs produced by `l8k discover` already contain the resolved profile.
Profile flags remain available as generation-time overrides. Generation keeps
its source file unchanged and writes the exact effective configuration to
`<OUTPUT_DIR>/.l8k/resolved-config.yaml` for later deploy and validation.

For InfiniBand `rdma_shared` with the DOCA driver disabled, generation warns
that host IPoIB netdevs and the shared-plugin `rdma_cm`, `umad` and `uverbs`
character devices must be provisioned by the host owner. Check
[IPoIB prerequisites](../../docs/user/profiles.md#ipoib-with-inbox-drivers);
generated names and loaded-module lists do not prove host readiness.

## Profile Selection Flags

| Flag | Required | Values | Description |
|------|----------|--------|-------------|
| `--fabric` | Auto-defaulted | `ethernet`, `infiniband` | Network fabric. Auto-defaults from the cluster's unanimous `linkType` when omitted (Unit 5 fabric probe); skipped+warned when groups disagree or any has unverified linkType. |
| `--deployment-type` | Auto-defaulted | `sriov`, `rdma_shared`, `host_device` | Deployment type. Auto-defaults to `sriov`. |
| `--spectrum-x` | — | `RA2.1`, `RA2.2`, `RA2.3`, `RA2.4`, `RA3.1` | Enable Spectrum-X profile by passing the SPC-X RA version. Implies ethernet fabric, sriov deployment, and multirail. |
| `--multiplane-mode` | Auto-defaulted with `--spectrum-x` | `none`, `swplb`, `hwplb` | Auto-defaults from GPU platform plus east-west PF deviceID: H100/H200/B200/GB200 → `none`; B300/GB300 → the GA `swplb` path. Select `hwplb` explicitly for B300/GB300; VR/CX9 defaults to `hwplb` on RA2.4/RA3.1. Unknown platforms fall back to NIC family. |
| `--number-of-planes` | Auto-defaulted with `--spectrum-x` | `1`, `2`, `4`, `8` | Single-plane platforms → 1; B300/GB300 → 2; VR/CX9 HWPLB on RA2.4/RA3.1 → 8. Pass 4 explicitly for quad-plane B300. An explicit `none` also implies 1, and an explicit 1 implies `none`. |
| `--topology-scheme` | Required with `--spectrum-x` | `2-tier`, `3-tier` | Selects the Spectrum-X topology addressing scheme. |
| `--ip-version` | Required with `--spectrum-x` | `ipv4`, `ipv6` | Selects per-node IPv4 `/31` or IPv6 `/64` CIDRPool allocation. |
| `--topology-file` | Required with `--spectrum-x` | path | spcx-gen/reference-generator or contract-compliant NVIDIA AIR topology JSON. The format is detected from the JSON structure. |
| `--multirail` | Auto-defaulted | — | Auto-defaults to `true`. Explicit `multirail: false` in YAML and `--multirail=false` on the CLI are both preserved. |
| `--save-deployment-files` | Yes | — | Output directory for generated YAMLs |
| `--groups` | — | `dgx-b200-h100-nvl,pe-xe9680-h200` | Restrict output to the named source groups (comma-separated). Mutually exclusive with `--gpu-type`. |
| `--gpu-type` | — | `NVIDIA-H200` | Restrict output to source groups whose `gpuType` matches (case-insensitive). Mutually exclusive with `--groups`. |
| `--for` | — | preset directory name | Skip discovery: synthesize `clusterConfig` from a topology preset. Requires `--node-selector`. List options with `l8k preset list`. |
| `--node-selector` | Required with `--for` | `key=val,key2=val2` | Identifies which nodes the synthesized clusterConfig targets at apply time. |
| `--skip-network-operator-helm` | — | boolean | Omit `values.yaml`; with `--deploy`, also skip Network Operator chart installation and Helm preflight checks. |

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
  --output json

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

## Interface Naming And Group Selection

When `nicConfigurationOperator.deployNicInterfaceNameTemplate` and multirail
are enabled, standard profiles retain naming for multiple selected source groups.
For a single-group inventory or one source selected with `--groups` / `--gpu-type`,
they skip naming and use PCI selectors or saved interface names. RDMA-shared
retains naming if any selected east-west PF lacks an interface name; Spectrum-X
retains its required templates. Explicit `deployNicInterfaceNameTemplate: false`
disables standard-profile naming. The fallback preserves saved configuration and
leaves NIC configuration templates enabled independently.

The emitted standard naming layout must assign each physical NIC
(`domain:bus:device`) to only one outer `railPciAddresses` entry. Splitting its
PCI functions across entries is rejected before replacing output, because NCO
has only one rail index per physical NIC. This check applies only to emitted
naming templates; skipped naming and excluded groups do not trigger it.

## Netplan Interface-Name Conflict

Before emitting a `NicInterfaceNameTemplate`, generation checks
`clusterConfig[].netplanManaged` for the selected source groups. If an affected
group would receive the CR, `l8k generate` returns a validation error because
the host netplan `set-name` rule can conflict with NCO's udev naming rule for
the same NIC. Clean up the affected `set-name` stanzas in the host netplan
configuration, re-run `l8k discover`, and then retry `l8k generate`.

Discovery also sets `clusterConfig[].netplanManagementUnknown` when no match was
found but it could not inspect every worker. Generation fails closed for those
groups until the discovery warning is resolved and discovery is re-run.

The check follows the actual render plan: a group excluded with `--groups` or
`--gpu-type` does not block generation, and neither does a group for which no
`NicInterfaceNameTemplate` is rendered. Do not clear either netplan field by
hand; re-run discovery so the config reflects the host state.

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
read `../k8s-network-engineer/references/profile-decision-tree.md`.

## Output

Generated YAMLs are written to the output directory under `network-operator/`. Each profile normally emits a `values.yaml` (Helm values for the `nvidia/network-operator` chart) alongside the CR manifests:

When `validation.gpuDirect.enabled` is true, every generated example
DaemonSet requests `validation.gpuDirect.gpuResourceType` only on its primary
DOCA container. The request exposes the highest `GPU<N>` referenced by PF
topology, the image comes from the selected release's `validation.image`, and
`networkOperator.imagePullSecrets` is copied to the Pod spec. The primary
container also sets `LD_LIBRARY_PATH` to prefer the host-injected NVIDIA driver
libraries and exclude the CUDA compatibility-library directory. The
environment override is omitted when GPUDirect is disabled. Do not inject
these fields later at validation runtime.

```
output/
├── .l8k/
│   └── resolved-config.yaml              # versioned effective-config metadata
└── network-operator/
    ├── values.yaml                       # Phase 0 helm-install input for `l8k deploy`
    ├── 10-nicclusterpolicy.yaml
    ├── 11-nicnodepolicy-<group>.yaml
    ├── 20-ippool-<group>.yaml
    ├── 40-sriovnetworknodepolicy-<group>.yaml
    └── 50-sriovnetwork-<group>.yaml
```

`values.yaml` is rendered from the profile's `00-values.yaml` template. `--network-operator-release <MAJOR.MINOR>` populates the chart repository URL and image tag from the embedded catalog. For Spectrum-X profiles, the same catalog entry supplies the independently versioned xPlane repository and tag rather than reusing the generic Network Operator component coordinates. To install or upgrade the chart alongside the CRs, pass `--deploy` (and `--overwrite-existing` when the release already exists with different values).

For non-Spectrum-X profiles, `docaDriver.env` entries are forwarded into the
rendered `ofedDriver.env` list in either NicClusterPolicy (legacy and
host-device paths) or NicNodePolicy. This is an advanced escape hatch: custom
values override all generated driver environment values with the same name,
and duplicate custom names use the last value.

Set `networkOperator.skipHelmChart: true` or pass
`--skip-network-operator-helm` when another system owns the Helm release. The
output directory is cleaned and `values.yaml` is omitted; all Network Operator
custom-resource manifests are still rendered.

For Network Operator 26.1 and newer, the rendered values enable Maintenance
Operator requestor mode. Profiles with DOCA/OFED enable
`operator.maintenanceOperator.useRequestor`. Profiles that deploy the SR-IOV
Operator enable both the Network Operator drain requestor and
`sriov-network-operator.operator.externalDrainer`; the two SR-IOV switches are
a coordinated handoff and must not be separated. The generated
`MaintenanceOperatorConfig` gets the global limits from the config's
`maintenance` section.

Numeric `maintenance.maxUnavailable` values render as unquoted integers across
profiles; percentage values remain strings. Helm values retain the special
string `"0"` because the upstream chart would omit numeric zero. The chart renders
that value as integer `0` in the final `MaintenanceOperatorConfig`; OpenShift
CRs and legacy SR-IOV pool configs render integer `0` directly.

Generated values do not add
`network.nvidia.com/operator.nic-configuration.wait` to the SR-IOV config
daemon node selector. The daemon keeps the Network Operator chart's default
scheduling selectors; drain sequencing remains controlled by the
release-appropriate SR-IOV drain path described below.

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

## SR-IOV admission helpers

SR-IOV and Spectrum-X chart values disable both admission helpers through
`sriov-network-operator.operator.admissionControllers.enabled: false`.
Do not put `enableInjector` or `enableOperatorWebhook` under the chart
`sriovOperatorConfig` values: these are CR fields, not chart overrides.

## Common Mistakes

- **There is no `--profile` flag.** Profiles are selected via `--fabric` + `--deployment-type` (or `--spectrum-x`). Do NOT invent flags.
- **The multiplane flag is `--multiplane-mode`**, not `--spcx-multiplane` or `--multiplane`.

Generated SR-IOV node policies and NV-IPAM IPPools carry
`nvidia.kubernetes-launch-kit.source-groups` as a JSON array of selected source
IDs. IDs must be unique. The existing unnamed sole source keeps its empty ID and
uses explicit `[""]`; deploy/validate still require exactly one unnamed configured
source and check its workers. Preserve this metadata and the resolved sidecar.
Generation does not prove live targeting: deploy requires exact selector-to-worker
membership when inventory is populated. Preset-only empty worker lists retain a
visible selector-only, target-scope-unverified contract.

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
- Group identifiers produced by discovery omit complete `NVIDIA` segments and
  shorten common machine segments (`ThinkSystem` → `ts`, `PowerEdge` → `pe`).
  They are bounded to 30 bytes with balanced machine/GPU prefixes and a
  6-character deterministic hash suffix. The machine label uses the same value;
  the GPU label still retains its discovered value such as `NVIDIA-H200`. Use the exact persisted identifier from
  `cluster-config.yaml` with `--groups`; do not reconstruct it from long
  `machineType` and `gpuType` strings.
- Use `--groups <a,b,...>` (case-sensitive identifier list) or `--gpu-type <X>` (case-insensitive) to scope a generate to a subset of source groups in heterogeneous clusters. Mutually exclusive. Empty match is a validation error. Strict-subset filters split per-source rendering: NodePolicies emit one CR per source (each with its own machine-label nodeSelector but a shared bucket-level resourceName); IPPool/example DaemonSet emit one CR per bucket with an `In` list of source machine labels.
- Generated IPPools use `nvIpam.perNodeBlockSize` (default `10`). Generation warns when it is lower than `sriov.numVfs`, since the per-node block may not have an address for every VF.

> [!CAUTION]
> Generation does not apply anything to the cluster. Use `--deploy` or `k8s-launch-kit-deploy` to apply.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and output modes
- [k8s-launch-kit-discover](../k8s-launch-kit-discover/SKILL.md) — Produce the cluster config needed for generation
- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — Apply generated manifests
- [k8s-launch-kit-dryrun](../k8s-launch-kit-dryrun/SKILL.md) — Preview before applying

## OpenShift

`--flavor ocp` selects separate OpenShift profiles and generates no Helm values. Hardware policies use the same bucket and source-group render scopes as Kubernetes; optional NIC interface naming templates render per source group. Compatible groups share a network per GPU type and rail count. Custom workloads render once per shared network and workload namespace, referencing that namespace's NAD and the `openshift.io` SR-IOV resource. Verify each policy selector and PCI address against every targeted worker before deployment.

With `profile.ignoreARP: true`, generated `tuning` meta-plugins use only
`net.ipv4.conf.IFNAME.*` sysctls; the Kubernetes flavor also emits
`net.ipv4.conf.all.*`. Confirm that OpenShift's
`openshift-multus/cni-sysctl-allowlist` admits the three IFNAME entries before
workload validation. With source-based routing, `sbr` follows `tuning`.

See `docs/user/openshift.md` for the required external operators and full
generate-to-acceptance procedure.

## Custom Workloads And Validation

`--workload-manifest` replaces example DaemonSet templates with operational
`90-workload-*.yaml` files applied by deploy. It does not make the custom Pod,
Deployment, or DaemonSet a connectivity fixture. Supply separate `*example*.yaml`
DaemonSets with the required validation containers after each render, or run
static-only validation with `--connectivity=false` and report that limitation.
Kubernetes custom workloads use the first network namespace; OpenShift renders
per shared network bucket and namespace. See `docs/advanced/generation.md`.
For a complete fixture retention and workload example, use
`docs/user/workloads.md`.

Subset rendering does not isolate deployment deletion scope. Before overwrite,
review all stray CRs, including other cohorts and manual resources. DRA setup
requires the APIs/drivers/DeviceClasses documented in `docs/user/spectrum-x.md`.

## Standard NIC tuning

`nicConfigurationOperator.deployNicConfigurationTemplate` defaults to true.
Set it to false or pass `--deploy-nic-configuration-template=false` to opt out;
explicit YAML and CLI false values are preserved.
Root, generate and discover accept `--deploy-nic-configuration-template[=false]`.
It enables NCO independently of interface naming and renders per-source,
east-west PCI-scoped templates for standard SR-IOV, RDMA-shared and host-device
profiles on k8s/OCP. VF count comes from `sriov.numVfs`; link type comes from
`profile.fabric`. Ethernet gets DSCP trust and priority-3 PFC; InfiniBand omits
RoCE tuning. Spectrum-X keeps its dedicated required templates independently
of this standard-profile toggle. Only SR-IOV disables the Mellanox
plugin (Helm values on k8s, additive `disablePlugins` merge on OCP). Kubernetes
external Helm owners must configure that plugin themselves.

Deploy and validate consume the generated template and require all matched
NicDevices to report current `ConfigUpdateInProgress=False` with reason
`UpdateSuccessful`; stale/partial status waits and failures surface. Opting out
does not restore applied NIC settings. On k8s, omitted templates (also for
unselected groups) block deploy preflight unless `--overwrite-existing` is used,
which deletes them. OCP preserves existing templates and disabled plugins.
Generation rejects inventory with selected and excluded ports on the same PCI
device. Review whole-NIC scope, including ports absent from inventory, and qualify
runtime RoCE support for the installed NCO/DOCA/RHCOS versions before live use. See `docs/reference/configuration.md#nic-configuration-and-naming`.

## RA2.4 and RA3.1 integration

RA2.3 is limited to Network Operator 26.7; RA2.4 and RA3.1 default to 26.10 and
accept newer catalogued releases. RA2.4 and RA3.1 need a full doSPCX ConfigMap, preserving
binaryData and annotations and rendering in the resolved operator namespace.
With RA2.4/RA3.1, VR with ConnectX-9 defaults to `hwplb` and eight planes;
explicit YAML and CLI values take precedence. Explicit `swplb` with an omitted
plane count retains the CX9 four-plane fallback; this does not qualify that recipe.
VR discovery requires eight east-west ASIC NICs in four reciprocal NetworkBay
pairs. NetworkBays are numbered by ascending first-ASIC (`asic: 1`) PCI address;
rails cross first/second ASICs between NetworkBays 1/2 and 3/4.
Generation rejects VR inventories unless they contain four dense rails with two
physical NICs each. Re-run discovery with NetworkBay status for old configs that
assigned one rail per NIC; generation does not guess the pairing from PCI order. Missing or inconsistent status fails discovery. See `docs/user/discovery.md` for the rail mapping.
VR generation supports IPv4 and IPv6 HWPLB with eight planes on four dual-NIC rails.
It emits four per-rail naming templates (`pfsPerNic: 4`), eight per-NIC
`railRnicN` topology entries and CIDRPools, and eight workload attachments.
Each NIC endpoint has four PFs, one schedulable VF and its own IPv4 /31 or IPv6 /64. DRA uses
one claim per logical rail with one GPU and two VF requests. NIC input order
is preserved; it must agree with the physical switch topology. RDMA names
are `roce_rR_n0`/`roce_rR_n1`; custom RDMA prefixes require `%nic_id%` and no
plane placeholder. Compatible NCO must support disjoint PCI naming templates.
Reference topology JSON must include all eight planes per rail and explicit
zero-based host `attributes.host_index`; AIR uses its parsed host ordinal.
VR IPv4 defaults to site-zero `240.0.0.0/8`, with NIC(1), rail(2), POD(5),
SU(7), host(8), peer(1) bits after the first octet. Eight-plane POD is always
zero and host index is at most 251. Pools are per NIC/rail /11, allocations
are /31 with even host and odd gateway, and routes cover /11 and /8.
VR IPv6 uses `fd02:000S:NRPP:UUHH::peer/64`: site nibble `S`, NIC nibble
`N`, rail nibble `R`, POD byte `PP`, SU byte `UU`, and host byte `HH`.
`profile.spectrumX.ipv6SiteID` selects site 0..15 (default 0; YAML-only).
The two NICs use distinct per-NIC/rail /40 pools; each host gets a static /64,
with host `::1`, gateway `::2` (excluded from allocation), and route `fd02::/28`.
POD remains zero, SU is 0..127, and host index is 0..251. `hostFirstOctet`
affects only IPv4; nonzero `ipv6SiteID` is rejected outside VR IPv6.
Non-HWPLB VR, incomplete topology, and extra software-plane offsets
are rejected. Generation does not qualify runtime support or configure switches.
26.10 beta.2 lacks the packaged NCO platformType CRD: verify a compatible NCO
implementation and matching CRDs before deployment. Deploy checks the served
NCT schema after policy bootstrap and blocks additional resource apply when
platformType support is absent or cannot be inspected. External Helm, overwrite,
and dry-run do not bypass this check; dry-run needs APIs installed already.

Derived clusterConfig[].spectrumX.platformType uses the longest case-insensitive
substring from the internal doSPCX platform list in gpuType. A machineType
containing `VR` (case-insensitive) takes precedence and selects `vr`, even with
an empty gpuType. No preset or mapping
setting is needed. Discovery saves empty values and warns; generation rejects
unresolved selected RA2.4/RA3.1 groups. RTX/CX8 defaults to none/one plane. Mapping does
not itself qualify hardware. Optional spectrumX.ovsConfig is a string map;
clusterConfig[].spectrumX.swPlaneByRail assigns existing HWPLB rails, defaults to
zero, and must agree across merged groups. Refresh preserves explicit plane
assignments only for unchanged source/rail hardware and verified unchanged worker
membership (ordering does not matter). Changed or unknown membership drops prior
assignments with a review/reapply warning. Every selected RA2.4/RA3.1 source needs complete
dense rail IDs, uniform NICs per rail, and a divisible planes/NIC ratio; merged
sources must have the same effective layout. Merged GPU selectors must not include
excluded same-GPU sources in other buckets; select one source in that case.
One physical NIC cannot span rails
(the current ThinkSystem-SR650-V4-RTX-PRO-6000 preset has this unsupported layout). Multiple hardware partitions
inside one logical rail are outside this layout. Feature gates are not added.

RA2.4/RA3.1 generation permits a complete compatible GPU/rail-count cohort or a
single selected source. Multiple rail-pool parents are rejected because the
operator-created child policies and OVSNetworks would reuse rail names.
