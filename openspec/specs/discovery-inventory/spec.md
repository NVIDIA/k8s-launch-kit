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

#### Scenario: Restricted BlueField alongside a usable NIC

- **WHEN** bootstrap discovery verifies host-restricted BlueField PCI functions, including on a node with usable NICs
- **THEN** normal CLI output identifies their node and PCI addresses and reports a PCI-function count before namespace teardown; those devices remain excluded from saved inventory.
- **AND** a later PCI scan failure retains an already-verified publisher and warns that exclusion reporting is incomplete; read-only eligibility probes retain their existing failure policy.

#### Scenario: Read-only publisher eligibility is unknown by default

- **WHEN** a Ready existing-daemon node has no `NicDevice` and discovery cannot inspect its NIC eligibility
- **THEN** read-only discovery returns an inspection error instead of guessing from optional labels or returning an incomplete inventory.

#### Scenario: Read-only caller explicitly excludes an unverified publisher

- **WHEN** a Ready existing-daemon node has no `NicDevice`, no REST config is supplied, and the caller opts in to excluding unverified publishers
- **THEN** read-only discovery excludes that node and warns that the returned inventory may be incomplete; errors from an attempted probe remain errors.

#### Scenario: Read-only publisher is verified absent

- **WHEN** the existing daemon probe confirms that a node has no discoverable NIC or only restricted BlueFields
- **THEN** read-only discovery excludes that node from the required publisher set; probe and permission errors remain errors.

#### Scenario: Platform networking inspection unavailable

- **WHEN** optional API installer inspection is unavailable or ambiguous
- **THEN** fresh and refreshed discovery save hardware inventory and separate networking observations without changing desired component ownership; automatic generation rejects unresolved ownership evidence.

### Requirement: Aggregation and provenance

A group property MUST declare its aggregation policy for disagreement and incomplete membership. Live/preset precedence and deviation handling SHALL preserve the information required by consumers. Host-specific observations MUST NOT be persisted as universal group facts without a justified aggregation rule.

#### Scenario: Workers disagree

- **WHEN** eligible workers in one group report different capabilities
- **THEN** the declared aggregation rule determines the group result and preserves actionable disagreement diagnostics.

#### Scenario: Preset mismatch

- **WHEN** live topology deviates from a candidate preset
- **THEN** the matching/deviation path retains the applicable live topology rather than asserting an exact match.

#### Scenario: Dual-port BlueField-3 in NIC mode

- **WHEN** a BlueField-3 reports a VPD model with two physical ports, even with `status.dpu=false` or an unlisted SKU
- **THEN** live discovery classifies its PFs as north-south, preserves that classification during GPU PIX probing, and assigns no east-west rails; a group containing only north-south NICs is omitted.

#### Scenario: VR NetworkBay rail membership

- **WHEN** normal or read-only discovery resolves a VR group
- **THEN** each worker must report eight east-west ASIC NICs forming four complete reciprocal NetworkBay pairs, each with ASIC 1 and ASIC 0; NetworkBays ordered by ASIC 1 PCI address form rails by crossing opposite ASICs between NetworkBays 1/2 and 3/4, after GPU/preset enrichment, without changing PF order.
- **AND** missing, invalid or incomplete status, or differing worker mappings, stops discovery before saving inventory rather than falling back to sequential rails.

#### Scenario: Explicit read-only fabric enrichment

- **WHEN** a read-only caller enables fabric discovery with REST credentials
- **THEN** only existing daemon pods are executed, and every selected worker, raw PF represented by a retained rail, and completely enumerated port must report a unanimous configured link layer before group fabric is published.
- **AND** missing, partial, denied or conflicting optional observations remain unknown with diagnostics; caller cancellation remains fatal, and existing no-option/REST-only calls acquire no enrichment execs.

### Requirement: Truthful worker selectors

Discovery SHALL validate proposed identities and membership before node-label writes
and publish a selector only after successful writes and refreshed exact-set verification.
It SHALL reject collisions, failed label writes and stale labels that broaden selection,
without automatically deleting labels from excluded nodes. An unresolved sole
source SHALL preserve its existing empty identifier when an exact selector is
provable; unresolved product identity alone SHALL NOT prevent publication.

#### Scenario: Label publication fails

- **WHEN** a patch fails or refreshed labels select another worker
- **THEN** discovery fails without publishing the planned selector; completed patches may remain for a retry to revalidate.

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
