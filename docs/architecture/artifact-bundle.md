<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Artifact bundle

The Host workflow creates one validated snapshot of its generated artifacts per
operation. `pkg/bundle` loads a flat manifest directory or accepts rendered
files in memory. It retains the original bytes, source filename and document
number, parsed Kubernetes objects, and normalized Helm values. Both loading
paths use the same filename rules and deterministic order.

Files containing `example` in the basename are validation workloads. Other
resource files are deployment inputs. Only the exact name `values.yaml` is
Helm input. The loader rejects reserved alternatives such as `values.yml` and
`VALUES.YAML` with a rename error. Adjacent `cluster-config.yaml` and
`cluster-config.yml` are loaded through configuration code, outside this
bundle.

Construction rejects malformed YAML, missing `apiVersion`, `kind`, or
`metadata.name`, and repeated declared group/kind/namespace/name identities.
It reports the source file and physical document number. These are structural
checks; Kubernetes admission, effective namespace, schema and reference checks
remain with the cluster and the relevant consumer.

The bundle hides its parsed objects and values. Consumers receive copies so
deployment and OpenShift connectivity rewrites cannot change the retained
desired state. Deployment prepares and checks its resource selection before
Helm or Kubernetes mutations. Preflight projects expected resource identities
from the complete bundle. Validation, connectivity, report inference and
readiness polling reuse the same desired snapshot while continuing to refresh
live Kubernetes state.

Generation builds the bundle after ownership annotations and before replacing
the output directory. It writes the original annotated bytes. A combined
generate/deploy run passes that same bundle to the Network Operator plugin;
standalone deploy and validate load their own snapshot. The configuration
sidecar at `.l8k/resolved-config.yaml` remains on its existing path.
