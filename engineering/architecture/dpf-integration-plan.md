<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# DPF integration roadmap

**Status:** Accepted direction; implementation in progress

**Last reviewed:** 2026-08-07

**Current repository snapshots:**

- `k8s-launch-kit`: `4d083d5` — target-aware CLI foundation merged in PR #165;
- `dpf-install`: `20f1371`;
- `dpf-operator`: `97570f8`.

This plan describes how Launch Kit will absorb the useful behavior of
`dpf-install` while preserving the existing Network Operator host workflow.
The implemented target boundary is documented in
[Target-aware CLI architecture](../advanced/targets.md). The next Launch Kit
milestone is specified in the
[HostTarget migration plan](host-target-migration-plan.md). The current
repository-wide component and data-flow map is maintained in the
[Launch Kit architecture](overview.md).

## Objective

Launch Kit becomes a target-aware environment orchestrator. It retains one
public lifecycle:

```text
discover -> generate -> deploy -> validate
```

The lifecycle applies independently to the host and DPF management domains,
and later to a composed environment:

```text
Management cluster
  `-- DPF Operator and DPF custom resources
       `-- one or more generated DPU clusters

Workload/host cluster
  `-- Network Operator and host-side networking

External infrastructure
  |-- BlueField BMC and Redfish
  |-- local or jump-node command execution
  |-- storage providers
  `-- optional host provisioning
```

The management and workload roles may resolve to the same Kubernetes context
in Host Trusted deployments. They remain distinct logical roles because they
have different ownership, credentials, and lifecycle boundaries.

## Accepted architectural decisions

The following decisions are now established:

1. `host` and `dpf` are targets; an existing Network Operator plugin is not a
   target abstraction.
2. Omitting `--target` always means `host`.
3. Existing host commands, flags, configuration, artifacts, JSON, and exit
   codes remain compatible.
4. `dpf` is the canonical target name. A DPU is infrastructure managed by DPF,
   not the top-level target name.
5. Target-native configuration remains separate. Launch Kit will not create a
   host/DPF union configuration structure.
6. DPF owns its profile schema, release defaults, and generated manifests.
7. Environment composition sits above independently functional target
   drivers.
8. Destructive physical actions require named approval and are never implied
   by JSON or noninteractive mode.

## Current implementation status

PR #165 delivered the compatibility-first command boundary:

- `pkg/target` contains target names, phases, descriptors, capabilities,
  registry validation, invocation policy, and bound operations;
- lifecycle commands accept optional `--target`, with `host` as the default;
- explicitly supplied flags are validated against target ownership;
- `l8k schema` advertises target and phase capabilities additively;
- all DPF phases are recognized but unavailable, so DPF cannot fall through to
  host execution;
- the original host command bodies remained unchanged at that foundation
  stage.

The follow-on HostTarget migration is implemented in main. Every lifecycle
phase now binds a typed, immutable Host request through the registry and executes a concrete
Host operation. Standalone deploy and validate orchestration live outside
Cobra, process exit remains at the command boundary, and DPF is rejected
before any Host dependency is constructed.

The current implementation stage is independent **HBN-BGP**, starting with
discovery and followed by generation and deployment. Sentry and Host/DPF
composition are outside this stage. The initial profile is flat IPv4 BGP with
two unnumbered DPU uplinks, without mandatory EVPN or tenant VRFs. Desired
profile defaults are overridable; they are never evidence of switch state.

The inspected `dpfctl` implementation exposes an internal DPUFlavor template
renderer, not a public full-profile generation library. Launch Kit will own a
narrow native Go renderer for this profile, using release-matched DPF schemas
and reference fixtures. A future public renderer may replace that boundary;
its publication is not a prerequisite for this implementation. Do not copy
static documentation manifests without typed selection, reference validation,
and complete resource ownership and readiness handling.

HBN discovery records applied switch intent separately from operational
interfaces, LLDP hints and BGP sessions. CLI success requires both complete
physical inventory and passing use-case prerequisites. Unknown required facts
block readiness with specific reasons, while a complete inventory may still be
saved. BGP establishment and data-path validation remain post-deployment gates.
Switch inspection is read-only; desired defaults do not authorize switch writes.

## Responsibility boundaries

### Common target runtime

The common runtime owns only concerns with identical semantics across targets:

- target and phase resolution;
- common output and execution policy;
- phase start, completion, and error categorization;
- logical context and artifact resolution;
- top-level cancellation and timeouts;
- approvals and journal integration;
- the versioned automation result envelope.

It does not own host fabric choices, DPF trust mode, target-native defaults,
manifest rendering, readiness semantics, or internal operation ordering.

### Host target

The Host target owns the current Network Operator behavior:

- host `cluster-config.yaml` and CLI precedence;
- node, NIC, GPU, profile, and topology discovery;
- Network Operator profile matching and manifest rendering;
- Helm installation, NCP/NNP/network apply, and reconciliation;
- manifest, version, connectivity, and report validation;
- existing `plugin.Plugin` implementations.

Host-only artifacts remain under:

```text
deployment/network-operator/
```

### DPF target

The DPF target owns:

- typed DPF configuration and trust mode;
- management, workload, and generated DPU-cluster context requirements;
- BMC, Redfish, host PCI/rshim/MFT, and prerequisite discovery;
- DPF prerequisites, operator, system, BFB, flavor, deployment, and service
  plans;
- DPF resource readiness and DPU operational validation;
- DPF-specific evidence and diagnostics.

DPF-only artifacts use a peer directory:

```text
deployment/dpf/
```

### Environment composer

The composer receives complete target plans and owns only cross-target
contracts:

- shared-component ownership;
- singleton `NicClusterPolicy` composition;
- PF/VF and resource-name ownership;
- topology and host-to-DPU relationships;
- maintenance mutual exclusion;
- release compatibility;
- dependencies between target operations.

It does not merge target-native configuration structures.

## Phase semantics

| Phase | Host target | DPF target | Composed environment |
|---|---|---|---|
| Discover | Nodes, NICs, GPUs, capabilities, profiles | Contexts, existing DPF resources, BMC/Redfish inventory, host prerequisites, storage and load-balancer prerequisites | Correlate hosts, BlueFields, BMCs, and DPU clusters; identify ownership conflicts |
| Generate | Network Operator values and custom resources | Prerequisite, operator, system, provisioning, and use-case subplans | Resolve versions, shared components, contracts, and the operation DAG |
| Deploy | Network Operator Helm plus ordered NCP/NNP/network reconciliation | DPF Helm, system resources, discovery gate, BFB/provisioning, and services | Execute operations in dependency order across contexts |
| Validate | Host reconciliation, versions, and connectivity | DPF conditions, DPU phases, operational health, and DPU-cluster workloads | Verify topology, VF advertisement, CNI, traffic, storage, and maintenance contracts |

Discovery may be incomplete, but every value must be classified as
`observed`, `configured`, `derived`, or `unresolved`. Generation fails when a
required value remains unresolved. Launch Kit must not substitute plausible
lab defaults for CIDRs, BMC ranges, load-balancer addresses, BFB versions,
interface mappings, or service-network parameters.

The in-cluster `DPUDiscovery` resource is a deploy-time discovery gate and a
validation surface. It does not require another public lifecycle phase.

## Configuration model

Configuration evolves in three layers:

1. Host-only runs retain the flat `cluster-config.yaml` format.
2. DPF-only runs use a typed `dpf-config.yaml` owned by the DPF target.
3. Multi-target runs use a versioned environment document that references the
   target-native files.

```yaml
apiVersion: launchkit.nvidia.com/v1alpha1
kind: Environment
metadata:
  name: dpf-lab
spec:
  clusters:
    management:
      kubeconfigRef: management
    workload:
      kubeconfigRef: workload
    dpu:
      source: DPUCluster

  targets:
    host:
      configRef: cluster-config.yaml
    dpf:
      configRef: dpf-config.yaml

  executors:
    default: local
    bootstrap:
      type: ssh
      host: jump-node
      credentialRef: jump-node-credentials

  approvals:
    allowed: []
```

Resolved secrets are never written to configuration, plans, journals, logs,
or validation reports. Persist only references to environment variables,
files, Kubernetes Secrets, or credential providers.

## Environment artifacts

Host-only and DPF-only runs keep their existing peer layouts. A composed
environment may add:

```text
deployment/
|-- environment.yaml
|-- inventory.yaml
|-- plan.yaml
|-- catalog.lock.yaml
|-- checksums.yaml
|-- contracts/
|   |-- ownership.yaml
|   |-- topology.yaml
|   |-- networking.yaml
|   |-- maintenance.yaml
|   `-- compatibility.yaml
|-- targets/
|   |-- host/
|   `-- dpf/
|       |-- prerequisites/
|       |-- operator/
|       |-- system/
|       `-- usecase/
`-- validation/
```

The generated plan records source and release provenance, a non-secret digest,
operations and dependency edges, destination context, readiness gates,
destructive-action classification, timeout and retry policy, dry-run support,
compensating or uninstall actions, and expected resource identities.

## Revised implementation roadmap

### Stage 0: Target-aware command foundation — complete

Delivered by PR #165:

- target vocabulary and explicit registry;
- host-by-default CLI selection;
- target-owned flag validation;
- additive schema capabilities;
- unavailable DPF guard;
- compatibility-oriented tests and documentation.

### Stage 1: Finish HostTarget execution — implemented

Follow the [HostTarget migration plan](host-target-migration-plan.md):

- characterize all host phase contracts;
- extract standalone deploy and validation orchestration from Cobra;
- bind immutable, phase-specific host requests;
- execute every host phase through the bound target operation;
- preserve existing host output, artifacts, errors, and exit codes;
- remove the no-op `PreRun` adapter after every phase is migrated.

Exit criteria: omitted target and explicit `--target host` use the same
concrete driver and are behaviorally equivalent across all four commands and
the root pipeline.

### Stage 2: Establish the release-matched generation contract

Use a small separately versioned DPF rendering module if a suitable public
contract becomes available. Until then, implement the bounded HBN-BGP renderer
in Go inside the DPF target. Reference automation informs the resource graph;
it is not executed as a Python runtime or imported as deployment authority.

The contract must provide a typed profile schema, deterministic artifacts,
artifact identities and ordering, release provenance, compatibility metadata,
and golden fixtures. Launch Kit must not import the full operator dependency
graph unnecessarily or copy DPF documentation manifests.

Exit criteria: the renderer deterministically renders the selected profile and
release, validates against that release's DPF API schemas, and uses no local Go
`replace` directives. BF4 uses BlueFieldSoftware with an OS ISO and PSID-specific
PLDM artifacts; do not substitute a BF3 BFB. Experimental hardware support and
production qualification are reported separately.

### Stage 3: DPF discovery and `dpf-install` import

Start with Zero Trust passthrough:

- add typed `dpf-config.yaml` and logical context roles;
- import `dpf-install` setup data without importing numeric checkpoints;
- discover existing DPF resources, contexts, BMC/Redfish inventory, host
  prerequisites, storage, and load-balancer state;
- preserve credential references without materializing secrets;
- report the provenance and resolution state of every required value.

Exit criteria: `l8k discover --target dpf` produces a complete or explicitly
unresolved DPF configuration and never guesses infrastructure values. Completed
inventory requires read-only verification of the authenticated DPU BMC account and
its effective Administrator privileges. Host/ARM-only inventory is partial and
cannot bypass this gate. Preserve physical identity and authorization failure
evidence independently; never rotate passwords or probe privilege by mutation.
The current DPF target still rejects generation, deployment and validation before
consuming inventory. A future generator must require fresh complete discovery and
revalidate its own authorization needs; saved facts do not authorize provisioning.

Inventory `v1alpha3` has a versioned public projection for both host and DPU
records; internal collector/check APIs keep their existing domain state. Public
facts use dedicated fields: identity, PSID, administrator verification, runtime
versions, per-card firmware/PSID, next-boot settings and host privileges. Named
DPU firmware components retain version/activation; raw vendor metadata and
command output are not serialized. `issues` carries only failures, mismatches
and unavailable facts, with field/code/message/tool rather than a generic
state/value wrapper. ID is exported only for an eligible final identity.
Version-aware decoding preserves these authority boundaries and rejects
contradictions. Legacy `v1alpha1` and `v1alpha2` remain readable without inferring
missing success. Required report check results and qualification boundaries stay
separate from compact device facts. Firmware conflict comparison follows the same
whitespace handling as collection while retaining original measured strings;
serialization does not make a padded version eligible for minimum-version checks.

SSH collection keeps one connection for normal reads. A command deadline closes
that transport to bound stalled session operations; the next independent read may
reconnect with the original endpoint, credentials and trust snapshot. The failed
command is not replayed. Reconnection failure remains a required-identity failure,
and overall cancellation forbids recovery. Final physical identity rereads remain
mandatory after optional observations.

The public software BOM is a projection of the internal release catalog:
operator/component coordinates, exact hardware applicability, BFB version/URL
and known digest, plus fixed named minimum fields. Missing minima reconstruct
unknown requirements for compatibility checks; decoding uses saved coordinates
rather than current catalog defaults. Catalog sources/revisions, generic
requirements and static limitation prose stay internal. Host BMC collection is
limited to identity, PCI mapping and advertised reset capabilities; DPU ERoT and
BMC firmware remain distinct components.

Host/DPU records do not create evidence tables or reference lists, regardless of
transport. Mandatory live reads, identity and PSID rereads, administrator checks,
cleanup and publication remain unchanged. Management/probe evidence retains its
own contract. The public `scopeDigest` guards resolved refresh endpoints/trust;
`requestedScope` and its detailed policy bookkeeping stay internal. Material
fingerprints compare the public fact projection so removed provenance does not
create false changes. Refresh preserves source backups and existing stable
physical identity/scope conflict guards.

### Stage 4: DPF generation for HBN-BGP

- enable `l8k generate --target dpf`;
- use the release-matched native Go renderer described above;
- render prerequisites, operator, system, and HBN-BGP subplans under
  `deployment/dpf/`;
- produce checksums, a release lock, expected resource identities, readiness
  gates, and a target operation DAG;
- introduce a versioned target automation result without changing the
  existing host JSON contract.

Exit criteria: generated output is deterministic, release-matched, contains no
secrets, and matches DPF golden fixtures.

### Stage 5: DPF deploy, resume, validate, and diagnostics

- implement Kubernetes and Helm executors first;
- add local/SSH and BMC execution only where DPF APIs cannot express the
  required action;
- replace numeric stages with an atomic, digest-bound operation journal;
- query live state before skipping any resumed operation;
- require named approval for DPU reimage, reboot, credential mutation, and
  other destructive actions;
- add per-kind validators for `DPFOperatorConfig`, `DPUCluster`,
  `DPUDiscovery`, `DPUDevice`, `DPU`, and `DPUDeployment`;
- reject stale conditions whose observed generation is behind;
- inspect generated DPU-cluster workloads and integrate DPF describe, dump,
  and sosreport evidence.

Exit criteria: a one-DPU Zero Trust deployment is idempotent, resumes after an
interruption or reboot, and passes management-plane, provisioning, DPU-cluster,
and passthrough validation.

### Stage 6: Host Trusted HBN plus OVNK and environment composition

After Host and DPF work independently:

- resolve target configs through an environment document;
- generate both target plans independently;
- arbitrate NFD, Multus, Maintenance Operator, storage, CNI, and device-plugin
  ownership;
- merge DPF host requirements into the singleton `NicClusterPolicy`;
- reject PF/VF overlaps and duplicate resource names before deployment;
- resolve a tested DPF, Network Operator, Kubernetes, BFB, firmware, DOCA/OFED,
  and service-chart cohort;
- add host-to-DPU accelerated-CNI and connectivity validation.

Exit criteria: Host Trusted HBN plus OVNK is deployed from one composed plan
without competing shared resources or uncoordinated disruptive actions.

### Stage 7: Remaining parity and lifecycle

- HBN with GPU/RDMA rails;
- SNAP and storage offload;
- coordinated host/DPU maintenance;
- unified topology and health;
- VPC/isolation and advanced services;
- upgrade, uninstall, use-case replacement, and optional host scale-out;
- parity fixtures for every supported `dpf-install` use case.

If an external compatibility wrapper is introduced, retire it only after the
native path passes identical fixtures and hardware-lab parity gates.

## `dpf-install` parity map

| `dpf-install` capability | Launch Kit destination |
|---|---|
| Setup YAML and precedence | Typed DPF configuration plus `l8k import dpf-install` |
| Interactive setup | Discovery output and explicit missing-field prompts |
| Local and remote execution | Executor abstraction |
| Tool installation and repository clone | Read-only preflight; explicit bootstrap actions only |
| BMC password and serial handling | BMC discovery plus opt-in credential/bootstrap action |
| OVN-K primary CNI | Generated Host target dependency |
| NFS/local storage setup | Pluggable storage provider |
| DPF Operator installation | DPF target deploy |
| DPF system resources | DPF system subplan |
| Use-case stage | Versioned DPF profile catalog |
| Kubespray scale-out | Optional host-provisioner executor |
| Sysinfo | Target validation report and `sosreport` |
| Checkpoint/resume | Digest-bound journal plus live-state queries |
| Validate-only | First-class validate phase |
| Dry run | Per-operation preview classification |
| Upgrade | Future `l8k upgrade --target dpf` |
| Clean/uninstall | Explicit uninstall lifecycle |
| Use-case switching | Plan diff, ordered removal, approval, then deployment |

## Prioritized cross-plane use cases

| Priority | Use case | Purpose |
|---|---|---|
| P0.1 | Zero Trust passthrough | First independent DPF vertical slice with minimal Host coupling |
| P0.2 | HBN plus OVNK accelerated primary CNI | First complete Host/DPF composition and singleton-policy proving point |
| P1 | HBN underlay plus GPU/RDMA rails | Unified workload-to-uplink topology and validation |
| P1 | HBN plus SNAP storage | Coordinated network and storage offload |
| P1 | Coordinated maintenance | Prevent simultaneous host driver and DPU disruption |
| P1 | Unified topology and health | Correlate host, PCI, DPU, and BMC state |
| P2 | VPC and isolation | Tenant isolation spanning Host and DPU |
| P2 | Argus, DTS, and Blueman | Cross-plane observability and security evidence |
| P2 | Multi-DPU roles | Place network, storage, and telemetry services without PF/VF conflicts |
| P3 | Firefly/PTP and DOCA Weave | Follow-on work after core parity and upstream maturity |

The current delivery prioritizes independent HBN-BGP. Passthrough and Host
Trusted HBN plus OVNK remain roadmap use cases; they do not add prerequisites
to this independent DPU-plane milestone.

## Acceptance strategy

Required layers include:

- unit tests for schemas, precedence, operation ordering, and ownership;
- golden tests against the DPF renderer;
- fake local, SSH, Helm, Kubernetes, and Redfish executors;
- envtest coverage for DPF validators and stale-condition cases;
- management-cluster tests without hardware;
- one-DPU Zero Trust and Host Trusted hardware-lab tests;
- combined Network Operator and DPF tests;
- cancellation, retry, reboot, journal-corruption, and rollback injection;
- parity fixtures for every supported `dpf-install` profile.

The final parity gate requires:

- generated resources match the selected DPF release;
- existing setup fields import without importing old checkpoint authority;
- no secret appears in an artifact;
- deployment is idempotent;
- resume verifies live state;
- stale readiness never passes validation;
- unsupported version combinations fail before deployment;
- shared-component and PF/VF conflicts fail before deployment;
- destructive actions require explicit approval;
- every phase emits a stable automation result and evidence index;
- composed validation proves control-plane readiness and Host/DPU data paths.

## Decisions still required

1. Will DPF publish a small public Go renderer or a stable `dpfctl generate`
   contract?
2. Which precise DPF 26.4, Network Operator, Kubernetes, BFB, firmware, and
   service-chart versions form the first tested cohort?
3. Which DPF APIs cover reimage, reboot, and credential workflows, and which
   actions still require out-of-band executors?
4. Which destructive bootstrap actions are supported initially? The default
   recommendation excludes BMC password rotation.
5. What is the ownership and release cadence of the environment compatibility
   descriptor?

Safe defaults remain: omission means Host, DPF 26.4 is the provisional parity
baseline, Zero Trust passthrough ships first, Kubespray is optional, BMC
password rotation is deferred, and environment composition begins only after
both target drivers implement all four phases independently.

### HBN native bootstrap and workload handoff

The HBN path resolves profile defaults, explicit site allocations, and observed
identities separately. `provisioning.bootstrap` provides the reserved tenant
VIP and control-plane interface; profile defaults cover names, VRID, tenant
capacity and pod CIDR. A literal observed management API endpoint can supply its
IP/port. Missing allocations remain required unknowns during discovery.

The management renderer emits namespaces, zero-trust `DPFOperatorConfig`, a
Kamaji `DPUCluster`, and singleton `DPUDiscovery` scans for exactly the selected
IPv4 BMC endpoints. It emits neither credential bytes nor fabricated
`DPUDevice`/`DPUNode` objects. Operator Helm rendering must bind the same config
name and namespace in its process arguments: CRD schema validity alone does
not prove that the operator watches the generated configuration.

The workload renderer consumes resolved HBN coordinates/effect settings,
selected device and tenant selectors, PSID-specific artifacts, and explicit
flavor/resource inputs. The executor must verify those selectors against live
identities and release only selected external node-effect holds. HBN's chart
source API has no digest field; retaining a desired digest is not verification
of bytes fetched by DPF. Artifact consumption verification remains an explicit
deployment gate. Discovery checks workload and loopback pool overlap against
observed switch prefixes; cluster/site IPAM coverage remains a separate gate.

Native execution helpers now provide explicit management access, identity checks
around API operations, retained-chart Helm install or exact release verification,
and resource-specific reported readiness. They do not yet enable the DPF deploy
CLI. The remaining runner must enforce the fixed stage dependencies, ownership,
RBAC, tenant convergence, enrollment identity, consumed artifact checks and
physical-effect boundaries before it can report a deployment result.

Bootstrap ordering follows resource ownership: external prerequisites precede
the operator chart; the chart's manager and CRDs precede DPFOperatorConfig;
that configuration creates DPF's system components. Waiting for those components
before creating the configuration would prevent a clean install. DPUCluster
does not publish generation freshness in the selected API, so its reported Ready
state requires companion Kamaji desired/status and tenant API verification.

Cancellation preserves partial effects for explicit recovery. The Helm transport
keeps request cancellation active through response-body consumption. A failed
identity check after Create can mean the write committed; the runner must retain
that uncertain outcome and must not retry it against another cluster identity.

The native plan loader strictly decodes the retained intent, reconstructs the
explicit generation inputs from the bundle, and invokes the existing generator
to compare the entire result before API access. It does not resolve site defaults
again or reinterpret relative credential paths from the retained source file.
Operator release identity and serial-bound BMC endpoints are explicit retained
inputs. Older bundles missing those inputs require regeneration.

Native custom-resource reconciliation uses create-or-verify. Existing resources
require matching bundle and desired-object ownership markers and complete spec
equality after applying the retained CRD schemas' defaults. Status is checked
separately. The pinned DPUCluster controller's deterministic admin-kubeconfig
name is the narrowly permitted spec addition. Other conflicts stop execution;
there is no implicit adoption, patch, deletion or rollback.

The object execution boundary records and synchronizes an attempt before
reconciliation, then persists the result before dependent work. A report-write
failure stops execution. Readiness polling rechecks UID, ownership and desired
content before evaluating current status; it never recreates a disappeared
object. Reports record safe reasons instead of arbitrary API response bodies.
These primitives do not complete the remaining tenant, enrollment, consumed
artifact or HBN data-plane stages and do not enable the deploy CLI.

Namespace execution treats namespaces as shared infrastructure: it creates only
an absent namespace and otherwise verifies an active, non-deleting identity
without changing existing metadata or policy. Dependent stages retain the
namespace UID so deletion and recreation cannot silently change their boundary.

The operator installer checks that every rendered object is absent before a new
Helm install and disables Helm's update path during that install. This prevents
adoption even if an object with matching Helm annotations appears after the
absence check. The pinned chart must not introduce hooks, separately installed
CRDs, or namespace ownership. Installation is not transactional: earlier creates
and a failed Helm release record can remain after an error. Exact release-record
verification and current controller/resource health are separate checks.

The management bootstrap composition validates the retained plan before API
access, checks external dependencies and scoped permissions, creates or verifies
namespaces, installs or verifies the retained Helm release, waits for matching
CRDs and manager configuration, then creates or verifies DPFOperatorConfig.
It checks cluster-wide configuration ownership before effects and again after
the manager becomes ready. These observations do not provide an atomic
cluster-wide lock; concurrent provisioning must be excluded by the caller.
The final receipt requires the selected configuration version, namespace UIDs,
and manager identity and full default-normalized specification to remain valid.
Only manager-relevant Kubernetes defaults are normalized; unknown fields fail
closed. This receipt covers management bootstrap, not a complete HBN deployment.

Management permission checks use explicit, resource-scoped authorization
reviews, including named RBAC escalation and binding rights for chart resources.
Missing explicit rights leave implicit RBAC coverage unverified; a union of rules
from multiple authorizers is not proof that Kubernetes' RBAC escalation checks
will accept a write. NFD worker readiness is required only when the retained
configuration uses host-side detection. Zero-trust Redfish discovery with that
detector disabled still requires the NFD API and master, but does not require
management nodes to run NFD workers.

HBN workload address reservation is profile-managed. Generation emits one
singleton DPUServiceIPAM exclusion per possible per-DPU subnet, reserving the
configured host index from ordinary dynamic IPAM allocation without excluding
the subnet itself. The gateway and host indexes must differ and remain usable;
reservation enumeration is bounded to 4096 subnets. Explicit static IPAM requests
can bypass exclusions and must not target the reserved host slot.

Discovery validates this address plan separately from host readiness. Actual
host address selection joins the selected DPU identity to its tenant Node and
CIDRPool allocation, then verifies the host PF0 interface. It must never infer
subnet assignment or PF0 identity from list order. The later host operation adds
only the workload-specific route; the management/default route is preserved.
Until those runtime steps exist and pass, a valid reservation does not mark
host configuration ready.

Tenant access uses a separate client for the independent cluster. The management
client reads the selected DPUCluster, its Kamaji TenantControlPlane, API Service,
keepalived resources and deterministic admin Secret. Resource convergence checks
use the selected release's tenant Kubernetes version; the management Kubernetes
version remains a separate input. Tenant access requires embedded, verified TLS
credentials from `super-admin.conf`, with no local-file, executable plugin,
impersonation, proxy or server override. Requests reject redirects.

The endpoint is the retained tenant VIP and Kamaji's allocated API NodePort.
The client binds source resource identities and specs plus the Secret UID and
credential bytes, then observes the tenant's own `kube-system` UID and API version.
Source replacement, credential rotation or tenant identity changes stop this
client and require reopening access after fresh inspection. Routine metadata or
status updates alone do not invalidate unchanged identity. Resource convergence
and authenticated API access remain separate from external VIP allocation,
keepalived image qualification, storage behavior, enrollment and HBN traffic.


Tenant storage discovery and management preflight share the bounded
`managementprobe.Collector.CollectTenantStorage` reader. For the supported DPF
Kamaji layout, it reads `DataStore/default`, correlates ordinal endpoints through
the headless Service and StatefulSet to ready pods, and checks the pods' claims
against their bound PV identities. Fresh datastore/controller status and the
local-path provisioner are required. The supported layout currently requires the
pinned default `cluster.local` DNS domain; custom cluster domains require an
explicit retained input and collector adaptation before they can pass. Read
failures and unsupported layouts retain
specific reasons; neither class presence nor datastore validation alone passes.
Raw datastore TLS/authentication fields remain transient and are not logged or
saved. No Secrets or probe PVCs are read/created by this check. Existing tenants
may share the store. The result establishes current backing-resource health,
not a direct etcd transaction, remaining physical capacity or future growth.
Deployment recollects instead of trusting the persisted discovery result;
ordered tenant creation must repeat this prerequisite immediately before effects.
