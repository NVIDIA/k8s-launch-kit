# Profiles Summary

The repository has eight Kubernetes profile families and five OpenShift
variants. Use [Deployment Profiles](../../../docs/user/profiles.md) for selection
and [OpenShift](../../../docs/user/openshift.md) for prerequisites and live
qualification. Each profile's `profile.yaml` is the template inventory.

Kubernetes profiles normally render `00-values.yaml` as `values.yaml` unless
Helm management is disabled. OpenShift profiles use separately installed
operators and have no Helm values template. Conditional templates may render
no file; group and namespace suffixes depend on resource scope.

Host-device keeps its driver and device plugin in the singleton
NicClusterPolicy and never renders a NicNodePolicy. Other standard Kubernetes
profiles use NicNodePolicy where their release gates allow it. Example
DaemonSets are validation fixtures, skipped by deploy. A custom workload
replaces the fixture with an operational `90-workload-*.yaml` resource.

Spectrum-X pairing is exact for RA2.1/26.1, RA2.2/26.4 and RA2.3/26.7;
RA2.4 and RA3.1 support 26.10 and newer catalogued releases. RA2.3 requires legacy
profile ConfigMap data; RA2.4 and RA3.1 require a full doSPCX ConfigMap. DRA in RA2.2
and newer requires separately available GPU
DRA support and the DeviceClasses described in the
[Spectrum-X guide](../../../docs/user/spectrum-x.md#dra-workload-allocation).

## Kubernetes Template Inventory

### `host-device-rdma`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `20-ippool.yaml`
- `35-nicinterfacenametemplate.yaml`
- `30-hostdevicenetwork.yaml`
- `40-example-daemonset.yaml`

### `ipoib-rdma-shared`

With the DOCA driver disabled, the host owner must provision IPoIB netdevs and
RDMA shared-plugin character devices (`rdma_cm`, `umad`, `uverbs`). Generation
emits an advisory, including offline preset generation; it does not inspect or
load host modules. See the [IPoIB prerequisites](../../../docs/user/profiles.md#ipoib-with-inbox-drivers).


- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `11-nicnodepolicy.yaml`
- `20-ippool.yaml`
- `35-nicinterfacenametemplate.yaml`
- `30-ipoibnetwork.yaml`
- `40-example-daemonset.yaml`

### `macvlan-rdma-shared`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `11-nicnodepolicy.yaml`
- `20-ippool.yaml`
- `35-nicinterfacenametemplate.yaml`
- `30-macvlannetwork.yaml`
- `40-example-daemonset.yaml`

### `spectrum-x`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `25-nicinterfacenametemplate.yaml`
- `28-spectrumxprofile-configmap.yaml`
- `30-nicconfigurationtemplate.yaml`
- `60-cidrpool.yaml`
- `80-spectrumxrailpoolconfig.yaml`
- `85-resourceclaimtemplate.yaml`
- `90-example-daemonset.yaml`

### `spectrum-x-ra2.1`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `25-nicinterfacenametemplate.yaml`
- `30-nicconfigurationtemplate.yaml`
- `40-sriovnetworkpoolconfig.yaml`
- `50-sriovnetworknodepolicy.yaml`
- `55-ovsnetwork.yaml`
- `60-cidrpool.yaml`
- `80-spectrumxrailpoolconfig.yaml`
- `90-example-daemonset.yaml`

### `spectrum-x-ra2.2`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `25-nicinterfacenametemplate.yaml`
- `30-nicconfigurationtemplate.yaml`
- `60-cidrpool.yaml`
- `80-spectrumxrailpoolconfig.yaml`
- `85-resourceclaimtemplate.yaml`
- `90-example-daemonset.yaml`

### `sriov-ethernet-rdma`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `11-nicnodepolicy.yaml`
- `20-ippool.yaml`
- `30-nicinterfacenametemplate.yaml`
- `35-sriovnetworkpoolconfig.yaml`
- `40-sriovnetworknodepolicy.yaml`
- `50-sriovnetwork.yaml`
- `60-example-daemonset.yaml`

### `sriov-ib-rdma`

- `00-values.yaml`
- `10-nicclusterpolicy.yaml`
- `11-nicnodepolicy.yaml`
- `20-ippool.yaml`
- `30-nicinterfacenametemplate.yaml`
- `35-sriovnetworkpoolconfig.yaml`
- `40-sriovnetworknodepolicy.yaml`
- `50-sriovibnetwork.yaml`
- `60-example-daemonset.yaml`

## OpenShift Variants

OpenShift provides `sriov-ethernet-rdma-ocp`, `sriov-ib-rdma-ocp`,
`host-device-rdma-ocp`, `macvlan-rdma-shared-ocp`, and
`ipoib-rdma-shared-ocp`. The OpenShift guide distinguishes Ethernet SR-IOV live
qualification from rendering/server-dry-run coverage for the other profiles.
Inspect each variant's `profile.yaml`; Kubernetes Helm/requestor assumptions
do not carry over to the externally installed Red Hat SR-IOV Operator.

## RA2.4 and RA3.1 integration

RA2.3 is limited to Network Operator 26.7; RA2.4 and RA3.1 default to 26.10 and
accept newer catalogued releases. RA2.4 and RA3.1 need a full doSPCX ConfigMap, preserving
binaryData and annotations and rendering in the resolved operator namespace.
With RA2.4/RA3.1, VR with ConnectX-9 defaults to `hwplb` and eight planes;
explicit YAML and CLI values take precedence. Explicit `swplb` with an omitted
plane count retains the CX9 four-plane fallback; this does not qualify that recipe.
VR discovery requires eight east-west ASIC NICs in four reciprocal NetworkBay
pairs. NetworkBays are numbered by ascending first-ASIC (`asic: 1`) PCI address;
rails cross first/second ASICs between NetworkBays 1/2 and 3/4. Missing or inconsistent
status fails discovery. See `docs/user/discovery.md` for the rail mapping.
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
