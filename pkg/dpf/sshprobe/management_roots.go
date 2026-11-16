// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const maxManagementRoots = inventory.MaxHostManagementRoots

// HostManagementInterfaces selects only the authenticated SSH server address and
// observed IPv4/IPv6 default paths. Unrelated virtual interfaces are not roots.
func HostManagementInterfaces(runtime *inventory.RuntimeObservation) ([]string, error) {
	if runtime == nil || runtime.Interfaces == nil {
		return nil, fmt.Errorf("complete host interfaces/routes required")
	}
	for _, key := range []string{"interfaces", "routes"} {
		if runtime.Observations[key].State != "" && runtime.Observations[key].State != "observed" {
			return nil, fmt.Errorf("host interface/route source unobserved")
		}
	}
	if runtime.SSHServerAddress == "" || runtime.Observations["ssh-server-address"].State != "" {
		return nil, fmt.Errorf("authenticated SSH local address unobserved")
	}
	ip, err := netip.ParseAddr(runtime.SSHServerAddress)
	if err != nil {
		return nil, fmt.Errorf("SSH local address invalid")
	}
	indexes := map[int]string{}
	names := map[string]bool{}
	selected := map[string]bool{}
	matches := 0
	for _, iface := range runtime.Interfaces {
		if !hostInterfaceName.MatchString(iface.Name) || iface.Name == "." || iface.Name == ".." || strings.HasPrefix(iface.Name, "-") || iface.Index < 1 || indexes[iface.Index] != "" || names[iface.Name] {
			return nil, fmt.Errorf("ambiguous host interface identities")
		}
		indexes[iface.Index], names[iface.Name] = iface.Name, true
		for _, text := range iface.Addresses {
			prefix, e := netip.ParsePrefix(text)
			if e != nil {
				return nil, fmt.Errorf("invalid host address")
			}
			if prefix.Addr().Unmap() == ip.Unmap() {
				selected[iface.Name] = true
				matches++
			}
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("SSH local address does not identify exactly one current interface")
	}
	for _, route := range runtime.Routes {
		prefix, e := netip.ParsePrefix(route.Destination)
		if e != nil {
			return nil, fmt.Errorf("invalid host route")
		}
		if prefix.Bits() == 0 {
			// The host route decoder permits index zero only for explicit
			// nonforwarding routes (throw/unreachable/blackhole/prohibit).
			if route.InterfaceIndex == 0 && nonForwardingRoute(route.Type) {
				continue
			}
			if indexes[route.InterfaceIndex] == "" {
				return nil, fmt.Errorf("default path interface unobserved")
			}
			selected[indexes[route.InterfaceIndex]] = true
		}
	}
	if len(selected) > maxManagementRoots {
		return nil, fmt.Errorf("management interface observation limit exceeded")
	}
	result := []string{}
	for name := range selected {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func collectManagementRoots(ctx context.Context, remote runner, runtime *inventory.RuntimeObservation, add func(string, string) string) {
	const stage = "host-management-roots"
	runtime.SSHServerAddress = ""
	if runtime.Network != nil {
		for i := range runtime.Network.Links {
			runtime.Network.Links[i].PhysicalPFRoots = nil
		}
	}
	connection, err := remote.Run(ctx, "host-ssh-connection", `printf '%s\n' "$SSH_CONNECTION"`)
	fields := strings.Fields(connection)
	if err == nil {
		if len(fields) != 4 {
			err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: stage}
		} else {
			for _, i := range []int{0, 2} {
				if _, e := netip.ParseAddr(fields[i]); e != nil {
					err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: stage}
				}
			}
			for _, i := range []int{1, 3} {
				n, e := strconv.Atoi(fields[i])
				if e != nil || n < 1 || n > 65535 {
					err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: stage}
				}
			}
		}
	}
	if err != nil {
		runtime.SSHServerAddress = ""
		runtime.Observations["ssh-server-address"] = unavailable(err, "ssh")
		return
	}
	runtime.SSHServerAddress = fields[2]
	add("host-ssh-connection", "ServerAddress")
	names, err := HostManagementInterfaces(runtime)
	if err != nil || runtime.Network == nil {
		runtime.Observations[stage] = unavailable(&Error{Code: "HOST_MANAGEMENT_PATH_UNMEASURED", Stage: stage}, "sysfs")
		return
	}
	command := managementRootsCommand(names)
	text, err := remote.Run(ctx, stage, command)
	if err != nil {
		runtime.Observations[stage] = unavailable(err, "sysfs")
		return
	}
	roots, err := parseManagementRoots(text, names)
	if err != nil {
		runtime.Observations[stage] = unavailable(&Error{Code: "HOST_MANAGEMENT_ROOTS_INVALID", Stage: stage}, "sysfs")
		return
	}
	for i := range runtime.Network.Links {
		link := &runtime.Network.Links[i]
		link.PhysicalPFRoots = roots[link.Name]
		if len(link.PhysicalPFRoots) > 0 {
			add(stage, link.Name)
		}
	}
	runtime.Observations[stage] = observed("sysfs")
}

func parseManagementRoots(text string, names []string) (map[string][]string, error) {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	result := map[string][]string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !wanted[fields[0]] || !externalPCIAddress.MatchString(fields[1]) || seen[fields[0]+" "+strings.ToLower(fields[1])] {
			return nil, fmt.Errorf("invalid physical root observation")
		}
		seen[fields[0]+" "+strings.ToLower(fields[1])] = true
		result[fields[0]] = append(result[fields[0]], strings.ToLower(fields[1]))
		if len(seen) > maxManagementRoots {
			return nil, fmt.Errorf("physical root limit")
		}
	}
	if len(result) != len(wanted) {
		return nil, fmt.Errorf("incomplete physical root observation")
	}
	for _, values := range result {
		sort.Strings(values)
	}
	return result, nil
}

// Fixed read-only Linux sysfs reader. One shared budget covers interfaces,
// total interface/physfn visits and output rows. Lower links and physfn resolve
// physical parentage rather than BDF inequality; no temporary files are created.
const managementRootReader = `set -euo pipefail
limit=$1; shift
[[ "$limit" =~ ^[1-9][0-9]*$ && $# -le "$limit" ]] || exit 1
calls=0; rows=0
walk_interface() {
 local name=$1 ancestry=$2 path before device pci initial entry lower lowers root
 ((calls+=1)); ((calls<=limit)) || return 1
 [[ "$name" =~ ^[a-zA-Z0-9_.:-]{1,15}$ && "$name" != . && "$name" != .. && "$name" != -* && "$ancestry" != *"|$name|"* ]] || return 1
 path="/sys/class/net/$name"; [[ -d "$path" ]] || return 1
 before=$(readlink -f -- "$path") || return 1
 device=$(find "$path/" -mindepth 1 -maxdepth 1 -name device -print) || return 1
 if [[ -n "$device" ]]; then
  [[ -L "$path/device" ]] || return 1
  pci=$(readlink -f -- "$path/device") || return 1
  initial=$pci; root=''
  while [[ -z "$root" ]]; do
   ((calls+=1)); ((calls<=limit)) || return 1
   [[ -d "$pci" ]] || return 1
   entry=$(find "$pci/" -mindepth 1 -maxdepth 1 -name physfn -print) || return 1
   if [[ -z "$entry" ]]; then
    root=${pci##*/}; [[ "$root" =~ ^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$ ]] || return 1
   else
    [[ -L "$pci/physfn" ]] || return 1
    pci=$(readlink -f -- "$pci/physfn") || return 1
   fi
  done
  [[ -d "$pci" && $(readlink -f -- "$path/device") == "$initial" ]] || return 1
  physical_roots+=("$root")
 else
  lowers=$(find "$path/" -mindepth 1 -maxdepth 1 -name 'lower_*' -print) || return 1
  [[ -n "$lowers" ]] || return 1
  while IFS= read -r lower; do
   [[ -L "$lower" ]] || return 1
   entry=$(readlink -f -- "$lower") || return 1
   walk_interface "${entry##*/}" "$ancestry|$name|" || return 1
  done <<<"$lowers"
 fi
 [[ $(readlink -f -- "$path") == "$before" ]] || return 1
}
for iface in "$@"; do
 physical_roots=(); walk_interface "$iface" '|' || exit 1
 roots=$(printf '%s\n' "${physical_roots[@]}" | sort -u) || exit 1
 [[ -n "$roots" ]] || exit 1
 while IFS= read -r root; do ((rows+=1)); ((rows<=limit)) || exit 1; printf '%s %s\n' "$iface" "$root"; done <<<"$roots"
done`

func managementRootsCommand(names []string) string {
	// All names come from HostManagementInterfaces' bounded token validation.
	arguments := ""
	for _, name := range names {
		arguments += " '" + name + "'"
	}
	return "bash -c '" + strings.ReplaceAll(managementRootReader, "'", "'\\''") + "' -- " + strconv.Itoa(maxManagementRoots) + arguments
}

// VerifyHostManagementPhysicalSeparation requires measured physical ancestry
// for the actual SSH and default paths, including VF physfn/card-slot identity.
// It is shared by initial and owned pending-reset eligibility.
func VerifyHostManagementPhysicalSeparation(host inventory.Device, adapter inventory.Adapter) error {
	names, err := HostManagementInterfaces(host.Runtime)
	if err != nil || host.Runtime.Network == nil || (host.Runtime.Observations["host-management-roots"].State != "" && host.Runtime.Observations["host-management-roots"].State != "observed") {
		return &Error{Code: "HOST_MANAGEMENT_ROOTS_UNMEASURED", Stage: "host-management-roots"}
	}
	if !externalPCIAddress.MatchString(adapter.PCIAddress) || len(adapter.PCIFunctions) == 0 {
		return &Error{Code: "HOST_MANAGEMENT_ROOTS_UNMEASURED", Stage: "host-management-roots"}
	}
	selectedSlot := pciSlot(strings.ToLower(adapter.PCIAddress))
	for _, pf := range adapter.PCIFunctions {
		if !externalPCIAddress.MatchString(pf.Address) || pciSlot(strings.ToLower(pf.Address)) != selectedSlot {
			return &Error{Code: "HOST_MANAGEMENT_ROOTS_UNMEASURED", Stage: "host-management-roots"}
		}
	}
	total := 0
	for _, name := range names {
		var roots []string
		for _, link := range host.Runtime.Network.Links {
			if link.Name == name {
				if roots != nil {
					return &Error{Code: "HOST_MANAGEMENT_ROOTS_UNMEASURED", Stage: "host-management-roots"}
				}
				roots = link.PhysicalPFRoots
			}
		}
		total += len(roots)
		if len(roots) == 0 || total > maxManagementRoots {
			return &Error{Code: "HOST_MANAGEMENT_ROOTS_UNMEASURED", Stage: "host-management-roots"}
		}
		seen := map[string]bool{}
		for _, root := range roots {
			if !externalPCIAddress.MatchString(root) || seen[strings.ToLower(root)] {
				return &Error{Code: "HOST_MANAGEMENT_ROOTS_UNMEASURED", Stage: "host-management-roots"}
			}
			seen[strings.ToLower(root)] = true
			if pciSlot(strings.ToLower(root)) == selectedSlot {
				return &Error{Code: "HOST_MANAGEMENT_ON_SELECTED_CARD", Stage: "host-management-roots"}
			}
		}
	}
	return nil
}
