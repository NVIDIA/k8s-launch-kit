<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Documentation Publishing

The standalone docs site is built with MkDocs Material and published to GitHub Pages by `.github/workflows/docs.yml`.

## Layout

```text
mkdocs.yml
requirements-docs.txt
docs/
|-- index.md
|-- user/
|-- integrator/
|-- reference/
|-- advanced/
|-- architecture/
`-- assets/
```

The old RST files under `docs/` are retained for compatibility, but the GitHub Pages site uses the Markdown pages listed in `mkdocs.yml`. The DPF roadmap and HostTarget migration history are published and linked from [Architecture](../architecture/overview.md), outside the primary navigation. Keep their status labels and `llms.txt` entries current.

## Content Ownership

Each customer behavior has one primary explanation. Planning owns scenario and
profile choice; Discovery owns inventory effects; Configuration owns field
defaults and lookup; Generation owns bundle output and workload transformation;
Deployment and Cleanup own their respective deletion scopes; Validation owns
acceptance; and Automation owns JSON and shell failure handling. Keep short
links and consequential warnings at the action rather than repeating full
contracts in README or skills.

Task articles should state the outcome, applicability, inputs and permissions,
ordered actions, expected evidence, and failure/next step. Reference articles
optimize lookup; architecture and design plans describe implementation or
future work. Label snippets that are illustrative or partial so readers do not
mistake them for a runnable, qualified site configuration.

## Local Build

```bash
python3 -m venv /tmp/l8k-docs
/tmp/l8k-docs/bin/python -m pip install -r requirements-docs.txt
/tmp/l8k-docs/bin/mkdocs build --strict
```

Serve locally:

```bash
/tmp/l8k-docs/bin/mkdocs serve
```

## Behavioral Documentation Checks

Build the current CLI and run the offline consistency check:

```bash
make build
python3 scripts/check-docs.py --binary build/l8k
```

The CI build job runs this check on every pull request and main-branch push.
It compares long flags in Markdown/RST shell examples and Cobra help examples
against the actual command flags, verifies the lifecycle applicability matrix
and `llms.txt` page inventory, and renders the published standard Profile YAML
with an embedded preset. It checks source preservation, effective metadata,
and the documented Kubernetes custom-workload namespace/fixture behavior.
It executes only help and explicitly constructed offline generation commands;
shell snippets from documentation are never executed. Architecture plans are
excluded because they can describe future interfaces.

These checks do not qualify a live deployment, external links, every YAML
fragment, or every semantic claim. Review changes to flags, defaults, profiles,
output contracts, and operator behavior against the relevant user guide, CLI
reference, README, and skills in the same pull request. MkDocs checks structure
and local links; it cannot establish agreement with runtime behavior.

Keep canonical explanations in the published Markdown pages and link skill
references to them. Retained RST files are compatibility material; port useful
operational details before retiring or shortening them.

## GitHub Pages Workflow

The workflow builds documentation for every pull request and main-branch push,
including code-only changes that can affect behavior. It has two phases:

| Event | Behavior |
| --- | --- |
| Pull request | Build with `mkdocs build --strict`. |
| Push to `main` | Build, upload the Pages artifact, and deploy with `actions/deploy-pages`. |
| Manual dispatch | Build and deploy from the selected ref. |

Repository settings must allow GitHub Pages deployment from GitHub Actions.
