<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Deployment Mechanics

`l8k deploy` applies a generated bundle in dependency order and waits for controller reconciliation.

```bash
l8k deploy \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

If `<deployment-files>/network-operator/` exists, Launch Kit uses it automatically. Otherwise it reads YAML files directly from `<deployment-files>`.

## Helm Install Or Upgrade

When the bundle contains `values.yaml` and the selected release supplies a Helm repository URL, Launch Kit uses the Helm Go SDK to manage the `network-operator` release:

- No release: install the selected chart in `networkOperator.namespace`.
- Matching chart and values: no-op.
- Different chart or values: fail and show the conflict.
- Different chart or values with `--overwrite-existing`: run Helm upgrade.
- Release stuck in a pending Helm state: fail with rollback/uninstall guidance.

The chart is downloaded to a temporary directory. Launch Kit does not add or modify a local Helm repository.

If Helm metadata is absent, phase 0 is skipped so environments that manage the chart out of band can still apply the generated CRs.

## Preflight

Before applying custom resources, Launch Kit compares the bundle with the cluster:

| Check | Detects |
| --- | --- |
| Helm chart version | Installed chart differs from the selected release. |
| Helm values | Installed user values differ from generated `values.yaml`. |
| Component versions | Live `NicClusterPolicy` component versions differ from the release catalog. |
| Stray resources | Managed Network Operator CRs exist but are not in the generated bundle. |

Without `--overwrite-existing`, any mismatch stops deployment and all detected drift is reported together.

With `--overwrite-existing`, Launch Kit authorizes the Helm upgrade, deletes stray managed CRs, and lets server-side apply converge owned `NicClusterPolicy` fields. This can remove resources, so inspect the preflight report before enabling it.

## Apply Order

After Helm and preflight, deployment proceeds in four manifest phases:

1. Apply `NicClusterPolicy` and wait for a terminal state.
2. Apply each `NicNodePolicy` and wait for each policy.
3. Apply every remaining manifest without serial waits so controllers reconcile concurrently.
4. Poll each phase-3 resource until it is `READY` or `ERROR`.

Launch Kit gates changed resources on controller observation, avoiding a false success from stale status left by an earlier generation.

Example workload manifests are not part of deployment. Files with `example` in the name are reserved for `l8k validate`.

## Terminal State

The shared resource-state registry classifies generated objects as:

| State | Meaning |
| --- | --- |
| `READY` | Controller reconciliation and kind-specific checks passed. |
| `IN-PROGRESS` | Controller has not reached the requested state. |
| `ERROR` | Controller or a kind-specific cross-check reported failure. |
| `MISSING` | Object does not exist; used by validation rather than immediately after apply. |

Kind-specific checks include per-component Network Operator state, SR-IOV per-node sync, matched PF counts, NIC configuration templates, IP pools, and Spectrum-X rail configuration.

## Timeout

The default deploy budget is unbounded because SR-IOV and driver reconciliation can exceed a small fixed timeout on large clusters.

Bound the entire Helm, apply, and reconciliation operation to a maintenance window:

```bash
l8k deploy \
  --deployment-files ./deployment \
  --deploy-timeout 90m
```

There is no independent per-manifest deadline.

## Server-Side Dry Run

```bash
l8k deploy \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --dry-run
```

Dry run sends resources through Kubernetes server-side validation without persisting them, runs Helm in dry-run mode, reports preflight effects, and skips reconciliation polling.

## Manual Inspection

```bash
kubectl get nicclusterpolicy -o yaml
kubectl get nicnodepolicy -o yaml
kubectl get pods -n nvidia-network-operator -o wide
```

Finish every applied deployment with [Validation](../user/validation.md). Validation is a separate acceptance stage and produces the green-light report.
