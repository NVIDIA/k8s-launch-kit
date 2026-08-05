---
name: k8s-launch-kit-deploy
version: 1.3.2
description: "Use this skill when the user wants to deploy generated NVIDIA networking manifests to a Kubernetes cluster using k8s-launch-kit (l8k). Activate for: applying manifests, deploying to cluster, the `l8k deploy` subcommand or the legacy --deploy flag on `l8k generate`, applying generated files, or any mention of pushing l8k output to a live cluster. Even if the user just says 'apply these' or 'push to cluster' after generating manifests, use this skill."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Deploy

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Apply previously generated NVIDIA networking manifests to a Kubernetes cluster.

## Usage

The standalone subcommand (preferred):

```bash
l8k deploy [--deployment-files <DIR>] [--kubeconfig <PATH>] [--dry-run]
```

`l8k deploy` reads YAML files from `--deployment-files` (default `./deployment`) and applies them in dependency order. It auto-prefers `<DIR>/network-operator/` (the layout `l8k generate` produces) and falls back to `<DIR>` itself.

When the deployment directory contains a `values.yaml` (the `l8k generate` profile renderer emits one per profile), Phase 0 runs first: the Helm Go SDK installs (or upgrades, with `--overwrite-existing`) the `nvidia/network-operator` chart in the namespace from `networkOperator.namespace`. The chart version and Helm repo URL come from the embedded release catalog selected via `--network-operator-release`. Phase 0 is skipped silently when `values.yaml` is absent — backward compatible with users managing the chart out of band.

When `networkOperator.imagePullSecrets` is non-empty, l8k reads the named
Docker Secrets from the operator namespace and uses compatible credentials to
authenticate both the Helm repository index and chart archive requests. The
Secret must exist before Phase 0, and the kubeconfig must allow `get secrets`.
l8k never logs or persists the credential. It sends credentials only to an
exact matching chart host, with one intentional NGC mapping from `nvcr.io` to
`helm.ngc.nvidia.com`; unrelated registry credentials are not forwarded.

Network Operator 26.1+ requestor mode is a Helm-level change: the generated
values add Network Operator Deployment environment variables and enable the
SR-IOV external drainer where applicable. Applying only the generated CRs
cannot enable requestor mode. When upgrading an existing release whose values
differ, pass `--overwrite-existing`; otherwise l8k intentionally stops at the
values conflict.

The legacy one-shot form (still supported, useful when you want to generate and apply in a single step):

```bash
l8k generate --user-config <CONFIG> --fabric <FABRIC> --deployment-type <TYPE> --deploy [--kubeconfig <PATH>]
```

## Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--deployment-files` | — | Directory with manifests to apply (default `./deployment`) |
| `--kubeconfig` | — | Path to kubeconfig with cluster-admin access (falls back to `$KUBECONFIG`) |
| `--dry-run` | — | Server-side dry-run (`client.DryRunAll`) — cluster validates without persisting |
| `--overwrite-existing` | — | Converge detected l8k-owned drift: upgrade a mismatched Helm release, delete conflicting generated-resource kinds, and rewrite owned policy fields. Spectrum-X operator-generated child resources are excluded from conflicts. |

## Examples

```bash
# Apply manifests from ./deployment to the cluster reachable via $KUBECONFIG
l8k deploy

# Apply from a specific directory with explicit kubeconfig
l8k deploy --deployment-files /tmp/my-output --kubeconfig ~/.kube/config

# Server-side dry-run before a production apply
l8k deploy --dry-run

# Apply newly generated requestor-mode values to an existing release
l8k deploy --deployment-files ./output --kubeconfig ~/.kube/config \
  --overwrite-existing

# Agent mode
l8k deploy --output json --yes 2>/dev/null

# Legacy single-shot: generate + deploy in one invocation
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output \
  --deploy --kubeconfig ~/.kube/config
```

## Resource Apply Order

l8k applies resources in dependency order:

1. **NicClusterPolicy** (cluster-wide: Multus, CNI, NV-IPAM, operators) — wait for ready before continuing
2. **NicNodePolicy** per group (OFED driver, device plugins) — wait for each
3. Network resources (SriovNetwork / HostDeviceNetwork / MacvlanNetwork / IPoIBNetwork)
4. **IPPool** (NV-IPAM address allocation)
5. **NicInterfaceNameTemplate** (when needed)
6. Example workload DaemonSets (optional)

## Post-Deploy Verification

During reconciliation, `NicConfigurationTemplate` and
`NicFirmwareTemplate` wait for the operator-populated `status.nicDevices`
list to reflect the current node, NIC type, PCI-address, serial-number, and
part-number selectors. l8k validates only those
named `NicDevice` objects and waits for their corresponding
`spec.configuration` or `spec.firmware` to reflect the current template
payload and for device conditions to observe the current device generation;
unrelated device configuration state does not block deployment. A
`NicConfigurationTemplate` gates on `FirmwareUpdateInProgress` only when the
matched device has `spec.firmware`; configuration-only deployments ignore a
stale firmware condition.

During preflight, do not classify `SriovNetworkPoolConfig`,
`SriovNetworkNodePolicy`, or `OVSNetwork` objects labeled with
`spectrumx.nvidia.com/owner-name` as strays. They are child resources generated
and reconciled by the Spectrum-X operator from `SpectrumXRailPoolConfig`, not
manifests owned by l8k. Unlabelled objects of the same kinds remain subject to
the normal conflict check.

```bash
kubectl get nicclusterpolicy -o yaml          # Check policy state
kubectl get nicnodepolicy                     # Per-group state
kubectl get pods -n <operator-ns>             # Verify all pods Running
kubectl get sriovnetworknodestates -A         # Check SR-IOV VF allocation
kubectl get maintenanceoperatorconfigs -A -o yaml # Check global concurrency
kubectl get nodemaintenances -A                # Check active requests
```

For SR-IOV on release 26.1+, verify that the generated Helm values contain both
`operator.maintenanceOperator.useDrainControllerRequestor: true` and
`sriov-network-operator.operator.externalDrainer.enabled: true`. For OFED,
verify `operator.maintenanceOperator.useRequestor: true`. Do not try to enable
these by applying `MaintenanceOperatorConfig` alone.

> [!CAUTION]
> This is a **write** command — confirm with the user before executing on production clusters.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and exit codes
- [k8s-launch-kit-dryrun](../k8s-launch-kit-dryrun/SKILL.md) — Preview before deploying
- [k8s-launch-kit-clean](../k8s-launch-kit-clean/SKILL.md) — Remove an installed deployment
- [k8s-launch-kit-troubleshoot](../k8s-launch-kit-troubleshoot/SKILL.md) — Debug deployment failures
