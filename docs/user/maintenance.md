<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Maintenance

Use this procedure for a change to deployment intent, Network Operator release, or a Launch Kit generated bundle. Updating only the local `l8k` binary does not change cluster resources until you generate and apply a new bundle. Coordinate the maintenance window, driver/module policy, and application traffic checks with their owners.

## Change a deployment

1. Record `l8k version`, the selected Network Operator release, current cluster context, external Helm/application owner, and the accepted validation report. Preserve the source config, referenced profile/topology/workload files, and **entire** current bundle including `.l8k/resolved-config.yaml`. A sidecar records effective settings; it is not a source file to feed back through `--user-config`.
2. Copy and edit the source configuration for the intended change. Choose a new output directory so the accepted bundle remains available:

   ```bash
   cp ./cluster-config.yaml ./proposed-cluster-config.yaml
   l8k generate --user-config ./proposed-cluster-config.yaml \
     --save-deployment-files ./proposed-deployment
   ```

   Pass the same explicit profile, group, topology, and workload options used for the old bundle when they are part of the desired state. Review `proposed-deployment/.l8k/resolved-config.yaml`, Helm values, resource identities, selectors, IP ranges, VF counts, and driver/maintenance settings. Compare the full old and proposed `network-operator/` inventories, including resources that disappear and any other hardware cohorts. Do not assume a filtered `--groups` render owns only its selected nodes.
3. Select a safe interruption budget using [Fields](#fields) and the [release behavior](#release-behavior) below. Preview the **proposed** bundle:

   ```bash
   l8k deploy --deployment-files ./proposed-deployment --dry-run
   ```

   A dry run checks preflight and API admission, not eventual readiness or traffic. Resolve unintended Helm drift and strays before apply. `--overwrite-existing` authorizes the [reported deletion scope](../advanced/deployment.md#stray-resource-deletion-boundary); use it only after reviewing each affected identity and ownership.
4. Apply the approved bundle with `l8k deploy --deployment-files ./proposed-deployment`. Run `l8k validate --deployment-files ./proposed-deployment --wait 10m`, then use the [acceptance outcomes](validation.md#acceptance-outcomes) and the application's own traffic checks. Retain both bundles and reports with the change record.

## Recover from an incomplete change

| Failed stage | Next action |
| --- | --- |
| Config or generation | Correct the proposed input and render into a new directory; no cluster application has occurred. |
| Helm or preflight | Read the exact value diff and stray inventory. Confirm external ownership and the intended release before another apply. |
| Controller reconciliation | Inspect operator events, policy status, affected workers, and the applicable maintenance requests. Fix the specific cause before rerunning a bounded deploy or validation. |
| Connectivity or application traffic | Retain the report and failing pods; compare intended workers, rails, devices, and peer routes before changing network resources. |

The previous bundle is a record of earlier desired state, not a transactional rollback. Reapplying it can still change or delete resources, and a driver or firmware downgrade may need a separate approved procedure. Do not use `clean` or blanket overwrite as recovery. See [Troubleshooting](troubleshooting.md) and [Remove a deployment](cleanup.md) for those distinct tasks.

The `maintenance` section controls how many nodes NVIDIA operators can process at once during DOCA/OFED upgrades and SR-IOV configuration.

```yaml
maintenance:
  maxParallelOperations: 4
  maxUnavailable: 4
  maxNodeMaintenanceTimeSeconds: 3600
  maxParallelUpgrades: 4
```

## Fields

| Field | Default | Meaning |
| --- | --- | --- |
| `maxParallelOperations` | `4` | Global Maintenance Operator work limit. Positive integer or `1%` through `100%`. |
| `maxUnavailable` | `4` | Maximum unavailable nodes. Non-negative integer or `1%` through `100%`. |
| `maxNodeMaintenanceTimeSeconds` | `3600` | Cleanup delay for a Ready `NodeMaintenance` request. |
| `maxParallelUpgrades` | `4` | Legacy OFED upgrade limit for Network Operator releases before `26.1`. |

Omitted fields receive the defaults above; explicit zeros are preserved.
Integer-or-percentage values must be YAML integers or percentage strings such
as `"25%"`. Numeric strings such as `"4"`, fractions such as `1.5`, and
percentages outside `1%`–`100%` are rejected. Only `maxParallelOperations` and
`maxUnavailable` accept percentages.

| Setting | Operational meaning |
| --- | --- |
| `maxParallelOperations: 0` | Rejected by l8k; the scheduler would have no available work slots. |
| `maxUnavailable: 0` | Pauses new maintenance work. |
| `maxNodeMaintenanceTimeSeconds: 0` | Makes a Ready request immediately eligible for collection; it neither disables cleanup nor sets an operation timeout. |
| `maxParallelUpgrades: 0` | Unlimited upgrades on the legacy pre-26.1 OFED path; ignored by requestor mode. |

Maintenance Operator computes parallel-operation percentages from all cluster
nodes, rounding up. It computes unavailable-node percentages from all cluster
nodes, rounding down; nodes already cordoned or NotReady consume the budget
even when another controller made them unavailable. The upstream native SR-IOV drain
path computes its percentage from the selected pool and rounds down. A small
percentage may therefore allow zero unavailable nodes. For an externally
installed OpenShift SR-IOV Operator, confirm its version's pool-budget
semantics before choosing a percentage.

Two limits apply together in requestor mode: a request starts only when both
have capacity. With `maxUnavailable: 4` and two nodes already unavailable, at
most two more nodes may become unavailable. Keep the Ready-request cleanup
delay below the idle interval of any cluster autoscaler.

## Release Behavior

| Flow | Before Network Operator 26.1 | Network Operator 26.1 and newer |
| --- | --- | --- |
| DOCA/OFED upgrade | Network Operator drains nodes directly and `maxParallelUpgrades` is effective. | Network Operator creates `NodeMaintenance` requests and global Maintenance Operator limits are effective. |
| SR-IOV configuration | SR-IOV Operator internal drain controller uses `SriovNetworkPoolConfig.spec.maxUnavailable`. | External drainer and Network Operator requestor hand off draining to Maintenance Operator limits. |

OpenShift profiles use the Red Hat SR-IOV Operator's native drain controller for all supported Network Operator releases. Their generated `SriovNetworkPoolConfig.spec.maxUnavailable` carries `maintenance.maxUnavailable`; the external drainer behavior in the table applies to Kubernetes profiles.

## Upgrade Existing Releases

On Kubernetes, requestor mode is partially configured through Helm values.
Applying only generated CRs cannot enable the requestors. The SR-IOV handoff
requires both `operator.maintenanceOperator.useDrainControllerRequestor` and
`sriov-network-operator.operator.externalDrainer.enabled`; do not enable only
one side. OpenShift uses its separate certified-operator configuration path.

Before using overwrite, review the [stray deletion boundary](../advanced/deployment.md#stray-resource-deletion-boundary);
it can delete manual resources and resources from other cohorts.

For a proposed release change, render into a separate directory and preview first:

```bash
l8k generate \
  --user-config cluster-config.yaml \
  --network-operator-release 26.1 \
  --fabric ethernet \
  --deployment-type sriov \
  --save-deployment-files ./proposed-deployment
l8k deploy --deployment-files ./proposed-deployment \
  --kubeconfig "$KUBECONFIG" --dry-run
```

Apply through the [change procedure](#change-a-deployment) after reviewing
the release transition, Helm values, strays, and maintenance impact.
