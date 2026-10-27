// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"k8s.io/client-go/rest"
)

// Only IPoIB netdevs have the dev_port semantics used here. Require a
// unique direct device mapping; never use RDMA discovery's rail-order fallback.
const partitionProbe = `set -eu
root=$1
iface=$2
net="$root/class/net/$iface"
type=$(cat "$net/type")
case "$type" in ''|*[!0-9]*) echo 'invalid network interface type' >&2; exit 1;; esac
if [ "$type" != 32 ]; then echo not-infiniband; exit 0; fi
set -- "$net"/device/infiniband/*
if [ "$#" != 1 ] || [ ! -d "$1" ]; then echo 'ambiguous or missing RDMA device mapping' >&2; exit 1; fi
dev=${1##*/}
index=$(cat "$net/dev_port")
case "$index" in ''|*[!0-9]*) echo 'invalid IPoIB dev_port' >&2; exit 1;; esac
if [ "$index" -gt 254 ]; then echo 'invalid IPoIB dev_port' >&2; exit 1; fi
port=$((index + 1))
path="$root/class/infiniband/$dev/ports/$port"
layer=$(cat "$path/link_layer")
if [ "$layer" != InfiniBand ]; then echo 'IPoIB port is not InfiniBand' >&2; exit 1; fi
printf '%s %s\n' "$dev" "$port"
for key in "$path"/pkeys/*; do cat "$key"; done
`

var partitionDeviceName = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// partitionWarning is advisory: even a limited-only table can communicate
// with a full peer. An unrelated full key is not proof of usable connectivity.
func partitionWarning(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) == 1 && fields[0] == "not-infiniband" {
		return "", nil
	}
	if len(fields) < 3 || !partitionDeviceName.MatchString(fields[0]) {
		return "", fmt.Errorf("missing or malformed device/port/P_Key table")
	}
	port, err := strconv.Atoi(fields[1])
	if err != nil || port < 1 || port > 255 {
		return "", fmt.Errorf("invalid InfiniBand port")
	}
	limited := []string{}
	full := false
	for _, field := range fields[2:] {
		if !strings.HasPrefix(field, "0x") || len(field) != 6 {
			return "", fmt.Errorf("malformed P_Key %q", boundedTraceOutput(field))
		}
		key, err := strconv.ParseUint(field[2:], 16, 16)
		if err != nil {
			return "", fmt.Errorf("malformed P_Key %q", boundedTraceOutput(field))
		}
		if key&0x7fff == 0 {
			continue
		} // Empty slots are not membership.
		if key&0x8000 != 0 {
			full = true
		} else {
			limited = append(limited, fmt.Sprintf("0x%04x", key))
		}
	}
	if full {
		return "", nil
	}
	prefix := fmt.Sprintf("InfiniBand %s port %d", fields[0], port)
	if len(limited) == 0 {
		return prefix + ": no usable P_Key entries; check the subnet manager partition configuration", nil
	}
	return prefix + ": only limited-membership P_Keys (" + strings.Join(limited, ", ") + "). Two limited members cannot communicate within the same partition; ask the subnet manager administrator to grant appropriate full membership to the intended host/VF GUIDs", nil
}

type partitionExec func(context.Context, []string) (kubeclient.ExecResult, error)

func collectPartitionWarnings(ctx context.Context, pod TestPod, exec partitionExec) []string {
	// One shared budget per pod bounds diagnostic cost even with many rails.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rails := make([]string, 0, len(pod.InterfacesByRail))
	for rail := range pod.InterfacesByRail {
		rails = append(rails, rail)
	}
	sort.Strings(rails)
	var warnings []string
	for _, rail := range rails {
		iface := pod.InterfacesByRail[rail]
		prefix := fmt.Sprintf("Node %s pod %s/%s rail %s interface %s: ", pod.Node, pod.Namespace, pod.Name, rail, iface)
		// Positional arguments avoid interpolating network annotation values into shell code.
		var warning string
		var err error
		if iface == "" || iface == "." || iface == ".." || strings.ContainsAny(iface, "/\x00") {
			err = fmt.Errorf("invalid interface name")
		} else {
			var res kubeclient.ExecResult
			res, err = exec(ctx, []string{"/bin/sh", "-c", partitionProbe, "l8k-pkeys", "/sys", iface})
			if err != nil {
				err = fmt.Errorf("%w (stderr: %s)", err, boundedTraceOutput(res.Stderr))
			} else {
				warning, err = partitionWarning(res.Stdout)
			}
		}
		if err != nil {
			warning = "P_Key membership unverified: " + err.Error()
		}
		if warning != "" {
			warnings = append(warnings, prefix+warning)
		}
	}
	return warnings
}

func probePartitionWarnings(ctx context.Context, restConfig *rest.Config, pod TestPod, container string) []string {
	return collectPartitionWarnings(ctx, pod, func(ctx context.Context, command []string) (kubeclient.ExecResult, error) {
		return kubeclient.ExecInPod(ctx, restConfig, pod.Namespace, pod.Name, container, command)
	})
}
