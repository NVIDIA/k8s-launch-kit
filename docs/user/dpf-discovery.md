<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# DPF discovery preview

Start with the [step-by-step CLI workflows](dpf-workflows.md) for first inventory,
address-candidate lookup and refresh. This page provides the access and output reference.

`l8k discover --target dpf` collects hardware inventory from explicitly selected
host/DPU Redfish or SSH endpoints. The DPF estate is independent of Host networking.
Inventory mode can run before Kubernetes exists; an explicitly supplied management
kubeconfig adds management observations and must work if supplied.

This build exposes an **experimental inventory preview**. Inventory success means
that selected sources were read, physical identities and unique host/card mappings
were established, each DPU BMC account had verified administrator privileges, and cleanup/publication succeeded. It does **not** mean DPF
provisioning readiness or production hardware support. Missing optional card
metadata remains visible, including cards without serials. Hardware support and
deployment readiness checks do not block inventory collection. Overall deployment `acceptance` remains
`not-evaluated`, even when `discovery.outcome` is `complete` and the command exits zero.

Explicitly select `discovery.probes.mode: inventory`. The default `management`
mode is unavailable in this preview; selecting it stops the command before
remote access. Inventory reports management topology when a kubeconfig is supplied,
but does not test management-network connectivity. The separate experimental
[Sentry BF4 workflow](dpf-sentry.md) provides generation, deployment and resource
validation for an existing DPF environment. The root DPF pipeline remains unavailable.

## Inputs and invocation

Supply explicit host endpoints and DPU BMC endpoints for a complete inventory.
Each endpoint selects exactly one source: Redfish (`bmcEndpoint`) or SSH (`ssh`).
A host may contain several selected or unselected cards. With no explicit DPU
endpoints, BlueField cards are derived from host observations; their BMC/ARM
endpoints remain unknown and discovery is partial with a nonzero exit status. ARM-only
DPU sources also leave the required BMC administrator check failed. With explicit DPU endpoints, every selected source must
succeed and its card identity must match exactly one host adapter. Sources are not
silently substituted after access failures. The CLI does not scan subnets.

```yaml
management:
  kubeconfigRef:
    file: ./credentials/management.kubeconfig
  context: management
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
  probes:
    mode: inventory
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
| Separate Sentry workflow | Explicit device/tenant bindings, deployment release, OS/firmware hashes, local charts, service networking and existing controller/Secret access; see [required deployment inputs](dpf-sentry.md#prepare-the-deployment-configuration) |

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

The CLI warns on stderr unless quiet output is requested. Your `discovery` input
retains the selected TLS policy; `inventory.scopeDigest` protects refresh against
a changed scope or trust policy. The inventory does not repeat the configured
endpoint/trust list as `requestedScope`. A selected policy does not establish
successful TLS or hardware verification. Use a fresh input without
saved `inventory` and a new output when changing the verification policy or custom
CA contents. This BMC option does not relax management kubeconfig TLS verification.

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
Omitted/null selects the `26.4.0` default; explicit empty or unknown releases
are rejected. The resolved BOM is saved under `inventory.software`, with operator
and component coordinates, exact hardware models and BFB version/URL plus a
digest when known. Documented minima use named fields such as
`minimumNICFirmware` and `minimumHostMFT`. Omitted minima remain unknown in
software checks; they are not inherited from a newer catalog. Catalog source
references, revision lists and generic requirement objects are not saved.
The current catalog has exact BlueField-3 models `B3210`, `B3220` and `B3240`; generic family names and BlueField-4 do not
establish applicability. Desired BFB/component coordinates are not measured
versions. Only documented minimum versions are compatibility checks; unsupported
firmware requirements remain unknown. Model applicability and software checks do
not qualify hardware. Sentry generation validates its separately selected BF4
profile, desired cohort and explicit enrollment bindings; this discovery BOM is
not a substitute for those deployment inputs.

On the supported BF4/B4240 Redfish reader, `inventory.dpus[].psid`
records a successfully verified PSID from the selected System's `AssetTag`, falling back to the
identity-selected chassis's `AssetTag`. Missing, null, empty and `N/A` values
remain unknown. Both values, when supplied, must be bounded ASCII identifiers
and agree exactly; the selected value keeps its original spelling. The
same identity-bound source fields are reread before the PSID becomes observed.
This is a Redfish fact obtained from authenticated `AssetTag` reads;
it does not create an SSH/runtime observation or infer a PSID from a model.

Failures and missing PSIDs are recorded in `issues` with `field: psid`.
`PSID_NOT_EXPOSED` and `PSID_READER_UNSUPPORTED` mean this source supplies no PSID.
`PSID_INVALID`, `PSID_CONFLICT`, `PSID_CHANGED` and `PSID_RECHECK_FAILED` leave
no eligible `psid` value. These optional gaps preserve
physical inventory and discovery's existing acceptance policy. A downstream
consumer requiring PSID must require a single observed value associated with the
selected device and reject contradictory or invalid readings across its sources;
it must not turn an unavailable Redfish value into success by substituting SSH.
The PSID fact and eligibility contribute to the inventory fingerprint and refresh
diff. Release qualification and administrator authorization remain separate checks.

SSH can report NIC firmware/PSID, MFT version, NVConfig and privilege observations
when the existing tools and permissions permit the read. Current and next-boot
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
the saved detailed report retains each reason. Completing a read does not mean
all optional facts were available. Concurrent endpoints can interleave whole
status lines. `--quiet` suppresses progress; it does not change acceptance.

The final text report uses configured endpoint names, falling back to addresses,
and shows DPU model/serial, host association and BMC administrator status. It
separates problems that prevent discovery from prerequisites for provisioning
and checks not requested in this run. Identical later-stage findings are grouped
with their affected sources. An unqualified hardware combination means this
build has not established qualification; it is not a detected hardware fault.
An unsupported BOM model means software compatibility is unknown. Neither is
silently marked passed, and the JSON check states and exit rules are unchanged.

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

### Internal identifiers in saved inventory

The detailed YAML/JSON retains identifiers for automation and traceability:

| Field | Meaning |
| --- | --- |
| Device `recordKey` | Deterministic key for a selected observation source; used to connect checks to that device record. Normal text shows its name/address instead. |
| `scopeDigest` | Digest of the selected scope and recorded trust policy, used to detect incompatible refresh inputs. |
| `fingerprint` | Digest of selected material facts, used to compare inventories. |

These `sha256:` values are internal identifiers/comparison digests, not secrets,
signatures or independent proof that hardware is correct.

Host and DPU records omit evidence maps, reference lists and raw vendor resource
metadata. Named facts and field-specific issues remain available for review.
Management observations and connectivity probes retain their own source metadata.
A refresh preserves source backups; removing old device metadata alone does not
report a physical change.

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
| 2 | Invalid input, incomplete required identity/mapping or DPU BMC administrator evidence, unavailable management mode or proven mismatch |
| 3 | Required observation, authentication, TLS, API or remote cleanup failure |
| 1 | Cancellation or local output error |
