// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

type Collector struct{}

type runner interface {
	Run(context.Context, string, string) (string, error)
}

func (Collector) Collect(ctx context.Context, target config.Target, timeout time.Duration) (device inventory.Device, evidence map[string]inventory.Evidence, err error) {
	device = emptyDevice(target)
	evidence = map[string]inventory.Evidence{}
	if target.SSH == nil || (target.Role != "hosts" && target.Role != "dpus") {
		device.ReasonCode = "SSH_CONFIGURATION_INVALID"
		return device, evidence, &Error{Code: device.ReasonCode, Stage: "target"}
	}
	user, password := target.SSH.Credentials()
	c, err := connect(ctx, target.Endpoint, credentials{user: user, password: password, key: target.SSH.PrivateKey(), knownHosts: target.SSH.KnownHosts()}, timeout)
	if err != nil {
		device.ReasonCode = Code(err)
		return device, evidence, err
	}
	defer c.Close()
	return collect(ctx, target, c)
}

func emptyDevice(target config.Target) inventory.Device {
	return inventory.Device{
		RecordKey: inventory.Digest("dpf-access-v1", []string{target.Role, target.Endpoint, target.SystemURI}),
		InputRef:  target.InputRef, SSHEndpoint: target.Endpoint, ObservationState: "unavailable",
		Qualification: "unqualified", InventoryCompleteness: "unknown", BMCAddressState: "not-evaluated",
		Authentication: inventory.Authentication{Method: "ssh", PasswordRead: "not-evaluated", ClientCertificateSupport: "unknown", DPFClientAuthentication: "not-evaluated"},
		Firmware:       []inventory.Firmware{}, EvidenceRefs: []string{},
		Time:    inventory.TimeObservation{State: "unknown", ReasonCode: "NOT_EVALUATED"},
		Runtime: &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}},
	}
}

func collect(ctx context.Context, target config.Target, remote runner) (device inventory.Device, evidence map[string]inventory.Evidence, err error) {
	device = emptyDevice(target)
	evidence = map[string]inventory.Evidence{}
	defer func() {
		if err != nil {
			device.ReasonCode = Code(err)
		}
	}()
	device.ReaderAdapter = "linux-host-ssh-v1"
	if target.Role == "dpus" {
		device.ReaderAdapter = "bluefield-arm-ssh-v1"
	}
	privilege := ""
	if target.SSH != nil && target.SSH.Sudo {
		privilege = "sudo -n "
	}
	add := func(stage, property string) string {
		id := inventory.Digest("dpf-evidence-v1", []string{device.RecordKey, stage, property})
		evidence[id] = inventory.Evidence{Kind: "ssh", InputRef: target.InputRef, ResourceURI: stage, Property: property, ObservedAt: time.Now().UTC(), Vantage: target.Role + "-os", Adapter: device.ReaderAdapter, Authenticated: true}
		device.EvidenceRefs = append(device.EvidenceRefs, id)
		return id
	}
	read := func(stage, command string) (string, error) { return remote.Run(ctx, stage, command) }
	if target.Role == "dpus" {
		dmi, e := read("arm-dmi", privilege+"dmidecode -s system-serial-number")
		if e != nil {
			return device, evidence, e
		}
		fru, e := read("arm-fru", privilege+"ipmitool fru print 0")
		if e != nil {
			return device, evidence, e
		}
		serial, model, maker, e := armIdentity(dmi, fru)
		if e != nil {
			return device, evidence, e
		}
		device.SerialNumber, device.SerialNumberRaw = serial, strings.TrimSpace(dmi)
		device.Model, device.Manufacturer = model, maker
		device.IdentityKey = []string{"nvidia", serial}
		device.ID = inventory.PhysicalID("dpu", device.IdentityKey)
		add("arm-dmi", "SerialNumber")
		add("arm-fru", "Product Serial")
		device.BMCAddressState = "unavailable"
		lan, e := read("arm-ipmi-lan", privilege+"ipmitool lan print")
		if e == nil {
			device.BMCEndpoint, e = bmcAddress(lan)
		}
		if e == nil {
			device.BMCAddressState = "observed-local-ipmi"
			add("arm-ipmi-lan", "IP Address")
			device.Runtime.Observations["bmcAddress"] = inventory.Observation{State: "observed", Values: []string{device.BMCEndpoint}}
		} else {
			device.Runtime.Observations["bmcAddress"] = inventory.Observation{State: "unavailable", ReasonCode: Code(e)}
		}
	} else {
		uuid, e := read("host-dmi", privilege+"dmidecode -s system-uuid")
		if e != nil {
			return device, evidence, e
		}
		uuid = strings.TrimSpace(uuid)
		if !validUUID(uuid) {
			return device, evidence, &Error{Code: "SSH_HOST_IDENTITY_INVALID", Stage: "host-dmi"}
		}
		device.SystemUUID = uuid
		device.IdentityKey = []string{"system-uuid", strings.ToLower(uuid)}
		device.ID = inventory.PhysicalID("host", device.IdentityKey)
		add("host-dmi", "UUID")
		pci, e := read("host-pci", "lspci -Dnn -d 15b3:")
		if e != nil {
			return device, evidence, e
		}
		cards, e := pciCards(pci)
		if e != nil {
			return device, evidence, e
		}
		pciEvidence := add("host-pci", "PCIAddress")
		for _, card := range cards {
			a := inventory.Adapter{Key: inventory.Digest("dpf-adapter-pci-v1", []string{device.ID, card.address[:len(card.address)-2]}), PCIAddress: card.address, VendorID: card.vendor, DeviceID: card.device, Model: card.model, IdentityState: "partial", ReasonCode: "CARD_SERIAL_MISSING", Selection: "not-selected", EvidenceRefs: []string{pciEvidence}}
			text, e := read("host-vpd:"+card.address, privilege+"lspci -D -vv -s "+card.address)
			if e == nil {
				serial, slot, parseErr := vpd(text)
				if parseErr != nil {
					return device, evidence, parseErr
				}
				a.SerialNumber, a.SerialNumberRaw, a.Slot = serial, serial, slot
				a.EvidenceRefs = append(a.EvidenceRefs, add("host-vpd:"+card.address, "SerialNumber"))
				if serial != "" {
					a.IdentityState = "observed"
					a.ReasonCode = ""
				}
			} else {
				a.ReasonCode = Code(e)
			}
			for _, bdf := range card.functions {
				a.PCIFunctions = append(a.PCIFunctions, collectPF(ctx, remote, bdf, device.Runtime, add))
			}
			device.Adapters = append(device.Adapters, a)
		}
		device.Runtime.Observations["pciScope"] = inventory.Observation{State: "observed", Values: []string{"NVIDIA network PCI functions; grouped by host-local PCI slot"}}
	}
	collectRuntime(ctx, remote, device.Runtime, add)
	// Recheck physical identity after optional observations, so disconnects and
	// device changes cannot publish stale identity as a successful observation.
	if target.Role == "dpus" {
		dmi, e := read("arm-dmi-recheck", privilege+"dmidecode -s system-serial-number")
		if e != nil {
			return device, evidence, e
		}
		fru, e := read("arm-fru-recheck", privilege+"ipmitool fru print 0")
		if e != nil {
			return device, evidence, e
		}
		serial, model, maker, e := armIdentity(dmi, fru)
		if e != nil {
			return device, evidence, e
		}
		if serial != device.SerialNumber || model != device.Model || maker != device.Manufacturer {
			return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "arm-identity-recheck"}
		}
	} else {
		uuid, e := read("host-dmi-recheck", privilege+"dmidecode -s system-uuid")
		if e != nil {
			return device, evidence, e
		}
		if strings.TrimSpace(uuid) != device.SystemUUID {
			return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "host-identity-recheck"}
		}
		pci, e := read("host-pci-recheck", "lspci -Dnn -d 15b3:")
		if e != nil {
			return device, evidence, e
		}
		cards, e := pciCards(pci)
		if e != nil {
			return device, evidence, e
		}
		if len(cards) != len(device.Adapters) {
			return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "host-pci-recheck"}
		}
		for i, card := range cards {
			a := device.Adapters[i]
			if card.address != a.PCIAddress || card.vendor != a.VendorID || card.device != a.DeviceID || card.model != a.Model {
				return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "host-pci-recheck"}
			}
			if len(card.functions) != len(a.PCIFunctions) {
				return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "host-pci-functions-recheck"}
			}
			for j, bdf := range card.functions {
				if bdf != a.PCIFunctions[j].Address {
					return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "host-pci-functions-recheck"}
				}
			}
			if a.SerialNumber == "" {
				continue
			}
			text, e := read("host-vpd-recheck:"+card.address, privilege+"lspci -D -vv -s "+card.address)
			if e != nil {
				return device, evidence, e
			}
			serial, slot, e := vpd(text)
			if e != nil {
				return device, evidence, e
			}
			if serial != a.SerialNumber || slot != a.Slot {
				return device, evidence, &Error{Code: "INVENTORY_CHANGED", Stage: "host-vpd-recheck"}
			}
		}
	}
	if ctx.Err() != nil {
		return device, evidence, &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: "collection"}
	}
	device.ObservationState = "observed"
	device.InventoryCompleteness = "observed"
	for _, a := range device.Adapters {
		if a.IdentityState != "observed" {
			device.InventoryCompleteness = "partial"
		}
	}
	return device, evidence, nil
}

func collectPF(ctx context.Context, remote runner, bdf string, runtime *inventory.RuntimeObservation, add func(string, string) string) inventory.PCIFunction {
	pf := inventory.PCIFunction{Address: bdf, Netdevs: []string{}}
	base := "/sys/bus/pci/devices/" + bdf
	for _, p := range []struct{ field, command string }{{"driver", "readlink " + base + "/driver"}, {"numa", "cat " + base + "/numa_node"}, {"netdevs", "ls -1 " + base + "/net"}} {
		stage := "host-pf:" + bdf + ":" + p.field
		text, err := remote.Run(ctx, stage, p.command)
		if err == nil {
			text = strings.TrimSpace(text)
			switch p.field {
			case "driver":
				if text == "" || strings.ContainsAny(text, "\r\n\t ") {
					err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: stage}
				} else {
					pf.Driver = path.Base(text)
				}
			case "numa":
				n, e := strconv.Atoi(text)
				if e != nil || n < -1 {
					err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: stage}
				} else {
					pf.NUMANode = text
				}
			case "netdevs":
				if text != "" {
					pf.Netdevs = strings.Split(text, "\n")
				}
			}
		}
		if err != nil {
			runtime.Observations[stage] = inventory.Observation{State: "unavailable", ReasonCode: Code(err)}
		} else {
			runtime.Observations[stage] = inventory.Observation{State: "observed"}
			add(stage, p.field)
		}
	}
	return pf
}

func collectRuntime(ctx context.Context, remote runner, runtime *inventory.RuntimeObservation, add func(string, string) string) {
	for _, probe := range []struct{ stage, command string }{
		{"hostname", "hostname"}, {"os", "cat /etc/os-release"}, {"interfaces", "ip -j address show"}, {"routes", "ip -j -4 route show"}, {"dns", "cat /etc/resolv.conf"},
	} {
		text, err := remote.Run(ctx, probe.stage, probe.command)
		if err == nil {
			switch probe.stage {
			case "hostname":
				runtime.Hostname = strings.TrimSpace(text)
				if runtime.Hostname == "" || strings.ContainsAny(runtime.Hostname, "\r\n\t ") {
					runtime.Hostname = ""
					err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: probe.stage}
				}
			case "os":
				for _, line := range strings.Split(text, "\n") {
					if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
						runtime.OS = strings.Trim(value, "\"")
					}
				}
				if runtime.OS == "" {
					err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: probe.stage}
				}
			case "interfaces":
				runtime.Interfaces, err = interfaces(text)
			case "routes":
				runtime.Routes, err = routes(text, runtime.Interfaces)
			case "dns":
				for _, line := range strings.Split(text, "\n") {
					f := strings.Fields(line)
					if len(f) >= 2 && f[0] == "nameserver" {
						runtime.DNS = append(runtime.DNS, f[1])
					}
				}
			}
		}
		if err != nil {
			runtime.Observations[probe.stage] = inventory.Observation{State: "unavailable", ReasonCode: Code(err)}
		} else {
			runtime.Observations[probe.stage] = inventory.Observation{State: "observed"}
			add(probe.stage, probe.stage)
		}
	}
}
