<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Targets and platform adaptations

## Purpose

Add a target, Kubernetes distribution/flavor or supported phase/profile combination with an explicit capability and adaptation boundary. Inputs are selection, prerequisites and platform policy; outputs are supported lifecycle behavior and a scoped qualification claim.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Decide target versus Host flavor; define defaults and supported phases/profiles.
- Declare APIs/operators/versions, namespaces, selectors, external ownership and admission/security requirements.
- For discovery, generation, deploy, validate and clean, state reuse, adaptation or explicit unavailability.

## Requirements

### Requirement: Selection and capabilities

A platform extension MUST expose its supported capability matrix, preserve existing default selection and reject unavailable paths without silently substituting another target/flavor.

#### Scenario: Unavailable lifecycle

- **WHEN** a caller selects a reserved target with an unavailable phase
- **THEN** execution rejects that combination before fallback infrastructure is invoked.

#### Scenario: Existing invocation

- **WHEN** an existing caller omits platform selection
- **THEN** its documented default remains compatible.

### Requirement: Platform policy across phases

Platform adaptations SHALL preserve explicit supported user values and use durable operator configuration mechanisms. Extensions MUST define missing-prerequisite handling, external ownership and admission requirements across every advertised lifecycle phase, including cleanup.

#### Scenario: Explicit namespace

- **WHEN** a supported explicit namespace differs from the platform default
- **THEN** the applicable lifecycle consumers use that value instead of independently resetting it.

#### Scenario: Externally managed operator

- **WHEN** the selected platform requires an externally installed operator
- **THEN** deployment honors that installation boundary and reports missing prerequisites rather than claiming it installed the operator.

#### Scenario: OpenShift interface tuning

- **WHEN** `ignoreARP` is enabled for a supported OpenShift secondary network
- **THEN** generation emits only interface-scoped ARP and reverse-path sysctls in its tuning meta-plugin, retains source-based routing order when selected, and documents the Multus allowlist prerequisite.

### Requirement: Qualification claims

Support claims MUST identify the platform, profile, hardware and phases actually verified. Rendering, simulated tests, server dry-run and live validation SHALL be distinguished.

#### Scenario: One profile qualified

- **WHEN** a live test succeeds for one platform/profile/hardware combination
- **THEN** documentation records that combination without extrapolating live qualification to other profiles.

## Baseline and limits

`host` and reserved `dpf` are targets; `k8s` and `ocp` are Host flavors. No general Provider interface is assumed. OpenShift live qualification is limited to its documented matrix. This contract supplies policy to [workflow execution](../workflow-execution/spec.md), [profile planning](../profile-planning/spec.md) and [managed effects](../managed-effects/spec.md).

## Integration and evidence

- Implementation: [flavor](../../../pkg/config/flavor.go), [targets](../../../pkg/target), [profiles](../../../pkg/profiles), [OCP configuration](../../../pkg/networkoperatorplugin/ocp_config.go).
- Existing suites: [Host driver](../../../pkg/target/host/driver_test.go), [OCP config](../../../pkg/networkoperatorplugin/ocp_config_test.go), [OCP validation](../../../pkg/networkoperatorplugin/ocp_validate_test.go).
- User documentation: [OpenShift prerequisites and qualification](../../../docs/user/openshift.md), [targets](../../../docs/advanced/targets.md).
