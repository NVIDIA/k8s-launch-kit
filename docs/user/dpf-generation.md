<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Generate HBN-BGP deployment files

After [DPF discovery](dpf-discovery.md), generate the management bootstrap and
HBN-BGP manifests on the CLI machine:

```bash
l8k generate --target dpf --use-case hbn-bgp \
  --inventory dpf-discovered.yaml \
  --save-deployment-files ./hbn-bgp
```

The CLI reads the saved inventory and downloads the selected operator and HBN
charts over HTTPS. It does not log in to hosts, DPUs or switches, read credential
references, or access Kubernetes. Use the separate
[DPF deployment workflow](dpf-deployment.md) to apply the reviewed bundle.
The [workflow diagram](dpf-workflows.md#hbn-bgp-generation) shows every step and
failure path.

Do not combine generation with `--deploy`: generate and review the bundle first,
then run `l8k deploy --target dpf` with explicit `--inventory` and the generated
`--deployment-files` path.

## Prepare the inputs

The current `26.8.0` beta catalog contains one narrow BlueField-4 provisioning
recipe: exact model `B4240` with the observed PSID `MT_0000001774`. The release
catalog selects its software artifacts and maintained HBN device configuration;
discovery must verify the selected model, PSID and artifact bytes from the CLI
machine over HTTPS. The OS ISO and PLDM coordinates refer to restricted internal
distribution endpoints and are not public download links; the CLI execution
environment needs direct access to those catalog-selected sources. Other BF4
models or PSIDs remain unsupported until the catalog contains their exact
recipe. This scoped recipe does not qualify the broader BF4 family or establish
runtime behavior. Do not work around an unsupported selection by adding raw
firmware or flavor fields to your input.

The current BF4/HBN-BGP cohort remains beta. Generation success confirms the
local bundle and its schemas; it does not qualify hardware or runtime behavior.

Use `dpf-discovered.yaml` from discovery. It identifies each selected DPU,
its exact host adapter, verified BMC administrator access and PSID, plus the
management cluster identity and Kubernetes version. It also retains the settings
checked by discovery. If the selection or desired settings change, rerun discovery.

You do not supply firmware URLs/checksums, DPUFlavor, boot configuration, OVS
scripts, Helm values or component versions. Launch Kit selects software from the
release SBOM and renders its maintained HBN-BGP implementation. An unsupported
release/hardware combination stops generation with a catalog-support error.

The optional overrides file changes only supported defaults. For example:

```yaml
hbn:
  memory: 8Gi
```

Apply such a change during discovery, then pass its output to generation. See
[configuration ownership and overrides](../reference/configuration.md#dpf-discovery-configuration).
Tenant VIP/interface and allocation declarations belong in the inventory. The
release fixes the tenant service range; the profile defaults the pod range,
networking and lifecycle policies. Generation checks the resulting address plan.

The native workflow checks every directed host pair sequentially, with three ordinary
and three DF packets at one-second intervals. Packet spacing alone requires `4 × N × (N
− 1)` seconds for `N` selected physical hosts. Discovery and generation reject a matrix
whose lower bound cannot fit the two-hour native maximum. This rejects 43 or more
selected hosts under the current protocol; fewer hosts are not guaranteed to finish
because provisioning, SSH and API checks take additional time. Generation checks this
deterministic native-plan limit without measuring live readiness.

## Plan host workload addresses

Each DPU receives a subnet from the workload address pool. The default pool is
`10.0.120.0/22`, divided into `/29` subnets. HBN uses address index `1` in its
allocated subnet as the gateway, and Launch Kit reserves index `2` for the host.
For example, an allocation of `10.0.120.8/29` gives HBN gateway `10.0.120.9` and
reserves `10.0.120.10` for its host.

The gateway and host address indexes are part of the maintained use case.
Change the workload pool and subnet prefix through supported networking overrides
when needed; you do not need to configure these internal reservations.

The indexes must be different usable addresses within each subnet. Generation
excludes the host address from ordinary dynamic IPAM allocation in every possible
subnet; it does not predict which DPU will receive which subnet. The pool may
contain between 2 and 4096 such subnets. The per-DPU prefix must be more
specific than the pool prefix, so the workload-pool route differs from the
host's connected subnet. A larger pool needs a broader per-DPU subnet or a
smaller workload range.

The host address can only be configured after DPF has allocated that DPU's subnet
and its host-facing interface has been identified. The intended host route covers
the workload pool through that DPU's HBN gateway. It preserves the existing host
default route and management network. Generation prepares the address
reservations; the deploy command configures the host only after observing its
actual DPF allocation and verifying the selected interface.

The profile also carries MTUs through the complete datapath. The DPF high-speed
network and physical service-chain ports use the uplink MTU; the host-facing
service-chain port and host workload interface use the workload MTU. These
values do not change the independent control-plane MTU.

## Review the output

The output directory contains:

- Management namespaces, the DPF operator chart and its rendered preview.
- DPFOperatorConfig, the tenant DPUCluster and one exact BMC discovery target per selected DPU.
- Software/flavor definitions, address pools, service interfaces, HBN service configuration and DPUDeployment.
- The verified chart archives, original source YAML and `.l8k/resolved-config.yaml`.
- An index recording the ordered stages and required deployment checks.

The physical and host-facing interface definitions carry selectors derived from
the intended DPU cluster and selected DPUs' node names. Generation does not check
whether live labels uniquely identify those targets. HBN's resource requests are recorded
both for its container and for DPF's check that the services fit the chosen DPU
flavor. Generation verifies the manifest shapes; actual available resources and
interface readiness still require deployment checks.
Regenerate older preview bundles that omit these selectors or DPF resource
requirements; editing their manifests invalidates the retained deployment plan.

The resolved configuration records the effective defaults and overrides,
including the operator release name and namespace, each selected DPU's BMC
address, the BMC reset policy, the release-selected tenant Kubernetes version,
and the selected OS and firmware URLs and checksums. The management cluster and
DPU cluster have separate Kubernetes versions; generation does not upgrade the
management cluster. Keep it with the manifests so later steps can verify the selected
devices and software without resolving the inputs again. To change the plan,
change the inventory or optional overrides, rerun discovery and generate a new directory; do not edit files
inside an existing bundle.

Generation validates the full bundle before publishing the directory. Repeating
an identical generation leaves that directory unchanged. Use a new destination
for changed inputs; generation does not replace a different or incomplete bundle.

A successful result means the artifacts are ready for review. Discovery blockers
remain unresolved until the environment is corrected and checked again. BGP
adjacency, host traffic, tenant health and consumed artifact versions need live
validation after deployment; they cannot be established by rendering YAML.
