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

#### Scenario: DPF execution receipt cannot be persisted

- **WHEN** a native DPF object operation cannot durably record its attempt or result
- **THEN** no dependent effect begins; an attempt without a confirmed result remains uncertain and a later run must inspect actual cluster state.
- **AND** readiness polling cannot accept a replacement UID, ownership drift, or desired-spec mismatch even when the controller reports Ready.

#### Scenario: DPF management bootstrap completes

- **WHEN** the retained operator release, API schemas, manager and DPFOperatorConfig become ready
- **THEN** the management receipt requires the selected configuration version, stable namespace and manager UIDs, and the complete manager specification after supported API defaults.
- **AND** this receipt does not claim tenant, enrollment, HBN service or traffic readiness.

#### Scenario: Conflicting DPF configuration appears during bootstrap

- **WHEN** another DPFOperatorConfig appears while the operator is installing or becoming ready
- **THEN** a fresh cluster-wide ownership check stops dependent configuration creation without adoption or deletion.
- **AND** the caller must exclude concurrent provisioning because a list followed by creation is not an atomic cluster-wide lock.

#### Scenario: DPF tenant creation follows management bootstrap

- **WHEN** the private control-plane executor advances from management bootstrap to a new tenant
- **THEN** explicit retained site declarations for VIP reservation, DHCP exclusion and selected-node VRRP ID availability are required; missing or false declarations stop effects and are never inferred from profile defaults or network observations.
- **AND** it verifies the retained bootstrap identities, current backing storage, complete tenant registry and fresh network facts for the selected control-plane nodes before creating the DPUCluster.
- **AND** failed probe cleanup or a changed selected node set stops creation; persisted discovery results cannot replace these observations.
- **AND** an existing exact owned target follows ownership-aware verification rather than an unused-VIP check, and successful tenant API access does not claim enrollment or HBN traffic readiness.

## Baseline and limits

The root pipeline runs discover/generate/deploy, not automatic validation. Exactly-once dispatch is not a promise of global idempotency or a non-reusable operation object. Discovery may mutate supplied config and use a global logger. It performs pre-clean before some worker validation and defers cleanup only after successful setup. See [effects](../managed-effects/spec.md), [artifacts](../artifact-handoff/spec.md) and [outcomes](../outcomes-evidence/spec.md) for those independent responsibilities.

## Integration and evidence

- Implementation: [target registry](../../../pkg/target), [Host adapters](../../../pkg/target/host), [CLI routing](../../../pkg/cmd/target_cli.go), [application deploy](../../../pkg/app/deploy.go).
- Existing suites: [target](../../../pkg/target/target_test.go), [Host driver](../../../pkg/target/host/driver_test.go), [deploy](../../../pkg/target/host/deploy_test.go), [launcher](../../../pkg/target/host/launcher_test.go).
- Engineering: [target extension](../../../engineering/advanced/targets.md).
