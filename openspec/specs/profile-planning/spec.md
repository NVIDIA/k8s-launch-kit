<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Profile selection and resource planning

## Purpose

Integrate a profile, template, grouping rule, identifier or topology allocation. Inputs are effective configuration and normalized inventory; outputs are a resource plan and rendered artifacts for exactly the selected groups.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Declare fabric/flavor/hardware/release eligibility, feature combinations and template set.
- Define render multiplicity independently of Kubernetes API scope: singleton, bucket, source, worker or network namespace.
- Define stable names, selectors, references, topology allocation, native Helm/API types and validation workload capabilities.

## Requirements

### Requirement: Eligibility and exact selection

A profile extension MUST define eligibility and preserve exact selected-group scope through rendering. Shared-network merging SHALL NOT broaden machine-specific PCI/device selection or introduce excluded groups.

#### Scenario: Mixed group subset

- **WHEN** only a subset of heterogeneous source groups is selected
- **THEN** rendered selectors, hardware settings and references address that subset without a cross-product of machine-specific devices.

#### Scenario: Unsupported feature combination

- **WHEN** the requested profile combination is outside declared eligibility
- **THEN** selection rejects it or follows an explicitly documented selection policy.

### Requirement: Multiplicity and unique identities

A template extension MUST declare render multiplicity, API scope and reference namespaces separately. New or changed identity rules SHALL detect collisions before output-map assignment can overwrite an artifact. Stable inputs SHALL produce stable identities.

#### Scenario: Two sources collide

- **WHEN** two selected sources would produce the same output path or resource identity
- **THEN** the changed planning path reports the collision before discarding either artifact.

#### Scenario: Namespace fan-out

- **WHEN** a template is rendered once per network namespace
- **THEN** its API scope and references are derived explicitly; a per-namespace render is not assumed to imply a namespaced Kind.

### Requirement: Consumer compatible payloads

Profiles SHALL emit native chart/API value shapes and declare the tools, images, devices and resources required by their advertised validation checks. Extensions MUST verify these prerequisites across applicable profile branches.

#### Scenario: Ambiguous scalar

- **WHEN** a chart expects a string whose text resembles a number or boolean
- **THEN** rendered YAML preserves the required string type.

#### Scenario: New check workload

- **WHEN** a profile advertises a check needing a tool or device
- **THEN** its workload provides the declared prerequisite or explicitly reports that check unsupported.

### Requirement: Opt-in standard NIC configuration

Standard SR-IOV, RDMA-shared and host-device profiles SHALL support a default-disabled NIC configuration template option on Kubernetes and OpenShift. Enabled templates SHALL target each selected source group's east-west NIC type and PCI addresses, route VF count from SR-IOV settings and link type from the resolved fabric, and configure PCI and Baremetal GPUDirect optimizations. Ethernet SHALL additionally configure DSCP trust and priority-3 PFC; InfiniBand SHALL omit RoCE settings. Only SR-IOV profiles SHALL disable the Mellanox plugin, preserving unrelated OCP disabled plugins. Spectrum-X SHALL reject the new option.

#### Scenario: Default or explicit opt-out

- **WHEN** the option is omitted or explicitly false
- **THEN** no standard NIC configuration template or new plugin-disable request is rendered.

#### Scenario: Standard profile selection

- **WHEN** the option is enabled for a supported standard profile and selected hardware groups
- **THEN** NCO is enabled independently of naming and templates retain exact per-source selectors, fabric-appropriate settings and configured VF count.

#### Scenario: Operator configuration ownership

- **WHEN** enabled SR-IOV configuration is applied to OpenShift with other plugins already disabled
- **THEN** mellanox is added without removing the existing disabled plugins; RDMA-shared and host-device profiles do not request this operator setting.

#### Scenario: Spectrum-X conflict

- **WHEN** the new option is enabled with Spectrum-X
- **THEN** configuration is rejected without changing Spectrum-X templates.

## Baseline and limits

Current profile lookup is first-match; this spec does not introduce universal ambiguity rejection. Existing render maps do not universally detect pre-overwrite collisions: the collision requirement is an extension obligation, with legacy coverage to be assessed when touched. `ScopeClusterWide` may render a namespaced ConfigMap, while SCCs may render per workload namespace. [Resource kinds](../resource-kinds/spec.md) owns API/readiness integration; [artifacts](../artifact-handoff/spec.md) owns post-render structural validation.

## Integration and evidence

- Implementation: [profile selection](../../../pkg/profiles), [profile metadata/templates](../../../profiles), [scope registry](../../../pkg/networkoperatorplugin/scopes.go), [render plan](../../../pkg/networkoperatorplugin/render_plan.go), [templates](../../../pkg/networkoperatorplugin/templates.go).
- Existing suite: [template generation](../../../pkg/networkoperatorplugin/templates_test.go); extend it with collision and full/subset integration cases, not just file counts.
- User documentation: [generation](../../../docs/advanced/generation.md), [heterogeneous clusters](../../../docs/user/heterogeneous-clusters.md).
