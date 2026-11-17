// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"regexp"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

var versionToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._()+-]{0,127}$`)
var bdfToken = regexp.MustCompile(`(?i)^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

func collectHardware(ctx context.Context, remote runner, device *inventory.Device, privilege, role string, add func(string, string) string) {
	h := &inventory.HardwareObservation{Cards: []inventory.CardHardware{}, RShim: unavailable(&Error{Code: "NOT_APPLICABLE"}, "systemctl")}
	device.Runtime.Hardware = h
	version, versionErr := remote.Run(ctx, "mft-version", privilege+"mlxfwmanager --version")
	if versionErr == nil {
		matches := regexp.MustCompile(`(?i)\bmft\s+(\d+\.\d+\.\d+(?:-\d+)?)\b`).FindAllStringSubmatch(version, -1)
		if len(matches) != 1 {
			versionErr = &Error{Code: "SSH_RUNTIME_INVALID", Stage: "mft-version"}
		} else {
			device.Runtime.Observations["mft-version"] = observed("mlxfwmanager", matches[0][1])
			add("mft-version", "MFTVersion")
		}
	}
	if versionErr != nil {
		device.Runtime.Observations["mft-version"] = unavailable(versionErr, "mlxfwmanager")
	}
	var cards []pciCard
	if role == "hosts" {
		for _, a := range device.Adapters {
			cards = append(cards, pciCard{address: a.PCIAddress, model: a.Model})
		}
		text, err := remote.Run(ctx, "rshim-service", "systemctl show rshim --property=LoadState --property=ActiveState")
		if err == nil {
			values := []string{}
			for _, line := range strings.Split(text, "\n") {
				key, value, ok := strings.Cut(line, "=")
				if ok && (key == "LoadState" || key == "ActiveState") && versionToken.MatchString(value) {
					values = append(values, key+"="+value)
				}
			}
			if len(values) != 2 {
				err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: "rshim-service"}
			} else {
				h.RShim = observed("systemctl", values...)
				add("rshim-service", "service-state")
			}
		}
		if err != nil {
			h.RShim = unavailable(err, "systemctl")
		}
	} else {
		text, err := remote.Run(ctx, "arm-pci", "lspci -Dnn -d 15b3:")
		if err == nil {
			cards, err = pciCards(text)
		}
		if err != nil {
			device.Runtime.Observations["arm-pci"] = unavailable(err, "lspci")
			return
		}
		device.Runtime.Observations["arm-pci"] = observed("lspci")
		add("arm-pci", "PCIAddress")
	}
	for _, card := range cards {
		// Never interpolate any address other than a validated collector-owned BDF.
		if !bdfToken.MatchString(card.address) {
			continue
		}
		c := inventory.CardHardware{PCIAddress: card.address, Domain: "host-pci", Model: card.model, NVConfig: map[string]inventory.Observation{}, EvidenceRefs: []string{}, Privilege: unavailable(&Error{Code: "NOT_APPLICABLE"}, "mlxprivhost")}
		if role == "hosts" {
			for _, a := range device.Adapters {
				if a.PCIAddress == card.address {
					c.SerialNumber = a.SerialNumber
				}
			}
		} else {
			c.Domain = "arm-pci"
			stage := "arm-vpd:" + card.address
			text, err := remote.Run(ctx, stage, privilege+"lspci -D -vv -s "+card.address)
			if err == nil {
				c.SerialNumber, _, err = vpd(text)
				if err == nil && c.SerialNumber == "" {
					err = &Error{Code: "PROPERTY_NOT_EXPOSED", Stage: stage}
				}
			}
			if err != nil {
				device.Runtime.Observations[stage] = unavailable(err, "lspci")
			} else {
				device.Runtime.Observations[stage] = observed("lspci")
				c.EvidenceRefs = append(c.EvidenceRefs, add(stage, "SerialNumber"))
			}
		}
		stage := "firmware:" + card.address
		text, err := remote.Run(ctx, stage, privilege+"mlxfwmanager --query -d "+card.address)
		if err == nil {
			c.Firmware, c.PSID, err = parseFirmware(text, card.address)
		}
		if err != nil {
			c.Firmware = unavailable(err, "mlxfwmanager")
			c.PSID = unavailable(err, "mlxfwmanager")
		} else {
			c.EvidenceRefs = append(c.EvidenceRefs, add(stage, "firmware-psid"))
		}
		if strings.Contains(strings.ToLower(card.model), "bluefield") {
			for _, parameter := range []string{"INTERNAL_CPU_OFFLOAD_ENGINE", "INTERNAL_CPU_RSHIM", "NUM_OF_VFS"} {
				stage = "nvconfig:" + card.address + ":" + parameter
				text, err = remote.Run(ctx, stage, privilege+"mlxconfig -d "+card.address+" q "+parameter)
				var value inventory.Observation
				if err == nil {
					value, err = parseNVConfig(text, parameter)
				}
				if err != nil {
					value = unavailable(err, "mlxconfig")
				} else {
					c.EvidenceRefs = append(c.EvidenceRefs, add(stage, parameter))
				}
				c.NVConfig[parameter] = value
			}
			if role == "dpus" {
				stage = "privilege:" + card.address
				text, err = remote.Run(ctx, stage, privilege+"mlxprivhost -d "+card.address+" q --full")
				if err == nil {
					c.Privilege, err = parsePrivilege(text)
				}
				if err != nil {
					c.Privilege = unavailable(err, "mlxprivhost")
				} else {
					c.EvidenceRefs = append(c.EvidenceRefs, add(stage, "host-restrictions"))
				}
			}
		}
		h.Cards = append(h.Cards, c)
	}
}

func parseFirmware(text, bdf string) (inventory.Observation, inventory.Observation, error) {
	fw, psid := missing("mlxfwmanager"), missing("mlxfwmanager")
	seen := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, hasColon := strings.Cut(strings.TrimSpace(line), ":")
		if hasColon && (key == "PCI Device Name" || key == "PSID") {
			value = strings.TrimSpace(value)
			if old, ok := seen[key]; ok && old != value {
				return fw, psid, &Error{Code: "SSH_RUNTIME_INVALID"}
			}
			seen[key] = value
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "FW" {
			if old, ok := seen["FW"]; ok && old != fields[1] {
				return fw, psid, &Error{Code: "SSH_RUNTIME_INVALID"}
			}
			seen["FW"] = fields[1]
		}
	}
	// An exact -d query is the binding. If a PCI address is reported, it must agree.
	if address := seen["PCI Device Name"]; address != "" && !strings.EqualFold(address, bdf) && !strings.EqualFold("0000:"+address, bdf) {
		return fw, psid, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	if v := seen["FW"]; versionToken.MatchString(v) {
		fw = observed("mlxfwmanager", v)
	}
	if v := seen["PSID"]; versionToken.MatchString(v) {
		psid = observed("mlxfwmanager", v)
	}
	if fw.State != "observed" && psid.State != "observed" {
		return fw, psid, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	return fw, psid, nil
}

func parseNVConfig(text, parameter string) (inventory.Observation, error) {
	// Default mlxconfig q reports Next Boot, not necessarily the running state.
	// Preserve this distinction rather than infer mode from a pending value.
	next := strings.Contains(text, "Next Boot")
	var value string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == parameter {
			if len(f) != 2 || !versionToken.MatchString(f[1]) || value != "" {
				return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
			}
			value = f[1]
		}
	}
	if value == "" || !next {
		return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	return observed("mlxconfig", "next-boot="+value), nil
}

func parsePrivilege(text string) (inventory.Observation, error) {
	host := ""
	values := []string{}
	seen := map[string]bool{}
	levels := 0
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "host index" {
			if value == "" || strings.Trim(value, "0123456789") != "" {
				return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
			}
			host = value
			continue
		}
		if key != "level" && key != "disable_rshim" && key != "disable_tracer" && key != "disable_port_owner" && key != "disable_counter_rd" {
			continue
		}
		id := "host[" + host + "]." + key
		if host == "" || seen[id] {
			return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		if key == "level" {
			if value != "RESTRICTED" && value != "PRIVILEGED" {
				return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
			}
			levels++
		} else if value != "TRUE" && value != "FALSE" {
			return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		seen[id] = true
		values = append(values, id+"="+value)
	}
	if levels == 0 {
		return inventory.Observation{}, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	return observed("mlxprivhost", values...), nil
}
