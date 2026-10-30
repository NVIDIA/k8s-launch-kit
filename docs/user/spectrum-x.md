<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Spectrum-X

Spectrum-X profiles render multi-rail AI interconnect manifests. The selected RA version and Network Operator release determine the manifest shape.

## Required inputs and owners

Before generating, the fabric owner must confirm a configured Spectrum-X switch fabric, rails/planes, non-overlapping address allocations, and the selected workers and east-west PFs. The hardware/operator owner must confirm the Network Operator release and RA pairing in the [version matrix](#version-matrix), driver and maintenance policy, and a **validated Spectrum-X profile** for the exact hardware and RA. RA2.3 needs that profile as a full ConfigMap or raw `data.profile` YAML; a schema illustration is not a site-approved profile. Where topology-derived CIDRPools are used, obtain a matching [topology JSON export](#topology-driven-cidrpools) from the fabric owner before generation. DRA requires the separate [allocation driver prerequisites](#dra-workload-allocation).

## Deploy RA2.3

1. [Discover](discovery.md) and review the intended hardware in `./cluster-config.yaml`, or use a complete, reviewed source config for that cluster. Confirm east-west PF identity, rails, worker groups, networking settings, and maintenance budget. Obtain a validated **full ConfigMap** at `./site-ra23-profile.yaml` from the hardware owner and a matching `./topology.json` for topology-derived pools. A raw `data.profile` file needs the additional `--spectrum-x-configmap-name` option described [below](#ra23-profile-configmap).
2. Generate into a new directory. This example uses topology-derived pools; the profile and topology files must already exist:

   ```bash
   l8k generate --user-config ./cluster-config.yaml \
     --network-operator-release 26.7 --spectrum-x RA2.3 \
     --spectrum-x-config ./site-ra23-profile.yaml \
     --topology-scheme 2-tier --topology-file ./topology.json \
     --save-deployment-files ./deployment-ra23
   ```

3. Review `deployment-ra23/.l8k/resolved-config.yaml`, the generated ConfigMap, `NicConfigurationTemplate` NIC type **and** PCI address selectors, rail/plane resources, per-host IP assignments, workload namespaces, and Helm values. Compare selected workers with topology host endpoints exactly, including case and FQDN form. Resolve unmatched workers, overlap, missing allocations, and any stray-resource preflight finding before apply.
4. Preview with `l8k deploy --deployment-files ./deployment-ra23 --dry-run`. After site approval, run `l8k deploy --deployment-files ./deployment-ra23`. Treat dry-run as admission/preflight evidence; it does not prove reconciliation or traffic.
5. Run `l8k validate --deployment-files ./deployment-ra23 --wait 10m`. Review the [acceptance outcomes](validation.md#acceptance-outcomes), including intended workers, rails, completed gating families, and exclusions. Retain the full bundle and report.

For a profile/schema failure, compare the supplied ConfigMap with the [RA2.3 contract](#ra23-profile-configmap). For host matching and allocation failures, use [topology diagnostics](#topology-driven-cidrpools). For controller or traffic failures, use [Troubleshooting](troubleshooting.md). The existence of generated YAML or a successful server dry-run does not establish hardware qualification.

## Version Matrix

| RA version | Network Operator release | Profile path | Notes |
| --- | --- | --- | --- |
| RA2.1 | `26.1` | `spectrum-x-ra2.1` | Uses the RA2.1 SR-IOV operator chain and v1alpha1 glue CRs. |
| RA2.2 | `26.4` | `spectrum-x-ra2.2` | Uses v1alpha2 `SpectrumXRailPoolConfig`. |
| RA2.3 | `26.7` only | `spectrum-x` | Uses v1alpha2 `SpectrumXRailPoolConfig` and a legacy profile ConfigMap. |
| RA2.4 | `26.10` and newer catalogued releases | `spectrum-x-ra2.4` | Uses a doSPCX data-bundle ConfigMap and per-group platformType. |
| RA3.1 | `26.10` and newer catalogued releases | `spectrum-x-ra3.1` | Uses the same doSPCX bundle contract; permits eight-plane VR HWPLB generation. |

RA2.2 and RA2.3 output does not include the removed `spec.withBCM` field.
Adding it causes the v1alpha2 CRD to reject the manifest during strict
decoding.

Select the release line explicitly when generating, as in the [RA2.3 procedure](#deploy-ra23).

## Multiplane Modes

| Mode | Use |
| --- | --- |
| `none` | No plane separation. Used for ConnectX-7 and BlueField-3 SuperNIC topologies. |
| `swplb` | Software plane load balancing. Renders per-rail, per-plane resources. |
| `hwplb` | Hardware plane load balancing for larger topologies. |

### Platform-derived defaults

When `multiplaneMode` or `numberOfPlanes` is absent, l8k combines the
discovered GPU platform with the east-west NIC device ID:

| Platform | Default mode | Default planes | Notes |
| --- | --- | --- | --- |
| H100, H200, B200, GB200 | `none` | 1 | Single-plane architecture. |
| B300 | `swplb` | 2 | Conservative dual-plane default; pass 4 explicitly for a quad-plane topology. |
| GB300 | `swplb` | 2 | Dual-plane architecture. |
| VR with ConnectX-9 | `hwplb` | 8 | RA2.4/RA3.1; explicit `swplb` retains the CX9 four-plane fallback when its plane count is omitted. |

The platform is read from `clusterConfig[].gpuType`, with `machineType` as a
fallback. On RA2.4/RA3.1, a `machineType` containing `VR` takes precedence,
including without a GPU product. `--for` presets participate in the same resolution before manifests
are rendered.

The generated `NicConfigurationTemplate` derives both
`spec.nicSelector.nicType` and `spec.nicSelector.pciAddresses` from the target
source group's east-west PFs. There is no separate `spectrumX.nicType` setting.
Each source hardware group gets its own template, and the operator matches the
intersection of NIC type and PCI address. A north-south DPU is therefore not
selected even when it reports the same device ID as an east-west SuperNIC.
Generation requires every selected east-west PF to have the same non-empty
`deviceID` and a non-empty `pciAddress`.

Template names remain source-group based in both full and strict-subset
renders, so changing `--groups` does not rename retained templates. When
upgrading from a Launch Kit version that generated one merged, type-only
template, deploy preflight reports that legacy template as a stray resource.
Review the report, then use `--overwrite-existing` to delete the broad template
before applying the new PCI-scoped templates.

B300 and GB300 support both `swplb` and `hwplb`, so platform type does not
identify which load-balancing mechanism the fabric uses. l8k defaults to the
documented GA `swplb` path. Select `hwplb` explicitly when the site topology
requires hardware plane load balancing. Explicit config values and CLI flags
always override these defaults.

```bash
l8k discover \
  --spectrum-x RA2.3 \
  --multiplane-mode hwplb \
  --number-of-planes 4
```

## RA2.3 Profile ConfigMap

RA2.3 requires the Spectrum-X profile data as either a full ConfigMap YAML or raw `data.profile` YAML.

The full ConfigMap must use the NIC Configuration Operator discovery label and store the profile as a YAML string:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: site-ra23-profile
  namespace: nvidia-network-operator
  labels:
    network.nvidia.com/operator.nic-configuration.spectrum-x-profile: ""
data:
  profile: |
    useSoftwareCCAlgorithm: true
    docaCCVersion: "<validated-version>"
    mlxConfig:
      none:
        "1023":
          postBreakout:
            EXAMPLE_NVCONFIG_PARAMETER: "example-value"
    runtimeConfig:
      roce:
        - name: <parameter-name>
          value: "<validated-value>"
          valueType: string
          dmsPath: "<validated-dms-path>"
```

Replace the placeholders with a validated profile for the target hardware and RA release. The NIC Configuration Operator repository provides a complete [example Spectrum-X profile ConfigMap](https://raw.githubusercontent.com/Mellanox/nic-configuration-operator/refs/heads/main/docs/examples/spectrum-x/example-spectrum-x-profile-configmap.yaml) covering `mlxConfig`, RoCE, adaptive routing, congestion control, and inter-packet gap sections.

Generate from the full ConfigMap:

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3 \
  --spectrum-x-config ./spectrum-x-profile-configmap.yaml
```

Raw profile data:

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3 \
  --spectrum-x-config ./profile.yaml \
  --spectrum-x-configmap-name site-ra23-profile
```

The generated ConfigMap uses the label expected by the NIC Configuration Operator and stores the profile under `data.profile`.

## RA2.4 doSPCX profile

RA2.4 and RA3.1 default to Network Operator 26.10 and accept newer catalogued releases.
RA2.3 is restricted to 26.7. Supply a validated full doSPCX ConfigMap with
`data.format: dospcx-data.tar.gz/v1` and base64
`binaryData.dospcx-data.tar.gz` through `--spectrum-x-config` or
`profile.spectrumX.profile`. Raw legacy `data.profile` is not accepted for RA2.4 or RA3.1.
The generated singleton retains bundle bytes and source annotations; its
namespace always matches the resolved Network Operator namespace, including
`--network-operator-namespace` overrides. The NIC template receives
`version: RA2.4` or `version: RA3.1` as selected, independently of the ConfigMap name.

**Release prerequisite:** Network Operator v26.10.0-beta.2 includes the rail-pool
`swPlane` and `ovsConfig` fields, but its packaged NCO CRD lacks `platformType`.
RA2.4/RA3.1 deployment requires a 26.10-or-newer operator build that includes the
doSPCX NCO implementation and matching CRDs. Generation alone does not establish
that the selected published chart satisfies this prerequisite. Deploy reads the
live NCO CRD after NicClusterPolicy/NicNodePolicy bootstrap and requires the NIC
template's served API version to expose `platformType` as a string. An absent
field or failed inspection stops deployment before the doSPCX ConfigMap and NIC
templates are applied. This also applies to standalone deploy, externally managed
Helm, overwrite, and dry-run. Dry-run needs the compatible APIs installed already.
Helm, ordinary preflight remediation and policy bootstrap may already have run
when this check fails; deployment does not roll those operations back.

Platform is derived into `clusterConfig[].spectrumX.platformType` from the raw
GPU product label, except that a `machineType` containing `VR` (case-insensitive)
takes precedence and selects `vr` even when `gpuType` is empty. Launch Kit matches `h100`, `h200`, `b200`, `gb200`, `b300`,
`gb300`, `vr`, and `rtx` as case-insensitive substrings; the longest match wins
(`GB300` selects `gb300`). Equal-length conflicting matches and no matches
produce an empty value. No preset or custom mapping table is required. Discovery
saves empty values and warns; RA2.4/RA3.1 generation rejects unresolved selected groups
before replacing output. Saved platform values are recomputed, not user overrides.
An RTX product selects `rtx`; with ConnectX-8 it defaults to `none` and one plane.
Mapping a family does not qualify every GPU variant or NIC layout for that recipe.
The current `ThinkSystem-SR650-V4-RTX-PRO-6000` preset splits functions of one
physical NIC across rails. RA2.4 rejects that layout because NCO assigns one rail
per physical NIC; it requires a representable layout before generation can proceed.

### VR dual-NIC rails

[VR discovery](discovery.md#vr-networkbay-rails) derives four logical rails
from eight east-west NICs. Each rail has two four-PF NICs; their first-seen
inventory order selects NIC 0 and NIC 1 and must match the switch topology.
Generation rejects old inventories with one rail per NIC instead of guessing
physical pairing from PCI addresses. Re-run NetworkBay discovery for those inputs.

Discovery saves the effective RA naming in
`clusterConfig[].spectrumX.interfaceNames` (`netdevPrefix` and `rdmaPrefix`).
These are derived values: resolution refreshes them after mode or naming
changes, including for older configs without this block. Change the existing
`spectrumX.singlePlane`, `hwplb`, or `swplb` prefix settings for custom naming.
PF inventory names remain observed values. Naming templates and PF selectors
consume the same resolved per-group prefixes; standard profiles are unaffected.
VR HWPLB uses `eth_r%rail_id%_p%plane_id%` and
`roce_r%rail_id%_n%nic_id%`; traditional HWPLB retains `roce_r%rail_id%`.

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

`SpectrumXRailPoolConfig.spec.railTopology` contains **eight NIC groups**, named
`rail0nic0`, `rail0nic1`, through `rail3nic1`. These are four logical rails,
not eight independent rails. NIC 0 selects planes 0–3 with `swPlane: 0`; NIC 1
selects planes 4–7 with `swPlane: 1`. This matches XPlane's per-NIC group and
single-VF model. The corresponding pools are `rail-0-nic-0` through
`rail-3-nic-1`. Applying only four eight-PF groups exposes only one workload
endpoint per rail and loses the second NIC's independently addressed endpoint.

Example reference-generator link (repeat for all physical PF links):

```json
[
  {"node":"worker-a","interface":"eth_r0_p4",
   "attributes":{"role":"host","rail":0,"pod":0,"su":0,"host_index":1}},
  {"node":"leaf-a","interface":"swp3s0",
   "attributes":{"role":"leaf","plane":4}}
]
```

This endpoint is rail 0, NIC 1: host `240.128.0.2/31`, gateway `240.128.0.3`.
The site is `hostFirstOctet - 240` (0–15); `hostFirstOctet` defaults to 240 for
VR and must be 240–255. Explicit host indices prevent a topology list reorder
from changing assigned addresses. Missing NIC planes, reused switch ports,
conflicting coordinates, or two workers sharing an address coordinate are errors.
The topology must reflect actual cabling; generation cannot verify live LLDP.

Existing deployments need an explicit migration plan: the new per-NIC policies
and networks replace old per-rail endpoints. Retain tested operator images and
custom profile data, preserve active naming identities where specs match, and
review resource pruning before applying. Correct host pools alone do not update
switch addresses, routing filters, Class E support, or workload attachments.

Optional configuration:

```yaml
spectrumX:
  ovsConfig:
    max-idle: "0"
clusterConfig:
  - identifier: machine-a
    gpuType: NVIDIA-GB300
    spectrumX:
      platformType: gb300 # derived by discovery/resolution
      swPlaneByRail:
        0: 0
        1: 1
```

`ovsConfig` is empty by default and supplies strings to the operator's
`other_config` merge; matching keys override its defaults. `swPlaneByRail`
assigns software-plane offsets to existing HWPLB rails, defaulting to zero.
Use actual dense zero-based east-west rail IDs. Nonzero assignments require
HWPLB; unknown rails and negative values are rejected. Groups merged into one
rail pool must agree on effective assignments and rail layouts. Every selected
RA2.4/RA3.1 group requires complete dense rail IDs and a uniform master NIC count per
rail that divides `numberOfPlanes`. Merged groups must have the same rail count
and NICs per rail; machine-specific PCI addresses may differ. Functions of one
physical NIC must remain in the same rail. These checks also
apply when `swPlaneByRail` is omitted.

Discovery refresh preserves assignments only when source identity, GPU product,
rail hardware and verified worker membership remain unchanged. Worker ordering
does not matter. Changed or unknown membership drops the previous assignments
and warns; review the current cohort and reapply the intended values before
generation. Omitted assignments otherwise resolve to zero.
The operator computes `plane_id = len(pfNames) * swPlane + PF index`.
Multiple hardware partitions inside one logical rail are not supported by this
configuration. Both optional settings require RA2.4 or RA3.1. Existing DRA configuration
continues through `profile.spectrumX.useDRA`.

RA2.4/RA3.1 generation currently permits one rail-pool parent: a complete compatible
GPU/rail-count cohort, or one source selected with `--groups`. A strict subset
containing multiple sources or multiple buckets is rejected before replacing
output. A merged cohort is also rejected if its GPU selector would include an
excluded source with the same GPU product in another bucket; select one source
with `--groups` in that case. The operator names child policies and OVSNetworks directly from rail
names, so multiple parents would overwrite those children. Full heterogeneous
rail-pool support requires coordinated resource naming changes.

## Topology-Driven CIDRPools

Spectrum-X CIDRPools can be generated from either spcx-gen/reference-generator topology JSON or a contract-compliant NVIDIA AIR topology export. This replaces placeholder pool entries with per-host static allocations derived from the resolved topology. l8k detects the format by JSON structure:

- spcx-gen/reference-generator format has top-level `nodes` and `links` arrays.
- NVIDIA AIR format has a `content` object containing a `nodes` map and a `links` array.

The outer AIR `format` value is not used for detection.

The file contains a `nodes` inventory and two-endpoint entries under `links`. This minimal 2-tier example connects one Kubernetes worker to one leaf:

```json
{
  "nodes": [
    {
      "name": "compute-a",
      "role": "host",
      "type": "default"
    },
    {
      "name": "leaf-p0-r0",
      "role": "leaf",
      "type": "cumulus"
    }
  ],
  "links": [
    [
      {
        "node": "leaf-p0-r0",
        "interface": "swp1s0",
        "attributes": {
          "role": "leaf",
          "plane": 0,
          "pod": 0,
          "su": 0,
          "rail_group": [0]
        }
      },
      {
        "node": "compute-a",
        "interface": "eth_p0_r0",
        "attributes": {
          "role": "host",
          "rail": 0,
          "pod": 0,
          "su": 0
        }
      }
    ]
  ]
}
```

Add one host-to-leaf link for every selected worker rail. Host `node` values must match Kubernetes node names in the selected `clusterConfig` group. Every host endpoint requires `attributes.rail`, every leaf endpoint requires `attributes.plane`, and 3-tier allocation also requires host `attributes.pod`.

NVIDIA AIR exports do not carry those numeric attributes directly, so AIR support relies on the following naming contract. All AIR ordinals are one-based and l8k converts them to its zero-based addressing fields:

- A 2-tier host name contains hyphen-delimited `su<S>` and `h<H>` tokens, for example `worker-su01-rack01-h01`. A 3-tier host additionally contains `pod<D>`, for example `worker-pod01-su01-rack01-h01`.
- A leaf name starts with `leaf-`. A 2-tier leaf also contains `p<P>`, `su<S>`, and `r<R>` tokens, for example `leaf-p1-su001-r1`. A 3-tier leaf additionally contains `pod<D>`, for example `leaf-p1-pod01-su001-r1`.
- Each AIR host interface is named `rail<R>p<P>` and its endpoint `network_pci` value is `rail<R>`. That key must also exist in the host node's `network_pci` map.
- The plane, rail, SU, and, for 3-tier, pod values encoded by both link endpoints must agree. `h<H>` is the host position within its pod/SU and must be unique there.

Files with other AIR naming schemes are rejected with a contract error rather than assigned inferred addresses.

```bash
l8k generate \
  --network-operator-release 26.7 \
  --spectrum-x RA2.3 \
  --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
  --topology-scheme 2-tier \
  --ip-version ipv6 \
  --topology-file ./topology.json
```

For the RA2.2 and RA2.3 v1alpha2 profiles, every
`SpectrumXRailPoolConfig.spec.railTopology[].name` is consumer-visible. The
Spectrum-X Operator uses it as both the NetworkAttachmentDefinition name and
the device-plugin resource suffix. Launch Kit therefore renders `rail0` and
`nvidia.com/rail0` for a per-rail workload, or `rail0p0` and
`nvidia.com/rail0p0` for a per-rail-plane workload.

Both `ipv4` and `ipv6` generate complete nv-ipam CIDRPools. IPv4 preserves the
existing per-node `/31` allocation. Traditional-platform IPv6 uses the following layout;
[VR uses its NIC-aware layout](#vr-dual-nic-rails).

```text
fd02:00PP:RRDD:SSHH::peer/64
```

`PP`, `RR`, `DD`, `SS`, and `HH` are the zero-based plane, rail, pod, SU, and
host indices encoded as one byte each. Two-tier topologies require `DD=00`.
The host candidate is `::1`, the connected leaf and gateway are `::2`, and the
static allocation prefix is the canonical `/64` network. Each pool covers a
rail or rail-plane with a `/40`. The route is `/32` for a single-plane
deployment and `/24` for a dual- or quad-plane deployment.

In `swplb`, the plane is encoded and l8k emits one pool per rail-plane. In
`none` and `hwplb`, the address plane is zero and l8k emits one pool per rail.
`profile.spectrumX.hostFirstOctet` affects IPv4 only. The current topology
contract provides the fields required by this standard layout. VR additionally
requires explicit host indices and accepts the IPv6 site ID described above.

### Troubleshooting CIDRPool allocation errors

CIDRPool generation matches every selected `clusterConfig.workerNodes` value
against topology host endpoint `node` values using an exact, case-sensitive
comparison. l8k does not automatically equate short names with FQDNs because
that could allocate an address to the wrong node.

When no workers match, the error reports the topology file path and a sorted
summary of selected workers, topology hosts, exact matches, missing workers,
and topology-only hosts. It also calls out likely case or short-name/FQDN
mismatches. If no names are similar, verify that `--topology-file` points to
the topology export for the cluster represented by `cluster-config.yaml`.

When a worker matches by name but is absent from a generated pool, the error
reports that worker's available rail/plane coverage. Check every host
endpoint's `attributes.rail`; for `swplb`, also check the connected leaf
endpoint's `attributes.plane`. Each selected worker must have a link for every
rail, and for every rail/plane combination in `swplb` mode.

Large node lists are sorted and limited to the first eight entries, followed
by a `(+N more)` count, so generation failures remain readable.

## DRA Workload Allocation

For RA2.2 and RA2.3, set `profile.spectrumX.useDRA: true` in an otherwise
complete Spectrum-X configuration to render ResourceClaimTemplate-based
allocation. The snippet below is an overlay, not a complete RA2.3 input; the
[profile ConfigMap](#ra23-profile-configmap) is still required.

```yaml
profile:
  spectrumX:
    enable: true
    spcxVersion: RA2.3
    useDRA: true
```

Before deployment, verify these prerequisites in the target cluster:

- The `resource.k8s.io/v1` ResourceClaim and ResourceClaimTemplate APIs are
  served. Use a Kubernetes and driver combination supported by the operators
  installed at the site. The CLI toggle is not a compatibility qualification.
- A GPU DRA driver publishes the `gpu.nvidia.com` DeviceClass and devices.
  Launch Kit does not install that driver.
- The SR-IOV operator supports DRA and publishes the
  `sriovnetwork.k8snetworkplumbingwg.io` DeviceClass and VF resources. l8k enables
  its `dynamicResourceAllocation` feature gate in generated Helm values and
  sets `SpectrumXRailPoolConfig.spec.draEnabled: true`. If Helm is externally
  managed, apply equivalent settings through that owner.

Inspect availability before submitting workloads:

```bash
kubectl api-resources --api-group=resource.k8s.io
kubectl get deviceclasses gpu.nvidia.com sriovnetwork.k8snetworkplumbingwg.io
kubectl get resourceslices
```

Generated claims request a GPU plus VFs matching the rail resource name. When
PF/GPU PCI addresses are known, they also constrain
`resource.kubernetes.io/pcieRoot`. Review those addresses against the selected
nodes: this is a PCI-root constraint, not proof of a closer PCI-switch or NUMA
relationship. `swplb` creates one claim per rail/plane; the other modes create
one per rail with the required VF count. The example workload uses resource
claims instead of device-plugin limits.

After applying an application or running validation, inspect its claims and
scheduling events in the first configured network namespace:

```bash
kubectl get resourceclaimtemplates,resourceclaims -n <network-namespace>
kubectl describe resourceclaim -n <network-namespace> <claim-name>
kubectl describe pod -n <network-namespace> <pod-name>
```

Check allocation status, device selections, and pod scheduling before testing
traffic. Successful rendering proves the manifest shape only; qualify GPU/VF
allocation and connectivity on the intended hardware. Spectrum-X has no
qualified OpenShift profile in this build.

## Deployment Notes

- Discovery defaults to one rail per physical NIC and collapses multi-plane PFs to the master PF unless the NIC model is genuinely dual-port.
- North-south-only groups are not written to `cluster-config.yaml`; they do not produce Spectrum-X manifests.
- Spectrum-X rendering participates in heterogeneous group merging when source groups share GPU type and rail count.
- `--network-namespaces` does not fan out Spectrum-X resources; Spectrum-X renders into the first configured namespace.

### Migrating existing VR rail groups

The per-NIC layout changes generated resource names. For an existing deployment,
back up the current rail pool and its generated resources before applying the new
manifests. Superseded SR-IOV policies can overlap the new PF/VF selectors and
prevent reconciliation. Remove only the old policies and OVSNetworks owned by
that rail pool, then verify the new per-NIC resources reconcile. Retire the old
rail bridges and their patch ports after confirming ownership and that no
workloads use them; preserve the shared XPlane bridge and new per-NIC bridges.
This cleanup is an operator-managed migration, not an automatic Launch Kit step.
Check SR-IOV node status, XPlane startup, and workload networking separately.
