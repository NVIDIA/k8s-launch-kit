<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

<p class="page-kicker">Overview</p>

# NVIDIA Kubernetes Launch Kit

NVIDIA Kubernetes Launch Kit (`l8k`) helps platform teams configure NVIDIA networking for accelerated Kubernetes clusters. It discovers NIC and GPU topology, generates reviewable Network Operator and NIC Configuration Operator resources, applies them in dependency order, and checks the deployed result.

Use this site when you are deploying SR-IOV, RDMA shared-device, host-device, InfiniBand, or Spectrum-X networking. Start with [Plan your deployment](user/profiles.md) to choose a platform and profile, check what is qualified, and identify the site inputs and permissions you need.

The lifecycle commands operate on the `host` target by default. The reserved
`dpf` target has no available lifecycle phases; see the
[target extension contract](advanced/targets.md) if you are extending Launch Kit.

## Find Your Path

| If you are a... | Start here |
| --- | --- |
| Operator preparing a deployment | [Plan your deployment](user/profiles.md) |
| Operator deploying SR-IOV Ethernet on Kubernetes | [Quick Start](user/quick-start.md) |
| OpenShift administrator | [Deploy on OpenShift](user/openshift.md) |
| Operator inventorying a cluster | [Cluster Discovery](user/discovery.md) |
| Platform engineer selecting a topology profile | [Deployment Profiles](user/profiles.md) |
| Platform engineer managing mixed hardware | [Heterogeneous Clusters](user/heterogeneous-clusters.md) |
| Spectrum-X operator | [Spectrum-X](user/spectrum-x.md) |
| CI/CD or GitOps integrator | [Automation](integrator/automation.md) |
| Integrator adding an infrastructure target | [Target-aware CLI](advanced/targets.md) |
| AI agent integrator | [AI Skills](integrator/ai-skills.md) |
| Operator confirming a deployment is ready for use | [Validation](user/validation.md) |
| Application owner connecting a workload | [Use the network in an application](user/workloads.md) |
| Operator changing an existing deployment | [Change and upgrade](user/maintenance.md) |
| Operator removing a deployment | [Cleanup](user/cleanup.md) |
| Operator investigating a failed stage | [Troubleshooting](user/troubleshooting.md) |

## Workflow

<div class="workflow-diagram" role="img" aria-label="Launch Kit workflow: discover hardware, generate manifests, deploy resources, then validate the deployment">
  <div class="workflow-step">
    <span class="workflow-step__command">Discover</span>
    <span class="workflow-step__description">Hardware inventory</span>
  </div>
  <span class="workflow-arrow" aria-hidden="true">→</span>
  <div class="workflow-step">
    <span class="workflow-step__command">Generate</span>
    <span class="workflow-step__description">Manifests and values</span>
  </div>
  <span class="workflow-arrow" aria-hidden="true">→</span>
  <div class="workflow-step">
    <span class="workflow-step__command">Deploy</span>
    <span class="workflow-step__description">Ordered application</span>
  </div>
  <span class="workflow-arrow" aria-hidden="true">→</span>
  <div class="workflow-step">
    <span class="workflow-step__command">Validate</span>
    <span class="workflow-step__description">Acceptance report</span>
  </div>
</div>

Each stage is independently invocable:

- `l8k discover` bootstraps a private NIC discovery daemon and writes `cluster-config.yaml`.
- `l8k generate` renders a profile-specific manifest bundle under `deployment/network-operator/`.
- `l8k deploy` installs or upgrades the Network Operator Helm chart, applies CRs in dependency order, and waits for reconciliation.
- `l8k validate` runs deployment checks and produces a report. Review its completed checks and connectivity coverage before acceptance; [exit success alone is insufficient](user/validation.md#acceptance-outcomes).
- `l8k clean` removes Network Operator custom resources and uninstalls its Helm release unless config marks the chart externally owned.

Review discovered inventory, site networking settings, and generated artifacts
before deploying. [Quick Start](user/quick-start.md) shows that checkpoint in
one Kubernetes scenario. A successful validation exit alone does not demonstrate
complete data-plane coverage; use the [acceptance criteria](user/validation.md#acceptance-outcomes).

## Links

- [GitHub repository](https://github.com/NVIDIA/k8s-launch-kit)
- [Releases](https://github.com/NVIDIA/k8s-launch-kit/releases)
- [CLI reference](reference/cli.md)
- [Configuration reference](reference/configuration.md)
