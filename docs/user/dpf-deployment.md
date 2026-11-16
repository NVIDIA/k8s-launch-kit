<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Deploy HBN-BGP on DPUs

Use this procedure after [DPF discovery](dpf-discovery.md) and review of the
[generated HBN-BGP bundle](dpf-generation.md). Launch Kit uses an existing
management Kubernetes cluster and its required DPF foundations to provision the
selected DPUs, create their independent tenant control plane, configure each
selected host's workload interface and check HBN routing and traffic. The
[deployment workflow](dpf-workflows.md#hbn-bgp-deployment) gives the step-by-step
operations and decision paths.

The management Kubernetes cluster, required foundations and aligned switch
fabric must already exist. Launch Kit does not create the management VM or
configure switches. Deployment can replace selected DPU operating systems,
change DPU settings, reboot selected hosts and configure their workload-facing
interfaces. Use a maintenance window that excludes other device provisioning,
firmware writers and host maintenance. Keep the CLI machine online and able to
reach the management cluster and selected hosts during the run; host reboots can
interrupt access temporarily.

The current BF4/HBN-BGP cohort remains beta. A successful deployment reports the
selected provisioning, BGP, route and traffic checks; it does not qualify other
hardware or release combinations.

## Management-cluster prerequisites

Before starting, install versions compatible with the selected DPF release of
cert-manager, Argo CD, the Node Feature Discovery (NFD) API and master, NVIDIA
Maintenance Operator, and Kamaji. Kamaji needs a healthy shared etcd DataStore
and the supported local-path storage backend. Storage resource names may vary
when the DataStore endpoints and Kubernetes owner references consistently
identify the same etcd workload. The endpoints may use a custom Kamaji service
DNS domain when the complete member set uses one consistent canonical suffix.
See the
[management and storage prerequisites](dpf-discovery.md#check-hbn-bgp-prerequisites).
Launch Kit checks these components and their access; this deployment command
does not install them.

## Prepare the site and review the bundle

Keep the discovered site file, generated directory and its metadata together.
Provide management-cluster access and explicit, trusted access to the selected
hosts, DPU BMCs and switches. The CLI machine needs direct connectivity to these
endpoints. The management nodes also need the paths reported by discovery; CLI
reachability alone is insufficient. Each selected host must be able to boot its
existing local operating system after a system reboot. Host firmware setup is
outside this procedure.

The CLI machine must also reach the retained HBN image's manifest and config
metadata over anonymous HTTPS. The bounded verifier supports only anonymous
access or an anonymous Bearer token exchange on the same registry origin; it
does not use Docker credentials, kubelet pull credentials or credential providers.
A custom repository that requires Basic authentication or a foreign-origin token
realm is unsupported by this CLI verifier even if Kubernetes can pull it.
Deployment checks this exact retained image and its Linux/arm64 metadata before
any discovery probes or cluster changes, including on every `--resume` attempt.
An early pass proves current metadata access and identity; it does not prove
future image pulls, running containers or traffic. Runtime acceptance reads the
metadata again.

For the current BF4 enrollment path, each selected DPU BMC must grant
Administrator access, use the `admin` username and resolve to the same nonempty
password value. Separate protected credential aliases may reference that value.
Use protected password/key references and pinned SSH host keys; do not put
secrets in command arguments or generated manifests.

The selected release must support each observed DPU model and BMC firmware build
for host reset. Discovery reports this prerequisite; deployment verifies it
again before provisioning changes. An unsupported or unmeasured build blocks
the run. Live shutdown, task and firmware-copy checks still precede every reset.

Confirm these site decisions before generating the bundle:

| Decision | What to establish |
| --- | --- |
| Selected devices | Each DPU maps to the intended physical host and adapter. |
| Tenant API | Reserve the VIP, exclude it from DHCP and confirm its VRID is available on the selected control-plane nodes. Declare these facts under `provisioning.bootstrap.tenant.allocation` only with the responsible owner's confirmation. |
| Software | The OS ISO and each selected PSID firmware bundle match the intended DPU cohort; provide credential-free HTTPS URLs and checksums. |
| Fabric | Switch uplinks already have the intended port bindings, fabric VRF, BGP neighbors/policy, ASN and MTU. Deployment observes this configuration; it does not apply it. |
| Host workload network | The selected PF can be configured without taking over the host management interface, another port or its default route. |
| Host recovery | The selected host can return to its existing local operating system after a requested system reboot. The host OS reports no virtual machine, and its SSH and default-route paths have measurable PCI ancestry on a separate card from the selected DPU. |

Selected HBN hosts must be bare-metal Linux systems with the supported systemd
VM detector available. Discovery requires the OS to report no VM; missing tools,
unknown results and detected VMs stop before provisioning.

The native HBN deployment path requires
`provisioning.bootstrap.bmcFactoryResetPolicy: Never`, preserving existing BMC
network settings. A setting accepted by the general configuration schema does
not imply that the native deployment path supports it; see
[configuration reference](../reference/configuration.md#bootstrap-and-hbn-defaults).

`dpf.hbn.fabricVRF` describes the VRF expected on the selected switch uplinks;
the profile default is `default`. DPU FRR uses its default VRF. These are separate
routing roles, so review the switch fabric and DPU routing settings independently
in the resolved bundle. The release catalog provides the HBN-BGP release and
artifact defaults; the profile provides defaultable ASN, address-pool and MTU
values. Override the site file only when needed, then
regenerate and review a new bundle. Do not edit files in an existing generated
directory.

For complete field names and profile values, see
[HBN-BGP generation](dpf-generation.md) and the
[bootstrap and HBN configuration reference](../reference/configuration.md#bootstrap-and-hbn-defaults).

## Run deployment

```bash
l8k deploy --target dpf \
  --user-config dpf-discovered.yaml \
  --deployment-files ./hbn-bgp \
  --deploy-timeout 2h
```

The native workflow checks every directed host pair sequentially, with three ordinary
and three DF packets at one-second intervals. Packet spacing alone requires `4 × N × (N
− 1)` seconds for `N` selected physical hosts. Discovery and generation reject a matrix
whose lower bound cannot fit the two-hour native maximum. This rejects 43 or more
selected hosts under the current protocol; fewer hosts are not guaranteed to finish
because provisioning, SSH and API checks take additional time. Before image metadata
access or discovery probes, both fresh and resume runs reject a remaining deadline that
cannot cover the selected matrix. The CLI checks the budget again before creating a
first DPUDeployment, after preparation. A shorter timeout can therefore reject a smaller
cohort. Each resume still measures the entire matrix; earlier traffic passes do not
accumulate.

Both file arguments are required explicitly. The default and maximum DPF timeout
is two hours; a shorter positive timeout is supported. For automation, add
`--output json`, capture stdout separately from stderr and check the process exit
status. Keep the report path and partial effects from a failed run for inspection.

The DPF target does not support `--dry-run` or Host `--overwrite-existing`
semantics. It does not use Host `--kubeconfig`; management access comes from the
site file. This is an initial deployment procedure, not a general upgrade or
adoption path.

## What runs where

| Operation | Where it runs | What must be observed before continuing |
| --- | --- | --- |
| Verify HBN image metadata | The CLI reads bounded anonymous HTTPS registry metadata for the exact retained image. | Manifest/config digests and Linux/arm64 identity are verified before discovery probes or cluster changes; this is repeated on resume and again at runtime. |
| Refresh deployment prerequisites | The CLI reads selected hosts, BMCs, switches and the management API. | Required identities, access and desired settings still match the reviewed bundle; required unknown or failed facts stop deployment. |
| Install DPF management | The CLI uses the management Kubernetes API and retained chart; DPF controllers run on the management cluster. | Operator and configuration are ready with intended ownership. Conflicts are reported rather than silently adopted. |
| Create the DPU control plane | DPF and its controllers create the selected tenant cluster on the management nodes; the CLI checks through management and tenant APIs. | The tenant API and backing prerequisites are available and match the plan. This does not mean selected DPUs are installed. |
| Enroll selected cards | The CLI supplies the required credential and discovery targets; DPF contacts the selected BMCs. | Each selected DPU is enrolled with the intended device/host identity. The reviewed deployment selectors target those devices later; no manual label operation is required. |
| Stage software and declare HBN | DPF caches the selected OS and PSID firmware; the CLI verifies artifact checksums and creates the selected flavor, IPAM, interface, service and DPUDeployment declarations through the management API. | Artifact bytes match the reviewed bundle and the declarations target only the selected devices and are accepted. Downloads and accepted objects do not prove installed software or a running service. |
| Install and activate DPUs | DPF controllers install the OS and DPU agent. Before a requested host reboot, the CLI checks the request, safe shutdown state and current device, host and deployment prerequisites. After the host and DPU reconnect, it checks current prerequisites again before acknowledging the request. | The host and selected DPU return with the expected boot observations within the configured deployment timeout, and DPF reports them ready. |
| Recheck after activation | The CLI collects current host, BMC, switch, management, storage and access facts again. | All current host PF, backend and cabling checks pass for the same operation and devices before workload changes or traffic; pending-reset allowances no longer apply. |
| Configure host workload networking | The CLI reads each actual DPU IPAM allocation through the tenant/management APIs and configures the matching host PF over host access. | The allocated address, selected interface, MTU and workload route are applied and verified while preserving management connectivity and the default route. |
| Check HBN routing and traffic | The CLI reads the intended HBN containers and routing state through the tenant API, then probes selected hosts over host access. | Required BGP peers, remote workload routes and every directed ordinary and do-not-fragment (DF) traffic check pass. A one-host result cannot establish cross-host traffic. |

The table shows operation dependencies and Launch Kit checkpoints; DPF
controllers reconcile resources asynchronously, so HBN startup can begin after
DPU agents join while deployment is still progressing. Launch Kit waits for the
selected service and interface resources and performs final HBN acceptance only
after host workload networking. Do not treat a declared service, ready artifact
cache or DPU provisioning result as proof of BGP or traffic.

Each routing and traffic measurement rechecks the complete selected runtime
cohort, allocations and container identities. Physical DPU/BMC identity and
Administrator access are reverified at the start and final acceptance of the
runtime phase. A failed final physical or runtime check prevents `validated`,
even when earlier measurement rows passed. Provisioning, resets and host-network
changes retain their immediate full physical safety checks.

## Read the result and recover from a stop

On a fabric ownership stop, inspect the recorded allocation, cabling or route-path reason. Preserve existing routes and resources; correct access or resolve conflicting ownership with the infrastructure owner before rerunning.

`validated` means the selected multi-host BGP, installed-route and directed
traffic checks passed at the time of observation. A one-host run can be
`provisioned`, but cross-host routing and traffic remain unmeasured. Neither
state is a general hardware qualification, performance result or proof that
every failover path was exercised.

If deployment stops, retain the discovery handoff, reported operation records,
stdout and stderr. A stop can leave earlier management, tenant, enrollment,
software or host-network effects in place. Inspect current resources and device
state before another attempt, especially after timeout or interrupted host
reboot. Do not retry a host reset when the report does not establish whether it occurred.

Before creating the DPUDeployment, the CLI reserves a durable operation in the
existing operator namespace and reports its `namespace/name/UID` reference. The
caller needs ConfigMap create permission and get/update access to that operation
record. Keep the reference with the unchanged bundle and site file. Resume the
same initial deployment explicitly:

```bash
l8k deploy --target dpf --user-config dpf-discovered.yaml \
  --deployment-files ./hbn-bgp --resume NAMESPACE/NAME/UID
```

Use the exact reference printed by the failed run; JSON results expose it as
`operation.namespace`, `operation.name` and `operation.uid`. Each attempt writes
new reports. Before a first DPUDeployment is created, resume also rechecks the current chart
and cached ISO/PLDM bytes. An already-created exact DPUDeployment does not
refetch those bytes merely to retry runtime checks. Resume verifies the retained
management, tenant, selected-device,
software and workload identities instead of reinstalling them. It continues
supported activation work and inspects completed host transactions. After a
late runtime-check failure, it repeats the complete current BGP, route and traffic
matrix without reimaging devices or reconfiguring completed hosts. It does not
reuse an earlier traffic pass.

Use the same saved operation, configuration and bundle for resume. The CLI
rechecks current host, BMC, switch, operator, storage and access prerequisites,
and recognizes only resources, addresses and routes proven to belong to that
operation. Standalone discovery checks readiness for a new deployment; do not
delete an operation's enrollment, tenant, host addresses or routes just to make
those checks pass. Changed or unverifiable ownership stops recovery and
preserves the existing state. During an operation-owned pending reset, a selected
DPU's host interfaces can temporarily disappear. Resume proceeds only when it
can still verify that device, its host and separate management connectivity.
After activation it collects current host and network facts again; workload
configuration and traffic wait for those checks to pass. Do not reset a host
manually to make discovery pass. Resume repeats the complete traffic checks.

Before a requested reset or its acknowledgment, an unregistered DPU must still have
current bootstrap credentials. The CLI also checks these credentials while waiting for
initial tenant registration. A stop for missing, expired or unverifiable credentials
requires recovery with the deployment owner; preserve the operation and reset requests.
Resume does not renew tokens, extend their lifetime or repair enrollment credentials. A
registered Node can be NotReady for another reason; its separate readiness checks still
apply.

During a requested reboot, temporary SSH connection or negotiation failures stay
within the remaining deployment wait. A changed host identity or SSH trust stops
recovery. If completion remains uncertain, read the recorded SSH reason before
continuing; an uncertain result never permits another reset.

An existing reset claim permits observation only. Recovery needs the saved
host baseline, a changed boot on that same host/PF, and a new current Arm boot
for the exact pending DPU request before conditional acknowledgment. Missing
proof, an uncertain hold result, a partial host transaction, replaced resources,
or changed inputs stop with an action-required/conflict reason. A claim is never
expired, deleted or replaced to permit another reset. Coordinate ambiguous
physical outcomes with the responsible operator; do not bypass the claim or
edit the checkpoint. Earlier releases' diagnostic-only reports cannot authorize
resume. Before the operation reservation exists, a normal fresh invocation still
uses the existing create-or-verify checks and may reject unresolved partial work.

A matching tenant or resource name does not prove it is safe to reuse. Inspect
ownership, active workloads and network state. Coordinate any selected-tenant
cleanup with its owner and preserve unrelated resources. If desired input or
selection changes, update the site file, repeat discovery when observations
changed and generate a newly reviewed bundle. Host `l8k clean` is not DPF
teardown.
