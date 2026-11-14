<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Engineering documentation

These documents support Launch Kit development. They are not published in the
operator documentation or included in user documentation exports.

- [Current architecture and Go library](architecture/overview.md)
- [Read-only discovery library](architecture/discovery-library.md)
- [Artifact bundle internals](architecture/artifact-bundle.md)
- [Target extension contract](advanced/targets.md)
- [DPF integration plan](architecture/dpf-integration-plan.md)
- [Host target migration history](architecture/host-target-migration-plan.md)
- [Documentation maintenance and publishing](reference/docs-publishing.md)

See [CONTRIBUTING](../CONTRIBUTING.md) for development and review requirements.
Operator procedures and reference material belong under `docs/`; design plans,
implementation details and development validation belong here. Lab-specific
credentials, cluster inventories and test-run artifacts stay outside the repository.

## Build From Source

```bash
git clone https://github.com/NVIDIA/k8s-launch-kit.git
cd k8s-launch-kit
make build
```

The binary is written to `build/l8k`.

For a local install:

```bash
make install
```
