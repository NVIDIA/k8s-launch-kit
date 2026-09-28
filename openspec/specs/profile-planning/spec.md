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

## Baseline and limits

Current profile lookup is first-match; this spec does not introduce universal ambiguity rejection. Existing render maps do not universally detect pre-overwrite collisions: the collision requirement is an extension obligation, with legacy coverage to be assessed when touched. `ScopeClusterWide` may render a namespaced ConfigMap, while SCCs may render per workload namespace. [Resource kinds](../resource-kinds/spec.md) owns API/readiness integration; [artifacts](../artifact-handoff/spec.md) owns post-render structural validation.

## Integration and evidence

- Implementation: [profile selection](../../../pkg/profiles), [profile metadata/templates](../../../profiles), [scope registry](../../../pkg/networkoperatorplugin/scopes.go), [render plan](../../../pkg/networkoperatorplugin/render_plan.go), [templates](../../../pkg/networkoperatorplugin/templates.go).
- Existing suite: [template generation](../../../pkg/networkoperatorplugin/templates_test.go); extend it with collision and full/subset integration cases, not just file counts.
- User documentation: [generation](../../../docs/advanced/generation.md), [heterogeneous clusters](../../../docs/user/heterogeneous-clusters.md).
