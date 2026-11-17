// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bytes"
	"context"
	"maps"
	"net"
	"path"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const maxHostMACInterfaces = 4096 // Same bound as the host link inventory.

// collectHostPermanentMACs enriches only interfaces already attributed to host
// PCI functions. A current link-layer address is never substituted for a
// permanent address. PCI/name binding is reread around each ethtool query.
func collectHostPermanentMACs(ctx context.Context, remote runner, device *inventory.Device, label string) {
	if ctx == nil || remote == nil || device == nil || device.Runtime == nil || device.Runtime.Network == nil {
		return
	}
	runtime := device.Runtime
	if runtime.Observations == nil {
		runtime.Observations = map[string]inventory.Observation{}
	}
	owners := map[string][]string{}
	for _, adapter := range device.Adapters {
		for _, pf := range adapter.PCIFunctions {
			for _, name := range pf.Netdevs {
				owners[name] = append(owners[name], pf.Address)
			}
		}
	}
	for i := range runtime.Network.Links {
		if _, requested := owners[runtime.Network.Links[i].Name]; requested {
			runtime.Network.Links[i].PermanentMAC = ""
		}
	}
	if len(owners) > maxHostMACInterfaces {
		runtime.Observations["host-interfaces:permanent-mac"] = inventory.Observation{State: "unknown", ReasonCode: "HOST_INTERFACE_INVENTORY_LIMIT", Message: "Host PCI interface claims exceed the bounded permanent-MAC observation limit.", Tool: "sysfs"}
		return
	}
	output := ui.FromContext(ctx)
	for _, name := range slices.Sorted(maps.Keys(owners)) {
		functions := owners[name]
		stage := "host-interface:" + name + ":permanent-mac"
		var link *inventory.NetworkLink
		for i := range runtime.Network.Links {
			if runtime.Network.Links[i].Name == name {
				if link != nil {
					link = nil
					break
				}
				link = &runtime.Network.Links[i]
			}
		}
		if link == nil || len(functions) != 1 || !externalPCIAddress.MatchString(functions[0]) ||
			!hostInterfaceName.MatchString(name) || strings.HasPrefix(name, "-") || name == "." || name == ".." {
			runtime.Observations[stage] = inventory.Observation{State: "unknown", ReasonCode: "HOST_INTERFACE_BINDING_UNAVAILABLE", Message: "One observed network link and PCI function must uniquely identify the host interface before reading its permanent MAC."}
			output.Info("[UNAVAILABLE] %s: permanent MAC requires one valid, uniquely PCI-bound interface", label)
			continue
		}
		if link.PCIAddress != "" && !strings.EqualFold(link.PCIAddress, functions[0]) {
			runtime.Observations[stage] = inventory.Observation{State: "unknown", ReasonCode: "HOST_INTERFACE_BINDING_CONFLICT", Message: "The link and PCI inventory disagree about the host interface's device."}
			output.Info("[UNAVAILABLE] %s: interface %s has conflicting PCI bindings", label, name)
			continue
		}
		output.Info("[RUNNING] %s: interface %s permanent MAC and PCI binding", label, name)
		mac, err := readBoundPermanentMAC(ctx, remote, name, strings.ToLower(functions[0]), stage)
		if err != nil {
			runtime.Observations[stage] = unavailable(err, "ethtool")
			output.Info("[UNAVAILABLE] %s: interface %s permanent MAC (%s)", label, name, Code(err))
			continue
		}
		link.PermanentMAC = mac
		link.PCIAddress = strings.ToLower(functions[0])
		runtime.Observations[stage] = observed("ethtool")
		output.Info("[PASS] %s: interface %s permanent MAC and PCI binding verified", label, name)
	}
}

func readBoundPermanentMAC(ctx context.Context, remote runner, name, pci, stage string) (string, error) {
	command := "readlink -f /sys/class/net/" + name + "/device"
	expected := "/sys/devices/"
	readBinding := func(suffix string) error {
		text, err := remote.Run(ctx, stage+suffix, command)
		if err != nil {
			return err
		}
		devicePath := strings.TrimSpace(text)
		if !strings.HasPrefix(devicePath, expected) || strings.ContainsAny(devicePath, "\x00\r\n\t ") || path.Clean(devicePath) != devicePath || !strings.HasSuffix(devicePath, "/"+pci) {
			return &Error{Code: "INVENTORY_CHANGED", Stage: stage + suffix, Tool: "sysfs"}
		}
		return nil
	}
	if err := readBinding(":binding-before"); err != nil {
		return "", err
	}
	text, err := remote.Run(ctx, stage, "ethtool -P "+name)
	if err != nil {
		return "", err
	}
	mac, err := parsePermanentMAC(text)
	if err != nil {
		return "", err
	}
	if err := readBinding(":binding-after"); err != nil {
		return "", err
	}
	return mac, nil
}

func parsePermanentMAC(text string) (string, error) {
	value, found := strings.CutPrefix(strings.TrimSpace(text), "Permanent address: ")
	mac, err := net.ParseMAC(value)
	if !found || err != nil || len(mac) != 6 || mac[0]&1 != 0 || bytes.Equal(mac, make([]byte, 6)) {
		return "", &Error{Code: "SSH_RUNTIME_INVALID", Stage: "permanent-mac", Tool: "ethtool"}
	}
	return mac.String(), nil
}
