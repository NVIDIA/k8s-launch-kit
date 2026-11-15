<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Configuration parameters and resolution

## Purpose

Preserve user intent when adding a field, flag, default, normalization or coordinated override. Inputs are defaults, discovered hardware, source YAML and explicit command inputs; the output is validated effective configuration.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Define the authoritative YAML path/type, default or required value, applicability and validation constraints. Config-only and command-only parameters are valid.
- Define optional CLI exposure, supported commands and presence representation for public Go callers.
- Identify direct mappings versus coordinated requests, affected consumers and serialization. Keep inventories in types/tags and canonical config.

## Requirements

### Requirement: Precedence and explicit presence

Resolution SHALL apply canonical defaults, hardware defaults, user YAML and explicit CLI in increasing precedence, treating YAML null as unset. Explicit false, zero and empty values SHALL participate as supplied values and undergo effective validation after overrides. A new parameter MUST specify whether each empty value is valid.

#### Scenario: Explicit value survives defaults

- **WHEN** a supported field is supplied as false, zero or an empty value
- **THEN** lower-precedence defaults do not replace it merely because its Go value is zero; validation accepts or rejects it according to that field.

#### Scenario: Override repairs input

- **WHEN** a supported explicit override replaces an otherwise invalid source value
- **THEN** validation evaluates the resulting effective value.

### Requirement: Normalization and consumer wiring

A parameter extension MUST use the shared binding/resolution seams where applicable, define catalog/flavor normalization, and reach every advertised consumer. CLI exposure, schema metadata and documentation SHALL agree on command applicability and config paths. Selected releases SHALL remain authoritative for catalog-managed coordinates. Unsupported command exposure MUST NOT silently promise an effective override.

#### Scenario: Release coordinates conflict

- **WHEN** a selected catalog release conflicts with individual catalog-managed coordinates
- **THEN** effective coordinates match the selected release.

#### Scenario: A flag reaches its consumer

- **WHEN** a new flag is advertised for multiple commands
- **THEN** tests exercise each applicable typed request path through to the consumed value, including explicit zero-value presence.

### Requirement: Input isolation

Resolution SHALL leave caller inputs and shared defaults unchanged. Extensions MUST cover mutable nested values and the explicit-presence mechanism used by programmatic callers.

#### Scenario: Independent callers

- **WHEN** one caller modifies its resolved nested configuration
- **THEN** a subsequent resolution and the original source/defaults retain their own values.

### Requirement: DPF software release defaults

The DPF target SHALL resolve software coordinates from a release-owned BOM, separately from site access inputs and observed running versions. Omitted or null release selection SHALL use the catalog default; explicit empty or unknown releases SHALL fail before remote access. Resolution SHALL preserve source configuration and return isolated catalog values. Minimum requirements, desired targets, model applicability and provenance SHALL remain distinct; unknown requirements SHALL retain reasons and never imply qualification.

#### Scenario: Explicit unsupported release

- **WHEN** a DPF input selects a release absent from the embedded catalog
- **THEN** configuration fails without substituting the default release or contacting infrastructure.

#### Scenario: Inventory hardware is outside the BOM applicability

- **WHEN** identity collection succeeds for hardware outside the selected release BOM
- **THEN** inventory remains useful and a reasoned later-phase compatibility check is unresolved; no supported tuple is invented.

### Requirement: DPF use-case selection and desired profile defaults

An explicitly supplied DPF `--use-case` SHALL override the YAML selection and be
persisted in the effective saved configuration, without modifying the original
source bytes used for concurrent-edit detection. Unknown or explicitly empty
selections SHALL fail before remote access. Omitted/null HBN profile fields SHALL
use profile defaults; explicit invalid values SHALL NOT silently default.
Host target execution SHALL reject DPF-only use-case flags.

#### Scenario: Saving an explicit HBN-BGP CLI selection

- **WHEN** `--use-case hbn-bgp` overrides the source YAML
- **THEN** saved YAML contains the effective selection, preserves unrelated inputs, and still detects concurrent changes against the original source file.

#### Scenario: Refresh changes the requested fabric scope

- **WHEN** selected switches, trusted host keys, requested sudo, port bindings or the effective HBN profile change
- **THEN** the prior inventory cannot silently authorize that new scope; the operator uses a fresh seed and output.
- **AND** credential alias renaming or secret rotation alone does not constitute new physical scope; every switch is freshly inspected under the supplied access.

#### Scenario: BF4 artifact inputs are incomplete

- **WHEN** HBN discovery lacks a selected OS ISO or a PLDM artifact for an observed PSID
- **THEN** physical inventory collection may continue while readiness records the missing artifact input.
- **AND** supplied artifact URLs SHALL exclude embedded credentials, queries and fragments; SHA-256 values SHALL be lowercase 64-character hexadecimal digests.
- **AND** a complete selected PSID set SHALL resolve to exactly matching PLDM entries, rejecting case-insensitive duplicates and unrelated entries.
- **AND** input validation SHALL NOT claim artifact reachability, consumed-byte verification or release compatibility.

## Baseline and limits

Tag-driven wiring covers root/generate/discover; standalone deploy/validate also use typed Host mappings. Nonzero inference from `Options` cannot represent every explicit zero; use the existing presence-aware inputs. This contract does not promise purity for discovery. [Artifact handoff](../artifact-handoff/spec.md) owns persisted metadata and source selection.

## Integration and evidence

- Implementation: [config](../../../pkg/config), [option tags](../../../pkg/options/options.go), [config input](../../../pkg/configinput), [bindings](../../../pkg/configflags), [resolver](../../../pkg/resolve), [Host adapters](../../../pkg/target/host).
- Existing regression suites to extend: [resolver](../../../pkg/resolve/resolver_test.go), [YAML input](../../../pkg/config/input_test.go), [Host mapping](../../../pkg/target/host/driver_test.go). These are evidence starting points, not proof for a new field.
- User documentation: [configuration](../../../docs/reference/configuration.md).

### DPF bootstrap and HBN generation inputs

DPF HBN configuration keeps networking intent (`dpf.hbn`), release/profile generation options (`provisioning.hbn`), site bootstrap allocations (`provisioning.bootstrap`) and observed inventory separate. Omitted/null fields select profile defaults; explicit empty/zero/false values are preserved and validated. The resolver performs no credential reads or network access to fill bootstrap inputs. Only a literal observed management API IP/port may supply its endpoint defaults. Tenant VIP/interface remain required site allocations. Bootstrap-input completeness does not establish live address reservation, storage health, controller readiness or deployment permission.

- GIVEN HBN discovery without tenant VIP/interface WHEN bootstrap inputs are evaluated THEN retain the inventory and report each missing allocation as a required unknown with a reason.
- GIVEN a management API hostname WHEN deriving an operator endpoint THEN do not invent its resolved IP; require an explicit reachable API IP/port.
- GIVEN selected BMC endpoints WHEN rendering enrollment resources THEN use one singleton DPUDiscovery scan per endpoint; never scan the range between targets or create synthetic DPUDevices/DPUNodes.
- GIVEN custom operator configuration name/namespace WHEN installing the DPF operator THEN the Helm-rendered operator arguments must select that exact configuration; schema-valid ignored resources are not acceptable.
- GIVEN nodeEffect hold WHEN deploying THEN release only the identity-verified selected maintenance requests and wait for provisioning; rendering or applying a held deployment is not completion.

- GIVEN omitted HBN BMC reset policy WHEN resolving bootstrap THEN select the profile default `Never` and emit it explicitly in the DPF operator configuration; do not inherit an upstream factory-reset default.
- GIVEN an explicit `OnInitialization` policy WHEN rendering THEN preserve it, while leaving stable DHCP address recovery and BMC reauthentication as required live enrollment checks; generation does not establish those conditions.
- GIVEN an empty or unknown reset policy WHEN resolving THEN reject it with a field-specific error before artifact publication.
