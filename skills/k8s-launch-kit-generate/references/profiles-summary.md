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

Spectrum-X pairing is exact: RA2.1/26.1, RA2.2/26.4, RA2.3/26.7. RA2.3 requires
profile ConfigMap data. DRA in RA2.2/RA2.3 requires separately available GPU
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
