<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Managed effects, ownership and teardown

## Purpose

Integrate installation, apply, adoption/remediation, temporary resources, deletion or retention. Inputs are trusted target/scope and ownership policy; outputs are bounded mutations, cleanup results and retained diagnostics.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Define target/namespace/resource/field scope and distinguish Launch Kit installs, external installs, operator children and temporary resources.
- State permission to create, merge, adopt, overwrite, preserve, skip, delete or retain for each operation.
- Declare dependency/finalizer order, rerun behavior, partial-setup cleanup, debug retention and dry-run effects.

## Requirements

### Requirement: Operation-specific ownership

An effect extension MUST define its allowed mutation scope and preserve unrelated fields/resources outside that scope. External Helm ownership SHALL disable Helm installation/validation/removal as applicable without implicitly disabling custom-resource handling. Adoption/remediation MUST follow the operation-specific conflict policy.

#### Scenario: External Helm release

- **WHEN** Helm ownership is external and CR handling is requested
- **THEN** the Helm boundary is retained while the applicable custom-resource operation proceeds.

#### Scenario: Durable platform configuration

- **WHEN** an OCP adapter updates operator configuration
- **THEN** it uses the supported durable configuration mechanism and preserves unrelated settings.

### Requirement: Inspection and mutation decisions

New or changed preflight/ownership logic MUST distinguish verified absence from failed inspection. Each execution mode SHALL describe its actual prerequisites and effects; a dry-run label MUST NOT imply that all surrounding phases are read-only.

#### Scenario: List denied

- **WHEN** a changed preflight path cannot list candidate conflicting resources
- **THEN** it reports inspection unavailable and follows an explicit error policy rather than claiming verified absence.

#### Scenario: Preview with discovery

- **WHEN** a pipeline previews deployment after discovery
- **THEN** documentation and tests account for discovery resources independently of the deploy preview.

### Requirement: Teardown and partial failure

An effect extension MUST specify cleanup after partial setup, cancellation and rerun, including dependency ordering, finalizers, retained resources and failure evidence. Cleanup SHALL honor the documented operation scope and retention policy without promising transactions or success during API outage.

#### Scenario: Dependent finalizers

- **WHEN** clean removes resources with cross-resource finalizer dependencies
- **THEN** it issues deletions before waiting as required by those dependencies, handles recreated resources within its policy, and keeps Helm teardown last.

#### Scenario: Partial setup failure

- **WHEN** only some temporary resources were created before failure
- **THEN** the extension applies its declared best-effort cleanup/retention policy and reports cleanup failures without hiding the original failure.

## Baseline and limits

`clean` intentionally deletes all relevant namespaced CRs in the selected namespace plus a known cluster-kind set; it is not limited to this run. Preflight remediation and temporary-resource cleanup have different scopes. Current stray-resource preflight skips List errors and can report no conflict; some OCP Get errors are classified as missing. The stronger inspection requirement applies to new/changed paths and is not proof those legacy gaps are fixed. Discovery cleanup is registered only after successful setup. See [workflow modes](../workflow-execution/spec.md) and [platform policy](../platform-support/spec.md).

## Integration and evidence

- Implementation: [preflight](../../../pkg/networkoperatorplugin/preflight), [clean](../../../pkg/networkoperatorplugin/clean.go), [OCP config](../../../pkg/networkoperatorplugin/ocp_config.go), [discovery](../../../pkg/networkoperatorplugin/discovery/discover.go).
- Existing suites: [strays](../../../pkg/networkoperatorplugin/preflight/strays_test.go), [cleanup](../../../pkg/networkoperatorplugin/clean_test.go), [OCP namespace](../../../pkg/networkoperatorplugin/connectivity/ocp_namespace_test.go).
- User documentation: [cleanup scope](../../../docs/user/cleanup.md), [deployment](../../../docs/advanced/deployment.md).
