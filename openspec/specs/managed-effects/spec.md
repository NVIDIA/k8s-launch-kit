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

#### Scenario: Secondary-network installer ownership

- **WHEN** a bundle requests Multus/CNI installation or omits a live managed section
- **THEN** deploy inspects ownership before Helm/remediation/apply, rejects foreign installation and implicit managed-to-external migration, and does not let overwrite bypass the guard.
- **AND** polling retries after NCP disappearance recheck ownership and prerequisites using retained desired intent and configured worker membership; admission/apply responses do not replace desired intent.
- **AND** differently named NCP deletion candidates with managed Multus/CNI sections are protected even if their child installers are absent; exact candidates are rechecked before any remediation deletion, and changed policy identity/version cannot authorize a stale delete.
- **AND** omitted external prerequisites with unavailable optional health evidence remain unverified; mandatory live-policy inspection failures remain errors.

### Requirement: OpenShift tuning allowlist

OpenShift generation with ignoreARP SHALL emit one
`04-cni-sysctl-allowlist-patch.yaml` ConfigMap patch artifact. Deploy SHALL consume
its entries to update `openshift-multus/cni-sysctl-allowlist` before other operator
configuration writes and network apply, regardless of filename order,
without sending the patch through ordinary resource apply. It SHALL append
missing artifact entries while preserving existing entries, unrelated fields and concurrent
administrator changes. It SHALL require an existing platform ConfigMap and
report inspection/patch failures. The shared configuration SHALL persist through
cleanup; generation alone SHALL NOT modify it.

#### Scenario: Allowlist prerequisite fails

- **WHEN** the generated allowlist patch cannot read or patch its target ConfigMap
- **THEN** deploy returns the error before creating or updating SR-IOV, maintenance, NFD, or Subscription configuration.

#### Scenario: Repeat or dry-run deployment

- **WHEN** the allowlist already contains all entries or deploy uses dry-run
- **THEN** the complete allowlist is not rewritten, and needed dry-run patches use server-side dry-run without persistence.

#### Scenario: No requested tuning

- **WHEN** the artifact is absent
- **THEN** deployment does not inspect or patch the OpenShift allowlist; Kubernetes rejects an OCP patch artifact rather than applying it as an ordinary ConfigMap.

#### Scenario: Patch artifact validation

- **WHEN** a generated patch targets another resource or lacks an allowlist
- **THEN** deployment rejects it before cluster mutations; runtime validation compares required entries while allowing extra site entries.

### Requirement: DPF external reset proof lifetime

A supported DPF external host reset SHALL bind the complete selected device
cohort and current request to fresh Administrator, identity, shutdown and
firmware-state observations. Its local proof lifetime SHALL start before the
first contributing BMC read and remain bounded through API verification,
durable claim/journal writes, SSH preflight and command submission. Completing
collection SHALL NOT renew earlier observations. The maintenance window SHALL
exclude competing firmware/provisioning writers, including external MFT.

#### Scenario: Reset proof expires before submission

- **WHEN** prerequisite collection, API work or SSH preflight consumes the proof lifetime
- **THEN** no physical command or reset acknowledgement is submitted, the specific reason is retained, and any already-created one-time claim remains blocking.

#### Scenario: Connection fails after reset submission

- **WHEN** command dispatch occurred but its acknowledgement is uncertain
- **THEN** the result remains uncertain, the action is not automatically replayed, and separate fresh boot observations are required before acknowledgement.
- **AND** the bounded submission check does not claim atomic remote scheduling or hardware execution time.

### Requirement: DPF host-network shadow fidelity

Host-network preflight SHALL provide the qualified Netplan generator with the
complete bounded interface matching context it consumes, including current
interface names, MAC addresses and driver identities or verified absence.
The context SHALL remain private and participate in freshness checks through
generation and the host transaction. A failed or changed observation SHALL stop
dependent effects and report the phase's read/refusal reason or blocking
compensation uncertainty. Shadow validation SHALL NOT waive unrelated
generated-output checks to compensate for missing context.

#### Scenario: Management interface is matched by MAC or driver

- **WHEN** an unchanged management definition requires live interface matching data
- **THEN** isolated generation preserves its output while validating only the selected workload change.
- **AND** complete enumeration includes valid dot-prefixed names and preserves non-Ethernet address forms consumed by the qualified generator.

#### Scenario: Interface matching context changes

- **WHEN** an interface, current address or driver changes or cannot be read during preflight or the transaction
- **THEN** the operation follows the existing refusal or compensation policy without publishing a successful receipt.
- **AND** it retains the read/refusal reason before effects; after possible effects, blocking compensation uncertainty may supersede that reason when freshness or ownership cannot be reestablished safely.

### Requirement: DPF Netplan observation consistency

The host-network observer SHALL fingerprint the source set before reading the
merged Netplan configuration, compare it after that read, and recheck it before
publishing the observation. A changed source set SHALL prevent publication of a
successful snapshot. Source read failures SHALL retain their operation and exit
status rather than accepting a digest printed by a failed producer. These
freshness checks do not provide an atomic filesystem snapshot or protect against
arbitrary privileged ABA edits. The managed fragment retains its separate exact
content and ownership checks and is excluded from the surrounding source fence.
Within the transaction fence, that guard SHALL verify the expected exact fragment
or its expected absence before and after capture. Normal, complete-repeat and
verified-external success paths SHALL repeat the fence after
generated-configuration verification, before publishing a successful receipt.

#### Scenario: Sources change while the merged configuration is observed

- **WHEN** a source changes after the merged configuration is read, including during later matching-context collection
- **THEN** the observer returns a source-changed reason before publishing a usable snapshot or authorizing effects.

#### Scenario: A source fingerprint producer prints a digest and fails

- **WHEN** any bracket read prints a digest but exits unsuccessfully
- **THEN** the read fails with its operation and exit status; the printed digest cannot establish consistency.

#### Scenario: The managed fragment changes during a verification read

- **WHEN** the managed fragment changes during capture, appears while absence is expected, or changes during generated-configuration verification
- **THEN** the fragment guard or final fence prevents a successful receipt; matching selected runtime fields do not authorize the changed source.

### Requirement: DPF selected wait-online contribution

Host workload configuration MAY reconcile the selected interface's contribution
to the qualified Netplan any-routable wait. Removing its sole member MAY omit
that now-empty routable command line in the comparison view. It SHALL preserve
every nonselected member, other line and file metadata. Participation SHALL follow existing
selected-source optionality and qualified current-interface matching facts.
The contribution SHALL be checked on first configuration, complete repeat and
verification of an existing external declaration. Raw complete artifact fences
SHALL continue to detect changes after preview, including selected-only edits.
Adding a required static workload interface broadens the vendor's global OR
readiness predicate; this SHALL NOT be described as proof of management
routability or an unchanged global predicate.

#### Scenario: First static workload address on a required port

- **WHEN** a matching nonoptional selected port gains its first static workload address
- **THEN** only its any-routable membership may change in the shared wait-online artifact; unrelated policy and metadata remain significant.
- **AND** omitted optionality and explicit false retain the same required-port meaning, while explicit true remains optional.

#### Scenario: Owned contribution changes after preview or before repeat

- **WHEN** a selected contribution is removed, duplicated or changed after preview or before an early success path
- **THEN** raw freshness or owned-contribution verification prevents a successful receipt and follows the existing refusal or compensation policy.

### Requirement: Fresh DPF boundary before each provisioning hold release

DPF activation SHALL revalidate the full retained bootstrap, tenant, enrollment,
software, flavor and registry boundary before each pending selected maintenance
hold release, including after waiting for controller-created children. It SHALL
then observe the complete selected cohort and require the same maintenance UID,
a fresh resource version, no deletion and the expected action and requestors.
The hold patch SHALL retain its UID, resource-version and previous-annotation
preconditions. These checks SHALL NOT be described as an atomic cross-resource
transaction.

#### Scenario: A dependency changes while waiting for selected children

- **WHEN** retained software, flavor, registry, enrollment, tenant or bootstrap identity changes before a pending hold release
- **THEN** activation refuses that release before any new hold patch, one-time reset claim or reboot.

#### Scenario: A dependency changes between selected hosts

- **WHEN** one selected hold has been released and a dependency changes before another release
- **THEN** activation preserves the first effect's receipt and refuses subsequent releases and resets; the earlier observation does not authorize the remaining cohort.

#### Scenario: Maintenance changes during the refresh

- **WHEN** the refreshed maintenance is missing, replaced, deleting or has changed action, requestors or invalid hold state
- **THEN** activation refuses the pending effect; an already released hold requires no new patch.

### Requirement: DPF maintenance participants and completion

Activation SHALL derive additional service and chain participants from fresh,
complete observations bound to the retained deployment's ownership, logical
references and current versions, and cross-check the copied selected DPUSet and
DPU policies. Requestors SHALL be treated as sets, rejecting duplicate or foreign
participants. Native HBN-BGP activation SHALL refuse `applyOnLabelChange: true`
on either selected resource rather than infer authorization for an older upgrade.
Missing maintenance or an initially false hold annotation SHALL NOT
authorize an effect or establish completion history. Completion after controller
removal SHALL require same-run coordination history bound to the original
maintenance UID, plus fresh identity and consumed references for every selected
DPU and NodeEffectRemoved/Ready conditions observing that DPU's current
generation. A requested reset SHALL retain its durable one-time claim and
exact-round acknowledgement requirements. Later observers SHALL use the same
completion interpretation without requiring every intermediate polling state.

#### Scenario: Controller-coordinated service participants

- **WHEN** the selected maintenance includes current service and chain version requestors
- **THEN** activation verifies their finite deployment-bound graph and copied policy rather than rejecting every non-DPU requestor or allowing arbitrary name prefixes.

#### Scenario: Initial false hold with no coordination history

- **WHEN** the first observed maintenance annotation is false and this run has neither released that hold nor completed a qualified current-round reset and acknowledgement
- **THEN** the annotation alone cannot qualify later absence as completed coordination; no effect or reset replay is inferred from it.

#### Scenario: Polling misses maintenance removal transitions

- **WHEN** this run has qualified coordination history and the same selected DPU identities and consumed references report current completion after maintenance disappears
- **THEN** activation and later host-network observers may recognize completion without an observed intermediate Applied state; absence does not authorize a new effect.

#### Scenario: Foreign or replaced maintenance participant

- **WHEN** maintenance contains a foreign participant or its UID changes
- **THEN** activation refuses dependent effects and completion; an earlier coordination receipt does not authorize the replacement.

#### Scenario: Matching label-triggered upgrade policy

- **WHEN** a selected DPUSet and DPU both enable label-triggered node effects
- **THEN** native activation refuses before a hold patch, reset claim or reboot; equality between live policies does not authorize that upgrade.

### Requirement: Verified DPU deployment group binding

After enrollment identity and BMC administrator verification, native deployment
SHALL bind each selected DPUDevice to the exact selector retained in the plan.
It MAY add only an absent reserved Launch Kit deployment-group label. Conflicting
values, malformed labels or missing unowned selector labels SHALL stop execution.
Every write SHALL recheck the complete selected enrollment boundary, test the
fresh device UID and resource version, preserve unrelated metadata and spec, and
have a durable attempt record. An ambiguous write SHALL stop without automatic
replay. Completion SHALL require a fresh stored-label readback for the entire
cohort; later activation boundaries SHALL continue checking those labels. Caller
permissions SHALL include patch access to each exact selected DPUDevice name.

#### Scenario: Existing unrelated device labels

- **WHEN** a verified selected device lacks its reserved deployment-group label and has unrelated labels
- **THEN** Launch Kit adds only the retained label with identity preconditions, preserves other fields, and verifies the stored labels before activation.

#### Scenario: Conflicting deployment group

- **WHEN** a selected device already has a different value for a retained selector
- **THEN** deployment fails without replacing that label or creating a DPUDeployment.

### Requirement: DPF Kubernetes API error classification

The DPF API boundary SHALL publish only fixed operation prose and recognized
standard Kubernetes status reasons. A missing or unrecognized reason SHALL
become InternalError with HTTP 500, without retaining remote reason text,
message or details. Known standard classifications and context cancellation or
deadline identities SHALL remain available to callers.

#### Scenario: A nonstandard reason accompanies a conclusive HTTP code

- **WHEN** an API read returns an unrecognized reason with HTTP 404, 403, 409 or 503
- **THEN** reconciliation stops without treating that response as confirmed absence, authorization failure, conflict or retry permission; it creates no resource from that ambiguous read and does not expose the remote reason text.

### Requirement: Teardown and partial failure

An effect extension MUST specify cleanup after partial setup, cancellation and rerun, including dependency ordering, finalizers, retained resources and failure evidence. Cleanup SHALL honor the documented operation scope and retention policy without promising transactions or success during API outage.

#### Scenario: Dependent finalizers

- **WHEN** clean removes resources with cross-resource finalizer dependencies
- **THEN** it issues deletions before waiting as required by those dependencies, handles recreated resources within its policy, and keeps Helm teardown last.

#### Scenario: Partial setup failure

- **WHEN** only some temporary resources were created before failure
- **THEN** the extension applies its declared best-effort cleanup/retention policy and reports cleanup failures without hiding the original failure.

### Requirement: RA2.4 dependent API compatibility

Deploy SHALL inspect the live NIC Configuration Operator CRD for RA2.4 NIC
templates after NCP/NNP bootstrap and before applying additional manifests. The
manifest's served API version MUST explicitly define the platformType string
field. A missing field or failed inspection SHALL stop deployment; external Helm,
overwrite, and dry-run SHALL NOT bypass the check. Earlier bootstrap and ordinary
preflight remediation effects are retained if this check fails.

#### Scenario: Fresh compatible installation

- **WHEN** NCP reconciliation installs the compatible NCO CRD
- **THEN** the capability check runs after that reconciliation and permits dependent resource apply.

#### Scenario: Incompatible or unreadable live schema

- **WHEN** an RA2.4 bundle targets a served schema without platformType or the CRD cannot be read
- **THEN** deploy fails before applying the doSPCX ConfigMap and NIC templates; dry-run also requires the installed API.

## Baseline and limits

The DPF native execution primitives implement create-or-verify for retained
custom resources. Existing objects require the same bundle and intent markers,
fixed identity, and complete defaulted spec equality against the retained
release schemas. The selected release's deterministic DPUCluster kubeconfig
addition is allowed. After schema validation and defaulting, comparison also
represents an omitted `grub`, `sysctl`, `ovs` or `containerdConfig` as an empty
object on both independent copies of a
`provisioning.dpu.nvidia.com/v1alpha1` DPUFlavor, matching its typed admission
roundtrip. Nonempty values and all other content remain compared; recursive
empty-value pruning is not allowed. Unrelated differences are conflicts. Namespace and Helm
ownership remain separate operations. Standalone DPF deploy composes these
operations with tenant, selected-device, activation, host-network and runtime
checks. Resource creation alone does not prove those later stages succeeded.

DPF namespace bootstrap preserves existing namespace metadata and policy and
requires a stable active identity. A new DPF Helm installation requires absent
rendered resources and cannot use Helm's update/adoption path, including when a
matching-owner resource appears concurrently. Partial creates or a failed release
record remain for inspection; failure does not authorize automatic deletion.

`clean` intentionally deletes all relevant namespaced CRs in the selected namespace plus a known cluster-kind set; it is not limited to this run. Preflight remediation and temporary-resource cleanup have different scopes. Current stray-resource preflight skips List errors and can report no conflict; some OCP Get errors are classified as missing. The stronger inspection requirement applies to new/changed paths and is not proof those legacy gaps are fixed. Discovery cleanup is registered only after successful setup. See [workflow modes](../workflow-execution/spec.md) and [platform policy](../platform-support/spec.md).

## Integration and evidence

- Implementation: [preflight](../../../pkg/networkoperatorplugin/preflight), [clean](../../../pkg/networkoperatorplugin/clean.go), [OCP config](../../../pkg/networkoperatorplugin/ocp_config.go), [discovery](../../../pkg/networkoperatorplugin/discovery/discover.go).
- Existing suites: [strays](../../../pkg/networkoperatorplugin/preflight/strays_test.go), [cleanup](../../../pkg/networkoperatorplugin/clean_test.go), [OCP namespace](../../../pkg/networkoperatorplugin/connectivity/ocp_namespace_test.go).
- User documentation: [cleanup scope](../../../docs/user/cleanup.md), [deployment](../../../docs/advanced/deployment.md).
