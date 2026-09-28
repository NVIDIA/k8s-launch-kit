<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start

This walkthrough prepares a **Kubernetes SR-IOV Ethernet** deployment for two suitable workers. It discovers NICs, renders a reviewable bundle, previews changes, deploys, and checks the result. It is a procedure template: use the Network Operator release and network values approved for your cluster. The commands below use release `26.4` as a documented example; confirm it is appropriate for your hardware and installed `l8k` binary.

A two-worker example allows a connectivity check between nodes. It does not establish production qualification or make the built-in addressing and maintenance defaults suitable for your site. Complete [deployment planning](profiles.md) before starting. OpenShift and Spectrum-X have [separate](openshift.md) [procedures](spectrum-x.md).

## Before you start

- Install `l8k`; point `$KUBECONFIG` at the intended cluster and confirm the current context.
- Confirm two Ready, schedulable workers with discoverable NVIDIA NICs, image access for the temporary privileged discovery daemon, and permission to create its bootstrap resources and node labels.
- Agree with the fabric owner on selected east-west NICs, VLAN/rail and routing setup, an unused subnet and gateway, MTU, and VF capacity. Confirm the range does not overlap pod, service, management, storage, or external networks.
- Decide who owns the Network Operator Helm release. This example uses `l8k deploy`; for external ownership, follow [Plan your deployment](profiles.md#choose-a-platform-and-operating-model).
- Review running storage/RDMA workloads before allowing DOCA driver module unloads, and select a maintenance budget appropriate to the cluster. The default `maxUnavailable: 4` is not automatically appropriate for two workers.

```bash
kubectl config current-context
kubectl get nodes -o wide
l8k version
l8k schema
```

These commands inspect the environment; the next step creates temporary cluster resources and writes node labels. If the context, eligible workers, permissions, or fabric information is unclear, resolve that before discovery.

## 1. Discover

```bash
l8k discover \
  --kubeconfig "$KUBECONFIG" \
  --network-operator-release 26.4 \
  --save-cluster-config ./cluster-config.yaml
```

Discovery bootstraps a private NIC Configuration Daemon in `nvidia-k8s-launch-kit`, creates missing CRDs it needs, reads `NicDevice` state, and removes the temporary namespace when finished. A pre-installed Network Operator is not required. The saved file contains hardware groups, profile settings, and editable configuration.

**Review before generation:** open `cluster-config.yaml` and confirm each intended worker, east-west PF, rail, selected profile, Network Operator release, network namespace, and driver settings. Check `nvIpam` addressing against the site's approved range and `maintenance` limits against its disruption budget. Edit the source file to set site-owned values; discovery cannot establish that its sample/default address range is unused elsewhere. The `--node-selector` value controls later deployment selection, not where the discovery daemon runs. See [Discovery](discovery.md) if the inventory is incomplete or a node was excluded.

## 2. Generate

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./deployment
```

Generation leaves `cluster-config.yaml` unchanged. It writes manifests under `deployment/network-operator/` and the exact effective configuration at `deployment/.l8k/resolved-config.yaml`. Keep both directories together. The following deploy and validate commands automatically use the sidecar, including generation-time CLI overrides.

**Review before apply:** inspect the sidecar and all generated resources. Confirm the worker selectors and PCI addresses, `IPPool` subnet/gateway and exclusions, VF count and resource names, workload namespaces, DOCA driver and module-unload settings, maintenance settings, Helm values, and any resource identities that differ from an existing installation. [Generation](../advanced/generation.md#bundle-layout) explains the files; [Configuration](../reference/configuration.md) explains how values were resolved. Compare the bundle with the site's intended state, including any other hardware cohorts, before continuing.

## 3. Deploy

Preview server-side validation, Helm behavior, and preflight findings before application:

```bash
l8k deploy \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG" \
  --dry-run
```

Dry run does not persist the proposed resources or poll their reconciliation. It cannot establish that controllers will become ready or that workload traffic will pass. Review any Helm/values conflict or stray-resource inventory; `--overwrite-existing` can delete manual resources and resources from another cohort. See [deployment preflight and scope](../advanced/deployment.md#preflight) before deciding how to resolve a conflict.

After that review, apply the same bundle:

```bash
l8k deploy \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

Launch Kit installs or verifies the Network Operator Helm release, applies `NicClusterPolicy` and `NicNodePolicy` in order, applies the remaining custom resources, and waits for observed reconciliation. If chart authentication uses `networkOperator.imagePullSecrets`, create the named Secret in the configured Network Operator namespace first; phase 0 also needs permission to read it. Confirm the intended resources reached `READY` and that required component pods and SR-IOV node resources exist. A preflight conflict or reconciliation failure needs investigation; [Deployment](../advanced/deployment.md) and [Troubleshooting](troubleshooting.md) give the next checks.

## 4. Validate

```bash
l8k validate \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG" \
  --wait 10m
```

Open `deployment/network-operator/k8s-launch-kit-validation-report.html`. For this two-worker example, verify that both intended workers contributed usable test pods, the intended manifests are ready, the required static checks completed, and every selected data-plane check family has gating tests on the expected rails. Review skipped checks, missing endpoints, and non-gating cross-rail observations. A Kubernetes full-validation process can exit `0` while resources are in progress or connectivity is skipped; the wait deadline alone does not change that rule. Apply the [acceptance outcomes](validation.md#acceptance-outcomes) to the report before declaring the deployment ready.

If the report is incomplete or failed, retain it and follow [Troubleshooting](troubleshooting.md). After network acceptance, use the generated network in the intended workload or [plan a change](maintenance.md).

## Common Variants

- [Topology presets](presets.md) allow generation from known hardware without live discovery.
- [Automation](../integrator/automation.md) explains output capture, GitOps artifact handling, and release pinning.
- [Mixed hardware](heterogeneous-clusters.md) explains group selection and resource scope.
- [OpenShift](openshift.md) uses externally installed operators and different namespaces/permissions.
