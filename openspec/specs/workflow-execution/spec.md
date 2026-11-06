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

### Requirement: Actionable non-success outcomes

New and changed workflow checks SHALL persist a stable reason code and safe explanation for every failed, unavailable, unknown, skipped or deferred result. The result SHALL distinguish missing prerequisites from an attempted operation failure and identify the subject and source where known. Diagnostic persistence MUST NOT disclose credentials or treat optional unknowns as passes. Existing unrelated baseline paths are not retroactively migrated by this requirement.

#### Scenario: Prerequisite executable is missing

- **WHEN** a workflow cannot execute a check because a required tool is unavailable
- **THEN** the report and saved observations identify the executable and explain the missing prerequisite, while required versus optional policy determines whether dependent work may continue.

#### Scenario: Service does not exist in the current phase

- **WHEN** a check depends on an operator-created endpoint before that operator is installed
- **THEN** it is deferred with a reason and required phase, not reported as passed or used to fail initial hardware inventory.

### Requirement: Explicit DPF deployment boundary

The initial Sentry workflow SHALL consume existing management, tenant, enrolled
BlueField-4 and namespace/credential resources. It SHALL verify explicit cluster
identities, exact selected device membership and compatible release contracts.
It SHALL not silently install an operator, enroll hardware, adopt foreign objects,
execute reboot scripts, remove foreign ownership or modify unrelated Host resources.
Selected nodes SHALL use the external reboot method without pending power actions.

#### Scenario: Existing foreign owner

- **WHEN** a selected DPU belongs to a different DPUSet or DPUDeployment
- **THEN** deployment fails with a retained reason before creating replacement provisioning resources.

#### Scenario: Unreviewed reboot automation

- **WHEN** the selected DPUNode uses a reboot script or has a pending power request
- **THEN** preflight blocks provisioning and identifies the unresolved external transition.

#### Scenario: Multiple selected devices

- **WHEN** deployment submits a DPUSet for one selected device and DPF starts its host transition
- **THEN** the workflow waits for that DPUSet to become Ready and selected host transition markers to clear before submitting the next DPUSet, retaining the full identity, ownership, BMC and artifact checks before each physical create.
- **AND** timeout or cancellation retains the completed and pending observations and does not submit later DPUSets.

### Requirement: Honest admission and selected readiness

Unresolved required admission checks SHALL yield an incomplete result and nonzero
exit status. Readiness SHALL require current generation evidence according to the
kind's actual status contract and exact selected tenant-node coverage. Native
resource comparison SHALL accept declared API defaults while rejecting additional
effectful desired fields. Controller-owned workload projections SHALL accept normal
Kubernetes defaults without ignoring supplied fields or selected membership.

#### Scenario: Missing freshness

- **WHEN** a resource is marked Ready but lacks its required current generation evidence
- **THEN** validation remains unsuccessful and retains the reason.

#### Scenario: Missing future API during admission

- **WHEN** a dry-run cannot validate a required object because its API is not served
- **THEN** the result records unresolved admission and does not claim complete success.

#### Scenario: Unexpected hardware setting

- **WHEN** an existing or admitted DPUSet adds a non-default hardware setting absent from frozen intent
- **THEN** the workflow rejects desired-state drift rather than adopting that setting.

## Baseline and limits

The root pipeline runs discover/generate/deploy, not automatic validation. Exactly-once dispatch is not a promise of global idempotency or a non-reusable operation object. Discovery may mutate supplied config and use a global logger. It performs pre-clean before some worker validation and defers cleanup only after successful setup. See [effects](../managed-effects/spec.md), [artifacts](../artifact-handoff/spec.md) and [outcomes](../outcomes-evidence/spec.md) for those independent responsibilities.

## Integration and evidence

- Implementation: [target registry](../../../pkg/target), [Host adapters](../../../pkg/target/host), [CLI routing](../../../pkg/cmd/target_cli.go), [application deploy](../../../pkg/app/deploy.go).
- Existing suites: [target](../../../pkg/target/target_test.go), [Host driver](../../../pkg/target/host/driver_test.go), [deploy](../../../pkg/target/host/deploy_test.go), [launcher](../../../pkg/target/host/launcher_test.go).
- Engineering: [target extension](../../../engineering/advanced/targets.md).
