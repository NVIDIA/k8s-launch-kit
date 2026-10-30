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

For `profile.ignoreARP: true`, Kubernetes and OpenShift emit the same tuning
parameters: `arp_ignore=1`, `arp_announce=2`, and `rp_filter=0` under both
`net.ipv4.conf.all` and `net.ipv4.conf.IFNAME`.

Generation writes `network-operator/04-cni-sysctl-allowlist-patch.yaml` when
`ignoreARP` is enabled. This ConfigMap patch contains only the six matching `ignoreARP` expressions
in `data["allowlist.conf"]`. It is generated once for all selected groups and
namespaces, and can be inspected or edited before deployment.

During OpenShift deployment, Launch Kit consumes that artifact before changing
SR-IOV, maintenance, NFD, or Subscription configuration and before applying
network resources. It merges only the artifact's allowlist entries into the
existing `openshift-multus/cni-sysctl-allowlist` ConfigMap. Existing site entries,
comments, other data keys, and metadata are preserved; the generated metadata
does not transfer ownership of the platform ConfigMap. Repeated deploys skip an
already-complete allowlist, and concurrent changes are retried. Validation checks
that the artifact's entries are present, allowing additional site entries.

The deploying identity needs **get and patch** permission on that ConfigMap.
The ConfigMap must already exist; missing resources and API/RBAC errors stop
deployment. `--dry-run` uses a server-side dry-run patch and does not persist it.
Generation alone does not patch the cluster. Kubernetes rejects this OCP-specific patch, and bundles without the patch
do not touch this OpenShift resource. The shared allowlist additions
persist after deployment and are not removed by `l8k clean`.

```bash
oc -n openshift-multus get configmap cni-sysctl-allowlist -o yaml
```

When applying generated manifests outside Launch Kit, the cluster administrator
must merge the generated patch entries before workloads attach their networks.
Directly applying the ConfigMap manifest can replace `allowlist.conf` and remove
site entries; use Launch Kit deploy for the additive merge. Follow
[Red Hat's sysctl allowlist procedure](https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html/nodes/working-with-containers).
A successful ConfigMap patch does not prove propagation to each node or successful
pod attachment; verify those during workload validation.

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

For standard profiles, `--deploy-nic-configuration-template` opts into
[host NIC tuning](../reference/configuration.md#nic-configuration-and-naming).
It enables NCO and emits per-source NIC configuration; Ethernet includes RoCE
QoS, InfiniBand does not. Only SR-IOV adds `mellanox` to
`SriovOperatorConfig.spec.disablePlugins`, retaining other disabled plugins.
RDMA-shared and host-device do not change that setting. This new path has offline
coverage only; qualify runtime tuning with the site's NCO/DOCA/RHCOS releases.

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

Read `ocp-deployment/k8s-launch-kit-validation-report.html` against the [acceptance outcomes](validation.md#acceptance-outcomes). Platform-managed Multus and CNI plugin installer checks are reported as **UNVERIFIED** and do not fail validation; NCP ownership and readiness checks still apply. Use the connectivity results to assess runtime behavior; see [external networking prerequisites](validation.md#external-networking-prerequisites).

Validation creates temporary `l8k-validation-*` namespaces per workload namespace, copies required NADs and image pull secrets, and creates a validation ServiceAccount, narrow SCC, Role, RoleBinding, and example DaemonSets. It needs namespace/SCC creation and source NAD/secret read permission. An OpenShift run fails if resources are still reconciling and connectivity cannot run, no tests are planned, or a selected family has no gating test. The tool normally removes these test resources; `--keep` retains them for inspection. Record skipped families and missing worker/rail endpoints before accepting the result.

## Remove the intended resources

`l8k clean` rejects OpenShift. Preserve the externally installed operators and their Subscriptions. From the reviewed bundle and live inventory, list generated resources by **kind, name, and namespace**; confirm whether each is shared with another workload or cohort. Remove only approved identities through the site's OpenShift change process, wait for finalizers, and record what remains externally owned. Do not substitute the broad [Kubernetes cleanup](cleanup.md) command. If validation cleanup was interrupted, separately inspect `l8k-validation-*` namespaces and remove the exact retained test objects after API access returns.
