<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Maintenance

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

Regenerate and deploy with overwrite when the existing Helm release has different values:

```bash
l8k generate \
  --user-config cluster-config.yaml \
  --network-operator-release 26.1 \
  --fabric ethernet \
  --deployment-type sriov \
  --save-deployment-files ./deployment \
  --deploy \
  --overwrite-existing \
  --kubeconfig "$KUBECONFIG"
```
