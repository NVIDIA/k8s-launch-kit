// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"errors"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Collector struct{}

type runner interface {
	Run(context.Context, string, string) (string, error)
}

func (Collector) Collect(ctx context.Context, target config.Target, timeout time.Duration) (device inventory.Device, evidence map[string]inventory.Evidence, err error) {
	started := time.Now()
	label := target.DisplayName()
	logger := log.FromContext(ctx).WithName("ssh").WithValues("endpoint", label, "role", target.Role)
	ctx = log.IntoContext(ctx, logger)
	output := ui.FromContext(ctx)
	output.Info("[RUNNING] %s: SSH connection/session", label)
	connected := false
	defer func() {
		if !connected {
			output.Info("[FAIL] %s: SSH connection/session (%s)", label, Code(err))
		}
		logger.V(1).Info("SSH collection completed", "identityState", device.ObservationState, "identityEstablished", device.ID != "", "reasonCode", device.ReasonCode, "duration", time.Since(started))
	}()
	device = emptyDevice(target)
	evidence = map[string]inventory.Evidence{}
	if target.SSH == nil || (target.Role != "hosts" && target.Role != "dpus") {
		device.ReasonCode = "SSH_CONFIGURATION_INVALID"
		device.Message = "SSH discovery requires an explicit host or DPU SSH target."
		return device, evidence, &Error{Code: device.ReasonCode, Stage: "target"}
	}
	user, password := target.SSH.Credentials()
	auth := credentials{user: user, password: password, key: target.SSH.PrivateKey(), knownHosts: target.SSH.KnownHosts()}
	remote, err := newReconnectingRunner(ctx, target.Endpoint, auth, timeout)
	clear(auth.key)
	clear(auth.knownHosts)
	auth.password = ""
	if err != nil {
		device.ReasonCode = Code(err)
		device.Message = Reason(err)
		return device, evidence, err
	}
	connected = true
	output.Info("[PASS] %s: SSH connection/session established", label)
	defer func() {
		output.Info("[RUNNING] %s: SSH connection cleanup", label)
		cleanupStarted := time.Now()
		state := "complete"
		if closeErr := remote.Close(); closeErr != nil {
			state = "unconfirmed"
			output.Info("[UNAVAILABLE] %s: SSH connection cleanup could not be confirmed", label)
		} else {
			output.Info("[PASS] %s: SSH connection cleanup completed", label)
		}
		logger.V(1).Info("SSH connection cleanup completed", "state", state, "duration", time.Since(cleanupStarted))
	}()
	return collect(ctx, target, remote)
}

func emptyDevice(target config.Target) inventory.Device {
	return inventory.Device{
		RecordKey: inventory.Digest("dpf-access-v1", []string{target.Role, target.Endpoint, target.SystemURI}),
		InputRef:  target.InputRef, SSHEndpoint: target.Endpoint, ObservationState: "unavailable",
		Qualification: "unqualified", InventoryCompleteness: "unknown", BMCAddressState: "not-evaluated",
		Authentication: inventory.Authentication{Method: "ssh", PasswordRead: "not-evaluated", ClientCertificateSupport: "unknown", DPFClientAuthentication: "not-evaluated"},
		Firmware:       []inventory.Firmware{}, EvidenceRefs: []string{},
		Time:    inventory.TimeObservation{State: "unknown", ReasonCode: "NOT_EVALUATED", Message: "BMC clock and NTP are not queried through OS SSH."},
		Runtime: &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}},
	}
}

func collect(ctx context.Context, target config.Target, remote runner) (device inventory.Device, evidence map[string]inventory.Evidence, err error) {
	label := target.DisplayName()
	output := ui.FromContext(ctx)
	stage := "Physical inventory"
	output.Info("[RUNNING] %s: %s", label, stage)
	device = emptyDevice(target)
	evidence = map[string]inventory.Evidence{}
	defer func() {
		if err != nil {
			output.Info("[FAIL] %s: %s (%s)", label, stage, Code(err))
			device.ReasonCode = Code(err)
			device.Message = Reason(err)
			var diagnostic *Error
			if errors.As(err, &diagnostic) {
				if _, exists := device.Runtime.Observations[diagnostic.Stage]; !exists {
					device.Runtime.Observations[diagnostic.Stage] = unavailable(err, diagnostic.Tool)
				}
			}
		}
	}()
	if target.Role == "dpus" {
		output.Info("[UNAVAILABLE] %s: DPU BMC administrator privileges require an explicit Redfish endpoint", label)
	}
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
	read := func(stage, command string) (string, error) {
		text, e := remote.Run(ctx, stage, command)
		if e != nil {
			device.Runtime.Observations[stage] = unavailable(e, commandTool(command))
		}
		return text, e
	}
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
		fruFields, _ := fields(fru)
		if board := fruFields["Board Product"]; board != "" && len(board) <= 256 && !strings.ContainsAny(board, "\x00\r\n") {
			device.Runtime.Observations["board-product"] = observed("ipmitool", board)
			add("arm-fru", "Board Product")
		} else {
			device.Runtime.Observations["board-product"] = missing("ipmitool")
		}
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
			device.Runtime.Observations["bmcAddress"] = unavailable(e, "ipmitool")
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
			a := inventory.Adapter{Key: inventory.Digest("dpf-adapter-pci-v1", []string{device.ID, card.address[:len(card.address)-2]}), PCIAddress: card.address, VendorID: card.vendor, DeviceID: card.device, Model: card.model, IdentityState: "unknown", ReasonCode: "CARD_SERIAL_MISSING", Selection: "not-selected", EvidenceRefs: []string{pciEvidence}}
			text, e := read("host-vpd:"+card.address, privilege+"lspci -D -vv -s "+card.address)
			if e == nil {
				serial, slot, parseErr := vpd(text)
				if parseErr != nil {
					return device, evidence, parseErr
				}
				a.SerialNumber, a.SerialNumberRaw, a.Slot = serial, serial, slot
				a.EvidenceRefs = append(a.EvidenceRefs, add("host-vpd:"+card.address, "SerialNumber"))
				if serial != "" {
					a.IdentityState = "known"
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
	output.Info("[RUNNING] %s: Physical inventory read completed; identity recheck pending", label)
	stage = "Optional OS/network, firmware and tool observations"
	output.Info("[RUNNING] %s: %s", label, stage)
	collectNetwork(ctx, remote, device.Runtime, add)
	collectRuntime(ctx, remote, device.Runtime, add)
	collectHardware(ctx, remote, &device, privilege, target.Role, add)
	observed, unavailableCount := optionalCounts(device.Runtime)
	if unavailableCount > 0 {
		output.Info("[UNAVAILABLE] %s: Optional OS/network/firmware reads completed; %d observed, %d unavailable or unknown; inspect the detailed report for reasons, compatibility not evaluated", label, observed, unavailableCount)
	} else {
		output.Info("[PASS] %s: Optional OS/network/firmware reads completed; %d observations measured, compatibility not evaluated", label, observed)
	}
	// Recheck physical identity after optional observations, so disconnects and
	// device changes cannot publish stale identity as a successful observation.
	stage = "Physical identity recheck"
	output.Info("[RUNNING] %s: %s", label, stage)
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
	output.Info("[PASS] %s: Physical identity recheck verified; connection cleanup pending", label)
	device.InventoryCompleteness = "observed"
	for _, a := range device.Adapters {
		if a.IdentityState != "known" {
			device.InventoryCompleteness = "partial"
		}
	}
	return device, evidence, nil
}

func collectPF(ctx context.Context, remote runner, bdf string, runtime *inventory.RuntimeObservation, add func(string, string) string) inventory.PCIFunction {
	pf := inventory.PCIFunction{Address: bdf, Netdevs: []string{}}
	if !bdfToken.MatchString(bdf) {
		stage := "host-pf:" + bdf + ":iommu-group"
		runtime.Observations[stage] = unavailable(&Error{Code: "SSH_PCI_INVENTORY_INVALID", Stage: stage}, "sysfs")
		return pf
	}
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
			runtime.Observations[stage] = unavailable(err, commandTool(p.command))
		} else {
			runtime.Observations[stage] = inventory.Observation{State: "observed"}
			add(stage, p.field)
		}
	}
	stage := "host-pf:" + bdf + ":iommu-group"
	path := base + "/iommu_group"
	command := `sh -c 'p=` + path + `; d=` + base + `; [ -d "$d" ] || exit 1; if [ -L "$p" ]; then readlink -f "$p"; elif [ -e "$p" ]; then exit 1; else printf "absent\n"; fi'`
	text, err := remote.Run(ctx, stage, command)
	if err == nil {
		var absent bool
		pf.IOMMUGroup, absent, err = iommuGroup(text)
		if err == nil && absent {
			runtime.Observations[stage] = inventory.Observation{State: "unknown", ReasonCode: "IOMMU_GROUP_ABSENT", Message: "No IOMMU group is exposed for this PCI function.", Tool: "sysfs"}
			return pf
		}
		if err == nil && pf.IOMMUGroup == nil {
			err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: stage}
		}
	}
	if err != nil {
		runtime.Observations[stage] = unavailable(err, "sysfs")
		return pf
	}
	runtime.Observations[stage] = observed("sysfs", "group="+strconv.Itoa(*pf.IOMMUGroup))
	add(stage, "IOMMUGroup")
	return pf
}

func collectRuntime(ctx context.Context, remote runner, runtime *inventory.RuntimeObservation, add func(string, string) string) {
	for _, probe := range []struct{ stage, command string }{
		{"hostname", "hostname"}, {"os", "cat /etc/os-release"}, {"interfaces", "ip -j address show"}, {"dns", "cat /etc/resolv.conf"},
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
			case "dns":
				for _, line := range strings.Split(text, "\n") {
					f := strings.Fields(line)
					if len(f) >= 2 && f[0] == "nameserver" {
						address, _, _ := strings.Cut(f[1], "%")
						if net.ParseIP(address) == nil {
							err = &Error{Code: "SSH_RUNTIME_INVALID", Stage: probe.stage}
							runtime.DNS = nil
							break
						}
						runtime.DNS = append(runtime.DNS, f[1])
					}
					if len(f) >= 2 && (f[0] == "search" || f[0] == "domain") && runtime.Network != nil {
						runtime.Network.Resolver.Search = append(runtime.Network.Resolver.Search, f[1:]...)
					}
				}
			}
		}
		if err != nil {
			runtime.Observations[probe.stage] = unavailable(err, commandTool(probe.command))
		} else {
			runtime.Observations[probe.stage] = inventory.Observation{State: "observed"}
			add(probe.stage, probe.stage)
		}
	}
	collectRoutes(ctx, remote, runtime, add)
}

func collectRoutes(ctx context.Context, remote runner, runtime *inventory.RuntimeObservation, add func(string, string) string) {
	collected := []inventory.Route{}
	var firstErr error
	for _, probe := range []struct {
		stage   string
		command string
		family  int
	}{{"routes", "ip -j -4 route show table all", 4}, {"routes-v6", "ip -j -6 route show table all", 6}} {
		text, err := remote.Run(ctx, probe.stage, probe.command)
		if err == nil {
			var routesForFamily []inventory.Route
			routesForFamily, err = routes(text, runtime.Interfaces, probe.family)
			if err == nil {
				collected = append(collected, routesForFamily...)
			}
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		runtime.Routes = nil
		runtime.Observations["routes"] = unavailable(firstErr, "ip")
		return
	}
	runtime.Routes = collected
	runtime.Observations["routes"] = observed("ip")
	add("routes", "IPv4 and IPv6 routes")
}

// Count the typed optional facts, including nested card measurements. Successful
// commands alone do not make missing firmware/PSID/configuration observations pass.
func optionalCounts(runtime *inventory.RuntimeObservation) (observed, unavailableCount int) {
	count := func(observation inventory.Observation) {
		if observation.ReasonCode == "NOT_APPLICABLE" {
			return
		}
		if observation.State == "observed" {
			observed++
		} else {
			unavailableCount++
		}
	}
	for _, observation := range runtime.Observations {
		count(observation)
	}
	if runtime.Hardware != nil {
		count(runtime.Hardware.RShim)
		for _, card := range runtime.Hardware.Cards {
			count(card.Firmware)
			count(card.PSID)
			count(card.Privilege)
			for _, observation := range card.NVConfig {
				count(observation)
			}
		}
	}
	return observed, unavailableCount
}
