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

### Requirement: Closed DPF intent handoff

DPF Sentry generation SHALL retain a closed, hashed bundle containing the resolved
configuration, native resources, pinned chart archives, effective Helm values and
validation-only workload projections. Consumers SHALL verify the complete bundle
against its resolved profile intent using retained chart bytes before opening
cluster clients. Different desired intent requires generation into a new directory;
explicit access-path relocation SHALL not replace captured deployment intent.

#### Scenario: Reindexed foreign resource

- **WHEN** a bundle has consistent hashes but changed resources or stages inconsistent with its resolved profile
- **THEN** deploy and validate reject it before cluster access or writes.

#### Scenario: Relocated discovery output

- **WHEN** discovery saves a source config in another directory
- **THEN** chart-directory and credential-file references continue to resolve to their original locations.

#### Scenario: Workload projections

- **WHEN** a selected DPF release transforms service Helm inputs
- **THEN** validation projections include that release's effective input transformation and remain separate from resources applied by Launch Kit.

## Baseline and limits

Structural validation does not prove CRD schema/admission or readiness. Sidecar handling lives outside `pkg/bundle`. Atomic sidecar writing does not promise whole-directory atomicity or cluster transactions. The legacy Host handoff has no content hash binding manifests to effective config; its explicit override may differ from generated intent. DPF uses the closed intent contract above. Bundle checks cannot recover artifacts already lost to render-map overwrite; [profile planning](../profile-planning/spec.md) owns that boundary.

## Integration and evidence

- Implementation: [bundle](../../../pkg/bundle), [effective config](../../../pkg/config/effective.go), [Host paths](../../../pkg/target/host/paths.go).
- Existing suites: [bundle](../../../pkg/bundle/bundle_test.go), [application bundle](../../../pkg/app/bundle_test.go), [Host bundle](../../../pkg/target/host/bundle_test.go), [effective config](../../../pkg/config/effective_test.go), [paths](../../../pkg/target/host/paths_test.go).
- Architecture: [artifact bundle](../../../engineering/architecture/artifact-bundle.md).
