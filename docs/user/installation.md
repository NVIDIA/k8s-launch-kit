<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Installation

Install the latest `l8k` binary, then select the target Network Operator release with `--network-operator-release`. A single current `l8k` binary carries the release catalog for older supported Network Operator lines.

## Prerequisites

- Linux or macOS on `amd64` or `arm64`.
- Kubernetes credentials for discovery, deployment, validation, and sosreport collection.
- `kubectl` is recommended for inspection and troubleshooting.
- `curl` for the install script, Homebrew for the formula, or Docker/Podman for the container method.

A pre-installed Network Operator and NFD are not required for discovery. `l8k deploy` can install the selected Network Operator Helm chart from the generated `values.yaml`.

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

## Container

Set `L8K_VERSION` to a published release tag and pull the image:

```bash
export L8K_VERSION=vX.Y.Z
docker pull nvcr.io/nvidia/cloud-native/k8s-launch-kit:${L8K_VERSION}
```

Define a shell function that mounts the kubeconfig and current working directory:

```bash
l8k() {
  docker run --rm --network host \
    -v "$HOME/.kube:/root/.kube:ro" \
    -v "$PWD:/work" \
    -w /work \
    "nvcr.io/nvidia/cloud-native/k8s-launch-kit:${L8K_VERSION}" \
    "$@"
}
```

The generated configuration, manifests, and reports remain in the current directory. Ensure the Kubernetes API server address in the mounted kubeconfig is reachable from the container runtime.

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

Script, Homebrew, and source installs also place the profile templates under `<prefix>/share/l8k/profiles/`. Existing `l8k-config.yaml` and `presets/` overrides under the share directory are preserved during upgrades and are selected only when passed through `--config-dir`.

## Verify

```bash
l8k version
l8k schema | jq '.supportedNetworkOperatorReleases'
```

## Uninstall

Install script:

```bash
curl -fsSL https://raw.githubusercontent.com/NVIDIA/k8s-launch-kit/main/scripts/install.sh | \
  sh -s -- --uninstall
```

Use `-d <prefix>` with `--uninstall` if a custom install destination was used.

Homebrew:

```bash
brew uninstall l8k
brew untap nvidia/l8k
```

Container:

```bash
docker image rm "nvcr.io/nvidia/cloud-native/k8s-launch-kit:${L8K_VERSION}"
```
