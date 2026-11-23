<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Remove a Network Operator Deployment

## OpenShift configuration rollback

Use the deployment directory that was applied to the cluster, including its `.l8k/resolved-config.yaml` and `network-operator/` manifests. Verify the kubeconfig context, then review the identities displayed before confirming.

```bash
l8k clean --kubeconfig ~/.kube/config --flavor ocp \
  --deployment-files ./ocp-deployment --clean-timeout 45m
```

OpenShift cleanup selects the generated policies, networks, IP pools and NIC templates by kind, namespace and name. It checks live ownership before deleting an object. A conflicting owner, a replacement or a conflicting live spec stops cleanup. Launch Kit serializes deploy and clean for the same OpenShift deployment using a lock ConfigMap. After an abrupt process exit, confirm the recorded operation has stopped before removing the specific lock named in the error. Pause older Launch Kit binaries, custom writers and manual changes until cleanup finishes.

By default the command waits for finalizers, generated network attachments to disappear, and the selected NIC templates to clear their device configuration. For a removed SR-IOV node policy it checks the targeted PFs in `SriovNetworkNodeState`, including cleared desired interfaces, zero VFs and successful synchronization. For a configured node it also reads the SR-IOV daemon pod log to verify the current node-state generation. Missing or rotated log evidence leaves that reset unverified. Where a policy deployed a MOFED driver, the current driver container must exit successfully before cleanup can finish. `--skip-wait` skips the controller-specific checks and still waits for CR deletion. Both modes use the same deployment scope. A timeout or interrupted cleanup can be retried with the same bundle. Follow any checkpoint diagnostic before retrying a driver shutdown. In the Network Operator namespace, grant Launch Kit permission to read, create, update and delete its cleanup ConfigMaps. A small completion record lets a retry use earlier worker and driver checks after their controllers have gone. The next deployment invalidates the record before applying new configuration. If a root policy has already disappeared and the earlier run did not record its workload ownership and verification, clean reports that it cannot verify historical teardown. A previous `--skip-wait` run supplies no such evidence.

For shared settings, deploy records which fields it changed and their previous values. A surviving manual SR-IOV policy, network or driver consumer can keep the corresponding setting in place. Cleanup can restore those settings in the NFD, SR-IOV and Maintenance Operator configurations, the Network Operator Subscription environment and the CNI sysctl allowlist. Inspect retained effects in the command output or the JSON `cleanup.retained` array. With older deployments, cleanup can verify a generated CR through its version, live spec and server-side apply ownership. Shared settings without recorded prior values require a site decision.

OpenShift operators are installed separately through OLM. Even with `--manage-operators`, deploy only generates their installation manifests. Their Subscriptions, CSVs, OperatorGroups and namespaces stay installed. For platform-specific details, see the [OpenShift walkthrough](openshift.md#remove-the-intended-resources).

## Kubernetes deployment removal

Use `l8k clean` to tear down the Network Operator deployment on one **Kubernetes**
cluster. The command removes custom resources first so the installed
controllers can process their finalizers, then uninstalls the Helm release
unless the resolved config marks it externally owned.

Before running it, confirm the current kubeconfig context, intended operator namespace, live custom-resource inventory, and whether the Helm release is externally owned. The deletion scope includes **every** custom resource in the operator namespace and the five listed cluster-scoped kinds across the cluster, including manual or other-cohort instances. There is no cleanup dry run. Record approved identities and preserve the generated bundle and validation evidence.

```bash
l8k clean --kubeconfig ~/.kube/config
```

The command is destructive and asks for confirmation before changing the
cluster. Verify the kubeconfig and the displayed target namespace before
confirming.

The kubeconfig identity must be able to list CRDs cluster-wide, list, get, and
delete the selected custom resources, and manage the Helm release in the target
namespace. Cluster-administrator access normally satisfies this requirement.

## Namespace Resolution

The first available namespace source wins:

1. `--network-operator-namespace <namespace>`
2. `networkOperator.namespace` in `--user-config` or
   `./cluster-config.yaml`
3. `networkOperator.namespace` in an explicit
   `--config-dir/l8k-config.yaml`
4. `nvidia-network-operator`

Custom installation namespaces must be supplied by flag or trusted local
config. Cleanup deliberately does not infer a destructive target from
user-creatable in-cluster objects such as Helm release Secrets. The config is
read only for `networkOperator.namespace` and
`networkOperator.skipHelmChart`; stale release settings elsewhere in the file
do not block cleanup.

```bash
l8k clean --kubeconfig ~/.kube/config \
  --network-operator-namespace operator-system
```

## Deletion Boundary

Cleanup selects every namespaced custom resource in the Network Operator namespace. Cluster-scoped
resources cannot be associated with a namespace, so every live instance of
the five listed kinds is removed.

The command preserves:

- The resolved namespace
- CustomResourceDefinitions
- Secrets not owned by the Helm release, including unrelated registry
  credentials
- Custom resources outside the resolved namespace, except the five explicitly
  listed cluster-scoped kinds
- Generated configuration and deployment files on disk

When Helm is uninstalled, Helm release metadata and any Secret rendered as a
chart-managed resource are removed with the release.

Missing resources are skipped. Reconciled teardown also checks for outstanding
node configuration, generated attachments, and incomplete driver shutdown.

## Wait for reconciled teardown

By default, cleanup waits for networking changes to reconcile on workers. The target
namespace, cluster-scoped deletion boundary and Helm retention settings still
apply. Pause other deployment writers for the duration of cleanup.

```bash
l8k clean --kubeconfig ~/.kube/config \
  --network-operator-namespace network-operator --clean-timeout 45m
```

Safe clean records the SR-IOV node inventory and performs these steps while
the controllers can still reconcile their resources.

1. Remove Spectrum-X rail pool configs and wait for their finalizers. These
   controllers create SR-IOV policies and networks. The rail CR has one shared
   finalizer. Its removal does not confirm local OVS bridge cleanup on every
   worker.
2. Remove network definitions and wait for their generated
   NetworkAttachmentDefinitions, including attachments in workload namespaces.
3. Remove NIC configuration, firmware and interface-name templates and wait
   for template-derived intent to clear from the corresponding NicDevices.
   Devices that had pending configuration or firmware changes must acknowledge
   the empty spec.
   The operator does not expose confirmation of host udev rule removal or
   rollback of firmware settings.
4. Remove SR-IOV node policies and pool configs. Every observed node state must
   report an empty desired spec, `Succeeded`, and no remaining VFs on managed
   PFs. Cleanup also waits until `status.bridges.ovs` reports no managed bridges,
   including on a retry after the policies have disappeared.
   Empty maps and lists are normal API representations. The operator exposes
   no observed generation on this status. For a configured node, cleanup checks
   the configuration daemon's logs for the successful reconciliation of the
   empty spec's generation on that node. It repeats this check on retry even if
   the policy has already disappeared. Missing success evidence (for example,
   rotated logs) prevents cleanup from declaring a reset. When the drain
   controller reports a requested or current drain, cleanup waits for it to
   return to `Idle`. Active VFs outside the
   deployment can block the check if the operator reports them as managed.
5. Delete the remaining CRs and wait for finalizers. Delete the root
   `NicClusterPolicy` and `NicNodePolicy` with background propagation so the
   operator stops recreating their children. Track their observed Deployments,
   DaemonSets, ReplicaSets and pods in the operator namespace until they
   disappear. The `l8k-policy-workload-cleanup` ConfigMap retains observed
   workload UIDs across retries. Cleanup removes it when those workloads are
   gone. A pod whose entire ownership chain vanished before cleanup first
   observed it cannot be identified this way. For an operator-managed MOFED driver, observe
   each tracked pod through deletion and require a successful termination of
   its current `mofed-container` (exit code zero). The pod watch must reach
   the final inventory resource version by event or bookmark within two minutes.
   Re-scan for remaining
   resources before the Helm step.

`--clean-timeout` bounds custom-resource teardown, including the driver wait.
Zero (default) allows an unbounded wait. Errors stop further stages and leave
the Helm release available for inspection. Inspect the reported resource,
controller logs and node state before retrying with the same options.
During cleanup, avoid changing policies or restarting the controllers.

Safe clean needs cluster-wide read access to generated network attachments
and driver DaemonSets and pods. It also needs permission to watch pods and read SR-IOV configuration daemon
pod logs in the operator namespace. Cleanup reads, creates, updates and deletes its
checkpoint ConfigMaps in that namespace.
After worker reconciliation, a deployment with a driver creates a ConfigMap named
`l8k-mofed-cleanup-checkpoint` before deleting the driver-owning policies and removes
it after verifying shutdown. A lost watch, interruption, or nonzero driver
exit leaves the checkpoint and blocks a subsequent safe clean. Use its pod
inventory and node logs to verify driver recovery before an administrator
removes it. Some historical driver entrypoints report a nonzero exit on
shutdown. Pod disappearance alone cannot qualify those images.

## Legacy batch cleanup

With `--skip-wait`, cleanup uses the original batch order:

1. Discover every namespaced custom-resource instance in the resolved operator
   namespace and every instance of the Network Operator's known cluster-scoped
   kinds: `HostDeviceNetwork`, `IPoIBNetwork`, `MacvlanNetwork`,
   `NicNodePolicy`, and `NicClusterPolicy`.
2. Send a background deletion request to every discovered custom resource
   before waiting on any one of them. The `NicClusterPolicy` request is sent
   after the other known cluster-scoped kinds.
3. Monitor the complete deletion set until every custom resource is gone,
   including finalizer processing. Sending all requests first prevents one
   CR's finalizer from blocking on another CR that has not entered deletion.
4. Re-scan both scopes and repeat the delete-all-then-wait sequence for any
   custom resources created or exposed during policy teardown.
5. Unless `networkOperator.skipHelmChart: true` or `--keep-helm-chart` retains
   it, uninstall the `network-operator` Helm release and wait for Helm-managed
   resources to be removed.

## External Multus and CNI plugins

Component ownership does not expand cleanup: Launch Kit adds no deletion of
external DaemonSets or CNI host files. Removing an NCP still allows the Network
Operator to garbage-collect its own children, including managed Multus/plugins.
Explicit external settings do not turn those children into external resources
or make NCP deletion a safe ownership migration. The broad custom-resource
cleanup boundary above remains in effect; it can include NAD custom resources
in the selected namespace. Preserve platform networking outside that boundary
and review exact resources before cleanup.

## Keep the Helm Release

When the resolved config sets `networkOperator.skipHelmChart: true`, cleanup
treats the Network Operator Helm release as externally owned and retains it
while still deleting the same custom resources:

```yaml
networkOperator:
  skipHelmChart: true
```

Use `--keep-helm-chart` to request the same retention explicitly regardless of
config:

```bash
l8k clean --kubeconfig ~/.kube/config --keep-helm-chart
```

This option does not narrow the custom-resource deletion boundary.

## Automation

JSON output is non-interactive and auto-confirms the cleanup. Use it only when
the target has already been reviewed:

```bash
l8k clean --kubeconfig ~/.kube/config \
  --output json >cleanup.json 2>cleanup.log &&
  jq . cleanup.json
```

Check the cleanup process status before parsing and retain `cleanup.log`.
The [automation capture pattern](../integrator/automation.md#gitops-pattern)
shows how to propagate a failed command in a script.

A successful result includes the resolved namespace, the number of deleted
custom resources, whether Helm removed a release, and the effective Helm
retention policy derived from config and `--keep-helm-chart`:

```json
{
  "success": true,
  "phase": "clean",
  "deployed": false,
  "cleanup": {
    "namespace": "nvidia-network-operator",
    "customResourcesDeleted": 12,
    "helmReleaseRemoved": true,
    "keepHelmChart": false
  },
  "messages": []
}
```
