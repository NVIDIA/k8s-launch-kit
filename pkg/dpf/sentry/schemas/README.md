<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

These JSON data files preserve the complete v1alpha1 OpenAPI schema extracted
from each emitted DPF Kind's upstream CRD, without alteration. The envelope binds
group, version and Kind. `provenance.json` records each generated data hash, the
source CRD path/hash and exact upstream source commit. Both release candidate
26.8.0-rc.1 and GA26.8.0 are represented independently.

The offline validator rejects unknown fields that Kubernetes structural pruning
would otherwise remove, then checks OpenAPI and CEL create invariants. It never
defaults/mutates desired objects and does not simulate controller webhooks,
installed CRD drift, updates, physical effects or runtime qualification. The
matching installed API must still validate the complete plan before deployment.
