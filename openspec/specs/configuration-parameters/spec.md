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

### Requirement: DPF use-case selection and desired defaults

An explicitly supplied DPF `--use-case` SHALL override the YAML selection and be
persisted in the effective saved configuration, without modifying the original
source bytes used for concurrent-edit detection. Unknown or explicitly empty
selections SHALL fail before remote access. The release catalog SHALL declare
HBN-BGP-compatible releases and its workflow default separately from the generic
inventory default; selected release rows SHALL own HBN artifact defaults.
Networking and policy defaults SHALL remain profile-owned. Explicit invalid
values SHALL NOT silently default.
Host target execution SHALL reject DPF-only use-case flags.

#### Scenario: Saving an explicit HBN-BGP CLI selection

- **WHEN** `--use-case hbn-bgp` overrides the source YAML
- **THEN** saved YAML contains the effective selection, preserves unrelated inputs, and still detects concurrent changes against the original source file.

#### Scenario: Refresh changes the requested fabric scope

- **WHEN** selected switches, trusted host keys, requested sudo, port bindings or the effective HBN profile change
- **THEN** the prior inventory cannot silently authorize that new scope; the operator uses a fresh seed and output.
- **AND** credential alias renaming or secret rotation alone does not constitute new physical scope; every switch is freshly inspected under the supplied access.

#### Scenario: Release provisioning data is incomplete

- **WHEN** the selected release lacks a maintained flavor or immutable artifact for an observed model/PSID
- **THEN** physical inventory collection may continue, while readiness reports an actionable catalog-support blocker.
- **AND** generation SHALL stop; users SHALL NOT be asked to provide raw flavor, scripts or software tuples.
- **AND** artifact selection does not establish reachability, consumed bytes or runtime readiness.

### Requirement: Two DPF user inputs

DPF standalone discovery, generation and deployment SHALL require `--inventory` for environment access, selected systems and allocations. `--user-config` SHALL be optional and accept only typed supported release/profile overrides. Host input semantics SHALL remain unchanged and Host commands SHALL reject `--inventory`.

- GIVEN a minimal valid inventory WHEN no overrides file is supplied THEN select release/use-case defaults without requiring user-authored implementation.
- GIVEN raw software coordinates, DPUFlavor, lifecycle policy, unsupported resource keys or access fields in overrides WHEN loading THEN reject before credential reads or remote effects with the field location and remedy.
- GIVEN an explicit overrides file WHEN loading a prior handoff THEN replace retained override intent rather than deep-merging arbitrary Site maps; preserve presence and validate effective values.
- GIVEN discovery output WHEN passing it to generation THEN restore effective allowed settings without requiring the original overrides file, and reject settings that differ from the discovery configuration until rediscovery.
- GIVEN an overridden file reference WHEN saving elsewhere THEN preserve its inventory-relative referent; override files do not define access references.

## Baseline and limits

Tag-driven wiring covers root/generate/discover; standalone deploy/validate also use typed Host mappings. Nonzero inference from `Options` cannot represent every explicit zero; use the existing presence-aware inputs. This contract does not promise purity for discovery. [Artifact handoff](../artifact-handoff/spec.md) owns persisted metadata and source selection.

## Integration and evidence

- Implementation: [config](../../../pkg/config), [option tags](../../../pkg/options/options.go), [config input](../../../pkg/configinput), [bindings](../../../pkg/configflags), [resolver](../../../pkg/resolve), [Host adapters](../../../pkg/target/host).
- Existing regression suites to extend: [resolver](../../../pkg/resolve/resolver_test.go), [YAML input](../../../pkg/config/input_test.go), [Host mapping](../../../pkg/target/host/driver_test.go). These are evidence starting points, not proof for a new field.
- User documentation: [configuration](../../../docs/reference/configuration.md).

### DPF bootstrap and HBN generation inputs

DPF HBN inventory owns environment allocations under `tenant`; optional overrides own supported choices under `hbn` and `cluster`. Software coordinates are release-owned. Device flavor, boot/NIC/OVS settings, reset policy, node-effect policy and service wiring are fixed maintained implementation. The resolver performs no credential reads or network calls to fill desired settings. Only a literal observed management API endpoint may supply its endpoint defaults.

- GIVEN HBN discovery without tenant VIP/interface WHEN evaluating prerequisites THEN retain inventory and report each missing allocation with a reason.
- GIVEN a management API hostname WHEN deriving an operator endpoint THEN require an explicit reachable IP/port rather than guessing.
- GIVEN selected BMC endpoints WHEN rendering THEN use singleton discovery targets; never broaden to a subnet scan.
- GIVEN custom operator configuration name/namespace WHEN installing THEN render operator arguments selecting that exact resource.
- GIVEN the maintained hold policy WHEN deploying THEN release only identity-verified selected maintenance requests and wait for provisioning.
- GIVEN a retained unsupported native policy WHEN loading THEN reject before Kubernetes effects.
- GIVEN HBN-BGP provisioning WHEN resolving THEN select the maintained `Never` BMC reset policy; no public input may change it.

#### Scenario: Release-owned fixed tenant service range

- **WHEN** HBN-BGP resolves bootstrap inputs
- **THEN** its fixed tenant service CIDR comes explicitly from the selected release catalog, not a profile default or user override.
- **AND** HBN catalog rows require a canonical IPv4 unicast service range. Bootstrap checks retain generic address separation; Generate additionally requires exact equality with that selected controller contract, including direct callers and LoadPlan regeneration.
- **AND** optional persisted BOM summaries roundtrip a present value but leave legacy omission unknown without catalog hydration. Saved BOM facts do not authorize a different controller range.
- **AND** unchanged effective inputs retain the same resolved intent, source bytes and generated bundle identity without a new intent/index field or format version.
