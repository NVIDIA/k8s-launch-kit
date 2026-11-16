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

#### Scenario: Native DPF HBN matrix cannot fit the execution budget

- **WHEN** the serial complete directed ordinary/DF matrix requires packet spacing
  alone greater than or equal to the native maximum deployment duration
- **THEN** HBN discovery blocks deployment readiness and native generation rejects
  the plan; one selected DPU per uniquely associated physical host is required.
- **AND** fresh and resumed deployment compare the same protocol-derived lower
  bound against the actual remaining deadline before metadata or prerequisite
  transports and before management, physical or host-network effects.
- **AND** the budget is checked again before creating a first DPUDeployment after
  preparation; previously measured traffic never reduces a resumed matrix.
- **AND** passing this bound does not estimate completion time or qualify a cohort;
  provisioning, SSH, API fences and other work still consume the deadline.

#### Scenario: DPF HBN image metadata prerequisite

- **WHEN** native DPF deployment starts or resumes the retained plan
- **THEN** before discovery probes or management, enrollment, activation or host
  effects, it verifies bounded anonymous HTTPS manifest/config metadata, digests
  and Linux/arm64 identity for the exact retained HBN image reference.
- **AND** only anonymous or same-origin anonymous Bearer token exchange is
  supported by the CLI verifier; kubelet pull credentials are separate authority.
- **AND** failure persists an actionable non-success check and starts no dependent
  effect; runtime resolves metadata afresh rather than reusing the early identity.
- **AND** an early pass does not establish future image pulls, runtime readiness
  or workload traffic.

#### Scenario: DPF staged software verification

- **WHEN** native DPF execution verifies staged BF4 software against retained artifact locks
- **THEN** the software preparation stage uses the same retained plan as management, tenant and enrollment; it checks current enrollment and required download/tunnel permissions before creating the exact BlueFieldSoftware.
- **AND** controller readiness requires the current generation, Ready phase, and current Downloaded and Ready conditions; a pending condition keeps the wait pending and a terminal condition stops it.
- **AND** it hashes the selected ISO and PSID-specific PLDM bytes served from the verified DPF cache, rather than treating an origin URL download or controller Ready condition as digest verification.
- **AND** it rejects partial, transformed, redirected or mismatched streams and rechecks software, active-controller, registry, shared-storage and public-CA bindings after reading; failed tunnel cleanup prevents a completed verification receipt.
- **AND** a successful result describes the bytes served at verification time, not a guarantee of later hardware consumption or of reachability through a different delivery path.

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

#### Scenario: Resume the same initial DPF deployment

- **WHEN** the caller supplies the reported operation reference with the original site and bundle
- **THEN** deployment verifies the live operation UID, management/namespace/tenant/source identities, selected physical cohort and retained prerequisite receipts before continuing post-DPUDeployment work.
- **AND** a resumed first DD creation requires fresh read-only chart/cache-byte verification; an already-created exact DD, including a lost create response, avoids that large-byte dependency.
- **AND** prior prerequisite resources are verified without install, recreate or adoption; replacement, missing known identity or checkpoint CAS failure stops dependent effects.
- **AND** confirmed activation progress survives an interruption; current Ready or an absent/false hold alone cannot manufacture effect history.
- **AND** an existing reset claim selects observation only, requiring the saved authenticated host baseline and new current Arm proof for the same pending request before an exact conditional ACK; uncertainty never permits another reset.
- **AND** completed host transactions are inspected with their original request and fresh allocation fences; partial/changed transactions remain blocking.
- **AND** a late runtime failure can rerun the complete fresh BGP/FIB/traffic matrix without reimage or replay of completed host configuration; old measurement passes never fill new result rows.

#### Scenario: Owned DPF progress changes initial prerequisite observations

- **WHEN** an exact recorded operation resumes with enrolled devices, an existing tenant, configured host addresses or advertised workload/loopback routes
- **THEN** it loads the UID/plan/management/namespace/cohort-bound checkpoint and restores receipts read-only before assessing complete fresh discovery checks.
- **AND** the inventory and published discovery checks remain unchanged; only the execution assessment can replace initial enrollment/tenant vacancy predicates after full current resource, credential and tenant-source ownership verification.
- **AND** a host-address exception requires inspection of the original saved transaction and an unchanged complete current allocation binding; other addresses, management/default-route conflicts and partial transactions remain blocking.
- **AND** a route exception requires an exact current IPAM/CIDRPool allocation bound to the selected owned Ready Node/DPU, the selected VRF and BGP unicast protocol, and every explicit next hop on that DPU's freshly LLDP-corroborated uplinks. Missing, foreign, mixed and broader-prefix observations remain blocking.
- **AND** resource ownership and checkpoint UID/resourceVersion/data are reread before acceptance; ordinary fresh discovery, deployment eligibility and existing physical/effect fences remain strict.
- **AND** optional next-hop fields do not change older inventories' or bundles' canonical identities when those fields were absent.

#### Scenario: An owned pending reset temporarily removes host PF interfaces

- **WHEN** the exact recorded pending DPU reboot request is current and its present selected host PCI functions have positively measured absent netdevs
- **THEN** BeforeActivation may substitute only that host's unavailable PF, management-separation, backend and selected-uplink host-LLDP predicates after fresh operation, maintenance-release history, DPU/DPUNode UID, host UUID, boot ID and PCI/VPD proofs.
- **AND** current SSH/default-route interfaces must have measured direct PCI-network/lower-link/physfn roots on a different card slot and a current supported OS detector result reporting no VM; this same required check is presence-enforced before an initial deployment. Unrelated virtual interfaces do not expand its scope. Missing/unknown/VM classification, nested virtual buses, unknown roots, same-card VFs/functions, generic command failures, mismatched rounds and already-configured hosts remain blocking.
- **AND** after activation, fresh and resumed runs perform explicit AfterActivation collection and owned assessment with no pending-reset allowance, require strict current PF/backend/cabling checks, and rebind activation hosts/groups to the newly observed same physical cohort before host networking or runtime effects.
- **AND** BeforeActivation continues artifact-origin byte verification unchanged. AfterActivation validates source inputs and records generate-phase ARTIFACT_SOURCE_NOT_RECHECKED without another origin download; current saved software, registry and owned resource proofs remain mandatory, as does the existing before-first-DD byte rehash.
- **AND** raw discovery facts/checks are retained, collection reports unavailable observations without premature eligibility claims, and generate-only unknowns are not displayed as current-phase blockers.
- **AND** a missing required host check identity/host/PF subject/vantage or deploy-phase result is rejected using the discovery owner’s host check set before the handoff; provider outcome and owned-assessment contracts remain in force.
- **AND** optional typed VM classification, boot ID, SSH server address, link physical PF roots, measured netdev absence and nonforwarding route type preserve old encodings when omitted; empty legacy arrays never become positive absence proof. Physical reset/claim/ACK fences remain unchanged.

#### Scenario: SSH negotiation is interrupted after the one-time host reboot submission

- **WHEN** a requested host reboot has been submitted once and a recovery read reports the combined authentication/handshake failure category
- **THEN** that ambiguous category is observed within the existing remaining wait rather than treated as proof of credential rejection; the command is never resubmitted.
- **AND** explicit trust/configuration/identity failures remain fatal, pre-submission authentication failure does not start another action attempt, and cancellation/deadline remain bounded.
- **AND** an uncertain failure persists only finite allowlisted typed SSH code/stage and static reason text; arbitrary callback/library/remote text remains private, while the error cause chain is retained for classification.

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

#### Scenario: New tenant management services are still starting

- **WHEN** the tenant API is verified but the same retained operator configuration is not Ready while the new tenant's management services start
- **THEN** enrollment waits within the existing deployment deadline and retains the current readiness reason, without accepting replacement, ownership/specification drift, a different release version or terminal failure.
- **AND** it rechecks the complete bootstrap and tenant identities before BMC, shared-credential or discovery effects.
- **AND** later enrollment and activation fences remain immediate checks rather than implicit recovery loops.

#### Scenario: DPF selected-device enrollment

- **WHEN** the executor advances from the verified tenant API to selected-device enrollment
- **THEN** it binds runtime BMC access to retained serials and exact endpoints, verifies fresh Administrator access, and requires credentials compatible with the selected DPF discovery path.
- **AND** BMC credentials and TLS policy come from explicit runtime site access inputs; certificate failures never enable an implicit bypass, and an explicitly bypassed connection is not claimed as verified TLS.
- **AND** before controller activation or shared credential installation, it inspects the complete relevant resource scope and stops if unrelated resources could be activated; denied or incomplete inspection is not verified absence.
- **AND** the shared password is retained only in the required Kubernetes Secret; existing mismatched Secret data is never overwritten, and no password or password digest enters generated files or execution reports.
- **AND** scan completion alone does not satisfy enrollment: exact DPUDevice and DPUNode association and current credential, certificate and trust readiness are required; a completed scan with zero newly found devices can reuse exact ready enrollments, but cannot substitute for a missing selected device.
- **AND** factory-reset policy Never does not imply read-only enrollment, and partial effects are retained without implicit device deletion or recovery resets.
- **AND** the full selected cohort is reobserved before success; replacing an observed device/node or changing the tenant or shared credential invalidates the result.
- **AND** an existing DPUDevice without its required DPUNode is reported as partial enrollment that the upstream scan cannot repair; the executor does not hide it behind retries or automatically adopt another reboot mechanism.

## Baseline and limits

Standalone DPF HBN-BGP deploy composes the retained management, tenant, enrollment,
software, service, activation, host-network and runtime stages. Its explicit site
and bundle inputs do not enable a Host fallback. DPF standalone validate, the
composite pipeline and generate-with-deploy remain unavailable.

External reset handling requires independent current shutdown/task/copy checks
before a one-time host action, followed by a changed host boot and fresh selected
DPU agent/current-kernel observations. The controller's Pending state is not
shutdown proof. Annotation-only acknowledgement requires a freshly rechecked
same request and cohort, with resource-version preconditions. An agent-less
pre-OS reset without the supported completion observations stops explicitly;
the executor does not manufacture a baseline or wait on a circular dependency.

The root pipeline runs discover/generate/deploy, not automatic validation. Exactly-once dispatch is not a promise of global idempotency or a non-reusable operation object. Discovery may mutate supplied config and use a global logger. It performs pre-clean before some worker validation and defers cleanup only after successful setup. See [effects](../managed-effects/spec.md), [artifacts](../artifact-handoff/spec.md) and [outcomes](../outcomes-evidence/spec.md) for those independent responsibilities.

## Integration and evidence

- Implementation: [target registry](../../../pkg/target), [Host adapters](../../../pkg/target/host), [CLI routing](../../../pkg/cmd/target_cli.go), [application deploy](../../../pkg/app/deploy.go).
- Existing suites: [target](../../../pkg/target/target_test.go), [Host driver](../../../pkg/target/host/driver_test.go), [deploy](../../../pkg/target/host/deploy_test.go), [launcher](../../../pkg/target/host/launcher_test.go).
- Engineering: [target extension](../../../engineering/advanced/targets.md).
