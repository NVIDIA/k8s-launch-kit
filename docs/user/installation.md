<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Installation

Install the latest `l8k` binary, then select the target Network Operator release with `--network-operator-release`. A single current `l8k` binary carries the release catalog for older supported Network Operator lines.

## Install Script

```bash
curl -fsSL https://raw.githubusercontent.com/NVIDIA/k8s-launch-kit/main/scripts/install.sh | sh
```

Pin a release:

```bash
L8K_VERSION=v26.7.0-beta.4 \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/NVIDIA/k8s-launch-kit/main/scripts/install.sh)"
```

Use a custom destination:

```bash
curl -fsSL https://raw.githubusercontent.com/NVIDIA/k8s-launch-kit/main/scripts/install.sh | \
  sh -s -- -d "$HOME/.local"
```

## Homebrew

```bash
brew tap nvidia/l8k https://github.com/NVIDIA/k8s-launch-kit
brew install l8k
```

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

For development, symlink the source-tree assets instead of copying them:

```bash
make dev-install
```

## Installed Assets

The install places the binary on PATH. The default configuration and bundled topology presets are embedded in the binary. Use `--config-dir` when you need a filesystem override:

```text
/etc/l8k/
|-- l8k-config.yaml
`-- presets/
    `-- <preset-name>/topology.yaml
```

```bash
l8k discover --config-dir /etc/l8k --kubeconfig ~/.kube/config
l8k preset list --config-dir /etc/l8k
```

`--user-config <file>` has higher precedence than `--config-dir/l8k-config.yaml`. A `presets/` directory in `--config-dir` replaces the embedded preset catalog instead of merging with it.

## Verify

```bash
l8k version
l8k schema | jq '.supportedNetworkOperatorReleases'
```
