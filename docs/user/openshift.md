<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# OpenShift host deployments

Set `flavor: ocp` in the host configuration or pass `--flavor ocp` to discover, generate, deploy, and validate. The CLI flag overrides the file. The default remains `k8s`. OpenShift uses separate profile directories and never installs or upgrades the Network Operator Helm chart. Remove old `values.yaml` files before deploying generated OpenShift output.

Install the NVIDIA Network Operator through the certified Operators catalog before running l8k. Install the Red Hat SR-IOV Network Operator for SR-IOV profiles, Node Feature Discovery, and NVIDIA Maintenance Operator separately. l8k checks the expected successful CSV and required API, then configures their existing installations. Configuration changes may roll operator controllers. SR-IOV uses the Red Hat operator's native drain behavior; the NVIDIA external SR-IOV drainer integration is unavailable in that operator build. The Maintenance Operator still coordinates supported Network Operator driver operations.

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

Use `clusterConfig[].workerNodes` to limit discovery. Each OpenShift source group keeps its own node selector and PCI address; choose a selector that matches only its intended nodes before applying a policy. The generated `SriovNetworkNodePolicy` and `SriovNetwork` live in the SR-IOV operator namespace. NV-IPAM pools live in the Network Operator namespace, which its node daemon watches. The `SriovNetwork` creates its NAD in each requested workload namespace and requests `openshift.io/<resourceName>`.

The current OpenShift profiles cover SR-IOV Ethernet RDMA, SR-IOV InfiniBand RDMA, host device RDMA, macvlan with RDMA shared devices, and IPoIB with RDMA shared devices. Ethernet SR-IOV was exercised on a two-node OpenShift 4.22 cluster. The other profiles have only been rendered and need integration qualification on suitable hardware. Spectrum-X profiles have no OpenShift variant: RA2.1 requires Red Hat `OVSNetwork` and `SriovNetworkPoolConfig` alongside `SpectrumXRailPoolConfig` v1alpha1, while RA2.2/RA2.3 require `SpectrumXRailPoolConfig` v1alpha2 and the Network Operator's `spectrumXOperator` path (RA2.3 can also use DRA). Those controller/API combinations were not qualified with the certified bundle on this cluster. Generation fails clearly for these combinations; the missing profile is a qualification gap, not proof that a particular CRD is absent.

For connectivity validation, l8k temporarily creates a dedicated ServiceAccount, narrow SCC, Role, RoleBinding, and example DaemonSets in the workload namespace. It removes only objects it created during that invocation. Use an ordinary project for admission testing. If the API becomes unreachable during cleanup, inspect and remove the exact temporary objects after access returns.

`l8k clean` rejects `ocp` because its broad Kubernetes cleanup could remove external operator resources. Delete only the intended generated resources through normal OpenShift administration. Server dry-run can defer IPPool checks until NicClusterPolicy has installed the NV-IPAM API; a deferred check is not a successful validation.
