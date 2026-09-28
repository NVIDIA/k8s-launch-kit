<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Workflow execution and command integration

## Purpose

Integrate a lifecycle phase, target adapter, command or public entry point through capability-checked typed dispatch. Inputs are bound requests and context; outputs are phase results, artifacts and errors.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Declare modes, dependencies, input/output ownership, cancellation and early-initialization behavior.
- Identify the existing service reused by standalone/composite entry points and any intentional differences.
- Declare compatibility wrappers and concurrency restrictions for exported library APIs.

## Requirements

### Requirement: Capability checked dispatch

Lifecycle CLI extensions SHALL bind typed requests through the target registry and dispatch the selected operation once with command context. Unsupported target/phase combinations and incompatible explicit flags MUST reject without invoking fallback Host infrastructure. Process termination SHALL remain at the CLI boundary.

#### Scenario: Unsupported phase

- **WHEN** a selected target does not implement the requested phase
- **THEN** the command returns an unsupported-capability error without running a Host fallback.

#### Scenario: Default target

- **WHEN** the caller omits the target
- **THEN** dispatch preserves the documented default-target behavior.

### Requirement: Dependencies and partial failure

A workflow extension MUST propagate cancellation and errors, stop dependent stages after failure, and safely report partial initialization. Bound request isolation SHALL be preserved where the adapter snapshots caller inputs.

#### Scenario: Earlier phase fails

- **WHEN** generation fails in a discover/generate/deploy pipeline
- **THEN** deployment does not run and the available earlier-stage diagnostics remain reportable.

#### Scenario: Cancellation

- **WHEN** the command context is canceled during a phase
- **THEN** downstream calls receive cancellation and cleanup follows the declared effects policy.

### Requirement: Entry point mode semantics

Equivalent entry points SHALL reuse common semantics for equivalent modes. Intentional differences, including preview versus server-side dry-run, MUST be documented and tested at their dispatch boundaries.

#### Scenario: Composite deploy preview

- **WHEN** the root workflow reaches deployment with dry-run selected
- **THEN** the deploy phase previews instead of invoking standalone server-side deployment.

#### Scenario: Standalone deploy dry-run

- **WHEN** standalone deploy runs with dry-run selected
- **THEN** its Kubernetes deployment path uses server-side dry-run semantics; this does not describe earlier discovery effects.

## Baseline and limits

The root pipeline runs discover/generate/deploy, not automatic validation. Exactly-once dispatch is not a promise of global idempotency or a non-reusable operation object. Discovery may mutate supplied config and use a global logger. It performs pre-clean before some worker validation and defers cleanup only after successful setup. See [effects](../managed-effects/spec.md), [artifacts](../artifact-handoff/spec.md) and [outcomes](../outcomes-evidence/spec.md) for those independent responsibilities.

## Integration and evidence

- Implementation: [target registry](../../../pkg/target), [Host adapters](../../../pkg/target/host), [CLI routing](../../../pkg/cmd/target_cli.go), [application deploy](../../../pkg/app/deploy.go).
- Existing suites: [target](../../../pkg/target/target_test.go), [Host driver](../../../pkg/target/host/driver_test.go), [deploy](../../../pkg/target/host/deploy_test.go), [launcher](../../../pkg/target/host/launcher_test.go).
- User documentation: [targets](../../../docs/advanced/targets.md).
