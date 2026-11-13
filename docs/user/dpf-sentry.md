<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Deploy Sentry on enrolled BlueField-4 devices

Use the experimental `sentry-bf4` profile to generate, deploy and verify Sentry
resources for explicitly selected Zero Trust BlueField-4 devices. Start with the
[DPF workflows](dpf-workflows.md) for the complete sequence, including discovery.
This procedure consumes an existing DPF management cluster, DPU tenant cluster
and enrolled devices. It does not create VMs, bootstrap Kubernetes or DPF, enroll
devices, or remove another deployment's ownership.

## Where operations run

| Location | Operations |
| --- | --- |
| CLI machine during `generate` | Read the saved inventory, desired site settings, profile and four local chart archives; validate and save a bundle. No cluster connection, credential read or download occurs. |
| CLI machine during `deploy` or `validate` | Use the explicitly bound management kubeconfig/context, obtain the selected tenant's kubeconfig through DPF, verify cluster/device/namespace identities, and write a separate run report. |
| Management Kubernetes and DPF controllers during deploy | Accept the provisioning and service resources, download the chosen ISO/firmware, provision the selected devices, and reconcile service charts into the selected tenant or management cluster. |
| DPU OS and tenant Kubernetes | Run the configured DPU flavor, passthrough, Tower, policy controller and Argus workloads. Provisioning can reimage a DPU and require a host cold boot. |
| Management service namespace | Run the Sentry policy REST server. Its credential request supplies tenant policy access. |

For a Kamaji tenant with a control-plane VIP, DPF hosts tenant DNS on the
management cluster and exposes it through the tenant DNS Service. Deployment
checks the resources on both clusters; functional DNS resolution is a separate
network check.

Generation, server admission, resource readiness and functional policy enforcement
are separate checks. The profile and selected release do not by themselves
qualify a hardware/software combination for production.

## Prepare the existing environment

Before deployment, arrange these prerequisites through the existing cluster and
device administration process:

- A healthy DPF operator and management services, a healthy selected DPUCluster,
  and supported management/tenant API access. Select an available generation
  release from `l8k schema` under `dpf.generationReleases`.
- Enrolled DPUDevice and DPUNode objects matching the selected inventory's
  physical serials, BMC endpoints, PSIDs and host associations. Devices owned by
  another DPUSet or DPUDeployment require an explicitly authorized external
  transition before this workflow. Launch Kit does not perform that transition. The selected DPUNode must use the
  external reboot method, with no pending reboot or power-off request; arrange
  host power handling separately.
- A dedicated Sentry service namespace that already exists and is Active on
  **both** management and tenant clusters. It must differ from the existing DPF
  enrollment namespace. Launch Kit consumes these namespaces without creating
  or adopting them.
- An image-pull Secret in that service namespace on both clusters, containing
  `.dockerconfigjson` for the selected registries.
- Argo CD repository credentials for the selected OCI and HTTPS Helm chart
  registries, configured in the existing Argo CD namespace. Image-pull Secrets
  and the CLI machine's registry login do not provide this chart access.
- The same MITM CA Secret in that service namespace on both clusters, containing
  `ca.crt`, `tls.crt` and `tls.key`. Use a site-managed RSA CA suitable for Tower;
  distribute only its public certificate to test workloads.
- If LLM inspection is enabled, a tenant Secret containing the real
  `INFERENCE_API_KEY`, plus access to the configured inference service. No
  placeholder credential is accepted as evidence of working inspection.
- Host IOMMU configuration compatible with Argus. The
  [DPF 26.8 Argus requirements](https://networking-docs.nvidia.com/dpf/26.8.0/doca-argus-service)
  specify either IOMMU disabled or IOMMU enabled with passthrough for bare-metal
  hosts. The site administrator must choose and authorize the mode: these
  host-wide settings change DMA isolation and may require host maintenance and
  a reboot. Confirm the running host configuration. The DPU flavor's ARM kernel
  parameters do not configure the host's boot parameters, and Launch Kit does
  not choose or apply a host IOMMU mode.
- An untagged Layer 2 uplink on each selected DPU's `p0`, connected to an
  existing IPv4 DHCP server or configured relay. The DHCP scope must supply an
  address and router usable with `p0`'s MAC: Tower's egress helper temporarily
  borrows that MAC on `net2` to obtain its lease. The router and routing beyond
  it must reach the approved upstream test endpoint. Arrange switch/VLAN, DHCP
  and routing through the network administrator; Launch Kit does not provision
  them.
- Existing DPF BMC credentials, controller client certificates/trust and the
  necessary API permissions. Artifact serving, DPU management connectivity,
  DNS, time, image pulls and host power handling must work for the chosen cohort.
- BMC firmware exposing the Redfish schema supported by the selected DPF
  operator and this deployment preflight, including
  `/redfish/v1/Systems/BlueField_0` and a serial-bearing
  `/redfish/v1/Chassis/BlueField_0`. Discovery can identify older B4240 firmware
  through its legacy component/Card graph; that does not make it provisionable.
  Arrange a supported firmware update through the device administrator before
  enrollment when that schema is missing, then rediscover and enroll through the
  existing DPF process. Generation does not update firmware, and deployment
  does not bypass a failed BMC preflight or substitute SSH access.

For a functional host VF test, also prepare the host's VF allocation, device
resource and network attachment separately. This DPF workflow does not install
Host Network Operator or configure host workload networking.

Core policy enforcement is the default: `sentry.llmInspection: false`. It uses
Tower's fail-closed authorization path and Argus identity without an inference
credential. The chart retains an optional fail-open processing hook when its
inspection sidecar is disabled; core mode makes **no LLM inspection claim**.
Set `llmInspection: true` and supply `inferenceSecret` only when that additional
inspection path is intended and provisioned.

## Prepare the deployment configuration

Copy a successful [discovered configuration](dpf-discovery.md) to
`deployment-site.yaml`, retaining its `inventory`. Add the desired settings
below; replace every angle-bracket placeholder before generating. Do not edit
the inventory to turn an unknown or conflicting observation into a fact.

Every selected device must retain successful discovery evidence:
`bmcAdministratorVerified: true` in typed `v1alpha3` inventory, with an explicit
BMC endpoint and no ARM SSH endpoint. Legacy `v1alpha1`/`v1alpha2` inventory is
also accepted when its retained administrator observation is `state: observed`
with `reasonCode: BMC_ADMIN_VERIFIED`; missing or failed proof is never inferred.
Generation accepts the exact observed model `BlueField-4`, or `B4240`
from the reviewed `bluefield-card-v1` reader, and preserves that measured model.
Unselected records do not need to satisfy this selection gate. If older inventory
or a retained bundle lacks this proof, rerun discovery with DPU BMC administrator
credentials, review its completed report, and generate into a new bundle
directory. Do not add the proof by editing YAML. Saved privileges are an
observation; deployment still verifies live DPF access and its own prerequisites.

Firmware selection uses the PSID measured by discovery. Supported BF4 BMCs can
report it through Redfish; review `psid` and any `issues` for that field in typed
inventory (`observations.psid` in legacy inventory). Existing
host SSH measurements can also provide the PSID. All available measurements
must agree. A malformed, conflicting, changing or incompletely verified Redfish PSID blocks generation
even when another source reports a value. Resolve the recorded cause and
rediscover; do not copy a PSID from a different card or infer it from the model.

| Input class | What belongs here |
| --- | --- |
| Required site intent | Explicit installed DPF release; selected inventory device IDs; existing DPUDevice/DPUNode names; selected tenant name, namespace, resource UID, tenant cluster UID and exact label selector; tenant API server version; ISO and PSID-specific firmware URLs with SHA-256; local chart directory; ingress subnet/address; existing Secret names; reachable management policy-server address. |
| Observed bindings | Device serial, model, BMC endpoint, PSID and unique host association come from the inventory selected by ID. Management cluster UID and API version default from saved management observations; supply them explicitly when absent. Enrollment object names and tenant bindings require explicit inspection and input. |
| Defaultable settings | `dpf.name: sentry`, `dpf.profile: sentry-bf4`, `dpf.namespace: dpf-operator-system`, service namespace `<dpf.name>-services`, policy-server NodePort `30080`, and `llmInspection: false`. Explicit empty required values are rejected. |
| Release/profile pins | Chart versions/archive hashes, workload image digests and release-specific API schemas come from Launch Kit. They are separate from measured installed versions and site-selected OS/firmware artifacts. |

The management kubeconfig file and context are explicit references in the site
file. DPF lifecycle commands do not use `--kubeconfig`, `$KUBECONFIG` or an ambient
current context. Generation records the reference without opening that file;
deploy and validate need access to it. This slice requires self-contained kubeconfigs
with embedded CA and credential data, verified TLS, and no exec/auth-provider
plugins or file-based credential references. Record API **server** versions, rather
than substituting Kubernetes node/kubelet versions.

This is a configuration fragment to add to the discovered file:

```yaml
dpf:
  name: sentry
  namespace: dpf-operator-system
management:
  kubeconfigRef: {file: ./credentials/management.kubeconfig}
  context: management
  # Required here only when the saved management observation is absent:
  clusterUID: "<management-kube-system-uid>"
  kubernetesVersion: "<management-api-version>"
provisioning:
  devices:
    - id: "<selected-inventory-device-id>"
      dpuDevice: "<existing-device-name>"
      dpuNode: "<existing-host-node-name>"
  dpuCluster:
    name: "<existing-tenant-name>"
    namespace: "<existing-tenant-resource-namespace>"
    resourceUID: "<management-dpucluster-resource-uid>"
    clusterUID: "<tenant-kube-system-uid>"
    kubernetesVersion: "<tenant-api-version>"
    selector: {"site.example/tenant": "<existing-tenant-label-value>"}
  software:
    osISO:
      url: "https://artifacts.example/bf4.iso"
      sha256: "<64-lowercase-hex-characters>"
    pldm:
      MT_0000001774:
        url: "https://artifacts.example/psid-matched.fwpkg"
        sha256: "<64-lowercase-hex-characters>"
sentry:
  namespace: sentry-services
  chartDirectory: ./sentry-charts
  ingressCIDR: 192.168.100.0/24
  ingressIP: 192.168.100.1
  imagePullSecret: sentry-pull
  mitmSecret: sentry-mitm-ca
  policyServerAddress: 192.0.2.100
  llmInspection: false
```

Use the actual selected PSID as the `pldm` key. BlueField-4 consumes ISO/PLDM
artifacts; a BlueField-3 BFB is not interchangeable. If the existing desired
cohort includes separate NIC firmware, retain it under
`provisioning.software.nicFirmware` with its URL and SHA-256. Artifact URLs must
use HTTPS without embedded credentials, query strings or fragments.

`sentry.ingressCIDR` and `sentry.ingressIP` configure the `net1` ingress
network only. They do not configure `net2`, an egress gateway or a DHCP service.
The current profile obtains its egress IPv4 address and router through the `p0`
DHCP path described above; a management or BMC lease does not establish that path.

The tenant selector must match exactly the named existing DPUCluster. Arrange an
appropriate label externally if none exists; generation does not add one.
Deployment checks its name, namespace and UID, and checks exact device selector
membership before effects. Reusing a convenient broad selector is insufficient.

## Obtain and verify the four local charts

On the CLI machine, inspect the installed binary's catalog:

```bash
l8k schema > l8k-schema.json
jq '.dpf | {profiles, generationReleases, charts}' l8k-schema.json
```

Using authorized registry access and your normal credential store, obtain each
`dpf.charts` entry from its `repository` at its exact `version`. Put the bytes
under the listed `filename` in `sentry.chartDirectory`, and verify SHA-256 against
the catalog. Use the declared repository type: not every chart is available
through an OCI endpoint. Do not put registry passwords in command-line arguments
or site YAML. Generation rejects missing, modified or mismatched archives and
does not download replacements.

## Generate and review

Run on the CLI machine:

```bash
l8k generate --target dpf --user-config deployment-site.yaml \
  --dpf-release 26.8.0 --dpf-profile sentry-bf4 \
  --save-deployment-files ./sentry-bundle --output json \
  > generate.json 2> generate.log
```

Choose the release that actually matches the installed operator; the example
does not upgrade DPF. Review the result and process exit status. Inspect the
bundle's selected devices, resolved settings, management/tenant destinations,
resource names, device and tenant selectors, and artifact hashes before deploy.

The bundle includes native resources, chart archives, effective values and
validation-only chart projections. DPF owns chart deployment; Launch Kit does
not apply the projections a second time. `.l8k/bundle.yaml` identifies the DPF
target and binds the files; `.l8k/resolved-config.yaml` retains the effective
inputs using typed selected-device facts in resolved format `v1alpha2`. The
bundle index remains `v1alpha1`; valid legacy resolved `v1alpha1` metadata is
accepted with the same administrator and PSID checks. Preserve the bundle
unchanged. To change desired intent, generate a new bundle directory. Repeating identical generation at the same path is allowed;
different content does not overwrite an existing bundle.

## Check admission, then deploy

Server-side dry-run contacts the bound management and tenant APIs, checks live
identity/scope/prerequisites, and submits native resources with dry-run enabled:

```bash
l8k deploy --target dpf --deployment-files ./sentry-bundle \
  --dry-run --deploy-timeout 10m --output json \
  > admission.json 2> admission.log
```

Review every check. An API or admission prerequisite that is not yet served may
be reported as deferred/unresolved. Charts and future chart-created CRDs are
not installed by dry-run, and no runtime readiness is established. Unresolved admission returns an incomplete result and a nonzero exit status.

After reviewing the concrete plan and arranging any authorized ownership
transition and maintenance window, deploy with a budget that includes artifact downloads, firmware and OS installation,
required host power cycles, and service startup:

```bash
l8k deploy --target dpf --deployment-files ./sentry-bundle \
  --deploy-timeout 90m --output json > deploy.json 2> deploy.log
```

The CLI checks existing operator/tenant health, selected identities and ownership,
service namespaces/Secret keys, then creates the bundle's resources in dependency
order. It verifies the downloaded provisioning bytes before each DPUSet creates
physical effects. DPF performs reimaging, host power coordination and service
reconciliation. Launch Kit provisions devices sequentially, waiting for each
DPUSet to become Ready and host transition markers to clear before submitting
the next. Include all devices and required host power cycles in the deployment
timeout. External firmware caches must remain unchanged during provisioning.
A host cold boot also affects co-resident devices and workloads; include that
host-wide effect in the maintenance plan.

After the selected DPU nodes and DNS are ready, Launch Kit submits Tower, the
policy controller and Argus together, then waits for all three. The policy
controller installs the policy API used by Tower, while Tower provides the
policy socket used by the controller. The policy REST server starts afterward
on the management cluster.

Existing foreign owners or changed desired resources stop the operation.
Launch Kit does not silently adopt, overwrite or delete them. Cancellation or a
timeout stops CLI orchestration; it does not roll back controller work already
started. Preserve the report, inspect actual controller/device state and resolve
the reported cause before retrying the unchanged bundle. Upgrades and teardown
are outside this first workflow.

`generate --deploy` uses the same generated bundle and deployment path:

```bash
l8k generate --target dpf --user-config deployment-site.yaml \
  --dpf-release 26.8.0 --dpf-profile sentry-bf4 \
  --save-deployment-files ./sentry-bundle --deploy --output json
```

Add `--dry-run` to that form to check admission after local generation.
Generation alone does not need `--dry-run`. Use separate commands when an
explicit `--deploy-timeout` is needed; that flag belongs to standalone deploy.

## Verify resources and policy behavior

Check the unchanged bundle against current management and tenant resources:

```bash
l8k validate --target dpf --deployment-files ./sentry-bundle \
  --wait 10m --output json > validate.json 2> validate.log
```

This verifies bound identities, desired resource state and selected DPU coverage.
It does **not** run a Sentry allow/deny traffic test or prove LLM inspection.
Deploy and validate reports are stored outside the immutable bundle, beneath
`sentry-bundle.runs/<run-directory>/report.json`; retain these with stdout/stderr.

The pinned Argus chart has no application readiness probe. A Running/Ready
Argus pod or a successful resource validation does not prove that Argus attached
to the host or can identify its processes. Require successful host attachment,
a responding configured query socket, and the expected executable identity for
the VF test process before claiming functional enforcement.

Perform a separate functional test using your prepared host VF workload:

1. Place the test pod on the selected host with its existing VF resource and NAD.
   Confirm the real VF MAC, ingress subnet and route; require a working DPU
   egress address. Mount only the public MITM CA for HTTPS.
2. With no policy for that MAC, send HTTP and HTTPS through the VF interface to
   an approved test endpoint. Expect an attributable authorization denial.
3. Submit a policy for the actual host DPUNode and VF MAC through the policy
   server's `PUT /v1/policy`. Permit a known GET path for `/usr/bin/curl` and
   deny another. Poll `GET /v1/policy?nodeName=...&mac=...` until `applied`;
   writes initially return `202`/`in_progress`. The PUT JSON body contains
   `nodeName`, `mac` and `policy` (the policy YAML as a string).
4. Through the same VF, verify that the allowed HTTP/HTTPS request reaches the
   test server and the denied request receives the expected policy denial.
   Use `curl --interface <vf-interface>`, an explicit `--resolve` when VF DNS
   is unavailable, and `--cacert <public-ca-file>` for HTTPS. An egress `503`
   is not a successful positive test. Confirm Argus identifies the requesting
   process and the controller programs the intended DPU.
5. Delete the test policy through `DELETE /v1/policy`, poll GET until `404`,
   verify baseline denial returns, and remove only the test workload/resources.
   DELETE uses a JSON body containing the same `nodeName` and `mac`.

If LLM inspection was selected, test that path separately with the real service
credentials and inspect its result. Core policy success provides no such result.

### Distinguish network, Argus and policy failures

If Tower's `net2` has no IPv4 lease or usable router, inspect the existing egress
helper's DHCP result and ask the network administrator to verify the `p0` link,
untagged VLAN, DHCP scope or relay, MAC policy and return path. Resolve that
prerequisite before testing permitted upstream traffic. Do not start a second
DHCP client alongside the managed helper.

An attributable authorization denial is a policy result; an upstream connection
failure or egress `503` is not proof of denial or successful enforcement. With a
lease and route present, distinguish upstream reachability from the applied
policy, Argus process identity and controller programming. A positive test must
reach the approved upstream server, and the negative test must produce the
expected authorization denial.

If Argus is Running/Ready but has no responding query socket or host attachment,
inspect its initialization and DMA errors and verify the chosen host IOMMU mode
against the linked DPF requirement. A DPU ARM passthrough setting does not fix a
host translation failure. Resolve the reported prerequisite through the site
administrator, then verify actual executable identity; do not count pod readiness
alone as an Argus functional pass.
