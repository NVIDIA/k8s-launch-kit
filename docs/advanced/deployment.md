<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Deployment Mechanics

`l8k deploy` applies a generated bundle in dependency order and waits for controller reconciliation.

```bash
l8k deploy \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

If `<deployment-files>/network-operator/` exists, Launch Kit uses it automatically. Otherwise it reads YAML files directly from `<deployment-files>`.

Launch Kit loads and checks the complete flat artifact directory before Helm
installation, preflight remediation, or resource apply. Invalid YAML, missing
resource identity fields, and duplicate declared resources stop deployment
with the file and document number in the error. Only `values.yaml` is treated
as Helm values; rename `values.yml` or case variants to `values.yaml`. The
expected preflight inventory and applied objects come from that same snapshot.

## Helm Install Or Upgrade

When the bundle contains `values.yaml` and the selected release supplies a Helm repository URL, Launch Kit uses the Helm Go SDK to manage the `network-operator` release:

- No release: install the selected chart in `networkOperator.namespace`.
- Matching chart and values: no-op.
- Different chart or values: fail and show the conflict.
- Different chart or values with `--overwrite-existing`: run Helm upgrade.
- Release stuck in a pending Helm state: fail with rollback/uninstall guidance.

The chart is downloaded to a temporary directory. Launch Kit does not add or modify a local Helm repository.

If Helm metadata is absent, phase 0 is skipped so environments that manage the chart out of band can still apply the generated CRs.

Set `networkOperator.skipHelmChart: true` or pass
`--skip-network-operator-helm` to disable Helm management explicitly even if a
bundle still contains `values.yaml`. Launch Kit skips phase 0 and the Helm
chart-version/values preflight checks, but retains component-version and stray
resource preflight checks plus every manifest apply/reconciliation phase.

## Preflight

Before applying custom resources, Launch Kit compares the bundle with the cluster:

| Check | Detects |
| --- | --- |
| Helm chart version | Installed chart differs from the selected release. |
| Helm values | Installed user values differ from generated `values.yaml`. |
| Component versions | Live `NicClusterPolicy` component versions differ from the release catalog. |
| Stray resources | Instances of the enumerated networking CR kinds exist in the checked scope but are absent from the generated bundle, regardless of who created them. Recognized Spectrum-X controller children are excluded. |

The two Helm rows are reported as skipped when Network Operator Helm
management is disabled. The remaining rows still gate deployment.

Without `--overwrite-existing`, any mismatch stops deployment and all detected drift is reported together.

With `--overwrite-existing`, Launch Kit authorizes the Helm upgrade, deletes
every reported stray CR, and lets server-side apply converge policy fields.
Stray detection does **not** require an l8k ownership annotation. Manually
created resources and resources from another generated cohort can be deleted.

### Stray Resource Deletion Boundary

| Scope | Kinds checked |
| --- | --- |
| Cluster-wide | `NicClusterPolicy`, `NicNodePolicy`, `NicConfigurationTemplate`, `NicInterfaceNameTemplate` |
| Resolved Network Operator namespace | `SriovNetworkNodePolicy`, `SriovNetwork`, `SriovIBNetwork`, `SriovNetworkPoolConfig`, `OVSNetwork`, `IPPool`, `CIDRPool`, `MacvlanNetwork`, `IPoIBNetwork`, `HostDeviceNetwork`, `SpectrumXRailPoolConfig` (v1alpha1/v1alpha2) |

The expected bundle identities are retained. `NicDevice`,
`SriovNetworkNodeState`, and `SriovOperatorConfig` are outside this check.
Spectrum-X children have the exception described below. This boundary differs
from [clean](../user/cleanup.md), which performs broader removal.

Review the complete conflict list before approving overwrite, including
cluster-scoped resources. A bundle generated with `--groups` or `--gpu-type`
does not create an independent ownership boundary: resources from omitted
groups may be classified as strays. Use a combined intended inventory for
convergence, or apply an explicitly reviewed subset through a separately
managed deployment process without l8k stray remediation.

The Spectrum-X operator labels the SR-IOV pool configs, node policies, and OVS
networks it derives from `SpectrumXRailPoolConfig` with
`spectrumx.nvidia.com/owner-name`. Launch Kit leaves these controller-owned
objects out of stray detection and remediation. This makes a restarted
Spectrum-X deployment idempotent after the controller has created its child
resources, while unlabelled resources of the same kinds remain protected by
the normal conflict check.

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

For `NicConfigurationTemplate` and `NicFirmwareTemplate`, Launch Kit first waits for the operator to publish matched device names in `status.nicDevices` and for that name set to reflect the current `nodeSelector`, NIC type, PCI-address, serial-number, and part-number selectors. It then evaluates only those `NicDevice` objects and waits for the corresponding `spec.configuration` or `spec.firmware` field to reflect the current template payload. A successful device condition is accepted only after its `observedGeneration` catches up with the `NicDevice` generation. A configuration template checks `FirmwareUpdateInProgress` only when the matched device carries `spec.firmware`; without a deployed firmware template, a stale firmware condition from an older device generation does not block configuration reconciliation. Other discovered NICs do not block on configuration or firmware state. Changed templates are also observation-gated before this status is accepted, so status left by an earlier generation cannot produce a false success.

For `NicInterfaceNameTemplate`, `InterfaceNameMismatch` is retryable because the NIC configuration daemon can publish it while newly-written udev rules are still taking effect. Launch Kit starts a five-minute retry window when it first observes that mismatch. Initial device discovery and other ordinary in-progress states remain unbounded. Since phase-4 verification is ordered, the template gates later checks during this window. A persistent mismatch fails deployment after the local timeout and retains the per-node and per-port mismatch details.

## Timeout

The default deploy budget is unbounded because SR-IOV and driver reconciliation can exceed a small fixed timeout on large clusters. `NicInterfaceNameTemplate` is the only bounded exception: after the first `InterfaceNameMismatch`, its retry window is five minutes. A shorter deploy-wide timeout takes precedence.

Bound the entire Helm, apply, and reconciliation operation to a maintenance window:

```bash
l8k deploy \
  --deployment-files ./deployment \
  --deploy-timeout 90m
```

Other manifests have no independent per-manifest deadline.

## Server-Side Dry Run

```bash
l8k deploy \
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

Finish every applied deployment with [Validation](../user/validation.md). Review the report's readiness and connectivity coverage using the
[acceptance criteria](../user/validation.md#acceptance-outcomes); exit success
alone does not guarantee complete coverage.
