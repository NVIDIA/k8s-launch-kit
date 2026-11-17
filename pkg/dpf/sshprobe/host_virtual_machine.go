// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// systemd's --vm reports none with exit 1, a VM identifier with exit 0, and
// errors separately. Normalize only those exact cases; combined error output
// cannot become a positive no-VM result. No tools are installed by discovery.
const hostVirtualMachineReader = `set -uo pipefail
# FD 3 preserves stdout while command substitution separately captures stderr.
# A non-printing delimiter retains the exact stdout newline and exit status.
separator=$(printf "\034")
result=$( {
 diagnostic=$(systemd-detect-virt --vm 2>&1 1>&3; printf "\034%s" "$?")
 status=${diagnostic##*"$separator"}; diagnostic=${diagnostic%"$separator"*}
 printf "\034%s\034%s\034" "$status" "$diagnostic"
} 3>&1)
[[ "$result" == *"$separator" ]] || exit 1
result=${result%"$separator"}
[[ "$result" == *"$separator"* ]] || exit 1
value=${result%%"$separator"*}; rest=${result#*"$separator"}
[[ "$rest" == *"$separator"* ]] || exit 1
status=${rest%%"$separator"*}; diagnostic=${rest#*"$separator"}
[[ -z "$diagnostic" ]] || { printf "%s\n" "$diagnostic" >&2; exit 1; }
if [[ "$status" == 1 && "$value" == $'none\n' ]]; then printf "none\n"
else
 identifier=${value%$'\n'}
 if [[ "$status" == 0 && "$value" == "$identifier"$'\n' && "$identifier" != none && "$identifier" =~ ^[a-z][a-z0-9_-]{0,63}$ ]]; then printf "vm\n"; else exit 1; fi
fi`

func hostVirtualMachineCommand() string {
	return "bash -c '" + strings.ReplaceAll(hostVirtualMachineReader, "'", "'\\''") + "'"
}

func collectHostVirtualMachine(ctx context.Context, remote runner, runtime *inventory.RuntimeObservation, add func(string, string) string) {
	const stage = "host-virtual-machine"
	runtime.VirtualMachine = nil
	text, err := remote.Run(ctx, stage, hostVirtualMachineCommand())
	if err != nil {
		runtime.Observations[stage] = unavailable(err, "systemd-detect-virt")
		return
	}
	var vm bool
	switch strings.TrimSuffix(text, "\n") {
	case "none":
		vm = false
	case "vm":
		vm = true
	default:
		runtime.Observations[stage] = unavailable(&Error{Code: "HOST_VM_CLASSIFICATION_INVALID", Stage: stage}, "systemd-detect-virt")
		return
	}
	runtime.VirtualMachine = &vm
	delete(runtime.Observations, stage)
	add(stage, "VirtualMachine")
}
