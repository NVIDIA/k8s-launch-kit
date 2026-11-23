<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Maintaining the NVAIE catalog

`pkg/nvaie/catalog.json` is a reviewed documentation snapshot, initially covering
NVIDIA AI Enterprise infrastructure release 9.0. It is separate from the Network
Operator artifact catalog in `pkg/networkoperatorplugin/releases`: NVAIE names
product releases; the operator catalog owns image builds, repositories and chart
coordinates. Resolution joins them by exact operator version and DOCA product
release and rejects incompatible pins. Changing a minor-line catalog entry must
not silently change an existing NVAIE constraint.

To propose a refreshed snapshot from the public NVIDIA documentation:

```sh
python3 hack/update-nvaie-catalog.py --output /path/to/proposed-catalog.json
```

Review the proposed diff before replacing the bundled file. The updater discovers
the fingerprinted data asset through the matrix index, accepts bounded same-origin
HTTPS input, and extracts strict JSON declarations without executing JavaScript.
Its supported source shape and release are intentional review boundaries. A new
release or changed component schema needs parser, matching and release-resolution
tests before it is advertised. Do not suppress a schema failure or fall back to the
source's generic `version` field: release 9.0 uses `version90`.

Cross-check the component pins with the [lifecycle JSON](https://docs.nvidia.com/ai-enterprise/lifecycle/latest/_static/explorer-data.json)
and the [version-specific support matrix](https://docs.nvidia.com/ai-enterprise/release-9/latest/support/support-matrix-9/9.0.html).
Inspect product-specific platform documentation as well; NVAIE and Network Operator
publish different RKE2 combinations in this snapshot. Source URLs, retrieval time
and hashes accompany the catalog and appear in results. A fresh hash establishes
file identity, not semantic correctness or qualification.

`pkg/nvaie` owns typed catalog facts, read-only observations and evaluation. It does
not depend on Launch Kit config or the Network Operator release package. The shared
resolver owns the artifact-catalog join; Host discovery and validation supply live
clients and selected worker scope. Validation recollects facts rather than trusting
saved discovery results. HTML summarizes the same assessment in one Expected / Discovered / Match table;
JSON retains detailed observations and provenance.

## Preset GPU names

Matching uses `nvidia.com/gpu.product` and `nvidia.com/gpu.machine`, the same
Node labels used by discovery. Raw values stay in `NodeFacts`; `matrixGPU`
identifies the selected catalog product. No runtime probe or new saved inventory
field is needed. Evaluation and filtered matrix URLs use the same exact resolver.
GPU family names alone remain ambiguous; aliases require the full reviewed pair.
Ubuntu point releases match their LTS release, and explorer links use the exact
published OS option, such as `24.04 LTS`.

The complete bundled-preset audit is enforced by
`TestAllPresetGPUProductsMatchNVAIECatalog`. With the DGX compute tray added,
13 presets yield five normalized exact matches, six machine-specific aliases,
and two unresolved identities:

| Preset machine | Raw GPU | NVAIE product |
| --- | --- | --- |
| DGX-GB300-Compute-Tray | NVIDIA-GB300 | DGX GB300 NVL72 |
| GB300-NVL | NVIDIA-GB300 | GB300 NVL72 |
| PowerEdge-XE9680 | NVIDIA-H200 | H200 SXM |
| ThinkSystem-SR680a-V3 | NVIDIA-H200 | H200 SXM |
| UCSC-885A-M8-H22 | NVIDIA-H200 | H200 SXM |
| PowerEdge-XE7745 | NVIDIA-RTX-PRO-4500-Blackwell | RTX PRO 4500 Blackwell Server Edition |
| PowerEdge-R760xa | NVIDIA-H100-NVL | H100 NVL (exact) |
| ThinkSystem-SR675-V3 | NVIDIA-H200-NVL | H200 NVL (exact) |
| ThinkSystem-SR650-V4, ThinkSystem-SR675-V3, ThinkSystem-SR675-V3-System-Board | NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition | RTX PRO 6000 Blackwell Server Edition (three exact presets) |
| GB200-NVL | NVIDIA-GB200 | Unresolved: NVL4 versus NVL72 |
| GB300-DGX-Station | NVIDIA-GB300 | Desktop product not listed in this catalog |

The [NVL72 architecture](https://docs.nvidia.com/enterprise-reference-architectures/nvl72-ai-factory/latest/components.html)
and [DGX hardware guide](https://docs.nvidia.com/dgx/dgxgb200-user-guide/hardware.html)
identify the two GB300 compute-tray families. The
[NVIDIA system/GPU table](https://docs.nvidia.com/certification-technology/certified-systems/nvidia-certified-systems)
and [HGX specification](https://docs.nvidia.com/enterprise-reference-architectures/hgx-ai-factory-h100-h200-b200/latest/components.html)
establish the server-specific H200 SXM and RTX Server Edition aliases.
The [GB200 firmware guide](https://docs.nvidia.com/multi-node-nvlink-systems/nvfupd-guide/examples/gb200NVL.html)
also covers NVL4, so `GB200-NVL` cannot select NVL72 by name alone. The
[DGX Station guide](https://docs.nvidia.com/dgx/dgx-station-development-guide/latest/overview.html)
identifies its Desktop Superchip separately; it is not an NVL72 alias.

A selected product establishes the identity used in the matrix comparison,
not rack completeness, physical certification or current software compatibility.
Unknown machines and conflicting current GPU observations must not create a match.

Keep required software/platform checks separate from manual hardware, licensing
and vendor qualification. Compare API-server and kubelet versions and retain
recognized controller rollout coverage. A ready Pod image reference or node label is metadata;
it does not prove a binary digest. A kind test verifies API and report behavior
only. Test unknown/missing/RBAC failures, conflicting replicas, mixed workers,
exact release conflicts, conditional matrix rows and unchanged no-opt-in output.

## Component detector evidence

Keep the supported-method allowlist separate from the release BOM. Its methods and
limits are documented in [Host validation](../../docs/user/validation.md#nvidia-ai-enterprise-release-checks).
Adding a product pin must not automatically enable a detector or make installation
mandatory. To add a method, provide an authoritative installation identity, the
exact observed version source and its semantics, positive and negative fixtures,
coverage rules, and the live verification result or a precise unresolved condition
for the supported installation form. Keep source and fixture evidence distinct
from live qualification.

Container detectors compare declared artifact versions through the Kubernetes
API. The DOCA-HOST branch consumes existing discovery package evidence with
explicit membership and freshness. These checks add no `nvidia-smi`, package
queries or other runtime probes and do not verify loaded binaries. Driver and toolkit image tags describe their
containers; the report must not call them loaded-driver or installed-host versions.
Keep exact image/container/controller identity, ownership and rollout checks so
validator Pods or unrelated images cannot impersonate an operator installation.
A missing supported workload does not establish host package absence. DOCA-HOST's
existing discovery package probe and release-cohort validation are reused;
do not relabel saved data as live. Include scoped installed host packages
regardless of desired `docaDriver.enable`; that flag controls deployment intent,
not observed installation. With no installed package observations, preserve the
container or unknown-host path selected by that flag. Keep concurrent live
containers visible alongside package evidence; saved packages cannot override their mismatch or
resolve conflicting delivery. Preserve unscheduled DaemonSet intent as unknown
unless node-name or selector evidence excludes all selected workers. Do not
implement scheduler/affinity simulation to manufacture a scope verdict. Disabled container
mode alone does not prove that DOCA-HOST is installed. Include all selected
networking workers for DOCA, including CPU-only workers, and preserve the captured
host scope when presets change render topology.

Filtered source URLs reproduce the explorer's `sm_v=1`, `view=landing`, `mode=gpu`
query semantics. The HTTP response remains the same HTML shell for different
filters, so parsing result tables from that response is not a data source. Use the
reviewed static snapshot for decisions and label links as queries against the live
website. Unknown physical deployment permits only an explicitly labeled bare-metal
candidate query; known simulated/virtual targets get GPU-level links.

NIM uses the official `k8s-nim-operator` Deployment's `manager` container; chart
upgrade Jobs using the same image are not operator installation evidence. Run:ai
uses the official `runai-oss/go-operator` Deployment's `runai-go-operator` container.
The Run:ai 3.0 release linked from the NVAIE matrix publishes stable 3.0.x hotfixes,
so a two-part Run:ai pin compares the major/minor while retaining the observed patch.
Do not generalize this rule to exact component pins or prereleases. The detector
checks that operator artifact only; external control-plane and configured operand
versions remain outside its scope. No Helm Secret or Run:ai API credentials are
needed by the observer.

NIM was exercised on a live arm64 Kubernetes cluster. Run:ai identity and release
semantics are verified against the official chart and fixtures; positive live
verification remains pending registry image access.

Unsupported products remain in the reference catalog but are omitted centrally
from assessment components, so JSON and HTML both exclude them. This includes
Fabric Manager: neither a driver tag nor a switch OS version establishes its
independent version. BCM and DOCA Microservices are distinct products and must
not inherit the DOCA driver version. Keep failures of supported methods visible;
an API error is not a reason to hide a component row.
