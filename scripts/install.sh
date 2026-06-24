#!/bin/sh
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

# Install or uninstall l8k from GitHub Releases.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/nvidia/k8s-launch-kit/main/scripts/install-standalone.sh | sh
#   curl -fsSL ... | sh -s -- -d ~/local
#   curl -fsSL ... | sh -s -- --uninstall
#   L8K_VERSION=v1.2.0 sh install-standalone.sh
#
# Environment variables:
#   L8K_VERSION   Pin a specific version (default: latest release)
#   GITHUB_TOKEN  Authenticate GitHub API requests (avoids rate limits)

set -eu

REPO="nvidia/k8s-launch-kit"
INSTALL_DIR="/usr/local"
UNINSTALL=false

# --- Parse flags ---
while [ $# -gt 0 ]; do
    case "$1" in
        -d)
            INSTALL_DIR="$2"
            shift 2
            ;;
        --uninstall)
            UNINSTALL=true
            shift
            ;;
        -h|--help)
            echo "Usage: install-standalone.sh [-d install_prefix] [--uninstall]"
            echo ""
            echo "Options:"
            echo "  -d <prefix>    Install prefix (default: /usr/local)"
            echo "  --uninstall    Remove l8k binary and assets"
            echo ""
            echo "Environment variables:"
            echo "  L8K_VERSION    Pin a specific version (e.g. v1.0.0)"
            echo "  GITHUB_TOKEN   Authenticate GitHub requests"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Usage: install-standalone.sh [-d install_prefix] [--uninstall]"
            exit 1
            ;;
    esac
done

# --- Uninstall ---
if [ "$UNINSTALL" = true ]; then
    SUDO=""
    if [ ! -w "${INSTALL_DIR}/bin" ] 2>/dev/null; then
        SUDO="sudo"
    fi
    echo "Removing l8k from ${INSTALL_DIR}..."
    $SUDO rm -f "${INSTALL_DIR}/bin/l8k"
    $SUDO rm -rf "${INSTALL_DIR}/share/l8k"
    echo "l8k uninstalled."
    exit 0
fi

# --- Detect platform ---
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)         ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *)
        echo "Error: unsupported architecture: $ARCH" >&2
        exit 1
        ;;
esac

case "$OS" in
    linux|darwin) ;;
    *)
        echo "Error: unsupported OS: $OS (use Linux or macOS)" >&2
        exit 1
        ;;
esac

echo "Detected platform: ${OS}/${ARCH}"

# --- Resolve version ---
if [ -n "${L8K_VERSION:-}" ]; then
    VERSION="$L8K_VERSION"
else
    # Use redirect trick to avoid GitHub API rate limits.
    # The /releases/latest endpoint redirects to /releases/tag/vX.Y.Z —
    # we parse the version from the redirect URL.
    REDIRECT_URL=$(curl --proto '=https' --tlsv1.2 -fsSIo /dev/null -w '%{redirect_url}' \
        "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)
    VERSION=$(echo "$REDIRECT_URL" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+[^/]*' || true)

    # Verify the release has GoReleaser archives (older releases may not).
    # If not, fall back to the GitHub API to find the most recent release that does.
    if [ -n "$VERSION" ]; then
        VERSION_NO_V_CHECK="${VERSION#v}"
        CHECK_URL="https://github.com/${REPO}/releases/download/${VERSION}/l8k_${VERSION_NO_V_CHECK}_${OS}_${ARCH}.tar.gz"
        if ! curl --proto '=https' --tlsv1.2 -fsSIo /dev/null "$CHECK_URL" 2>/dev/null; then
            echo "Latest stable release (${VERSION}) has no binary archives."
            echo "Checking for the most recent release with binaries..."
            VERSION=""
        fi
    fi

    # Fallback: query the GitHub API for the most recent release with archives.
    if [ -z "$VERSION" ]; then
        AUTH_FLAG=""
        if [ -n "${GITHUB_TOKEN:-}" ]; then
            AUTH_FLAG="-H \"Authorization: token ${GITHUB_TOKEN}\""
        fi
        # eval expands the optional, header-bearing $AUTH_FLAG as separate
        # arguments; all input here is constructed by this script, not the user.
        VERSION=$(eval curl --proto "'=https'" --tlsv1.2 -fsSL $AUTH_FLAG \
            "https://api.github.com/repos/${REPO}/releases?per_page=10" 2>/dev/null \
            | grep -oE '"tag_name":\s*"v[^"]*"' \
            | head -1 \
            | grep -oE 'v[0-9]+[^"]*' || true)
    fi

    if [ -z "$VERSION" ]; then
        echo "Error: could not determine latest version." >&2
        echo "Set L8K_VERSION explicitly or check https://github.com/${REPO}/releases" >&2
        exit 1
    fi
fi
VERSION_NO_V="${VERSION#v}"

echo "Installing l8k ${VERSION}..."

# --- Download ---
ARCHIVE="l8k_${VERSION_NO_V}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

AUTH_HEADER=""
if [ -n "${GITHUB_TOKEN:-}" ]; then
    AUTH_HEADER="Authorization: token ${GITHUB_TOKEN}"
fi

echo "Downloading ${ARCHIVE}..."
# --proto '=https' refuses any redirect that downgrades to a clear-text protocol.
curl --proto '=https' --tlsv1.2 -fsSL ${AUTH_HEADER:+-H "$AUTH_HEADER"} "${BASE_URL}/${ARCHIVE}" -o "${WORK_DIR}/${ARCHIVE}"
curl --proto '=https' --tlsv1.2 -fsSL ${AUTH_HEADER:+-H "$AUTH_HEADER"} "${BASE_URL}/checksums.txt" -o "${WORK_DIR}/checksums.txt"

# --- Verify checksum ---
cd "$WORK_DIR"
EXPECTED_SUM=$(grep "${ARCHIVE}" checksums.txt | awk '{print $1}')
if [ -z "$EXPECTED_SUM" ]; then
    echo "Warning: archive not found in checksums.txt, skipping verification"
else
    if command -v shasum >/dev/null 2>&1; then
        ACTUAL_SUM=$(shasum -a 256 "${ARCHIVE}" | awk '{print $1}')
    elif command -v sha256sum >/dev/null 2>&1; then
        ACTUAL_SUM=$(sha256sum "${ARCHIVE}" | awk '{print $1}')
    else
        ACTUAL_SUM=""
        echo "Warning: no sha256sum or shasum available, skipping checksum verification"
    fi
    if [ -n "$ACTUAL_SUM" ]; then
        if [ "$EXPECTED_SUM" = "$ACTUAL_SUM" ]; then
            echo "Checksum verified."
        else
            echo "Error: checksum mismatch!" >&2
            echo "  Expected: ${EXPECTED_SUM}" >&2
            echo "  Got:      ${ACTUAL_SUM}" >&2
            exit 1
        fi
    fi
fi

# --- Extract ---
mkdir -p "${WORK_DIR}/extracted"
tar xzf "${ARCHIVE}" -C "${WORK_DIR}/extracted"

# --- Install ---
NEED_SUDO=false
if [ ! -w "${INSTALL_DIR}/bin" ] 2>/dev/null; then
    NEED_SUDO=true
fi

INSTALL_CMDS="
    mkdir -p '${INSTALL_DIR}/bin'
    install -m 755 '${WORK_DIR}/extracted/l8k' '${INSTALL_DIR}/bin/l8k'
    mkdir -p '${INSTALL_DIR}/share/l8k'
    rm -rf '${INSTALL_DIR}/share/l8k/profiles' '${INSTALL_DIR}/share/l8k/presets'
    cp -r '${WORK_DIR}/extracted/profiles' '${INSTALL_DIR}/share/l8k/'
    cp -r '${WORK_DIR}/extracted/presets' '${INSTALL_DIR}/share/l8k/'
    cp '${WORK_DIR}/extracted/l8k-config.yaml' '${INSTALL_DIR}/share/l8k/'
"

if [ "$NEED_SUDO" = true ]; then
    echo "Installing to ${INSTALL_DIR} (requires sudo)..."
    sudo sh -c "$INSTALL_CMDS"
else
    sh -c "$INSTALL_CMDS"
fi

# macOS: remove quarantine attribute
xattr -d com.apple.quarantine "${INSTALL_DIR}/bin/l8k" 2>/dev/null || true

# --- Verify ---
echo ""
echo "Installed successfully:"
echo "  Binary:   ${INSTALL_DIR}/bin/l8k"
echo "  Profiles: ${INSTALL_DIR}/share/l8k/profiles"
echo "  Presets:  ${INSTALL_DIR}/share/l8k/presets"
echo "  Config:   ${INSTALL_DIR}/share/l8k/l8k-config.yaml"
echo ""
"${INSTALL_DIR}/bin/l8k" version

# --- PATH check ---
case ":${PATH}:" in
    *":${INSTALL_DIR}/bin:"*) ;;
    *)
        echo ""
        echo "Warning: ${INSTALL_DIR}/bin is not in your PATH."
        echo "Add it with: export PATH=\"${INSTALL_DIR}/bin:\$PATH\""
        ;;
esac
