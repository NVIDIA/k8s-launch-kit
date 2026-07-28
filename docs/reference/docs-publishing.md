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
`-- assets/
```

The old RST files under `docs/` are retained for compatibility, but the GitHub Pages site uses the Markdown pages listed in `mkdocs.yml`.

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

## GitHub Pages Workflow

The workflow has two phases:

| Event | Behavior |
| --- | --- |
| Pull request | Build with `mkdocs build --strict`. |
| Push to `main` | Build, upload the Pages artifact, and deploy with `actions/deploy-pages`. |
| Manual dispatch | Build and deploy from the selected ref. |

Repository settings must allow GitHub Pages deployment from GitHub Actions.
