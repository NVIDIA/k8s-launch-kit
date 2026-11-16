#!/bin/bash
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

# Render the five network-operator-docs quick-start bundles.
#
# Usage:
#   hack/render-operator-doc-examples.sh <output-dir>
#
# Each profile is rendered with the embedded default config (no cluster-config.yaml)
# and these shared flags:
#   --node-selector nvidia.com/gpu.product=NVIDIA-H200
#   --network-operator-release 26.10
#   --skip-network-operator-helm
#
# Ethernet profiles use --for PowerEdge-XE9680-H200.
# InfiniBand profiles use --for ThinkSystem-SR680a-V3-H200.
# Multirail stays at the Launch Kit default.
#
# <output-dir>/<profile>/ receives the rendered network-operator YAML except
# values.yaml, plus complete.yaml (numbered manifests in filename order).

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
if [[ $# -ne 1 || -z "$1" ]]; then
	echo "usage: $0 <output-dir>" >&2
	exit 1
fi
mkdir -p "$1"
out="$(cd "$1" && pwd)"

l8k="${L8K:-$root/build/l8k}"
if [[ ! -x "$l8k" ]]; then
	make -C "$root" build
fi

release="26.10"
node_selector="nvidia.com/gpu.product=NVIDIA-H200"

render_profile() {
	local profile="$1"
	local preset="$2"
	local network_glob="$3"
	shift 3

	local work scratch dest
	work="$(mktemp -d)"
	scratch="$(mktemp -d)"
	ln -s "$root/profiles" "$work/profiles"

	echo "Rendering $profile from preset $preset"
	(
		cd "$work"
		"$l8k" generate \
			--for "$preset" \
			--node-selector "$node_selector" \
			--network-operator-release "$release" \
			--skip-network-operator-helm \
			--save-deployment-files "$scratch" \
			"$@"
	)

	dest="$out/$profile"
	rm -rf "$dest"
	mkdir -p "$dest"

	local rendered=0
	local path base
	for path in "$scratch/network-operator"/*.yaml; do
		base="$(basename "$path")"
		if [[ "$base" == "values.yaml" ]]; then
			continue
		fi
		cp "$path" "$dest/$base"
		rendered=$((rendered + 1))
	done
	if [[ "$rendered" -eq 0 ]]; then
		echo "no manifests rendered for $profile" >&2
		exit 1
	fi
	if [[ -e "$dest/values.yaml" ]]; then
		echo "values.yaml must not be published for $profile" >&2
		exit 1
	fi
	if [[ ! -f "$dest/10-nicclusterpolicy.yaml" ]]; then
		echo "missing 10-nicclusterpolicy.yaml for $profile" >&2
		exit 1
	fi
	if ! compgen -G "$dest/20-ippool*.yaml" >/dev/null; then
		echo "missing IP pool manifest for $profile" >&2
		exit 1
	fi
	if ! compgen -G "$dest/$network_glob" >/dev/null; then
		echo "missing network manifest ($network_glob) for $profile" >&2
		exit 1
	fi
	local examples
	examples="$(find "$dest" -maxdepth 1 -name '*example-daemonset*.yaml' | wc -l | tr -d ' ')"
	if [[ "$examples" != "1" ]]; then
		echo "expected one example DaemonSet for $profile, found $examples" >&2
		exit 1
	fi

	local complete="$dest/complete.yaml"
	local files=()
	local manifest
	while IFS= read -r manifest; do
		files+=("$manifest")
	done < <(find "$dest" -maxdepth 1 -name '[0-9]*-*.yaml' | sort)
	: >"$complete"
	local i
	for i in "${!files[@]}"; do
		cat "${files[$i]}" >>"$complete"
		if [[ "$i" -lt $((${#files[@]} - 1)) ]]; then
			printf '\n---\n' >>"$complete"
		fi
	done

	rm -rf "$work" "$scratch"
}

render_profile sriov-network-rdma PowerEdge-XE9680-H200 '50-sriovnetwork*.yaml' \
	--fabric ethernet --deployment-type sriov
render_profile host-device-rdma PowerEdge-XE9680-H200 '*hostdevicenetwork*.yaml' \
	--fabric ethernet --deployment-type host_device
render_profile macvlan-rdma-shared PowerEdge-XE9680-H200 '*macvlannetwork*.yaml' \
	--fabric ethernet --deployment-type rdma_shared
render_profile sriov-ib-rdma ThinkSystem-SR680a-V3-H200 '*sriovibnetwork*.yaml' \
	--fabric infiniband --deployment-type sriov
render_profile ipoib-rdma-shared ThinkSystem-SR680a-V3-H200 '*ipoibnetwork*.yaml' \
	--fabric infiniband --deployment-type rdma_shared

echo "Wrote quick-start bundles to $out"
