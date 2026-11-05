<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# DPF discovery preview

`l8k discover --target dpf` collects hardware inventory from explicitly selected
host/DPU Redfish or SSH endpoints. The DPF estate is independent of Host networking.
Inventory mode can run before Kubernetes exists; an explicitly supplied management
kubeconfig adds management observations and must work if supplied.

This build exposes an **experimental inventory preview**. Inventory success means
that selected sources were read, physical identities and unique host/card mappings
were established, and cleanup/publication succeeded. It does **not** mean DPF
provisioning readiness or production hardware qualification. Missing optional card
metadata remains visible, including cards without serials. The hardware qualification
catalog remains empty; its failed qualification checks are required for future
`generate`, not for inventory collection. Overall deployment `acceptance` remains
`not-evaluated`, even when `discovery.outcome` is `complete` and the command exits zero.

Management probes use an existing digest-pinned diagnostic image with Go
orchestration. The catalog pins `docker.io/nicolaka/netshoot:v0.16`, but Kubernetes
runtime qualification is pending. Default `management` mode rejects locally until
that cohort is qualified; explicitly select `inventory`. There is no automatic
downgrade. Management mode retains the three-control-plane topology requirement;
inventory merely reports that later-phase check. DPF generate/deploy/validate and
the root DPF pipeline remain unavailable.

## Inputs and invocation

Supply explicit host endpoints and, optionally in inventory mode, DPU endpoints.
Each endpoint selects exactly one source: Redfish (`bmcEndpoint`) or SSH (`ssh`).
A host may contain several selected or unselected cards. With no explicit DPU
endpoints, BlueField cards are derived from host observations; their BMC/ARM
endpoints remain unknown. With explicit DPU endpoints, every selected source must
succeed and its card identity must match exactly one host adapter. Sources are not
silently substituted after access failures. The CLI does not scan subnets.

```yaml
management:
  kubeconfigRef:
    file: ./credentials/management.kubeconfig
  context: management
credentials:
  dpu-reader:
    username: bmc-reader
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

The YAML is ordinary CLI configuration. `--target` chooses the schema; generated
facts live under `inventory`. There is no CRD wrapper. Unknown fields, duplicate
keys, wrong scalar types, ambiguous contexts and duplicate endpoints fail before
remote access. Lists and credentials stay in YAML; no password CLI flag exists.

| Source | Values |
| --- | --- |
| Required site inputs | Nonempty host list; each selected endpoint and credential alias; username and password reference or SSH private-key reference |
| Defaultable | Context from a supplied kubeconfig; verified system trust; SSH port22; SSH sudo false; collection 10m; request 15s; four endpoint collectors in flight (`maxConcurrentBMCs`) |
| Conditional | Kubeconfig and explicit DPUs for management mode; SSH known-hosts file; approved noninteractive sudo for protected reads; private Redfish CA; `systemURI` for multi-System BMCs |
| Observed | Physical-card/system identities, linked host adapters, component firmware, auth/time unknowns, management nodes/version |
| Release owned | Qualified adapter completeness predicates and the probe image/architecture digests |
| Later phases | Profile/release cohort, DPU API allocation, DHCP/service networks and controller authentication |

The management kubeconfig must use verified HTTPS and certificate/key or bearer
credentials. File and embedded representations of the same credential/trust field are mutually
exclusive; choose token or tokenFile. Exec/auth-provider plugins, basic and anonymous access are rejected
in this version. Ambient kubeconfig files are never merged. BMC authentication
uses one session login using the supplied username/password; Basic fallback is
not included in this preview. Login uses the public service root's `Links.Sessions`
when advertised, avoiding an unauthenticated read of a protected `SessionService`.
If absent, the client reads the advertised standard `SessionService` to locate
its sessions collection. Session writes are restricted to the standard
`/redfish/v1/SessionService/Sessions` collection and its exact returned session
child; same-origin absolute links and optional trailing slashes are supported.
Alternate OEM session paths are unsupported. No password guessing or credential repair occurs.

Passwords resolve only from `{env: NAME}` or `{file: path}`. Environment values
are verbatim. UTF-8 files lose at most one final LF/CRLF. File, SSH known-hosts and CA references
are relative to the input YAML and are rebased when saved elsewhere. Private files
are not copied. Endpoint origins require HTTPS; redirects, cross-origin links,
unbounded pages and arbitrary action endpoints are rejected.

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
for executor-to-BMC Redfish connections only. HTTPS remains required, but the
server is not authenticated and BMC credentials can be exposed to impersonation.
Prefer a verified certificate/CA through `caFile` when available.

The same field is accepted under `discovery.tls` as a shared BMC policy. An
endpoint `tls` block replaces the entire shared policy; `{insecureSkipVerify: false}`
restores system-trust verification for that endpoint. Omitted/null values use the
policy's default. `caFile` and `insecureSkipVerify: true` cannot be combined in the
effective policy. TLS policies are independent even when endpoints share credentials.

The CLI warns on stderr unless quiet output is requested and records
`tlsInsecureSkipVerify: true` on the affected `inventory.requestedScope` entries
in JSON and saved YAML, including failed observations. This records the selected
policy, not successful TLS or hardware verification. Changing the policy changes
the scope digest: use a fresh seed and new output when switching policies.
Management kubeconfig TLS and management-network probe TLS checks remain verified;
this BMC option does not relax them or qualify hardware.

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

This example needs no management kubeconfig or Redfish credential. Each SSH
credential uses exactly one password reference or unencrypted private-key file
reference. SSH host keys must match the explicitly supplied known-hosts file;
unknown/changed keys fail. Establish that trust through your site's trusted
inventory/onboarding process. `tls.insecureSkipVerify` never applies to SSH.

Host collection reads the system UUID and NVIDIA network PCI/VPD inventory,
including per-function network-device/driver/NUMA facts where available. It does
not claim an all-vendor hardware census. DPU ARM identity requires agreement
between system DMI serial and the NVIDIA BlueField built-in IPMI FRU serial.
OS interfaces, routes, DNS and local BMC LAN address are additional observations.
Supported remote tools must already exist; discovery installs nothing and invokes
only fixed read commands through Go SSH, optionally with `sudo -n`. It prompts for
no sudo password, changes no privileges and uses no remote Python helper.

ARM discovery needs a booted, reachable OS with sufficient permissions. Local
`ipmitool lan print` can report the BMC endpoint without Redfish. The endpoint is
recorded as `observed-local-ipmi`; discovery does not connect or send credentials
to that newly observed address. Missing optional tools/data remain unknown. It
never infers a BMC address from an ARM IP or a serial from a hostname.

Host and ARM PCI addresses belong to different domains; correlation uses physical
serial identity, not matching PCI addresses. Host-reported identity alone does not
attest trusted DPU state. Source evidence, SSH trust digest and any conflicts stay
in the output. SSH inventory success does not establish Redfish authorization,
controller mTLS enrollment or provisioning readiness. Those checks remain separate.

## Observations, reports and refresh

Physical identity comes from source-specific card/system fields, keeping raw serials
and evidence. Ports/PCI functions, CPU serials and addresses cannot substitute for
card identity. HPE collection follows explicit System/chassis/PCI relationships;
shared-chassis records require an ownership join. Partial adapters retain their
PCI/slot facts without fabricated serials. No supported release tuple is implied.
Every requested endpoint remains represented on failure; unrelated cards remain
outside selection. Unknown firmware/auth/time values stay unknown. Qualification
requires both a reviewed model/firmware tuple and an adapter-specific completeness
predicate; no site input can waive that requirement.

Complete discovery writes the requested YAML. Partial collection preserves an
existing handoff and writes `partial.yaml` plus `report.json` beneath
`<output-stem>.discovery/<run-id>/`. JSON stdout is one final DPF result, with
progress on stderr; syntax errors before dispatch retain normal Cobra stderr.
The report contains facts, checks, factual diffs, later-phase gaps, errors and
successfully written artifacts. Redfish errors retain sanitized method/resource,
HTTP status and observation/cleanup stage. Cleanup failures retain the exact
validated session URI when known, never its token. Verify ownership before manual
recovery; an unknown cleanup result is not success. Authentication at session
creation does not establish permission to read protected inventory. Saved YAML alone contains the factual handoff.
Explicit in-place refresh requires the same input/output path; it keeps
`source-before.yaml`. Stable sidecar locks serialize cooperating writers. External
edits between the final hash check and rename cannot be excluded; avoid concurrent
editing. Output paths must use real directories without symlink aliases.

| Exit | Meaning |
| --- | --- |
| 0 | Explicit-mode checks, cleanup and publication completed |
| 2 | Invalid input, incomplete required identity/mapping, unavailable management mode or proven mismatch |
| 3 | Required observation, authentication, TLS, API or remote cleanup failure |
| 1 | Cancellation or local publication/encoding error |

## Management probe implementation and qualification

Launch Kit builds no discovery image. The release catalog pins the existing
[`nicolaka/netshoot:v0.16`](https://github.com/nicolaka/netshoot/releases/tag/v0.16)
image by manifest and Linux amd64/arm64 digests. A fixed, embedded diagnostic
adapter invokes its `getent`, `curl`, `ip` and `jq` tools; the Go CLI resolves
destinations/public trust and validates the versioned JSON results. No Python,
runtime package installation, downloaded helper or site-supplied command is used.
The image repository can be mirrored without changing its pinned digest.

The NVIDIA candidate
[`nvcr.io/nvidia/k8s/toolbox`](https://catalog.ngc.nvidia.com/orgs/nvidia/k8s/containers/toolbox/latest/layers)
has suitable tools, but its inspected current image is based on Ubuntu 23.04 and
was built in September 2023. DOCA 3.3.0 base/full runtime images were also inspected;
both lack `curl` and `ip`. Netshoot is the selected existing-image alternative.

The runner uses an existing namespace, an immutable ConfigMap containing the
public request, public CAs and fixed adapter, and exact run-owned Pods. It creates no namespaces, RBAC, Secrets,
operators or DPF objects. Each management node has separate Pod-network and
host-network probes. Admission, node/Pod UIDs, image identity, request immutability
and full row coverage are checked before evidence is accepted. No BMC credential
or kubeconfig is sent to a probe. Pods run as non-root with a read-only root
filesystem, dropped capabilities and a bounded memory-backed scratch volume.
Each resolved IP remains a separate observation, including failed alternatives.

Cleanup checks exact UID/resourceVersion and admitted-spec ownership, preserving
foreign modifications. Receipts are journaled atomically before creates and after
UID receipts. Ambiguous creates are inspected by exact name/spec; unresolved
outcomes remain explicit leftovers. SIGKILL can leave resources; consult
`resources.json` and verify current UIDs/ownership before operator recovery.
Automatic cleanup of other runs is not implemented.

The published manifest/platform digests and local non-root tool execution have
been checked. The opt-in `dpf_probe_image` Go test tag exercises the fixed adapter
inside the existing image against local DNS/TLS/HTTP fixtures; set
`DPF_PROBE_TEST_IMAGE` to the immutable catalog reference when running that test.
It defaults to Linux amd64; set `DPF_PROBE_TEST_PLATFORM=linux/arm64` for arm64.
Local containers do not establish Kubernetes network paths or admission behavior.
Before enabling management mode in the release catalog, exercise host-network
admission, scheduling, successful log collection, cancellation and cleanup on
a controlled cluster, and qualify actual vendor resource graphs, component
versions, physical-card correlation and completeness predicates. Software fixtures
establish parser/control-flow behavior; they do not establish physical hardware
support. Discovery performs no firmware, NTP, trust enrollment or power action.
