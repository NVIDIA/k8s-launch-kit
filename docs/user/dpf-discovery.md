<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# DPF discovery reference

Start with the [step-by-step CLI workflows](dpf-workflows.md) for first inventory,
address-candidate lookup and refresh. This page provides the access and output reference.

`l8k discover --target dpf` collects hardware inventory from explicitly selected
host/DPU Redfish or SSH endpoints. The DPF estate is independent of Host networking.
Inventory mode is the default and can run before Kubernetes exists. An explicitly
supplied management kubeconfig adds management observations and must work if supplied.

DPF discovery remains an **experimental** capability. Inventory success means
selected sources were read and the requested inventory was saved; it does not
mean DPF provisioning readiness or hardware qualification. Missing optional card
metadata remains visible. Hardware support and deployment readiness checks do
not block inventory collection.

Discovery defaults to `inventory` mode, including with HBN-BGP. Supply a
kubeconfig to add management topology observations. Explicitly select
`discovery.probes.mode: management` to test per-node network paths. Management probes currently support Linux/amd64 nodes. DPF HBN-BGP
artifact generation and standalone [deployment](dpf-deployment.md) are available.
DPF standalone validation and the composite DPF pipeline are not available.

Inventory establishes physical identities, host associations and BMC access. It
does not certify hardware support for every deployment. A selected use case
checks its own required hardware, firmware and controller capabilities.

## Management-network probes

Set `discovery.probes.mode: management` and supply an explicit management
kubeconfig. Hosts may use SSH or Redfish; every selected DPU must have an
explicit Redfish BMC endpoint for the network checks. Select an existing namespace with `discovery.probes.namespace`;
otherwise the kubeconfig namespace, then `default`, is used. The account must
be able to read Nodes and the namespace and create, inspect, read logs from,
and delete the temporary Pods and ConfigMap there. Namespace admission policy
must allow host-network Pods. Launch Kit does not change admission policy.

The cluster must have exactly three control-plane nodes. All nodes reported by the management cluster
must be Ready, schedulable Linux/amd64 nodes and
able to pull the published, digest-pinned `nicolaka/netshoot` image. An optional
`discovery.probes.imageRepository` mirror must contain the same pinned manifests.
Unsupported node scope is rejected before probe resources are created.

From the CLI, Launch Kit creates one immutable ConfigMap with public request
and CA data. It then runs two temporary non-root Pods on each management node:

| Probe | Where it runs | Operations |
| --- | --- | --- |
| Pod network | That node's ordinary Pod network namespace | Resolve the management API and each selected DPU BMC address, connect over TCP, and check HTTPS using each endpoint’s explicit trust policy. |
| Node network | That node's host network namespace | Repeat the same connection checks; read interfaces, link state and routes. For HBN-BGP with a configured tenant VIP, ask the kernel which route it would use for that VIP. |

These Pods receive no BMC credentials or Kubernetes service-account token,
mount no host filesystem, and do not change networking. They make unauthenticated
requests to the API version endpoint and each BMC's Redfish root. An HTTP
401/403 can still prove that the HTTPS transport path works; it does
not prove DPF controller authentication.

BMC probes reuse the endpoint's TLS policy. With explicit `insecureSkipVerify: true`,
a successful handshake is reported as `tls: unverified`; the connectivity check
can pass, but it does not verify server identity. This supports the initial BMC
bootstrap path. Later DPF controller authentication remains a separate check.
Management API probes always verify TLS. For trusted BMC connections, supply
the endpoint's public `caFile` and a certificate valid for its name or address.
A failed DNS, TCP or TLS check remains a blocker while other independent paths
are collected. Admission, identity, image or malformed-response failures stop
probe execution. Live progress names each node and network namespace.

The CLI removes only the temporary Pods and ConfigMap it created. Inspect cleanup
failures and the saved resource report before retrying; do not delete resources
belonging to another run.

For a new HBN tenant, interface/route checks require every node selected by the
tenant selector to have the configured interface up, a suitable IPv4 prefix,
and an on-link route with a source address on that interface. A VIP already
assigned on a selected node is a conflict for new-tenant creation. These checks
do not reserve the VIP, prove VRID uniqueness, or test failover. Missing allocation
information continues to block readiness even when every network probe passes.
A matching registered tenant confirms its identity and configured endpoint only.
It does not prove that its control plane, virtual IP or API is currently healthy;
those require separate live checks before reuse.

Use the build that produced an experimental inventory, or a newer compatible
build, for subsequent commands. Older builds can reject newly added fields.

## Inputs and invocation

For HBN-BGP, also see [Check HBN-BGP prerequisites](#check-hbn-bgp-prerequisites).

Supply explicit host endpoints and DPU BMC endpoints for a complete inventory.
Each endpoint selects exactly one source: Redfish (`bmcEndpoint`) or SSH (`ssh`).
A host may contain several selected or unselected cards. With no explicit DPU
endpoints, BlueField cards are derived from host observations; their BMC/ARM
endpoints remain unknown and discovery is partial with a nonzero exit status. ARM-only
DPU sources also leave the required BMC administrator check failed. With explicit DPU endpoints, every selected source must
succeed and its card identity must match exactly one host adapter. Sources are not
silently substituted after access failures. The CLI does not scan subnets.

```yaml
credentials:
  dpu-reader:
    username: bmc-maintainer
    passwordRef:
      env: DPU_BMC_PASSWORD
  host-reader:
    username: bmc-reader
    passwordRef:
      env: HOST_BMC_PASSWORD
discovery:
  dpus:
    - bmcEndpoint: https://192.0.2.11
      credentialRef: dpu-reader
  hosts:
    - bmcEndpoint: https://192.0.2.21
      credentialRef: host-reader
```

```bash
l8k discover --target dpf --user-config dpf-site.yaml \
  --save-cluster-config dpf-discovered.yaml --output json
```

The YAML is ordinary CLI configuration; collected facts live under `inventory`.
Unknown fields, duplicate
keys, wrong scalar types, ambiguous contexts and duplicate endpoints fail before
remote access. Lists and credentials stay in YAML; no password CLI flag exists.

| Source | Values |
| --- | --- |
| Required site inputs | Nonempty host list and explicit DPU BMC endpoints with administrator credentials for completion; each selected endpoint and credential alias; username and password reference or SSH private-key reference |
| Defaultable | `dpf.release: 26.4.0`; context from a supplied kubeconfig; system certificate trust; SSH port `22`; SSH sudo `false`; collection timeout `10m`; request timeout `15s`; up to four endpoints queried at once (`maxConcurrentBMCs`) |
| Conditional | Kubeconfig for optional management observations; SSH known-hosts file; approved noninteractive sudo for protected reads; private Redfish CA; `systemURI` when a BMC exposes multiple systems |
| Observed | Physical-card/system identities, linked host adapters, named DPU firmware versions, administrator verification, runtime hardware/network facts and management nodes/version |
| Release defaults | Software bill of materials (BOM), artifact coordinates and documented minima for supported models; omitted minima remain unknown and do not establish deployment readiness |
| Optional lookup inputs | Explicit ARM/BMC MAC selectors; optional local `dnsmasq-leases` files. Lookup returns address hints, never identity or permission to connect |
| Later phases | Deployment profile, DPU API allocation, DHCP/service networks and controller authentication |

The management kubeconfig must use verified HTTPS and certificate/key or bearer
credentials. File and embedded representations of the same credential/trust field are mutually
exclusive; choose token or tokenFile. Exec/auth-provider plugins, basic and anonymous access are rejected
in this version. Only the supplied kubeconfig is used. The BMC must support
standard Redfish session login with the supplied username/password and allow
the account to read inventory. Basic authentication and vendor-specific session
paths are unsupported. Discovery deletes its login session when finished; a cleanup
failure is reported. It does not guess passwords or repair credentials.

DPU BMC administrator authorization is required during discovery. The CLI reads
its own session, the advertised standard AccountService account collection and
the matching local account's linked role. An advertised account URI whose final
component matches the session username is read first and its returned username
is verified; unrelated account records need not be readable. For numeric/opaque
account URIs, the bounded complete collection must identify one matching account. The account username must match the
session, and the account must expose `Enabled: true`, `Locked: false` and
`PasswordChangeRequired: false`. Its role must grant all five standard privileges:
`Login`, `ConfigureManager`, `ConfigureUsers`, `ConfigureComponents` and
`ConfigureSelf`. A custom role granting this set is accepted. A username of
`admin`, a role name alone or OEM-only privileges do not establish authorization.
Missing, denied, malformed or conflicting account/role evidence fails closed.

The owned session's `Roles` must match the account's assigned role. If `Roles`
is absent, the CLI requires the session's `UserAuthenticationSource` to identify
the trusted local AccountService. A current `LocalAccountAuth: Enabled` setting
does not prove how an existing session authenticated and cannot replace this proof.
`LocalAccountAuth: Disabled` and explicitly external session sources are
unsupported. `Fallback` and `LocalFirst` require that explicit local session
source, because those modes can authenticate through external providers.
When mode/source information is absent, a matching effective session role is
required; a username collision or an absent LDAP setting is insufficient.
Malformed or inconsistent exposed fields remain unverified even when another
field could otherwise establish the binding. This proves observed Redfish
privileges and matching local-account state, not future write success or DPF
controller authentication. External-provider role mappings are not evaluated.

Review `bmcAdministratorVerified` on each DPU, any `issues` entry for that field, and the required
`dpf.management.bmc-administrator` check. `BMC_ADMIN_REQUIRED` means the role lacks
administrator privileges; `BMC_ADMIN_ACCESS_DENIED` means required account/role
reads were denied; `BMC_ADMIN_UNVERIFIABLE` means evidence was missing, malformed,
untrusted or inconsistent; `BMC_ADMIN_ACCOUNT_RESTRICTED` means disabled or locked;
`BMC_PASSWORD_CHANGE_REQUIRED` means an administrator must handle the password
change separately. Discovery never rotates passwords, changes roles, or tests
privileges with a write. Physical identity evidence remains in partial output.
Host BMC accounts need inventory read access; the administrator gate applies to
DPU BMC accounts. BF4 B4240 identity requires the exact advertised NVIDIA service
product and board model plus trusted System/chassis links. Newer firmware exposes
matching system/card serials. Older firmware can expose the card serial on its
B4240 BMC component, reciprocally contained by the physical Card linked to the
same System. Absent System metadata stays absent; exposed conflicting identities
fail. These graph and identity properties are reread before success. Free-text
descriptions and similar model names cannot qualify other hardware. This identity
support does not establish release qualification.

Passwords resolve only from `{env: NAME}` or `{file: path}`. Environment values
are verbatim. UTF-8 files lose at most one final LF/CRLF. File, SSH known-hosts and CA references
are relative to the input YAML and are rebased when saved elsewhere. Private files
are not copied. BMC endpoints require HTTPS; redirected connections are not supported.

Environment references are convenient for an automation system that injects
secrets for one invocation. They are not required: environment values are
inherited by child processes and can be exposed by process inspection or
diagnostic dumps, depending on the operating system and permissions. Avoid
typing literal passwords in shell commands or saving them in shell startup files.

For a local secret file or a file supplied by a secret manager, use:

```yaml
credentials:
  dpu-reader:
    username: admin
    passwordRef:
      file: ./credentials/dpu-bmc-password
```

The file contains the password only, not YAML. Restrict the directory to its
owner (`0700`) and the file to its owner (`0600`), keep it outside version control,
and manage its lifecycle separately. A file reference avoids exporting the secret
to the process environment; it does not encrypt the file. Choose one `env` or
`file` reference per password. Discovery does not save resolved passwords in the
inventory or logs.

For BMCs with untrusted certificates, explicitly opt in per endpoint:

```yaml
discovery:
  dpus:
    - bmcEndpoint: https://192.0.2.11
      credentialRef: dpu-reader
      tls:
        insecureSkipVerify: true
```

This fragment extends the complete input above. `insecureSkipVerify` is a YAML
boolean, defaults to `false`, and skips certificate-chain and hostname verification
for CLI-to-BMC Redfish connections only. HTTPS remains required, but the
server is not authenticated and BMC credentials can be exposed to impersonation.
Prefer a verified certificate/CA through `caFile` when available.

The same field is accepted under `discovery.tls` as a shared BMC policy. An
endpoint `tls` block replaces the entire shared policy; `{insecureSkipVerify: false}`
restores system-trust verification for that endpoint. Omitted/null values use the
policy's default. `caFile` and `insecureSkipVerify: true` cannot be combined in the
effective policy. TLS policies are independent even when endpoints share credentials.

The CLI reports warnings on stderr. The saved input records the selected TLS
policy so a refresh can identify changed scope or trust settings. A selected
policy does not establish successful TLS or hardware verification. Use a fresh
input and new output when changing the verification policy or custom CA contents.
This BMC option does not relax management kubeconfig TLS verification.

## Optional SSH inventory in Zero Trust

SSH collects observations without using host RShim or DPU Redfish. Run the CLI
from a management node that can reach the configured endpoints. This version uses
direct SSH; it does not read ambient SSH configuration, agents or ProxyJump rules.
Existing jump connectivity must be provided through the chosen execution location.

```yaml
credentials:
  host-os:
    username: inventory
    passwordRef: {env: HOST_SSH_PASSWORD}
  arm-os:
    username: ubuntu
    privateKeyRef: {file: ./credentials/arm-key}
discovery:
  hosts:
    - credentialRef: host-os
      ssh:
        address: 192.0.2.21:22
        knownHostsFile: ./credentials/known_hosts
        sudo: true
  dpus:
    - credentialRef: arm-os
      ssh:
        address: 192.0.2.11:22
        knownHostsFile: ./credentials/known_hosts
        sudo: true
  probes: {mode: inventory}
```

This example collects partial evidence without a management kubeconfig or Redfish
credential; it exits nonzero because DPU BMC administrator proof is missing.
For a complete inventory, select the DPU BMC through Redfish instead of ARM SSH. Each SSH
credential uses exactly one password reference or unencrypted private-key file
reference. SSH host keys must match the explicitly supplied known-hosts file;
unknown/changed keys fail. Establish that trust through your site's trusted
inventory/onboarding process. `tls.insecureSkipVerify` never applies to SSH.

Host collection reports the host identity and NVIDIA network cards, with PCI,
driver and NUMA information where available. DPU identity must be consistent
across the available system and card records; conflicting serials prevent a
complete inventory. Interfaces, routes, DNS and local BMC LAN address are additional
observations. Required tools must already exist; see the [tool prerequisites](dpf-workflows.md#initial-inventory-prepare).
Discovery runs read commands, optionally with `sudo -n`. It installs nothing,
prompts for no sudo password and changes no privileges.

ARM discovery needs a booted, reachable OS with sufficient permissions. Local
`ipmitool lan print` can report the BMC endpoint without Redfish. The endpoint is
saved as `bmcEndpoint` alongside `method: ssh`; discovery does not connect or send
credentials to that newly observed address. Missing optional tools/data are
reported in `issues` with the affected `field`, a stable `code`, safe `message`
and `tool` where applicable; required-source failures retain their cause too. It
never infers a BMC address from an ARM IP or a serial from a hostname.

If an optional SSH command times out, its result remains unavailable. Before the
next read, Launch Kit reconnects to the same configured endpoint using the same
credentials and verified host keys; it does not retry the timed-out command.
Reconnection failures, failed final identity reads, changed identity, and the
overall discovery deadline still prevent a complete inventory.

Host and ARM PCI addresses belong to different domains; correlation uses physical
serial identity, not matching PCI addresses. If a same-serial host adapter exposes
a PCI vendor other than NVIDIA (`15b3`), the required association fails with
`HOST_ADAPTER_VENDOR_CONFLICT`. Inspect the endpoint, serial and PCI evidence and
rerun with corrected sources; a matching serial cannot override contradictory
vendor identity. A missing vendor stays unknown and does not establish hardware
qualification. Other vendors remain in host inventory, and a contradictory
candidate is never discarded to make another same-serial candidate appear unique.
Host-reported identity alone does not
attest trusted DPU state. Sources and any conflicts stay in the output.
SSH observations do not satisfy the required DPU BMC administrator check.
Controller mTLS enrollment and provisioning readiness remain separate later-phase checks.

## Software BOM and optional address lookup

`dpf.release` selects the release's software BOM independently of live measurements.
Omitted/null selects `26.4.0` for general inventory or the release catalog's
HBN-BGP default, `26.8.0` today, when that use case is selected. Explicit empty or unknown
releases are rejected. The resolved BOM is saved under `inventory.software`, with operator
and component coordinates, exact hardware models and BFB version/URL plus a
digest when known. Documented minima use named fields such as
`minimumNICFirmware` and `minimumHostMFT`. Omitted minima remain unknown in
software checks; they are not inherited from a newer catalog. Catalog source
references, revision lists and generic requirement objects are not saved.
The current catalog has exact BlueField-3 models `B3210`, `B3220` and `B3240`; generic family names and BlueField-4 do not
establish applicability. Desired BFB/component coordinates are not measured
versions. Only documented minimum versions are compatibility checks; unsupported
firmware requirements remain unknown. Model applicability and software checks do
not qualify hardware or enable DPF deployment profiles.

On the supported BF4/B4240 Redfish reader, `inventory.dpus[].psid`
records a successfully verified PSID from the selected System's `AssetTag`.
The identity-selected chassis is used only when the System field is absent or
null, following DPF's source selection. The selected value must start with
uppercase `MT_` and contain at most 32 ASCII letters, digits, underscores, dots
or hyphens. An empty, `N/A` or malformed selected value remains invalid rather
than falling back to another source. An absent value remains unknown. The
selected source and value are reread before the PSID becomes observed;
unselected chassis metadata does not replace or invalidate that selection.
This is a Redfish fact obtained from authenticated `AssetTag` reads;
it does not create an SSH/runtime observation or infer a PSID from a model.

Failures and missing PSIDs are recorded in `issues` with `field: psid`.
`PSID_NOT_EXPOSED` and `PSID_READER_UNSUPPORTED` mean this source supplies no PSID.
`PSID_INVALID`, `PSID_CONFLICT`, `PSID_CHANGED` and `PSID_RECHECK_FAILED` leave
no eligible `psid` value. An invalid optional AssetTag is retained as an issue
and does not prevent saving the inventory or its JSON report. These optional
gaps preserve physical inventory and discovery's existing acceptance policy. A downstream
consumer requiring PSID must require a single observed value associated with the
selected device and reject contradictory or invalid readings across its sources;
an absent or unsupported Redfish PSID may be supplied by an independently verified
SSH measurement, but a rejected or conflicting Redfish PSID must not be bypassed.
Refresh results also compare the observed PSID and eligibility. Release
qualification and administrator authorization remain separate checks.

SSH can report NIC firmware/PSID, MFT version, NVConfig and privilege observations
when the existing tools and permissions permit the read. On hosts, if MFT cannot
read a card's firmware or PSID, Launch Kit also reads `devlink -j dev info` and
matches the same host PCI function. It retains the MFT failure reason. Running
and stored firmware remain distinct; stored firmware cannot prove the running
version. Contradictory readings remain failures. Current and next-boot
configuration remain distinct. DPU Redfish firmware is grouped by named
component under `firmware`, such as `bmc`, `nic`, `cec` and `uefi`. Each version
retains activation when known; an absent activation does not mean running. Raw
vendor names, firmware resource dumps and source-reference bookkeeping are
omitted. A failed source, partial firmware collection, identity change or explicit version/activation conflict
cannot satisfy a minimum through that source. An independent valid SSH observation
can still satisfy a minimum unless explicit component conflicts remain.

`discovery.addressLookup` optionally combines named ARM/BMC MAC selectors with local
`dnsmasq-leases` snapshots and eligible neighbors already read through selected SSH
sources. Results and source/device reasons are saved under `inventory.addressLookup`.
An address candidate is unverified and never receives credentials automatically.
Follow the [candidate procedure](dpf-workflows.md#address-candidates) to prepare
lookup inputs, interpret ambiguity and explicitly add a verified endpoint.

## Observations, reports and refresh

Normal output shows live status on stderr as work happens, followed by a final
summary on stdout. Each status line identifies the selected source and the
operation: session connection, administrator verification, inventory reads,
identity rechecks, cleanup, host/card matching or output publication.
`RUNNING` precedes the operation; `PASS` follows a successful check or completed
read; `FAIL` identifies a failed operation. `UNAVAILABLE` identifies an optional
fact that could not be measured. Optional-read summaries count unavailable facts;
the saved detailed report retains actionable fact failures and required check reasons. Completing a read does not mean
all optional facts were available. Concurrent endpoints can interleave whole
status lines. `--quiet` suppresses progress; it does not change acceptance.

The final text report uses configured endpoint names, falling back to addresses,
and shows DPU model/serial, host association and BMC administrator status. It
separates problems that prevent discovery from prerequisites for provisioning
and checks not requested in this run. Identical later-stage findings are grouped
with their affected sources. An unsupported BOM model means software
compatibility is unknown; it does not mean a hardware failure was detected.
Inventory does not claim release or hardware qualification.

For operation timing and source/check diagnostics, add `--log-level debug`.
Use `--log-level trace` for Redfish request categories/statuses and SSH
stage/tool metadata. Logging does not include credentials, session tokens,
request/response bodies or raw remote command output. Debug/trace logs go to
stderr unless `--log-file` selects a file; normal live progress remains on stderr.

```bash
l8k discover --target dpf --user-config dpf-site.yaml \
  --save-cluster-config dpf-discovered.yaml --log-level debug
```

To watch progress while saving a machine-readable result, redirect only stdout:

```bash
l8k discover --target dpf --user-config dpf-site.yaml \
  --save-cluster-config dpf-discovered.yaml --output json \
  --log-level debug > discover.json
```

Redirecting `2>discover.log` saves progress and logs to that file instead of
displaying them. `--output json` still emits one final JSON document on stdout.

### Read the device results

Both host and DPU records use named fields for available facts and `issues` for
failures, mismatches and unavailable facts. Each issue identifies its `field` and
`code`, with a safe `message` and `tool` when available. Successful reads do not
create issue entries or repeat `state: observed` beside every value. The report's
checks still record required successes and later-phase gaps.

Review `id`, `serialNumber`, `model`, `psid` and `bmcAdministratorVerified` where
present. `id` is published only after the required physical identity rechecks
succeed; a retained serial or endpoint on a failed record is diagnostic context.
A missing fact stays unknown. `firmwareInventoryComplete: true` means DPU
firmware enumeration completed, not that every version is known or compatible.
`firmware` contains named component version lists; activation remains separate
from version. ERoT components stay separate from BMC firmware.

For SSH sources, `runtime.mftVersion`, `runtime.boardProduct` and
`runtime.hardware.cards` hold measured values. Each card can have `psid`,
`firmwareVersion`, `nextBoot` settings and `hostPrivileges`. For example,
`nextBoot.numVFs: 0` is an explicit measured zero, not a missing value; next-boot
settings do not prove the running mode. Host RShim service state, when readable,
is under `runtime.hardware.rshim`.

Host BMC discovery keeps system identity, PCI adapter identities and advertised
reset types for host-to-DPU mapping. It does not dump host firmware or manufacture
DPU-only component placeholders. Host/DPU output omits generic observation maps,
authentication/time placeholders, raw command strings and collection bookkeeping.

New saved inventories use format `v1alpha3`. This CLI also reads legacy
`v1alpha1` and `v1alpha2` conservatively: absent state or verification does not
become successful identity or administrator proof. Keep previous outputs as
history and refresh through discovery rather than editing version tags or facts.

### Review the saved handoff

Use the saved inventory and report to review selected identities, readiness checks
and field-specific reasons. Internal comparison fields in detailed output are for
the CLI's refresh behavior; operators do not need to interpret them or change
saved observations. The report keeps named facts and issues needed to resolve a
failed or unavailable check.

### Review and refresh the handoff

Review each card's serial and its host association. Missing or conflicting identity
must be resolved before accepting the inventory; an IP address, PCI address or
CPU serial does not establish card identity. Cards with incomplete information
remain visible with their available PCI/slot details. Every requested endpoint
remains represented on failure, and missing firmware or authorization facts
remain unknown. Inventory completion does not establish hardware support
for DPF deployment.

Complete discovery writes the requested YAML. Partial collection preserves an
existing handoff and writes `partial.yaml` plus `report.json` beneath
`<output-stem>.discovery/<run-id>/`. JSON stdout is one final DPF result, with
progress on stderr. Invalid command-line arguments are reported on stderr.
Configuration failures identify the field path and original YAML line/column
when available. Correct that location and retry; diagnostics omit field values
and source snippets so credential values are not repeated in errors.
The report contains facts, checks, factual diffs, later-phase gaps, errors and
successfully written artifacts. Redfish errors retain sanitized method/resource,
HTTP status and observation/cleanup stage. Cleanup failures retain the exact
validated session URI when known, never its token. Verify ownership before manual
recovery; an unknown cleanup result is not success. Authentication at session
creation does not establish permission to read protected inventory.
A complete in-place refresh uses the same input/output path and saves
`source-before.yaml` before replacing the inventory. A partial refresh leaves the
original file unchanged. Avoid concurrent runs or edits to the same output, and
use real directories without symlink aliases. Follow the [refresh procedure](dpf-workflows.md#refresh-inventory)
when inputs, trust settings or an older saved inventory need to change.

| Exit | Meaning |
| --- | --- |
| 0 | Explicit-mode checks, cleanup and publication completed |
| 2 | Invalid input, incomplete required identity/mapping or DPU BMC administrator evidence, unsupported probe platform or proven mismatch |
| 3 | Required observation, authentication, TLS, API or remote cleanup failure |
| 1 | Cancellation or local output error |

## Check HBN-BGP prerequisites

Use `--use-case hbn-bgp` to add fabric inspection to DPF inventory collection.
The CLI reads your selected switches over SSH from the machine running Launch
Kit. It runs NVUE commands for applied interfaces, VRFs, BGP and bridges, then
reads operational interfaces, bridges, LLDP and FRR BGP summaries. It does not
change switch configuration. Currently, switch inspection supports Cumulus
Linux with NVUE.

Add switch access and intended DPU uplink connections to your existing site
file. Use the DPU serial numbers from physical inventory. A port binding is
intent; an LLDP hostname alone does not verify it.

```yaml
dpf:
  useCase: hbn-bgp
  hbn:
    fabricVRF: default
credentials:
  fabric-reader:
    username: fabric-reader
    passwordRef:
      file: ./credentials/switch-password
discovery:
  switches:
    - name: leaf-a
      credentialRef: fabric-reader
      ssh:
        address: leaf-a.example
        knownHostsFile: ./credentials/switch-known-hosts
        sudo: true
      ports:
        - {name: swp1, dpuSerial: DPU-SERIAL-A, dpuInterface: p0}
        - {name: swp2, dpuSerial: DPU-SERIAL-A, dpuInterface: p1}
```

Keep the host and DPU endpoints and their credentials alongside these additions.
Each DPU needs one binding for `p0` and one for `p1`. If the mapping is unknown,
retain the DPU in inventory and resolve the missing mapping before deployment.
SSH requires a pinned known-hosts file; Redfish's TLS bypass does not apply.
With `sudo: true`, only LLDP and FRR reads use sudo. Password authentication
passes the sudo password through stdin; key authentication requires
noninteractive sudo authorization.

```bash
l8k discover --target dpf --use-case hbn-bgp \
  --user-config dpf-site.yaml --save-cluster-config dpf-discovered.yaml
```

The explicit CLI selection overrides `dpf.useCase` and is saved with the result.
HBN-BGP defaults to inventory mode and the release catalog's HBN-BGP release
`26.8.0`; the same catalog row supplies default HBN chart and image
coordinates. Other `dpf.release` values are rejected unless the catalog
explicitly supports HBN-BGP for them. The profile supplies operator namespace
`dpf-operator-system` and networking/policy defaults.
Override the operator namespace with `dpf.namespace`; it is independent of the
namespace used for optional management-network probes. Defaults are included in
the binary.

Supply `management.kubeconfigRef` to inspect the management cluster. Launch Kit
reads its Kubernetes version and ready control-plane nodes, DPF operator
Deployment, DPFOperatorConfig, tenant clusters, existing DPU enrollments, storage
classes, and served DPF API schemas. These API calls run from the CLI machine.
The default inventory mode creates no probe Pods; explicit management mode adds
the [management-network probes](#management-network-probes). Tenant clusters and DPU enrollments are read
across namespaces so an existing tenant VIP or physical serial claim cannot be
missed. Saved inventory includes the selected objects and concise conflict or
missing-identity details; unrelated tenant and enrollment records are omitted.
Denied or incomplete reads block readiness.
An existing operator must match the selected release, and its configuration
must use zero-trust mode. A release candidate does not match a selected GA
release. A clean cluster can be planned for operator installation; leftover or
incompatible resources require resolution before deployment.

For tenant storage, Launch Kit checks Kamaji's shared `default` datastore,
which the selected DPF release uses, then follows its endpoints to the etcd
StatefulSet, service, pods, claims and bound volumes in the selected DPF
namespace. Resource names are resolved from these live references. The current
supported storage backend is local-path: its StorageClass must use the
`cluster.local/local-path-provisioner` provisioner with wait-for-first-consumer
binding, and exactly one ready provisioner Deployment must be discoverable.
DataStore endpoints must use the default `cluster.local` service DNS domain.
Missing permissions, stale status, mismatched bindings, ambiguous references
and unsupported storage backends block readiness. These checks are read-only;
existing tenants may share the datastore.

A successful storage check describes the current backend and its bound volumes.
It does not measure remaining filesystem space or guarantee capacity for future
growth. Deployment repeats the storage check instead of trusting a saved
inventory. Discovery does not reserve a tenant API address.

Once the provisioning inputs and current device identities are complete, the
CLI downloads the selected operator and HBN charts over HTTPS and verifies
their digests and generated resource schemas. It uses that plan to ask the
management API whether the configured Kubernetes identity has the scoped
permissions needed for management-side provisioning. These checks include
operator installation, selected DPF resources, backing-resource inspection and
the software registry port-forward. Missing inputs, failed chart verification,
denied permissions and incomplete authorization responses block readiness with
a recorded reason.

The console summarizes successful permission checks in one line and lists each
denied or unmeasured operation separately. The structured readiness report keeps
the individual results for automation and troubleshooting.

The authorization results concern the CLI caller on the management cluster.
Controller service accounts, tenant credentials and host/switch access must be
verified separately. Roles and credentials can change, so deployment repeats its
permission checks before performing operations. No generated resources are
installed during this discovery check.

For the tenant control plane, add the reserved VIP and the interface on the
management control-plane nodes that will carry it. For example:

```yaml
provisioning:
  bootstrap:
    tenant:
      vip: 192.0.2.20
      interface: eth0
      allocation:
        vipReserved: true
        vipExcludedFromDHCP: true
        routerIDUniqueOnSelectedNodes: true
```

Replace the example address and interface with your site's allocation. Set the
three allocation declarations to `true` only after the site owner has reserved
the VIP for this tenant, excluded it from dynamic DHCP allocation, and verified
that the selected VRRP ID is available on the selected control-plane nodes.
Omitting a declaration leaves it unknown; `false` reports a failed prerequisite.
Both block readiness. Launch Kit records these as site declarations, not as
independent DHCP or IPAM verification. They cannot override an observed collision.
If another DPUCluster uses the same VRRP ID, Launch Kit compares its control-plane
node selector with the requested selector. Selectors that can select the same
nodes fail this check; selectors with conflicting values for a shared label key
are disjoint and do not conflict. An explicitly empty selector covers all nodes.
A missing or unreadable selector leaves the comparison unknown. A missing VRRP
ID on another configured endpoint also blocks the comparison. A cluster with no
Keepalived endpoint does not reserve a VIP or VRRP ID through that configuration.

The VIP must be outside DHCP allocation and unused by another tenant. Launch Kit defaults the tenant names,
capacity, pod CIDR and VRRP ID from the profile; verify that the address ranges
and VRRP ID do not conflict with existing use. Changing the VIP, VRRP ID or selected
nodes requires reviewing the declarations again. If discovery observes a literal
management API IP and port, it can reuse them for the operator configuration.
Otherwise, supply `provisioning.bootstrap.managementAPIIP` and
`managementAPIPort`. Missing values block use-case readiness without discarding
the hardware inventory. See the [configuration reference](../reference/configuration.md#dpf-discovery-configuration)
for defaults and overrides.

For each selected DPU, Launch Kit follows its verified host association to the
matching PCI adapter. Through host SSH it checks the PCI functions, `mlx5_core`
driver, network interfaces and IOMMU groups. For interfaces associated with a
host PCI function, it reads the permanent hardware MAC with `ethtool -P` and
checks the interface's sysfs PCI binding before and after the read. The saved
link keeps `permanentMAC` separately from its current `mac`; a missing tool,
denied read or changed binding leaves the permanent address unavailable with a
reason. The current address is never a substitute for a permanent one.
It also inspects IPv4/IPv6 addresses,
routes and bridge/bond relationships: a selected interface carrying a management
address or default route blocks readiness. Missing tools or denied reads retain
their specific failure reason. These checks do not assign host workload addresses.

Launch Kit also inspects the selected host workload port's persistent network
backend over that same explicit SSH connection. The current backend requires
Ubuntu 24.04, Netplan 1.1.2, systemd-networkd 255 and iproute2 6.1.0, with root
access through the configured account or sudo. Install the normal host tools
before discovery, including `jq`, `dmidecode`, `lspci`, `ethtool` and Netplan;
Launch Kit does not install them. Missing tools or unavailable kernel MTU limits
block the check with a reason. Other operating systems and network managers
remain unsupported by this host-configuration workflow.

The selected workload PF must already have an active Netplan configuration
managed by systemd-networkd, with its permanent MAC and interface name bound
unambiguously. An unmanaged port needs site preparation before this workflow
can configure its workload address. Discovery reports this prerequisite rather
than taking ownership of an unmanaged interface.

The check reads the active networkd mapping and merged Netplan configuration,
then validates the exact permanent-MAC/name binding, renderer and selected
interface settings. Unsupported DHCP, virtual-device membership, policy rules,
default routes and malformed address or route entries block the result. Raw
merged configuration is kept out of the saved inventory and logs. Discovery
runs reads only: it does not generate Netplan files, reload managers or change
interfaces.

If a read fails, the result identifies the operation, tool and exit status
without including raw network configuration or command output. Discovery stops
that host-network check at the failed read; independent checks can continue.

A current MTU of 1500 can pass the backend prerequisite when the port supports
the profile's desired MTU of 9000. The report identifies both values; observed
MTU never replaces the desired value. Exact reserved host address and route
checks happen after DPF supplies the real allocation. A backend pass establishes
support for the planned operation, rather than configured addresses, reboot
persistence, BGP adjacency or workload traffic. The flat profile supports one
selected DPU per physical host; multiple selected paths require a supported
multipath policy.

This host backend requires both the current and desired workload MTU to be at
least 1280. Crossing that boundary can remove or create existing IPv6 addresses
and routes, even in an IPv4 workload. A lower kernel-reported minimum does not
establish support for this workflow. An unsupported override remains unchanged
in the configuration and blocks readiness with an explanation.

To check cabling, Launch Kit reads permanent network-function MAC addresses from
the identity-selected DPU card through Redfish. It compares them with switch LLDP
port MACs and p0/p1 names or descriptions. Each binding needs a unique match;
a hostname or port label alone is insufficient. The saved DPU field
`networkFunctionMACs` records MAC ownership, not a physical-port assignment.
If this path cannot establish a binding, Launch Kit can receive LLDP on the
selected host's physical interfaces through SSH. It verifies the host UUID,
card serial, PCI functions and Linux `phys_port_name` before and after capture.
Each capture uses the existing `tcpdump` and `timeout` tools for up to 65 seconds;
it neither installs tools nor changes the interface's promiscuous mode. The
advertised switch chassis MAC, system name and port must match the switch facts
read through authenticated SSH. Expired or conflicting advertisements cannot
satisfy a binding. Missing tools and denied reads retain their failure reason.
This fallback does not replace a known mismatch from the first path. Saved facts
contain the selected peer identity, not packet captures. LLDP corroboration
does not authenticate the peer or prove workload packet delivery.

HBN-BGP discovery also checks the selected DPF release's initial BMC enrollment
reads. For DPF 26.8.0 on BlueField-4, these include the controller's BF4 chassis,
PSID and PF0 permanent-MAC resources. An older BMC can support physical inventory
while lacking those resources. In that case, inventory can complete but readiness
fails with the missing resource and a firmware compatibility explanation. Denied
reads and changing identities also block readiness. A pass covers these reads;
DPF controller authentication, firmware updates and other provisioning actions
remain later checks. Discovery does not update BMC firmware.

On each configured switch, Launch Kit reads both the NVUE interface list and
the individual selected ports. The per-port reads supply VRF and parent
relationships that the list may omit. Conflicting observations block readiness;
an absent bridge field alone does not prove that a port is routed. The switch's
own LLDP chassis MAC and system name are read through the authenticated SSH
connection. Your switch's display name in the site file is not its physical
identity. These operations read configuration and state; they do not change it.
Launch Kit reads the NVUE revision table before and after these switch reads.
The explicit active application ID must remain stable and match exactly one
saved-history record. An unconfirmed change, unsupported revision state, missing
identity or changing application blocks readiness. Historical saved revisions
and unrelated staged changes can remain present. This check reads NVUE's reported
state; it does not inspect the startup file or test persistence through a reboot.

Switch readiness uses the available switch configuration and routing
observations for the selected uplinks. Missing or conflicting observations stay
unknown or failed with a reason. A configuration observation does not prove
hardware forwarding, and discovery does not change switch configuration.

Through the same switch SSH connection, Launch Kit reads FRR's running BGP
configuration to check effective eBGP policy mode and filter attachments for the
selected peers and their peer groups. The supported flat profile has no required
import-prefix filter. A verified unfiltered configuration can pass this check;
an attached route map or another unsupported policy remains unknown with a
reason. An enabled requirement for import/export policies with none configured
fails. This check does not prove which routes a future DPU will advertise or
accept. Saved output keeps selected policy facts rather than raw configuration.

The switch address check reads interface addresses and IPv4/IPv6 routes from
all routing tables. Specific routes or addresses overlapping the workload or
loopback pool block readiness when they belong to the selected VRF; unresolved VRF ownership
remains unknown. A default route is not an address allocation. The saved switch
view retains selected peers and interfaces plus prefixes relevant to both pools.
It does not contain the switch's complete routing table. Site and cluster
IPAM allocation and effective BGP route policy are separate checks.

Omit `dpf.hbn` to use the flat IPv4 BGP
profile defaults, or override individual fields:

| Field | Default | Purpose |
| --- | --- | --- |
| `fabricVRF` | `default` | Switch VRF expected on the selected uplinks |
| `fabricASN` | `65000` | Expected switch BGP autonomous system |
| `dpuASNBase` / `dpuASNStride` | `65101` / `100` | Per-DPU ASNs allocated in sorted serial-number order |
| `uplinkMTU` / `workloadMTU` | `9216` / `9000` | Desired uplink and workload MTUs |
| `workloadCIDR` / `workloadPrefix` | `10.0.120.0/22` / `29` | Desired workload address pool and per-allocation prefix |

These defaults describe the desired configuration. They do not establish that
ASNs or addresses are free, that ports are routed, or that the fabric is ready.
Discovery compares the observed infrastructure with this expected configuration.
Customize only the values your environment needs; omitted values keep their
profile defaults. For example:

```yaml
dpf:
  release: "26.8.0"
  useCase: hbn-bgp
  hbn:
    fabricVRF: tenant-fabric
    fabricASN: 65010
    uplinkMTU: 9000
    workloadMTU: 8800
    workloadCIDR: 172.20.0.0/22
```

These overrides change what discovery expects and what generation uses. They do
not configure the switches during discovery. A different observed VRF or ASN is
a mismatch to resolve, rather than a replacement for your requested value. Keep
switch access and physical port bindings in the site's discovery inputs; they
are separate from these network defaults.

For example, an MTU check can report `expected at least 9216 bytes; configured
9000; operational 9000`. Change the desired MTU only if the use case permits the
smaller value, or correct the infrastructure and rerun discovery. `Not measured`
means the command could not establish a value; it does not mean the default was
observed.

Changing the selected DPU set can change ASN assignments; use a fresh output and
review the resolved profile and assignments before deployment.

Read **HBN-BGP readiness** separately from **physical inventory**. Missing switch
access, unknown facts and configuration mismatches block readiness and return a
nonzero status. A complete physical inventory can still be saved for diagnosis.
The report names each failed check and its reason; the inventory stores the
resolved profile and selected switch observations under `inventory.hbn`.

The current preview also keeps unverified provisioning, artifact, host and
routing-policy prerequisites blocked. It does not authorize deployment from a
successful fabric read. Live BGP adjacency, advertised routes and workload
traffic must be checked after deployment; discovery cannot prove those outcomes
before the DPU services exist.

After reviewing the inventory, follow [HBN-BGP generation](dpf-generation.md)
to prepare a local deployment bundle. Generation can run before live prerequisites
are remediated; its success does not change discovery's blocked readiness result.
The default DPU ASN base and stride cover five devices. For a larger selection,
override the allocation to fit the private ASN range and your fabric policy.
