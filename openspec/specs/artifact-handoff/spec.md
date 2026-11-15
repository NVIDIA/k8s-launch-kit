<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Desired artifacts and configuration handoff

## Purpose

Integrate a renderer, artifact role/format, bundle consumer, resolved metadata or directory API. Inputs are complete desired files and effective config; outputs are validated snapshots, isolated consumer views and persisted lifecycle handoff.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Declare format/role, identity, source file/document diagnostics and version compatibility.
- Identify the complete-set validation boundary and the effects it must precede.
- Specify raw-byte preservation, copy ownership and disk/memory equivalence; distinguish effective config metadata from bundle contents.

## Requirements

### Requirement: Validate desired state before dependent effects

Generation SHALL structurally validate the complete rendered artifact set before replacing output. Deploy SHALL structurally validate its desired inputs before Helm, remediation or apply. Artifact extensions MUST preserve deterministic loading and actionable source file/document diagnostics.

#### Scenario: Malformed final document

- **WHEN** the final document of a multi-document input is malformed
- **THEN** the operation reports its source and performs none of the dependent output replacement or deployment mutations.

#### Scenario: Duplicate supplied identity

- **WHEN** the supplied artifact set contains a duplicate resource identity
- **THEN** bundle construction rejects the duplicate instead of silently choosing one.

### Requirement: Retained snapshot and consumer isolation

Consumers SHALL reuse one retained desired snapshot per operation, preserve raw artifact bytes as supported by their role, and receive isolated mutable views. Directory wrappers SHALL share bundle semantics with in-memory callers; readiness polling SHALL refresh live state independently.

#### Scenario: Consumer mutates a copy

- **WHEN** a consumer changes its returned object
- **THEN** the retained desired snapshot and another consumer view remain unchanged.

#### Scenario: Cluster converges

- **WHEN** live state changes during polling
- **THEN** the next observation reads refreshed live state against the same desired snapshot.

### Requirement: Resolved configuration handoff

Generation SHALL preserve source YAML and write effective metadata at `.l8k/resolved-config.yaml`. Deploy/validate SHALL prefer explicit user config, then resolved metadata, then legacy fallback. Resolved metadata SHALL round-trip explicit empty values, check format compatibility and avoid reapplying defaults/catalog resolution as if it were source input.

#### Scenario: Transient generation override

- **WHEN** generation succeeds with a CLI override
- **THEN** source YAML remains unchanged and the effective sidecar records the resulting value.

#### Scenario: Explicit config overrides metadata

- **WHEN** a caller supplies a config path alongside deployment metadata
- **THEN** the explicit input wins; metadata does not silently replace the request.

#### Scenario: Resolved empty value

- **WHEN** supported empty values are saved and loaded as resolved metadata
- **THEN** loading preserves those values without filling them from defaults.

#### Scenario: Topology override retains host evidence

- **WHEN** generation applies a topology preset to an input with host package observations
- **THEN** discovery membership and completeness remain the source for driver ownership and host release compatibility; resolved metadata preserves that scope independently of render topology.
- **AND** later validation and repeated generation retain these checks; presets do not define host package state or worker membership.

### Requirement: Source membership survives artifact handoff

Rendered SR-IOV node policies and NV-IPAM IPPools SHALL identify their selected
source groups. Deploy SHALL check desired selectors before writes, and readiness
SHALL check live selectors against intended metadata and effective worker inventory.
All-empty preset inventory SHALL remain explicitly scope-unverified; incomplete or
ambiguous inventory SHALL fail rather than guessing a source association.

#### Scenario: Live policy selects an extra node

- **WHEN** a live selector includes a worker outside its intended source union
- **THEN** validation reports a scope error even if the desired selector or controller status is healthy; live annotations cannot redefine the intended union.

#### Scenario: Temporary live scope inspection failure

- **WHEN** deployment polling cannot read live selectors or Nodes because of a temporary API or connection failure
- **THEN** it retries within the existing deployment budget without evaluating readiness until fresh scope inspection succeeds.
- **AND** permission denials, invalid source metadata and selector mismatches remain terminal; pre-write inspection failures do not authorize writes.

### Requirement: IPPool cluster address collision protection

Generation SHALL check the validated artifact snapshot's NV-IPAM IPPool subnets
against explicit authoritative cluster pod/service CIDRs before output replacement.
Deployment SHALL check that snapshot against configured overrides and fresh
all-cluster pod ranges (OpenShift Network `spec.clusterNetwork` when served,
else Node pod CIDRs), InternalIPs and ServiceCIDRs before managed effects,
including external Helm, overwrite and dry-run. Each supplied override replaces
only its own address class. Observed ranges SHALL NOT become persisted overrides.

#### Scenario: Detected overlap

- **WHEN** an IPPool overlaps a known pod/service range or contains a Node InternalIP
- **THEN** deployment fails before Helm, remediation or apply, naming the source resource and conflict.

#### Scenario: OpenShift cluster network ranges

- **WHEN** the config.openshift.io/v1 Network `cluster` serves `spec.clusterNetwork` ranges and no pod override exists
- **THEN** deployment checks those ranges instead of Node pod CIDRs, independent of the configured flavor.
- **AND** an absent API/object or empty list falls back to Node pod CIDRs; denied reads and malformed ranges fail.

#### Scenario: Incomplete cluster address evidence

- **WHEN** no pod range is available from the OpenShift Network or a Node, and no authoritative override exists
- **THEN** deployment warns by Node name while checking all available ranges.
- **AND** absent GA and beta ServiceCIDR APIs warn; denied reads, malformed ranges and an empty served API fail unless authoritative service ranges bypass that API read.

## Baseline and limits

Structural validation does not prove CRD schema/admission or readiness. Sidecar handling lives outside `pkg/bundle`. Atomic sidecar writing does not promise whole-directory atomicity or cluster transactions. No content hash binds manifests to effective config; explicit override may differ from generated intent. Bundle checks cannot recover artifacts already lost to render-map overwrite; [profile planning](../profile-planning/spec.md) owns that boundary.

## Integration and evidence

- Implementation: [bundle](../../../pkg/bundle), [effective config](../../../pkg/config/effective.go), [Host paths](../../../pkg/target/host/paths.go).
- Existing suites: [bundle](../../../pkg/bundle/bundle_test.go), [application bundle](../../../pkg/app/bundle_test.go), [Host bundle](../../../pkg/target/host/bundle_test.go), [effective config](../../../pkg/config/effective_test.go), [paths](../../../pkg/target/host/paths_test.go).
- Architecture: [artifact bundle](../../../engineering/architecture/artifact-bundle.md).

### Requirement: DPF generation preserves the complete selected chart and resource intent

DPF HBN-BGP generation SHALL retain verified operator and HBN chart archives,
the original source YAML and resolved desired inputs in a single validated bundle.
It SHALL validate generated DPF custom resources against the selected operator's
schemas and reject fields the API would prune, while preserving fields explicitly
allowed by the schema. An operator post-render adjustment SHALL apply identically
to the preview and later Helm installation. Generation SHALL not claim runtime
readiness from archive or schema validation.

#### Scenario: Misspelled hardware flavor field

- **WHEN** a supplied DPUFlavor contains an unknown field outside a schema region permitting arbitrary values
- **THEN** generation reports its source field and publishes no bundle, instead of allowing the API to silently remove the setting.

#### Scenario: DPF address pool and PF interface

- **WHEN** HBN resources are generated for the selected DPF release
- **THEN** address pools and physical-function selectors use that release's actual field paths and survive structural pruning unchanged.

#### Scenario: Operator preview includes a cluster-scoped rule

- **WHEN** the selected operator chart renders a NodeFeatureRule
- **THEN** the full preview retains the rule at cluster scope, validates its identity, and uses the same scope correction during installation.

#### Scenario: Invalid or changed desired bundle

- **WHEN** rendering, schema validation or digest verification fails
- **THEN** no destination is published; an existing different or incomplete directory is retained and rejected.

#### Scenario: Retained DPF intent and manifests disagree

- **WHEN** a native lifecycle consumer loads an HBN bundle with an altered stage, selection, artifact, or desired resource, even with recalculated file hashes
- **THEN** it rejects the bundle before API access unless the full retained content matches generation from its explicit resolved inputs.
- **AND** omitted required operator identity or serial-bound BMC endpoints require regeneration; the consumer does not infer them from ambient configuration.
