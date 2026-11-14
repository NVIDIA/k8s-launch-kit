// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// hostHBNChecks evaluates host-side facts for each inventory-selected DPU. It
// only follows the exact correlated host adapter and never changes the input.
func hostHBNChecks(inv *inventory.Inventory) []inventory.Check {
	if inv == nil || len(inv.DPUs) == 0 {
		return []inventory.Check{hostCheck("host-inventory", "selected DPUs", "unknown", "HOST_DPU_SCOPE_UNAVAILABLE", "No selected DPU inventory is available to evaluate host prerequisites.")}
	}
	checks := make([]inventory.Check, 0, len(inv.DPUs)*5)
	for _, dpu := range inv.DPUs {
		subject := dpu.SerialNumber
		if subject == "" {
			subject = "unidentified DPU"
		}
		association, host, adapter, associationState, associationReason, associationMessage := resolveHostHBNAssociation(inv, dpu)
		if host != nil {
			subject += " @ " + hostDisplayName(*host)
		}
		checks = append(checks, hostCheck("host-association", subject, associationState, associationReason, associationMessage))

		if association == nil || host == nil || adapter == nil {
			checks = append(checks,
				hostCheck("host-pf", subject, "unknown", "HOST_PF_ASSOCIATION_UNAVAILABLE", "Selected host PCI functions cannot be evaluated without one exact DPU-to-host-adapter association."),
				hostCheck("host-iommu-group", subject, "unknown", "HOST_IOMMU_ASSOCIATION_UNAVAILABLE", "Selected host IOMMU groups cannot be evaluated without one exact DPU-to-host-adapter association."),
				hostCheck("host-management-separation", subject, "unknown", "HOST_PATH_ASSOCIATION_UNAVAILABLE", "Management-path separation cannot be evaluated without one exact DPU-to-host-adapter association."),
				hostCheck("host-configuration", subject, "unknown", "HOST_CONFIGURATION_NOT_VERIFIED", "The selected host PF0 binding and host network configuration have not been verified; an address reservation alone does not establish host readiness."),
			)
			continue
		}

		pfChecks, iommuChecks, netdevs := selectedHostFunctionChecks(*host, *adapter, subject)
		checks = append(checks, pfChecks...)
		checks = append(checks, iommuChecks...)
		separation := managementPathCheck(*host, netdevs, subject)
		checks = append(checks, separation)
		checks = append(checks, hostCheck("host-configuration", subject, "unknown", "HOST_CONFIGURATION_NOT_VERIFIED", "The selected host PF0 binding and host network configuration have not been verified; an address reservation alone does not establish host readiness."))
	}
	return checks
}

func hostCheck(suffix, subject, state, reason, message string) inventory.Check {
	return inventory.Check{ID: "dpf.hbn." + suffix, Subject: subject, Vantage: "executor", RequiredPhase: "deploy", State: state, ReasonCode: reason, Message: message, EvidenceRefs: []string{}}
}

func resolveHostHBNAssociation(inv *inventory.Inventory, dpu inventory.Device) (*inventory.Association, *inventory.Device, *inventory.Adapter, string, string, string) {
	if dpu.ID == "" || dpu.SerialNumber == "" || dpu.ObservationState != "observed" {
		return nil, nil, nil, "unknown", "HOST_DPU_IDENTITY_UNAVAILABLE", "A current DPU identity is required to select its host adapter."
	}
	serialCount := 0
	for _, selected := range inv.DPUs {
		if selected.SerialNumber == dpu.SerialNumber {
			serialCount++
		}
	}
	if serialCount != 1 {
		return nil, nil, nil, "failed", "HOST_DPU_IDENTITY_AMBIGUOUS", "The selected DPU serial is not unique in the inventory."
	}
	var rows []inventory.Association
	for _, row := range inv.Associations {
		if row.DPUID == dpu.ID || row.SerialNumber == dpu.SerialNumber {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, nil, nil, "unknown", "HOST_ASSOCIATION_UNVERIFIED", "No exact host association is recorded for the selected DPU serial."
	}
	if len(rows) != 1 || rows[0].DPUID != dpu.ID || rows[0].SerialNumber != dpu.SerialNumber || rows[0].HostID == "" || rows[0].HostAdapterKey == "" {
		return nil, nil, nil, "failed", "HOST_ASSOCIATION_CONFLICT", "DPU ID, serial, host ID, and adapter key do not form one unique exact association."
	}
	row := rows[0]
	var selectedHost *inventory.Device
	for i := range inv.Hosts {
		if inv.Hosts[i].ID != row.HostID {
			continue
		}
		if selectedHost != nil {
			return nil, nil, nil, "failed", "HOST_ASSOCIATION_CONFLICT", "The associated host ID occurs more than once in the inventory."
		}
		selectedHost = &inv.Hosts[i]
	}
	if selectedHost == nil {
		return nil, nil, nil, "unknown", "HOST_ASSOCIATED_HOST_UNAVAILABLE", "The exact associated host is not present in the inventory."
	}
	if selectedHost.ObservationState != "observed" {
		return nil, nil, nil, "unknown", firstReason(selectedHost.ReasonCode, "HOST_ASSOCIATED_HOST_UNAVAILABLE"), "The exact associated host identity was not successfully observed."
	}
	var selectedAdapter *inventory.Adapter
	for i := range selectedHost.Adapters {
		if selectedHost.Adapters[i].Key != row.HostAdapterKey {
			continue
		}
		if selectedAdapter != nil {
			return nil, nil, nil, "failed", "HOST_ASSOCIATION_CONFLICT", "The associated adapter key occurs more than once on the selected host."
		}
		selectedAdapter = &selectedHost.Adapters[i]
	}
	if selectedAdapter == nil || selectedAdapter.SerialNumber != dpu.SerialNumber {
		return nil, nil, nil, "failed", "HOST_ASSOCIATION_CONFLICT", "The associated host adapter does not uniquely identify the selected DPU serial."
	}
	return &row, selectedHost, selectedAdapter, "passed", "HOST_ASSOCIATION_EXACT", "The selected DPU serial maps to one observed host and one exact adapter key."
}

func hostDisplayName(host inventory.Device) string {
	if host.Runtime != nil && strings.TrimSpace(host.Runtime.Hostname) != "" {
		return strings.TrimSpace(host.Runtime.Hostname)
	}
	for _, endpoint := range []string{host.SSHEndpoint, host.BMCEndpoint} {
		parsed, err := url.Parse(endpoint)
		if err == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
	}
	return "unknown-host"
}

func selectedHostFunctionChecks(host inventory.Device, adapter inventory.Adapter, subject string) ([]inventory.Check, []inventory.Check, []string) {
	if len(adapter.PCIFunctions) == 0 {
		return []inventory.Check{hostCheck("host-pf", subject, "unknown", "HOST_PF_INVENTORY_UNAVAILABLE", "The selected host adapter has no observed PCI functions.")},
			[]inventory.Check{hostCheck("host-iommu-group", subject, "unknown", "HOST_IOMMU_INVENTORY_UNAVAILABLE", "No selected host PCI functions are available for IOMMU group checks.")}, nil
	}
	pfChecks := make([]inventory.Check, 0, len(adapter.PCIFunctions))
	iommuChecks := make([]inventory.Check, 0, len(adapter.PCIFunctions))
	netdevs := []string{}
	seenNetdevs := map[string]bool{}
	for _, pf := range adapter.PCIFunctions {
		pfSubject := subject + " / " + pf.Address
		if pf.Address == "" {
			pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "unknown", "HOST_PF_ADDRESS_UNAVAILABLE", "The selected host PCI function has no address."))
			iommuChecks = append(iommuChecks, hostCheck("host-iommu-group", pfSubject, "unknown", "HOST_IOMMU_ADDRESS_UNAVAILABLE", "The selected host PCI function address is unavailable."))
			continue
		}
		stage := "host-pf:" + pf.Address + ":driver"
		driverObs := runtimeObservation(host, stage)
		netdevObs := runtimeObservation(host, "host-pf:"+pf.Address+":netdevs")
		switch {
		case driverObs.State != "observed":
			pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "unknown", firstReason(driverObs.ReasonCode, "HOST_PF_DRIVER_UNOBSERVED"), "The selected host PCI function driver was not successfully observed."))
		case pf.Driver != "mlx5_core":
			pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "failed", "HOST_PF_DRIVER_UNSUPPORTED", "The selected host PCI function is not bound to the expected mlx5_core driver."))
		case netdevObs.State != "observed":
			pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "unknown", firstReason(netdevObs.ReasonCode, "HOST_PF_NETDEV_UNOBSERVED"), "The selected host PCI function network-interface read did not complete."))
		case len(pf.Netdevs) == 0:
			pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "unknown", "HOST_PF_NETDEV_UNOBSERVED", "The selected host PCI function has no observed network interface."))
		default:
			pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "passed", "HOST_PF_DRIVER_READY", "The selected host PCI function is present, bound to mlx5_core, and has an observed network interface."))
		}

		iommuStage := "host-pf:" + pf.Address + ":iommu-group"
		iommuObs := runtimeObservation(host, iommuStage)
		switch {
		case iommuObs.State != "observed":
			iommuChecks = append(iommuChecks, hostCheck("host-iommu-group", pfSubject, "unknown", firstReason(iommuObs.ReasonCode, "HOST_IOMMU_GROUP_UNOBSERVED"), "The selected host PCI function IOMMU group was not successfully observed."))
		case pf.IOMMUGroup == nil:
			iommuChecks = append(iommuChecks, hostCheck("host-iommu-group", pfSubject, "unknown", "HOST_IOMMU_GROUP_INCONSISTENT", "IOMMU group observation is successful but the typed group value is absent."))
		case *pf.IOMMUGroup < 0:
			iommuChecks = append(iommuChecks, hostCheck("host-iommu-group", pfSubject, "failed", "HOST_IOMMU_GROUP_INVALID", "The observed IOMMU group number is invalid."))
		case len(iommuObs.Values) != 1 || iommuObs.Values[0] != fmt.Sprintf("group=%d", *pf.IOMMUGroup):
			iommuChecks = append(iommuChecks, hostCheck("host-iommu-group", pfSubject, "unknown", "HOST_IOMMU_GROUP_INCONSISTENT", "The typed IOMMU group value contradicts its source observation."))
		default:
			iommuChecks = append(iommuChecks, hostCheck("host-iommu-group", pfSubject, "passed", "HOST_IOMMU_GROUP_OBSERVED", "The selected host PCI function has an observed IOMMU group."))
		}
		for _, netdev := range pf.Netdevs {
			if netdev == "" {
				continue
			}
			if seenNetdevs[netdev] {
				pfChecks = append(pfChecks, hostCheck("host-pf", pfSubject, "failed", "HOST_PF_NETDEV_AMBIGUOUS", "A selected network interface is attributed to multiple host PCI functions."))
				continue
			}
			seenNetdevs[netdev] = true
			netdevs = append(netdevs, netdev)
		}
	}
	return pfChecks, iommuChecks, netdevs
}

func runtimeObservation(host inventory.Device, key string) inventory.Observation {
	if host.Runtime == nil || host.Runtime.Observations == nil {
		return inventory.Observation{}
	}
	return host.Runtime.Observations[key]
}

func managementPathCheck(host inventory.Device, selectedNetdevs []string, subject string) inventory.Check {
	if host.Runtime == nil {
		return hostCheck("host-management-separation", subject, "unknown", "HOST_RUNTIME_UNAVAILABLE", "Host OS interfaces, network links, and routes are unavailable for management-path separation checks.")
	}
	for _, key := range []string{"interfaces", "network-links", "network-link-details", "routes"} {
		observation := runtimeObservation(host, key)
		if observation.State != "observed" {
			fallback := "HOST_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_")) + "_UNOBSERVED"
			return hostCheck("host-management-separation", subject, "unknown", firstReason(observation.ReasonCode, fallback), "Complete host interface, master-link, and IPv4/IPv6 route observations are required to verify management-path separation.")
		}
	}
	if host.Runtime.Interfaces == nil || host.Runtime.Routes == nil || host.Runtime.Network == nil || host.Runtime.Network.Links == nil {
		return hostCheck("host-management-separation", subject, "unknown", "HOST_NETWORK_FACTS_INCOMPLETE", "A required host interface, network-link, or combined route fact is absent from the typed inventory.")
	}
	if len(selectedNetdevs) == 0 {
		return hostCheck("host-management-separation", subject, "unknown", "HOST_PF_NETDEV_UNAVAILABLE", "No selected host PCI function has a network interface for management-path analysis.")
	}
	interfacesByName := map[string]inventory.Interface{}
	indexes := map[int]string{}
	for _, iface := range host.Runtime.Interfaces {
		if iface.Name == "" || iface.Index < 1 {
			return hostCheck("host-management-separation", subject, "unknown", "HOST_INTERFACE_FACT_INVALID", "Host interface names and indexes must be valid before route ownership can be checked.")
		}
		if _, found := interfacesByName[iface.Name]; found {
			return hostCheck("host-management-separation", subject, "unknown", "HOST_INTERFACE_FACT_AMBIGUOUS", "A host interface name occurs more than once in the typed inventory.")
		}
		if previous, found := indexes[iface.Index]; found && previous != iface.Name {
			return hostCheck("host-management-separation", subject, "unknown", "HOST_INTERFACE_INDEX_AMBIGUOUS", "A host interface index is assigned to multiple names in the typed inventory.")
		}
		interfacesByName[iface.Name] = iface
		indexes[iface.Index] = iface.Name
	}
	linksByName := map[string]inventory.NetworkLink{}
	for _, link := range host.Runtime.Network.Links {
		if link.Name == "" {
			return hostCheck("host-management-separation", subject, "unknown", "HOST_LINK_FACT_INVALID", "The host link graph contains an empty interface name.")
		}
		if _, found := linksByName[link.Name]; found {
			return hostCheck("host-management-separation", subject, "unknown", "HOST_LINK_FACT_AMBIGUOUS", "A host link name occurs more than once in the master graph.")
		}
		linksByName[link.Name] = link
	}
	pathNames := map[string]bool{}
	for _, netdev := range selectedNetdevs {
		current := netdev
		seenPath := map[string]bool{}
		for depth := 0; depth <= len(linksByName); depth++ {
			if seenPath[current] {
				return hostCheck("host-management-separation", subject, "unknown", "HOST_LINK_MASTER_CYCLE", "The selected PF master graph contains a cycle or ambiguous ancestry.")
			}
			seenPath[current] = true
			pathNames[current] = true
			link, found := linksByName[current]
			if !found {
				return hostCheck("host-management-separation", subject, "unknown", "HOST_LINK_MASTER_UNRESOLVED", "The selected host PF or one of its bridge/bond ancestors is absent from the complete link graph.")
			}
			iface, found := interfacesByName[current]
			if !found {
				return hostCheck("host-management-separation", subject, "unknown", "HOST_INTERFACE_UNRESOLVED", "The selected host PF or one of its bridge/bond ancestors is absent from the address and route snapshot.")
			}
			for _, address := range iface.Addresses {
				ip, _, err := net.ParseCIDR(address)
				if err != nil {
					return hostCheck("host-management-separation", subject, "unknown", "HOST_INTERFACE_ADDRESS_INVALID", "A selected host interface address cannot be parsed safely.")
				}
				if ip.IsGlobalUnicast() && !ip.IsLoopback() {
					return hostCheck("host-management-separation", subject, "failed", "HOST_ADDRESS_ON_SELECTED_PATH", fmt.Sprintf("Selected host PF %s or its bridge/bond ancestor already carries a global-unicast address; management separation is unverified.", current))
				}
			}
			if link.Master == "" {
				break
			}
			if depth == len(linksByName) {
				return hostCheck("host-management-separation", subject, "unknown", "HOST_LINK_MASTER_CYCLE", "The selected PF master graph exceeds its bounded ancestry and cannot be resolved safely.")
			}
			current = link.Master
		}
	}
	selectedIndexes := map[int]bool{}
	for name := range pathNames {
		selectedIndexes[interfacesByName[name].Index] = true
	}
	for _, route := range host.Runtime.Routes {
		_, prefix, err := net.ParseCIDR(route.Destination)
		if err != nil {
			return hostCheck("host-management-separation", subject, "unknown", "HOST_ROUTE_FACT_INVALID", "A host route destination cannot be parsed safely.")
		}
		bits, _ := prefix.Mask.Size()
		if bits == 0 && selectedIndexes[route.InterfaceIndex] {
			family := "IPV6"
			if prefix.IP.To4() != nil {
				family = "IPV4"
			}
			return hostCheck("host-management-separation", subject, "failed", "HOST_"+family+"_DEFAULT_ROUTE_ON_SELECTED_PATH", "The selected host PF or its bridge/bond ancestor owns a default route; management separation is unverified.")
		}
	}
	return hostCheck("host-management-separation", subject, "passed", "HOST_MANAGEMENT_PATH_SEPARATE", "The complete host snapshot shows no global-unicast address or IPv4/IPv6 default route on selected PFs or their bridge/bond ancestors; this does not establish host reachability.")
}

func firstReason(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
