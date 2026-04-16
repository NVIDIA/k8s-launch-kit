#!/bin/bash
# Copyright 2025 NVIDIA CORPORATION & AFFILIATES
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# SPDX-License-Identifier: Apache-2.0

# Install l8k binary, profiles, and default config to system paths.
#
# Usage:
#   scripts/install-local.sh                     # Install to /usr/local (copies files)
#   scripts/install-local.sh --dev-env           # Install symlinks for development
#   scripts/install-local.sh --prefix /opt/l8k   # Custom install prefix
#
# Install layout:
#   <prefix>/bin/l8k                       # Binary
#   <prefix>/share/l8k/profiles/           # Profile templates
#   <prefix>/share/l8k/presets/            # Predefined cluster topology presets
#   <prefix>/share/l8k/l8k-config.yaml    # Default configuration

set -euo pipefail

PREFIX="/usr/local"
DEV_ENV=false
SKIP_PRESETS_UPDATE=false
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dev-env)
            DEV_ENV=true
            shift
            ;;
        --prefix)
            PREFIX="$2"
            shift 2
            ;;
        --skip-presets-update)
            SKIP_PRESETS_UPDATE=true
            shift
            ;;
        -h|--help)
            echo "Usage: install-local.sh [--dev-env] [--prefix /path] [--skip-presets-update]"
            echo ""
            echo "Options:"
            echo "  --dev-env              Create symlinks instead of copies (for development)"
            echo "  --prefix               Install prefix (default: /usr/local)"
            echo "  --skip-presets-update   Skip downloading latest presets from GitHub"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Usage: install-local.sh [--dev-env] [--prefix /path] [--skip-presets-update]"
            exit 1
            ;;
    esac
done

SHARE_DIR="${PREFIX}/share/l8k"
BIN_DIR="${PREFIX}/bin"

# Verify binary exists
if [ ! -f "${REPO_ROOT}/build/l8k" ]; then
    echo "Error: binary not found at ${REPO_ROOT}/build/l8k"
    echo "Run 'make build' first."
    exit 1
fi

if [ "$DEV_ENV" = true ]; then
    echo "Installing l8k (dev mode — symlinks)..."
    mkdir -p "${BIN_DIR}"
    ln -sfn "${REPO_ROOT}/build/l8k" "${BIN_DIR}/l8k"
    mkdir -p "${SHARE_DIR}"
    ln -sfn "${REPO_ROOT}/profiles" "${SHARE_DIR}/profiles"
    ln -sfn "${REPO_ROOT}/presets" "${SHARE_DIR}/presets"
    ln -sfn "${REPO_ROOT}/l8k-config.yaml" "${SHARE_DIR}/l8k-config.yaml"
else
    echo "Installing l8k..."
    mkdir -p "${BIN_DIR}"
    install -m 755 "${REPO_ROOT}/build/l8k" "${BIN_DIR}/l8k"
    mkdir -p "${SHARE_DIR}"
    rm -rf "${SHARE_DIR}/profiles"
    cp -r "${REPO_ROOT}/profiles" "${SHARE_DIR}/profiles"
    rm -rf "${SHARE_DIR}/presets"
    cp -r "${REPO_ROOT}/presets" "${SHARE_DIR}/presets"
    cp "${REPO_ROOT}/l8k-config.yaml" "${SHARE_DIR}/l8k-config.yaml"
fi

echo ""
echo "Installed successfully:"
echo "  Binary:   ${BIN_DIR}/l8k"
echo "  Profiles: ${SHARE_DIR}/profiles"
echo "  Presets:  ${SHARE_DIR}/presets"
echo "  Config:   ${SHARE_DIR}/l8k-config.yaml"

# Try to download latest presets from GitHub (non-fatal on failure)
if [ "$SKIP_PRESETS_UPDATE" = false ] && [ "$DEV_ENV" = false ]; then
    echo ""
    echo "Downloading latest presets from GitHub..."
    "${BIN_DIR}/l8k" preset update --dir "${SHARE_DIR}/presets" 2>/dev/null \
        && echo "Presets updated successfully." \
        || echo "[WARNING] Could not download presets (offline?). Using bundled presets."
fi
