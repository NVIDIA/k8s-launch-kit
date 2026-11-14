// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

type hostCard struct {
	resource Resource
	owned    bool
	pci      *Resource
}

// collectHostAdapters joins only advertised resources of the selected System.
// Chassis membership alone is insufficient when a chassis is shared or its
// ownership is unknown; HPE's System PCI DeviceResourceId provides that join.
func collectHostAdapters(ctx context.Context, c *Client, system Resource, hostID string, hpe bool) ([]inventory.Adapter, error) {
	cards := map[string]*hostCard{}
	collections := map[string][]Resource{}
	observed := map[string]Document{system.URI: hostIdentityFields(system.Document)}
	readCollection := func(uri string) ([]Resource, error) {
		members, err := c.Collection(ctx, uri)
		if err == nil {
			collections[uri] = members
			for _, member := range members {
				observed[member.URI] = hostIdentityFields(member.Document)
			}
		}
		return members, err
	}
	putCard := func(resource Resource, owned bool) error {
		key := strings.TrimSuffix(resource.URI, "/")
		if existing := cards[key]; existing != nil {
			if !reflect.DeepEqual(hostIdentityFields(existing.resource.Document), hostIdentityFields(resource.Document)) {
				return &Error{Code: "INVENTORY_CHANGED", URI: resource.URI}
			}
			existing.owned = existing.owned || owned
		} else {
			cards[key] = &hostCard{resource: resource, owned: owned}
		}
		observed[resource.URI] = hostIdentityFields(resource.Document)
		return nil
	}
	if uri := Link(system.Document, "NetworkInterfaces"); uri != "" {
		interfaces, err := readCollection(uri)
		if err != nil {
			return nil, err
		}
		for _, iface := range interfaces {
			uri := Link(Object(iface.Document, "Links"), "NetworkAdapter")
			if uri == "" {
				return nil, &Error{Code: "ADAPTER_LINK_MISSING", URI: iface.URI}
			}
			uri, err = c.guard(uri)
			if err != nil {
				return nil, err
			}
			card, err := c.Get(ctx, uri)
			if err != nil {
				return nil, err
			}
			if err := putCard(Resource{URI: uri, Document: card}, true); err != nil {
				return nil, err
			}
		}
	}
	if hpe {
		chassisLinks, err := c.resourceLinks(Object(system.Document, "Links"), "Chassis")
		if err != nil {
			return nil, err
		}
		for _, uri := range chassisLinks {
			chassis, err := c.Get(ctx, uri)
			if err != nil {
				return nil, err
			}
			observed[uri] = hostIdentityFields(chassis)
			owners, err := c.resourceLinks(Object(chassis, "Links"), "ComputerSystems")
			if err != nil {
				return nil, err
			}
			exclusive := len(owners) == 1 && strings.TrimSuffix(owners[0], "/") == strings.TrimSuffix(system.URI, "/")
			networkURI := Link(chassis, "NetworkAdapters")
			if networkURI == "" {
				continue
			}
			adapters, err := readCollection(networkURI)
			if err != nil {
				return nil, err
			}
			for _, resource := range adapters {
				if err := putCard(resource, exclusive); err != nil {
					return nil, err
				}
			}
		}
		pciLinks, err := c.resourceLinks(Object(Object(Object(system.Document, "Oem"), "Hpe"), "Links"), "PCIDevices")
		if err != nil {
			return nil, err
		}
		for _, uri := range pciLinks {
			devices, err := readCollection(uri)
			if err != nil {
				return nil, err
			}
			for _, pci := range devices {
				class, hasClass := integer(pci.Document, "ClassCode", 255)
				if (!hasClass || class != 2) && Text(pci.Document, "DeviceType") != "NIC" {
					continue
				}
				id := Text(pci.Document, "DeviceResourceId")
				var matches []*hostCard
				if id != "" {
					for _, card := range cards {
						if Text(card.resource.Document, "Id") == id && card.pci == nil {
							matches = append(matches, card)
						} else if Text(card.resource.Document, "Id") == id && card.pci != nil {
							return nil, &Error{Code: "ADAPTER_PCI_AMBIGUOUS", URI: pci.URI}
						}
					}
				}
				if len(matches) > 1 {
					return nil, &Error{Code: "ADAPTER_PCI_AMBIGUOUS", URI: pci.URI}
				}
				if len(matches) == 1 {
					matches[0].owned = true
					matches[0].pci = &pci
				} else {
					// A PCI record is still useful when no NetworkAdapter exists.
					cards[strings.TrimSuffix(pci.URI, "/")] = &hostCard{resource: pci, owned: true, pci: &pci}
				}
			}
		}
	}
	if len(collections) == 0 {
		return nil, &Error{Code: "HOST_INVENTORY_UNAVAILABLE", URI: system.URI}
	}
	var adapters []inventory.Adapter
	serials := map[string]bool{}
	for _, key := range sortedKeys(cards) {
		card := cards[key]
		if !card.owned {
			continue
		}
		raw := Text(card.resource.Document, "SerialNumber")
		serial := ""
		state, reason := "unknown", "CARD_SERIAL_MISSING"
		// Observation location survives replacement and newly available serial
		// data. SerialNumber carries the separate physical identity.
		adapterKey := inventory.Digest("dpf-adapter-observation-v1", []string{hostID, key})
		if ValidSerial(raw) {
			serial = strings.TrimSpace(raw)
			if serials[serial] {
				return nil, &Error{Code: "CARD_IDENTITY_AMBIGUOUS", URI: card.resource.URI}
			}
			serials[serial] = true
			state, reason = "known", ""
		}
		model := Text(card.resource.Document, "Model")
		if model == "" {
			model = Text(card.resource.Document, "Name")
		}
		a := inventory.Adapter{Key: adapterKey, SerialNumber: serial, SerialNumberRaw: raw, Model: model, IdentityState: state, ReasonCode: reason, Selection: "not-selected", Slot: Text(Object(Object(card.resource.Document, "Location"), "PartLocation"), "ServiceLabel")}
		if card.pci != nil {
			pci := card.pci.Document
			a.PCIAddress = hpePCIAddress(pci)
			a.VendorID = hexID(pci, "VendorID")
			a.DeviceID = hexID(pci, "DeviceID")
			if slot := Text(pci, "LocationString"); slot != "" {
				a.Slot = slot
			}
		}
		adapters = append(adapters, a)
	}
	// Recheck both collection membership and the identity/ownership fields used
	// in joins. Telemetry counters are intentionally outside this snapshot.
	for _, uri := range sortedKeys(collections) {
		again, err := c.Collection(ctx, uri)
		if err != nil {
			return adapters, err
		}
		if !sameMembers(collections[uri], again) {
			return adapters, &Error{Code: "INVENTORY_CHANGED", URI: uri}
		}
	}
	for _, uri := range sortedKeys(observed) {
		again, err := c.Get(ctx, uri)
		if err != nil {
			return adapters, err
		}
		if !reflect.DeepEqual(observed[uri], hostIdentityFields(again)) {
			return adapters, &Error{Code: "IDENTITY_CHANGED", URI: uri}
		}
	}
	return adapters, nil
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// resourceLinks supports the array used by HPE OEM PCI links and standard links.
func (c *Client) resourceLinks(d Document, key string) ([]string, error) {
	value, exists := d[key]
	if !exists || value == nil {
		return nil, nil
	}
	values, array := value.([]any)
	if !array {
		values = []any{value}
	}
	var links []string
	seen := map[string]bool{}
	for _, v := range values {
		uri, err := c.guard(Link(Document{"v": v}, "v"))
		if err != nil {
			return nil, err
		}
		canonical := strings.TrimSuffix(uri, "/")
		if seen[canonical] {
			return nil, &Error{Code: "DUPLICATE_MEMBER", URI: canonical}
		}
		seen[canonical] = true
		links = append(links, uri)
	}
	return links, nil
}

func hostIdentityFields(d Document) Document {
	result := Document{}
	for _, key := range []string{"Id", "SerialNumber", "Model", "Name", "Links", "NetworkInterfaces", "NetworkAdapters", "Location", "DeviceResourceId", "ClassCode", "DeviceType", "SegmentNumber", "BusNumber", "DeviceNumber", "FunctionNumber", "VendorID", "DeviceID", "LocationString"} {
		if value, ok := d[key]; ok {
			result[key] = value
		}
	}
	if pci := Object(Object(Object(d, "Oem"), "Hpe"), "Links")["PCIDevices"]; pci != nil {
		result["HpePCIDevices"] = pci
	}
	return result
}
func integer(d Document, key string, max int) (int, bool) {
	n, ok := d[key].(float64)
	return int(n), ok && n >= 0 && n <= float64(max) && n == float64(int(n))
}
func hpePCIAddress(d Document) string {
	values := make([]int, 4)
	for i, field := range []struct {
		key string
		max int
	}{{"SegmentNumber", 65535}, {"BusNumber", 255}, {"DeviceNumber", 31}, {"FunctionNumber", 7}} {
		value, ok := integer(d, field.key, field.max)
		if !ok {
			return ""
		}
		values[i] = value
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", values[0], values[1], values[2], values[3])
}
func hexID(d Document, field string) string {
	if value, ok := integer(d, field, 65535); ok {
		return fmt.Sprintf("%04x", value)
	}
	return ""
}
