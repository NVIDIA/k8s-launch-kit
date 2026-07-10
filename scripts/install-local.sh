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

# Install the l8k binary and external profile templates to system paths.
#
# Usage:
#   scripts/install-local.sh                     # Install to /usr/local (copies files)
#   scripts/install-local.sh --dev-env           # Install symlinks for development
#   scripts/install-local.sh --prefix /opt/l8k   # Custom install prefix
#
# Install layout:
#   <prefix>/bin/l8k                       # Binary
#   <prefix>/share/l8k/profiles/           # Profile templates
#
# The default config and topology presets are embedded in the binary. Existing
# filesystem overrides are preserved and can be selected with --config-dir.

set -euo pipefail

PREFIX="/usr/local"
DEV_ENV=false
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
        -h|--help)
            echo "Usage: install-local.sh [--dev-env] [--prefix /path]"
            echo ""
            echo "Options:"
            echo "  --dev-env              Create symlinks instead of copies (for development)"
            echo "  --prefix               Install prefix (default: /usr/local)"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Usage: install-local.sh [--dev-env] [--prefix /path]"
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
else
    echo "Installing l8k..."
    mkdir -p "${BIN_DIR}"
    install -m 755 "${REPO_ROOT}/build/l8k" "${BIN_DIR}/l8k"
    mkdir -p "${SHARE_DIR}"
    rm -rf "${SHARE_DIR}/profiles"
    cp -r "${REPO_ROOT}/profiles" "${SHARE_DIR}/profiles"
fi

echo ""
echo "Installed successfully:"
echo "  Binary:   ${BIN_DIR}/l8k"
echo "  Profiles: ${SHARE_DIR}/profiles"
if [ -e "${SHARE_DIR}/presets" ] || [ -e "${SHARE_DIR}/l8k-config.yaml" ]; then
    echo ""
    echo "Existing config overrides were preserved under ${SHARE_DIR}."
    echo "Select them explicitly with: l8k --config-dir ${SHARE_DIR} ..."
fi
