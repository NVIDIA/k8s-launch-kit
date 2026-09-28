<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Discovery observations and inventory

## Purpose

Integrate a probe, observation source, capability or topology property without confusing worker-specific observations with group-wide facts. Inputs are eligible workers/devices and live, label, inferred or preset observations; outputs are normalized inventory and diagnostics.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Specify scope and correlation keys: node, device/PF, rail or group; eligible publishers, exclusions, prerequisites and bounded wait set.
- Specify source precedence, required/optional status, unknown/error fallback and provenance diagnostics.
- Define any-worker, all-worker or representative aggregation, persistence lifetime, and fresh versus refresh consumers.

## Requirements

### Requirement: Observation eligibility and uncertainty

A discovery extension MUST identify eligible sources and distinguish a missing or failed observation from a known value. Any fallback SHALL be explicit, bounded by context/deadlines, and documented with its effect on downstream decisions.

#### Scenario: Optional observation unavailable

- **WHEN** an optional probe fails on an eligible worker
- **THEN** the declared unknown/fallback policy is applied with diagnostics; no unsupported capability is inferred as a measured fact.

#### Scenario: Excluded publisher

- **WHEN** a worker is outside the declared eligible set
- **THEN** discovery does not wait for that worker to publish the new observation.

### Requirement: Aggregation and provenance

A group property MUST declare its aggregation policy for disagreement and incomplete membership. Live/preset precedence and deviation handling SHALL preserve the information required by consumers. Host-specific observations MUST NOT be persisted as universal group facts without a justified aggregation rule.

#### Scenario: Workers disagree

- **WHEN** eligible workers in one group report different capabilities
- **THEN** the declared aggregation rule determines the group result and preserves actionable disagreement diagnostics.

#### Scenario: Preset mismatch

- **WHEN** live topology deviates from a candidate preset
- **THEN** the matching/deviation path retains the applicable live topology rather than asserting an exact match.

### Requirement: Fresh and refresh behavior

An inventory extension MUST cover fresh discovery and existing-config refresh. Refresh SHALL replace inventory and explicit CLI-targeted fields while preserving unrelated source fields, comments, omissions and explicit values.

#### Scenario: Repeated refresh

- **WHEN** an existing user config contains custom keys and explicit false values
- **THEN** refresh changes the intended inventory/override fields and preserves unrelated user content.

## Baseline and limits

There is no universal probe registry or typed provenance/unknown framework. Existing trust queries intentionally fall back to unknown. Discovery mutates its supplied config and cluster labels; setup/teardown belongs to [managed effects](../managed-effects/spec.md). Rendering consumes these observations under [profile planning](../profile-planning/spec.md).

## Integration and evidence

- Implementation: [discovery](../../../pkg/networkoperatorplugin/discovery/discover.go), [GPU topology](../../../pkg/networkoperatorplugin/discovery/gputopology.go), [presets](../../../pkg/presets), [application integration](../../../pkg/app/discover.go).
- Existing suites: [discovery](../../../pkg/networkoperatorplugin/discovery/discover_test.go), [topology](../../../pkg/networkoperatorplugin/discovery/gputopology_test.go), [library](../../../pkg/networkoperatorplugin/discovery/library_test.go).
- User documentation: [discovery](../../../docs/user/discovery.md), [presets](../../../docs/user/presets.md).
