# Spectrum-X Configuration Guide

Use the published [Spectrum-X guide](../../../docs/user/spectrum-x.md) for
profile inputs, topology contracts, addressing, DRA prerequisites, and
qualification boundaries. The configuration resolver enforces these pairs:

| RA | Network Operator | Profile directory | Rail resources |
| --- | --- | --- | --- |
| RA2.1 | 26.1 | `profiles/spectrum-x-ra2.1/` | SR-IOV/OVS chain and v1alpha1 rail-pool CR |
| RA2.2 | 26.4 | `profiles/spectrum-x-ra2.2/` | v1alpha2 rail-pool CR |
| RA2.3 | 26.7 | `profiles/spectrum-x/` | v1alpha2 rail-pool CR and profile ConfigMap |
| RA2.4 | 26.10 and newer catalogued releases | `profiles/spectrum-x-ra2.4/` | v1alpha2 rail-pool CR, doSPCX bundle and per-source platformType |

Spectrum-X requires Ethernet, SR-IOV, and multirail. An invalid RA/release pair
fails validation; it does not fall back to an ordinary profile. RA2.3 requires
a full profile ConfigMap or raw profile YAML plus a ConfigMap name. RA2.4
requires a full doSPCX ConfigMap and the matching NCO implementation and CRDs.

## Mode Selection

| Mode | Planes | Resources |
| --- | --- | --- |
| `none` | 1 | Per rail |
| `swplb` | 2 or 4 | Per rail and plane |
| `hwplb` | 2 or 4 | Per rail, grouping planes |

Hardware defaults combine GPU platform and east-west NIC device ID.
H100/H200/B200/GB200 default to `none` with one plane; B300/GB300 default to
`swplb` with two planes. Select `hwplb` or quad-plane B300 explicitly from the
site fabric design. Cluster size alone does not determine the correct mode.

## Generation

Start with a discovered configuration, validated RA2.3 profile, and topology
file for the selected workers. Change mode/planes only to match the fabric:

```bash
l8k generate --user-config cluster-config.yaml \
  --spectrum-x RA2.3 --network-operator-release 26.7 \
  --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
  --multiplane-mode swplb --number-of-planes 2 \
  --topology-file ./topology.json --topology-scheme 2-tier --ip-version ipv4 \
  --save-deployment-files ./deployment --output json
```

Generation leaves the input unchanged and records resolved intent in
`deployment/.l8k/resolved-config.yaml`. Retain the complete output for later
deploy and validate, omitting an explicit config override to reuse the sidecar.

## Review The Output

- NIC configuration templates are per source group and constrain both NIC type
  and east-west PCI addresses; verify that north-south devices are excluded.
- Interface naming templates use `25-nicinterfacenametemplate.yaml` before the
  `30-nicconfigurationtemplate.yaml` resource in the generated ordering.
- RA2.3 also emits `28-spectrumxprofile-configmap.yaml`.
- `60-cidrpool.yaml` uses topology-derived static allocations. IPv4 allocates
  per-node `/31`; IPv6 uses `/64`, host `::1`, leaf/gateway `::2`, and `/40` pools.
- RA2.2/RA2.3 topology names are `rail0` or `rail0p0`, consumed as NAD names and
  `nvidia.com/<name>` device resources. They omit removed `spec.withBCM`.
- Optional `85-resourceclaimtemplate.yaml` requires the DRA API, GPU DRA driver,
  DeviceClasses, and SR-IOV DRA support described in the published guide.
- `90-example-daemonset.yaml` is a temporary connectivity workload; deployment
  skips example files. Application replacements render as `90-workload-*` and
  are operational resources, with separate test fixtures required.

Inspect full preflight conflicts before overwrite; scope is not restricted to
l8k-owned annotations or the selected groups. Qualification requires successful
allocation and the intended connectivity coverage on the target hardware.
Spectrum-X has no qualified OpenShift profile in this build.

## RA2.4 integration

RA2.3 is limited to Network Operator 26.7; RA2.4 defaults to 26.10 and
accepts newer catalogued releases. RA2.4 needs a full doSPCX ConfigMap, preserving
binaryData and annotations and rendering in the resolved operator namespace.
26.10 beta.2 lacks the packaged NCO platformType CRD: verify a compatible NCO
implementation and matching CRDs before deployment. Deploy checks the served
NCT schema after policy bootstrap and blocks additional resource apply when
platformType support is absent or cannot be inspected. External Helm, overwrite,
and dry-run do not bypass this check; dry-run needs APIs installed already.

Derived clusterConfig[].spectrumX.platformType uses the longest case-insensitive
substring from the internal doSPCX platform list in gpuType. No preset or mapping
setting is needed. Discovery saves empty values and warns; generation rejects
unresolved selected RA2.4 groups. RTX/CX8 defaults to none/one plane. Mapping does
not itself qualify hardware. Optional spectrumX.ovsConfig is a string map;
clusterConfig[].spectrumX.swPlaneByRail assigns existing HWPLB rails, defaults to
zero, and must agree across merged groups. Refresh preserves explicit plane
assignments only for unchanged source/rail hardware and verified unchanged worker
membership (ordering does not matter). Changed or unknown membership drops prior
assignments with a review/reapply warning. Every selected RA2.4 source needs complete
dense rail IDs, uniform NICs per rail, and a divisible planes/NIC ratio; merged
sources must have the same effective layout. Merged GPU selectors must not include
excluded same-GPU sources in other buckets; select one source in that case.
One physical NIC cannot span rails
(the current ThinkSystem-SR650-V4-RTX-PRO-6000 preset has this unsupported layout). Multiple hardware partitions
inside one logical rail are outside this layout. Feature gates are not added.
