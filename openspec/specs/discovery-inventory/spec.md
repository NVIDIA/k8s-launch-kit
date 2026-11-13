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

#### Scenario: Inventory-only read-only discovery cannot derive a safe selector

- **WHEN** a caller opts in to allow missing read-only node selectors and a group's workers cannot be selected without matching nodes outside the group
- **THEN** discovery returns the inventory with that group's `NodeSelector` empty, warns that `WorkerNodes` is authoritative, and does not relax errors for missing or unverified `NicDevice` resources.
- **AND** without the opt-in, discovery continues to return an error for that group.

#### Scenario: Platform networking inspection unavailable

- **WHEN** optional API installer inspection is unavailable or ambiguous
- **THEN** fresh and refreshed discovery save hardware inventory and separate networking observations without changing desired component ownership; automatic generation rejects unresolved ownership evidence.

### Requirement: Aggregation and provenance

A group property MUST declare its aggregation policy for disagreement and incomplete membership. Live/preset precedence and deviation handling SHALL preserve the information required by consumers. Host-specific observations MUST NOT be persisted as universal group facts without a justified aggregation rule.

#### Scenario: Workers disagree

- **WHEN** eligible workers in one group report different capabilities
- **THEN** the declared aggregation rule determines the group result and preserves actionable disagreement diagnostics.

#### Scenario: Netplan observation is incomplete on a worker

- **WHEN** discovery finds no netplan naming match but cannot inspect the netplan
  directory, parse a worker's observation, or obtain current PF MACs while
  checking a MAC-based `set-name` rule
- **THEN** it records `netplanManagementUnknown` and warns; generation of a
  `NicInterfaceNameTemplate` for that group warns and continues without treating
  uncertainty as a confirmed conflict.
- **AND** a positive match on any worker remains `netplanManaged` and blocks
  generation of an interface-name template, including when both flags are set.

#### Scenario: Netplan naming rules are conclusively absent

- **WHEN** host `/etc` is readable but has no netplan directory, or a complete
  readable configuration contains no MAC-based `set-name` rules
- **THEN** discovery records a verified negative without requiring PF MACs and
  does not set `netplanManagementUnknown`.

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

#### Scenario: GPU observations with containerized drivers

- **WHEN** the NIC daemon cannot provide complete GPU observations
- **THEN** discovery may use the Ready main container of an existing same-node GPU Operator feature-discovery DaemonSet after verifying ownership, host sysfs backing and source lifetime, without creating or modifying GPU workloads.
- **AND** physical host GPU PCI inventory, query indices and matrix coverage must agree before topology is accepted; absent labels do not establish CPU-only hardware, and unavailable optional evidence is diagnosed explicitly.

#### Scenario: GPU observations represent the whole retained PF

- **WHEN** GPU topology is attached to a group PF
- **THEN** every worker and raw PCI represented by its actual collapse projection must agree on the GPU tuple; node-local RDMA names remain transient.
- **AND** collapsed siblings must share a nearest GPU at a common proximity before balancing once per retained rail, and per-node product fallback must be aggregated without replacing known worker disagreement with a representative product.
- **AND** incomplete relevant PF coverage cannot trigger PIX-based traffic reassignment, preset precedence remains authoritative, measured GPU PCI addresses remain consistent with the final GPU index, and VR NetworkBay rails are assigned last.

#### Scenario: Explicit read-only GPU enrichment

- **WHEN** a caller enables GPU topology with REST credentials and a GPU Operator namespace
- **THEN** only existing pods are executed, no resources or host settings are changed, and netplan probing is not implicitly enabled.
- **AND** default and REST-only callers acquire no GPU probes; optional failures use the context UI warning sink, while caller cancellation remains fatal.

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

### Requirement: Reasoned DPF observations

New and changed discovery checks SHALL retain a stable reason code and safe human-readable explanation for failed, unavailable, unknown, skipped and deferred outcomes. Where known, the observation SHALL identify its subject, vantage, tool or resource, and provenance. Missing tools, denied access, malformed output, timeout, absent properties, unsupported capabilities and conflicts MUST NOT collapse into an unexplained not-measured result. Optional failures SHALL remain separate from required physical identity and association failures.

#### Scenario: Firmware tool is absent

- **WHEN** an SSH endpoint lacks an executable needed for an optional firmware observation
- **THEN** the saved inventory identifies the missing tool and reason, without installing it or discarding successfully collected identity.

#### Scenario: Restricted device rejects a query

- **WHEN** an installed tool cannot read a restricted device
- **THEN** the collector retains an access/query failure rather than claiming the tool is missing or the hardware absent, without changing privilege settings.

### Requirement: DPF locator candidates are not identity

Address lookup SHALL operate only on explicit MAC selectors and bounded declared sources. It SHALL preserve expired leases, ambiguous matches and unavailable sources as reasoned outcomes. Discovered addresses MUST NOT implicitly receive credentials, become selected endpoints or establish physical identity.

#### Scenario: Sources disagree

- **WHEN** usable sources associate one requested MAC with different addresses
- **THEN** all candidates retain source provenance and the result is ambiguous; discovery does not choose or connect to one.

#### Scenario: Lease source cannot be parsed

- **WHEN** a configured lease file is absent, unreadable, oversized or malformed
- **THEN** its unavailable result retains a safe reason and it contributes no candidates; other source evidence remains separately visible.

### Requirement: DPU BMC administrator evidence

DPF discovery SHALL require read-only evidence that each discovered DPU BMC's
authenticated local account has the full standard Administrator privilege set.
It SHALL bind the session username to its advertised own account and linked role,
or identify one matching account from a complete bounded collection,
verify enabled/unlocked/no-password-change state, and reject missing, denied,
malformed, unsafe or inconsistent evidence. Custom roles with the complete standard
privilege set SHALL be accepted; username, role name and OEM privilege names alone
MUST NOT substitute. Host-only and ARM-only observations SHALL remain partial.

#### Scenario: Administrator authorization cannot be established

- **WHEN** identity reads succeed but account or effective-role evidence is missing, denied, nonadministrator, restricted or requires a password change
- **THEN** discovery retains physical identity evidence with a specific safe administrator failure reason and fails its required discovery check without publishing a complete handoff.
- **AND** discovery neither changes passwords/roles nor tests authorization by mutation; only its normal temporary session creation and cleanup are permitted writes.

#### Scenario: BF4 B4240 firmware exposes different identity graphs

- **WHEN** the service advertises NVIDIA and the exact BF4/B4240 product
- **THEN** discovery accepts an NVIDIA B4240 physical Card with a matching System serial, or a serial-bearing B4240 BMC Component reciprocally contained by a physical Card and linked by that System.
- **AND** missing System properties remain absent; conflicting properties, unsafe links, multiple matching candidates or a changed identity graph prevent complete inventory. Free-text descriptions and fuzzy model names do not supply identity.

## Baseline and limits

There is no universal probe registry or typed provenance/unknown framework. Existing trust queries intentionally fall back to unknown. Discovery mutates its supplied config and cluster labels; setup/teardown belongs to [managed effects](../managed-effects/spec.md). Rendering consumes these observations under [profile planning](../profile-planning/spec.md).

## Integration and evidence

- Implementation: [discovery](../../../pkg/networkoperatorplugin/discovery/discover.go), [GPU topology](../../../pkg/networkoperatorplugin/discovery/gputopology.go), [presets](../../../pkg/presets), [application integration](../../../pkg/app/discover.go).
- Existing suites: [discovery](../../../pkg/networkoperatorplugin/discovery/discover_test.go), [topology](../../../pkg/networkoperatorplugin/discovery/gputopology_test.go), [library](../../../pkg/networkoperatorplugin/discovery/library_test.go).
- User documentation: [discovery](../../../docs/user/discovery.md), [presets](../../../docs/user/presets.md).
