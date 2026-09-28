<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Connectivity check planning and execution

## Purpose

Add a protocol, test family, endpoint type, metric or routing measurement. Inputs are selected topology, workloads, mode and endpoints; outputs are per-check observations, expectations and diagnostics.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Define the measurement claim and natural path, selection/defaulting and platform/profile prerequisites.
- Map both endpoints independently: node, pod, namespace, IP, interface, RDMA device and GPU as applicable; define ambiguity/fallback handling.
- Declare deterministic quick/full/strict coverage, expectations, units/thresholds, setup/server/batching, timeout budget and cleanup resource inventory.

## Requirements

### Requirement: Endpoints and measurement path

A check extension MUST resolve endpoints from authoritative topology with an explicit missing/ambiguous mapping policy. Its runner SHALL measure the claimed path without forcing an artificial path that invalidates that claim. Required workload tools/devices MUST be declared through profile/platform integration.

#### Scenario: Ambiguous rail identity

- **WHEN** multiple interfaces or devices could represent an endpoint
- **THEN** the check follows its declared ambiguity policy and does not guess a GPU/rail association.

#### Scenario: ICMP natural path

- **WHEN** ICMP qualifies a source route for the intended interface
- **THEN** ping binds the intended source IP while route qualification verifies the natural path; forcing a different device is not substituted as proof.

### Requirement: Expectations and measurement failure

New or changed checks MUST define required, observation-only and forbidden outcomes separately from the ability to perform a measurement. Required checks SHALL need a valid positive observation; forbidden checks SHALL need a valid negative observation. Setup/tool/transport failure MUST NOT become forbidden-check success in a new or changed classifier.

#### Scenario: Forbidden negative observation

- **WHEN** a valid measurement observes the prohibited path unavailable
- **THEN** the forbidden expectation succeeds and retains the negative evidence.

#### Scenario: Measurement cannot run

- **WHEN** the tool is missing or execution cannot perform the measurement
- **THEN** the new/changed classifier reports measurement failure, not successful isolation.

#### Scenario: Observation-only result

- **WHEN** a valid observation disagrees with an observation-only expectation
- **THEN** the result remains visible without independently becoming a required failure.

### Requirement: Execution budget and evidence

A check extension MUST declare units, thresholds, server/client lifetime, cancellation, batching and timeout contribution. Per-check evidence SHALL retain enough endpoint, expectation and diagnostic context to interpret its result and supply resource ownership to cleanup.

#### Scenario: Timeout or cancellation

- **WHEN** a check exceeds its budget or receives cancellation
- **THEN** execution terminates according to the declared policy, reports diagnostics and invokes the declared server/resource cleanup.

#### Scenario: Mode changes coverage

- **WHEN** the caller switches quick/full/strict mode
- **THEN** the planner deterministically applies documented endpoint coverage and expectation rules.

## Baseline and limits

The current seams are enums, functions and branches, not a plugin registry. ICMP cross-rail checks are observation-only in quick/full; strict source-based routing requires connectivity and strict destination-based routing forbids it. Generic forbidden-result finalization currently negates observed success and can clear execution errors; ICMP route errors have special handling. Existing planners may omit missing interface/RDMA mappings. The stronger classification/coverage decisions above require evidence when those paths change. [Outcomes](../outcomes-evidence/spec.md) owns aggregate acceptance, [profiles](../profile-planning/spec.md) workload capabilities and [effects](../managed-effects/spec.md) teardown policy.

## Integration and evidence

- Implementation: [connectivity package](../../../pkg/networkoperatorplugin/connectivity), especially `matrix.go`, `source.go` and `timeout.go`.
- Existing suites: [matrix](../../../pkg/networkoperatorplugin/connectivity/matrix_test.go), [source](../../../pkg/networkoperatorplugin/connectivity/source_test.go), [ICMP](../../../pkg/networkoperatorplugin/connectivity/icmp_test.go), [RDMA](../../../pkg/networkoperatorplugin/connectivity/rdma_test.go), [timeouts](../../../pkg/networkoperatorplugin/connectivity/timeout_test.go).
- User documentation: [validation](../../../docs/user/validation.md).
