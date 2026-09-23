<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# OpenShift host deployments

Set `flavor: ocp` in the host configuration or pass `--flavor ocp` to discover, generate, deploy, and validate. The CLI flag overrides the file. The default remains `k8s`. OpenShift uses separate profile directories and never installs or upgrades the Network Operator Helm chart. Remove old `values.yaml` files before deploying generated OpenShift output.

Install the NVIDIA Network Operator through the certified Operators catalog before running l8k. Install the Red Hat SR-IOV Network Operator for SR-IOV profiles, Node Feature Discovery, and NVIDIA Maintenance Operator separately. l8k checks the expected successful CSV and required API, then configures their existing installations. Network Operator deployment requires an OLM Subscription so l8k can persist its maintenance environment settings; the Subscription may have any name when its `spec.name` selects the NVIDIA Network Operator package. Configuration changes may roll operator controllers. SR-IOV uses the Red Hat operator's native drain behavior; the NVIDIA external SR-IOV drainer integration is unavailable in that operator build. The Maintenance Operator still coordinates supported Network Operator driver operations.

```yaml
flavor: ocp
networkOperator:
  namespace: nvidia-network-operator
  selectedRelease: "26.7"
sriov:
  operatorNamespace: openshift-sriov-network-operator
nfd:
  operatorNamespace: openshift-nfd
  configurationName: nfd-instance
maintenance:
  operatorNamespace: nvidia-maintenance-operator
networkNamespaces:
  - rdma-workloads
clusterConfig:
  - identifier: worker-a
    workerNodes: [worker-a]
  - identifier: worker-b
    workerNodes: [worker-b]
```

Use `clusterConfig[].workerNodes` to limit discovery. Each OpenShift source group must also have a node selector; generation rejects either field when empty. Hardware policies select each listed worker by hostname, so servers with the same machine/GPU label can retain distinct PCI layouts. Compatible groups with the same GPU type and east-west rail count share an IP pool, secondary network, SR-IOV drain pool, and validation DaemonSet so their pods can communicate. The shared resources select the exact union of those groups' `workerNodes` by hostname. The SR-IOV drain pool applies `maintenance.maxUnavailable` to Red Hat's native drainer. The generated `SriovNetworkNodePolicy`, `SriovNetworkPoolConfig`, and `SriovNetwork` live in the SR-IOV operator namespace. NV-IPAM pools live in the Network Operator namespace, which its node daemon watches. The `SriovNetwork` creates its NAD in each requested workload namespace and requests `openshift.io/<resourceName>`.

The current OpenShift profiles cover SR-IOV Ethernet RDMA, SR-IOV InfiniBand RDMA, host device RDMA, macvlan with RDMA shared devices, and IPoIB with RDMA shared devices. Ethernet SR-IOV was exercised on a two-node OpenShift 4.22 cluster. The other profiles passed rendering and server dry-run, but need live integration qualification on suitable hardware. Spectrum-X profiles have no OpenShift variant: RA2.1 requires Red Hat `OVSNetwork` and `SriovNetworkPoolConfig` alongside `SpectrumXRailPoolConfig` v1alpha1, while RA2.2/RA2.3 require `SpectrumXRailPoolConfig` v1alpha2 and the Network Operator's `spectrumXOperator` path (RA2.3 can also use DRA). Those controller/API combinations were not qualified with the certified bundle on this cluster. Generation fails clearly for these combinations; the missing profile is a qualification gap, not proof that a particular CRD is absent.

For connectivity validation, l8k creates a temporary `l8k-validation-*` namespace for each workload namespace. It copies only the required network attachments and image pull secrets, then creates the validation ServiceAccount, narrow SCC, Role, RoleBinding, and example DaemonSets there. This keeps the SCC identity outside the workload project. By default, l8k removes the test objects and namespaces after the run; `--keep` retains them for inspection. Validation needs permission to create namespaces and SCCs and to read the source network attachments and any image pull secrets. An OpenShift validation run fails if manifests are still reconciling and connectivity cannot run, no tests are planned, or a selected check family has no gating test. If the API becomes unreachable during cleanup, inspect and remove the exact temporary objects after access returns.

`l8k clean` rejects an OpenShift flavor or cluster because its broad Kubernetes cleanup could remove external operator resources. Delete only the intended generated resources through normal OpenShift administration. Server dry-run can defer IPPool checks until NicClusterPolicy has installed the NV-IPAM API; a deferred check is not a successful validation.
