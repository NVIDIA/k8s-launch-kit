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

Two limits apply together in requestor mode: a request starts only when both `maxParallelOperations` and `maxUnavailable` have capacity.

## Release Behavior

| Flow | Before Network Operator 26.1 | Network Operator 26.1 and newer |
| --- | --- | --- |
| DOCA/OFED upgrade | Network Operator drains nodes directly and `maxParallelUpgrades` is effective. | Network Operator creates `NodeMaintenance` requests and global Maintenance Operator limits are effective. |
| SR-IOV configuration | SR-IOV Operator internal drain controller uses `SriovNetworkPoolConfig.spec.maxUnavailable`. | External drainer and Network Operator requestor hand off draining to Maintenance Operator limits. |

## Upgrade Existing Releases

Requestor mode is partially configured through Helm values. Applying only generated CRs cannot enable the requestors.

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
