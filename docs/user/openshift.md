<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# OpenShift host deployments

This procedure prepares **SR-IOV Ethernet RDMA** on an existing OpenShift cluster. Ethernet SR-IOV was exercised on a two-node OpenShift 4.22 cluster. Other standard OpenShift profiles passed rendering and server dry-run but still need live integration qualification. Spectrum-X has no OpenShift variant. The example uses Network Operator release `26.7`; confirm its suitability for the site before proceeding.

## Before you start

Install the certified [NVIDIA Network Operator for OpenShift](https://docs.nvidia.com/networking/display/kubernetes2670/openshift/deployment-guide-openshift.html), the [Red Hat SR-IOV Network Operator](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/networking_operators/sr-iov-operator), [Node Feature Discovery](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/pdf/hardware_accelerators/index), and [NVIDIA Maintenance Operator](https://github.com/Mellanox/maintenance-operator#deployment) through their own installation workflows. Confirm release compatibility with the operator owners. Launch Kit configures those installations; it does not install their Helm charts. Check the successful CSVs, required APIs, and the NVIDIA Network Operator OLM Subscription before continuing. The Subscription may have any name if its `spec.name` selects the NVIDIA package. Launch Kit persists maintenance settings through it, and configuration changes can roll controllers.

```bash
oc config current-context
oc get nodes -o wide
oc get csv -A
oc get subscriptions.operators.coreos.com -A
oc api-resources | grep -E 'NicClusterPolicy|SriovNetworkNodePolicy|NodeFeatureDiscovery'
l8k version
```

Confirm permission to run privileged discovery, create the generated resources, and later create temporary validation namespaces and SCCs. Obtain approved worker/PF scope, switch settings, unused addressing, VF capacity, MTU, and maintenance limits from the site owners. See [deployment planning](profiles.md#check-prerequisites-and-site-inputs). Resolve failed CSVs, absent APIs, and admission restrictions with the operator owners first.

For `profile.ignoreARP: true`, check the Multus sysctl allowlist before
deploying workloads:

```bash
oc -n openshift-multus get configmap cni-sysctl-allowlist -o yaml
```

The allowlist must admit
`^net.ipv4.conf.IFNAME.arp_ignore$`,
`^net.ipv4.conf.IFNAME.arp_announce$`, and
`^net.ipv4.conf.IFNAME.rp_filter$`. Have the cluster administrator add any
missing expressions through the site's OpenShift configuration process.
Otherwise pod attachment can fail even when the generated resources reconcile.
Launch Kit renders these interface sysctls for OpenShift and omits the
`net.ipv4.conf.all.*` entries used by the Kubernetes flavor.

## 1. Discover the intended workers

Prepare a site-owned seed configuration. This illustrates the required scope, **not** a complete hardware inventory; discovery supplies NIC and GPU details. Replace the worker names and namespaces with site values:

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
    nodeSelector:
      kubernetes.io/hostname: worker-a
  - identifier: worker-b
    workerNodes: [worker-b]
    nodeSelector:
      kubernetes.io/hostname: worker-b
```

The seed entries identify workers for discovery. Discovery replaces them with hardware groups and assigns each group a node selector. A group can contain multiple workers. Preserve the seed file and merge live hardware into another file:

```bash
l8k discover --flavor ocp \
  --user-config ./cluster-config.yaml \
  --save-cluster-config ./discovered-cluster-config.yaml
```

Review workers, PFs, rails, labels, profile, release, and namespaces in the discovered file. Set site-approved IP ranges, gateways, driver settings, and maintenance budget before generation; discovery cannot prove that a default range is unused elsewhere.

## 2. Generate and review

```bash
l8k generate --flavor ocp \
  --user-config ./discovered-cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./ocp-deployment
```

Compatible groups with the same GPU type and east-west rail count share a pool, network, drain pool, and validation DaemonSet across their exact worker union. Hardware policies follow the same render scope as Kubernetes: one per compatible bucket, or one per selected source group for a strict subset. A group with multiple workers does not produce one policy per hostname. Groups with different rail counts get distinct networks. SR-IOV CRs live in the Red Hat operator namespace; NV-IPAM pools live in the NVIDIA operator namespace. The `SriovNetwork` creates NADs in requested workload namespaces and requests `openshift.io/<resourceName>`. The drain pool carries `maintenance.maxUnavailable` to Red Hat's native drainer.

Launch Kit sets NFD PCI `deviceLabelFields` to `[vendor]`, replacing the old field selection while retaining other NFD sources and PCI class whitelists. Review this change with the NFD owner. The Red Hat operator uses native draining; the NVIDIA external SR-IOV drainer integration is unavailable in that build. The Maintenance Operator coordinates supported driver operations.

Keep `ocp-deployment/.l8k/resolved-config.yaml` with the generated manifests. Check the `NicNodePolicy` and `SriovNetworkNodePolicy` selectors and PCI addresses against every worker they target. Also check the `SriovNetworkPoolConfig`, `SriovNetwork`, NV-IPAM pools, resource names, namespaces, NFD change, and any custom workload. OpenShift output must have no `values.yaml`; if one remains, regenerate into a clean directory with `--flavor ocp`. The generation [bundle checklist](../advanced/generation.md#review-the-bundle) gives the common review points.

If `profile.ignoreARP` and source-based routing are enabled, each secondary
network's `metaPlugins` should contain interface-only `tuning` sysctls
followed by `sbr`. Check that no `net.ipv4.conf.all.*` key appears there.

## 3. Preview and deploy

```bash
l8k deploy --flavor ocp --deployment-files ./ocp-deployment --dry-run
```

Review admission and preflight findings. Server dry-run neither waits for reconciliation nor proves traffic. After approval, apply the same bundle:

```bash
l8k deploy --flavor ocp --deployment-files ./ocp-deployment
```

If the operator, admission, or SR-IOV node state fails, retain the bundle and inspect the failed CSV/Subscription, controller events, and [stage-specific checks](troubleshooting.md).

## 4. Validate and decide

```bash
l8k validate --flavor ocp --deployment-files ./ocp-deployment --wait 10m
```

Read `ocp-deployment/k8s-launch-kit-validation-report.html` against the [acceptance outcomes](validation.md#acceptance-outcomes). Validation creates temporary `l8k-validation-*` namespaces per workload namespace, copies required NADs and image pull secrets, and creates a validation ServiceAccount, narrow SCC, Role, RoleBinding, and example DaemonSets. It needs namespace/SCC creation and source NAD/secret read permission. An OpenShift run fails if resources are still reconciling and connectivity cannot run, no tests are planned, or a selected family has no gating test. The tool normally removes these test resources; `--keep` retains them for inspection. Record skipped families and missing worker/rail endpoints before accepting the result.

## Remove the intended resources

`l8k clean` rejects OpenShift. Preserve the externally installed operators and their Subscriptions. From the reviewed bundle and live inventory, list generated resources by **kind, name, and namespace**; confirm whether each is shared with another workload or cohort. Remove only approved identities through the site's OpenShift change process, wait for finalizers, and record what remains externally owned. Do not substitute the broad [Kubernetes cleanup](cleanup.md) command. If validation cleanup was interrupted, separately inspect `l8k-validation-*` namespaces and remove the exact retained test objects after API access returns.
