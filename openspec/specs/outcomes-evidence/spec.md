<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Outcomes, validation acceptance and evidence

## Purpose

Integrate a stage/check result, aggregate gate, report section or automation output. Inputs are classified resource/check results and selected coverage; outputs are acceptance decisions, exit status and retained evidence.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Define coverage contribution across selected stages, families and endpoints.
- Specify skipped, deferred, in-progress, unsupported, failed, disabled and incomplete handling by mode/flavor.
- Define public fields/framing, stderr logs, exit mapping, report artifacts and compatibility for consumers.

## Requirements

### Requirement: Explicit acceptance policy

A result or gate extension MUST define command execution success separately from acceptance and qualification. It SHALL state how missing coverage and nonterminal/skipped results affect each applicable mode/flavor, preserving existing behavior unless deliberately changed and documented.

#### Scenario: No runnable endpoints

- **WHEN** a selected family produces no gating rows
- **THEN** the applicable mode policy determines acceptance and the missing evidence remains visible; it is not silently described as tested connectivity.

#### Scenario: Resource still progressing

- **WHEN** a resource classifier returns in-progress
- **THEN** aggregation applies the documented mode policy and does not describe that resource as ready.

### Requirement: Output compatibility

Output extensions SHALL preserve command-specific framing, existing public field meanings, diagnostic separation and exit mapping unless a compatibility change is explicit. Text, JSON and HTML MUST describe compatible evidence without assuming identical envelopes.

#### Scenario: Validation JSON stream

- **WHEN** automation consumes validation output
- **THEN** it can parse the sequence of JSON documents, retain process exit status and distinguish manifest/check results from the separate report-path object.

#### Scenario: New test family

- **WHEN** a new family is emitted
- **THEN** existing result/report consumers retain existing families and can identify the added family without redefining older field meanings.

### Requirement: Evidence existence and partial results

An advertised report path SHALL identify an artifact that was successfully written. Extensions MUST safely handle partial initialization and retain useful failure evidence; unavailable reports SHALL be reported as unavailable rather than advertised as complete.

#### Scenario: Report write fails

- **WHEN** validation cannot write the HTML report
- **THEN** output reports the failure and does not advertise that missing file as a successfully produced artifact.

#### Scenario: Early initialization fails

- **WHEN** validation fails before all reporters or endpoints exist
- **THEN** error reporting remains safe and preserves the available evidence.

## Baseline and limits

Current acceptance is intentionally not uniform:

| Mode | Current policy relevant to coverage |
| --- | --- |
| Full Kubernetes validation | Can return success with in-progress/skipped resource results or empty connectivity results; exit zero alone is not complete qualification. |
| Connectivity-only | Requires at least one gating row per selected family; this does not prove every intended endpoint was covered. |
| OpenShift validation | When connectivity is enabled, also enforces selected-family gating coverage, subject to its supported profile matrix. |

Validation emits multiple JSON documents. Root/discover/generate envelopes, standalone deploy status and other commands have different output shapes. This contract does not introduce a universal result envelope or strengthen all acceptance gates. [Connectivity](../connectivity-checks/spec.md) owns individual measurement truth tables; [resource kinds](../resource-kinds/spec.md) supplies classified resource evidence.

## Integration and evidence

- Implementation: [Host validation](../../../pkg/target/host/validate.go), [UI](../../../pkg/ui), [error mapping](../../../pkg/errors), [connectivity reports](../../../pkg/networkoperatorplugin/connectivity).
- Existing suites: [Host validation](../../../pkg/target/host/validate_test.go), [input validation](../../../pkg/target/host/validate_inputs_test.go), [reports](../../../pkg/networkoperatorplugin/connectivity/report_test.go), [text reports](../../../pkg/networkoperatorplugin/connectivity/text_report_test.go).
- Public consumer guidance: [automation](../../../docs/integrator/automation.md), [validation acceptance](../../../docs/user/validation.md).
