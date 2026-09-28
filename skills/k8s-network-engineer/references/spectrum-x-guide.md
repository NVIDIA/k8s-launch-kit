# Spectrum-X Configuration Guide

Use the published [Spectrum-X guide](../../../docs/user/spectrum-x.md) for
profile inputs, topology contracts, addressing, DRA prerequisites, and
qualification boundaries. The configuration resolver enforces these pairs:

| RA | Network Operator | Profile directory | Rail resources |
| --- | --- | --- | --- |
| RA2.1 | 26.1 | `profiles/spectrum-x-ra2.1/` | SR-IOV/OVS chain and v1alpha1 rail-pool CR |
| RA2.2 | 26.4 | `profiles/spectrum-x-ra2.2/` | v1alpha2 rail-pool CR |
| RA2.3 | 26.7 | `profiles/spectrum-x/` | v1alpha2 rail-pool CR and profile ConfigMap |

Spectrum-X requires Ethernet, SR-IOV, and multirail. An invalid RA/release pair
fails validation; it does not fall back to an ordinary profile. RA2.3 requires
a full profile ConfigMap or raw profile YAML plus a ConfigMap name.

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
