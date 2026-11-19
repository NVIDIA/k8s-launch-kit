<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Validate an existing DPF HBN-BGP deployment

`l8k validate --target dpf` checks the current deployment against its retained
generated bundle. Use the [standalone validation procedure](dpf-workflows.md#hbn-bgp-validation)
to run the checks and interpret the result. This command supports the generated
HBN-BGP deployment; the current BF4/HBN-BGP cohort remains beta.

Supply the discovery inventory with explicit management access and the complete
reviewed deployment directory. Add an overrides file only when needed; its
supported settings must agree with the retained deployment. Relative access-file
references remain relative to the inventory file. Passwords and keys belong in
referenced private files or environment variables, as described in the
[DPF input reference](../reference/configuration.md#dpf-discovery-configuration).

```bash
l8k validate --target dpf \
  --inventory dpf-discovered.yaml \
  --deployment-files ./hbn-bgp \
  --wait 10m
```

The inventory and bundle arguments are required explicitly. Management access
comes from the inventory, including its selected context. The DPF command does
not select an ambient Host kubeconfig. Source selection, trust policy and desired
deployment settings must still match the retained handoff. Credential rotation
alone does not authorize a different endpoint, trusted peer or deployment.

## Choose the scope

| Scope | What is checked | What success means |
| --- | --- | --- |
| Full, the default | Current management and tenant identities; retained operator/chart definitions and resource intent; selected controller dependencies; device enrollment and reported software; HBN allocations, interfaces, chains and running container identity; required physical BMC, host and switch facts; host workload configuration; BGP, installed remote routes and every directed cross-host ordinary and DF traffic pair. | Every required check and the complete selected measurement coverage passed. |
| Resources, with `--connectivity=false` | The same selected API resource, dependency, enrollment, reported-software, allocation and running-workload checks, including retained software/cache object health and current container image identity. | **RESOURCE CHECKS PASSED**. Physical access, host networking, switch topology, BGP, installed routes and cross-host traffic were outside this request. |

Use resource-only validation when you need a current API-level assessment:

```bash
l8k validate --target dpf \
  --inventory dpf-discovered.yaml \
  --deployment-files ./hbn-bgp \
  --connectivity=false
```

Resource-only validation does not contact host, switch or BMC endpoints. It still
requires structurally valid inputs and access to the selected APIs and required
container registry metadata. Full validation needs the declared physical access
and at least two selected physical hosts for cross-host coverage. A one-host
resource-only run may pass; a one-host full run reports missing cross-host
coverage and cannot pass full acceptance.

Runtime diagnostics require a verified physical selection and qualified host
network configuration across the selected cohort. An unavailable BMC or host
prerequisite can leave BGP, route and traffic checks unverified for that run;
independent Kubernetes resource checks remain visible. Restore the reported
prerequisite and rerun full validation.

## What runs where

| Location | Operations |
| --- | --- |
| CLI machine | Read the inventory, optional overrides, retained bundle and referenced local access material; verify that selection and desired settings agree; prepare the local report destination. |
| CLI machine to management and tenant Kubernetes APIs | Read current identities, Helm release metadata, resources, owners, status, selected Nodes, Pods and containers. Check required access. Existing controllers continue their own reconciliation. |
| CLI machine to the release chart repository | Download the selected system-networking chart, verify its recorded digest, and derive the expected system resources and configuration. |
| CLI machine to the retained container registry | Read bounded HTTPS image metadata needed to verify the selected running image. Registry metadata access does not prove a future image pull or the installed image payload bytes. |
| CLI machine to selected DPU BMCs, in full mode | Open authenticated Redfish sessions, read current card identity, administrator access and available software/MAC facts, recheck identity, and close the owned sessions. |
| Selected hosts and switches, reached over SSH in full mode | Read hardware, interfaces, persistent host configuration, routes and switch configuration/operational facts. When needed, receive LLDP in bounded captures on the exact selected host interfaces. |
| Existing selected HBN containers and source hosts, in full mode | Execute bounded read-only BGP and forwarding-table diagnostics; send source-bound ordinary and do-not-fragment traffic for every selected directed host pair. |
| CLI machine | Write the terminal result and local HTML report from the collected results. |

Validation observes existing state and sends diagnostic traffic. It does not
install or reconcile resources, provision or reset devices, change host
configuration, alter switch forwarding, warm artifact caches, or create deployment
operation records. The supported HBN checks use existing workloads. Redfish
session creation and Kubernetes authorization/exec requests can use HTTP POST
without installing or changing the deployment.

## Waiting and deadlines

`--wait=0` takes one current resource snapshot. A positive `--wait`, such as
`--wait 10m`, bounds the resource-observation phase and permits fresh snapshots
while required checks are pending and no terminal blocker is present. Missing
permissions, invalid or changed identity, failed checks and unavailable required
observations need attention; increasing the wait does not repair them. Pending
checks at deadline remain incomplete. Independent checks remain visible when
another source is unavailable.

Full-mode BGP, route and traffic diagnostics run after resource polling, rather
than on every waiting poll. `--connectivity-timeout` bounds that entire diagnostic
stage separately. Omitted or zero uses `5m + 2m × N + 30s × N × (N − 1)`, where
`N` is the selected physical-host count. For example, two hosts receive an
automatic ten-minute budget. The CLI shows the selected budget before diagnostics.
A positive value replaces it; it must exceed the packet-spacing lower bound
`4 × N × (N − 1)` seconds. That lower bound is not an estimate of completion time.

Negative durations are invalid. Do not pass `--connectivity-timeout` together
with `--connectivity=false`, even as zero. Individual API and SSH requests retain
the inventory's bounded request timeout, independently of these two phase
budgets. Cancellation or deadline expiry leaves partial results and an incomplete
outcome; earlier access or input failures can determine the final exit category.

## Read progress, results and the report

Progress goes to stderr and identifies running checks followed by PASS, FAIL,
PENDING, UNVERIFIED or SKIPPED as applicable. `--quiet` suppresses informational
progress while retaining the terminal result. Read each nonpass explanation and
next action. PENDING means the current selected resource is progressing;
UNVERIFIED means the required fact could not be established. Neither is a pass.
A host subject associated with a selected DPU identifies that host-to-card
relationship; it is not an additional physical host.

The final text gives the requested scope, verdict, counts and checks needing
attention. HTML adds grouped detail and measurement coverage. Full acceptance
requires the complete retained selection: an inaccessible source or disappearing
member cannot be omitted to create a passing smaller matrix. A host reboot,
replaced workload or changed allocation during diagnostics can invalidate earlier
measurements; inspect the final result.

HTML is enabled by default. Each run reserves a private directory beside the
bundle and writes `validation.html` there, for example
`./hbn-bgp-validation-<unique>/validation.html`. Only a successfully published
path is reported. Use `--report-path ./validation.html` for an explicit local
file whose parent already exists, or `--report-path -` to disable HTML. The report
must remain outside the retained bundle and must not overwrite inventory,
overrides, credentials, trust or kubeconfig inputs. An unsafe or unverifiable
explicit destination stops preparation before remote checks. Choose the private
default or disable HTML if an explicit destination cannot be proved safe.

Cancellation still permits publication of accumulated results. A report write
failure remains an error even when every measured check passed; it does not erase
the measured verdict. A failed publication preserves a previous complete report
at the same destination. The report is self-contained and contains reviewed
summaries rather than raw command output, credentials or session tokens.

## Automate without losing the result

DPF validation emits one terminal JSON document once validation starts. Capture
stdout, stderr and the process status separately, including on failure:

```bash
validation_status=0
l8k validate --target dpf \
  --inventory dpf-discovered.yaml \
  --deployment-files ./hbn-bgp \
  --output json >validation.json 2>validation.log || validation_status=$?
if [ -s validation.json ]; then
  jq . validation.json
fi
exit "$validation_status"
```

Read `mode`, `verdict`, `checks`, coverage and `report.status` together with the
exit status. `resource_checks_passed` applies only to `mode: resources`.
`report.path` is useful only when `report.status` is `published`. Disabled HTML
is reported explicitly; a bad destination or failed publication is not a missing
optional success detail. Invalid command binding can fail before the DPF result
exists, and stdout failure can prevent delivery even when HTML was written.

| Exit | DPF validation meaning |
| --- | --- |
| `0` | All required checks for the requested scope passed and requested output succeeded. |
| `1` | Report or stdout publication failed with no higher-priority validation failure. |
| `2` | Invalid arguments, inputs or retained intent. |
| `3` | Required access, cluster identity or selected identity could not be verified. |
| `4` | Failed or incomplete validation, including remaining required pending/unverified coverage, cancellation or timeout unless an earlier input/access category applies. |

The priority is input errors, then access/identity errors, then validation
failure/incompleteness, then publication-only errors. Host validation retains its
own JSON stream, flags, waiting and acceptance behavior; see the
[validation guide](validation.md) and [automation reference](../integrator/automation.md).

## Limits and next actions

Restore unavailable access and repeat validation against the same reviewed
selection. If a required known-hosts or TLS trust policy cannot be established,
physical reads stay unverified while independently safe API checks can continue.
A changed trusted peer or desired deployment requires deliberate rediscovery and
regeneration, rather than editing saved observations. Follow the reported action
for health or configuration failures with the responsible infrastructure owner.
Validation does not perform repairs. Use the [deployment recovery guide](dpf-deployment.md#read-the-result-and-recover-from-a-stop)
when an earlier deployment operation itself remains incomplete.

A pass is a point-in-time result for the selected checks. It does not establish
whole OS/firmware payload hashes, physically installed software bytes, future
image pulls, RDMA function or throughput, every ECMP member, failover, or a broader
hardware/release qualification. Unobservable release requirements remain explicit
limitations; no version minimum or successful measurement is inferred.
