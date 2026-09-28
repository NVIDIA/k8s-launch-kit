<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Resource-kind integration and readiness

## Purpose

Add a Kubernetes Kind/GVK, served version, readiness strategy or companion relationship. Inputs are desired resources and fresh cluster observations; outputs are classified state with evidence and dependency placement.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Declare GVK, API scope, profiles/platforms/releases, prerequisites and apply/verification order.
- Choose own-object status, companion evidence, explicit platform adapter or justified existence-only validation.
- Define how evidence corresponds to desired payload/selectors/devices, and classify missing, progressing, success, retryable and terminal states.

## Requirements

### Requirement: Explicit Kind integration

A new Kind integration MUST declare its API/scope and readiness strategy, including a rationale for existence-only behavior. It SHALL integrate with render planning and deployment dependencies without relying accidentally on an unknown-Kind fallback.

#### Scenario: Existence-only resource

- **WHEN** a new resource has no meaningful readiness status
- **THEN** the integration explicitly documents why existence is sufficient and tests absence and presence.

#### Scenario: Unavailable API

- **WHEN** the required served version or CRD is unavailable
- **THEN** the integration reports the missing prerequisite rather than treating the resource as ready.

### Requirement: Authoritative and fresh evidence

New or changed readiness strategies MUST define evidence freshness for the current desired payload and selected devices. Transport or RBAC errors SHALL remain distinguishable from authoritative missing/progress/ready states in those strategies.

#### Scenario: Stale success

- **WHEN** an object retains successful status from before a desired selector/payload change
- **THEN** the strategy does not claim convergence until its declared freshness and coverage conditions hold.

#### Scenario: Partial device coverage

- **WHEN** only some intended devices have converged
- **THEN** a strategy requiring all selected devices reports incomplete progress, not success.

#### Scenario: Observation denied

- **WHEN** the API denies the observation
- **THEN** the changed strategy returns an inspection error rather than evidence of readiness.

### Requirement: Consistent lifecycle interpretation

Deploy and standalone validate SHALL share readiness meaning for equivalent evidence, with explicit adaptations for apply history and platform-specific paths. Extensions MUST test companion convergence, timeout and terminal failure as applicable.

#### Scenario: Same observed resource

- **WHEN** deploy and validate observe equivalent desired/live state
- **THEN** their classifiers agree, except for a documented freshness adaptation that depends on deployment history.

## Baseline and limits

Unregistered GVKs currently default to existence-only; some scope handling also has legacy fallback. OCP operator configuration has specialized validation outside the registry. `NeedsObservationGate` uses resource-version changes as a heuristic, not universal observed-generation proof; standalone validation lacks apply history. These are not upgraded by this documentation. [Managed effects](../managed-effects/spec.md) owns mutation/deletion policy, and [outcomes](../outcomes-evidence/spec.md) owns aggregate acceptance.

## Integration and evidence

- Implementation: [registry](../../../pkg/networkoperatorplugin/crstate/registry.go), [state](../../../pkg/networkoperatorplugin/crstate/state.go), [deployment](../../../pkg/networkoperatorplugin/deployment_bundle.go), [validation](../../../pkg/networkoperatorplugin/validate.go).
- Existing suites: [registry](../../../pkg/networkoperatorplugin/crstate/registry_test.go), [NIC configuration](../../../pkg/networkoperatorplugin/crstate/nicconfig_test.go), [Spectrum-X](../../../pkg/networkoperatorplugin/crstate/spectrumx_test.go), [deployment bundle](../../../pkg/networkoperatorplugin/deployment_bundle_test.go), [OCP validation](../../../pkg/networkoperatorplugin/ocp_validate_test.go).
