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

## Baseline and limits

Tag-driven wiring covers root/generate/discover; standalone deploy/validate also use typed Host mappings. Nonzero inference from `Options` cannot represent every explicit zero; use the existing presence-aware inputs. This contract does not promise purity for discovery. [Artifact handoff](../artifact-handoff/spec.md) owns persisted metadata and source selection.

## Integration and evidence

- Implementation: [config](../../../pkg/config), [option tags](../../../pkg/options/options.go), [config input](../../../pkg/configinput), [bindings](../../../pkg/configflags), [resolver](../../../pkg/resolve), [Host adapters](../../../pkg/target/host).
- Existing regression suites to extend: [resolver](../../../pkg/resolve/resolver_test.go), [YAML input](../../../pkg/config/input_test.go), [Host mapping](../../../pkg/target/host/driver_test.go). These are evidence starting points, not proof for a new field.
- User documentation: [configuration](../../../docs/reference/configuration.md).
