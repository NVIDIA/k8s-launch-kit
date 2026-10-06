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
RA2.4 supports 26.10 and newer catalogued releases. RA2.3 requires legacy
profile ConfigMap data; RA2.4 requires a full doSPCX ConfigMap. DRA in RA2.2
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
