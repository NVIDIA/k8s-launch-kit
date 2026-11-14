// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import "strings"

// b4240Service recognizes the two observed BF4 service shapes without deriving
// missing ComputerSystem properties from names, descriptions or URI spellings.
func b4240Service(root, system Document) bool {
	if !strings.EqualFold(Text(root, "Vendor"), "NVIDIA") {
		return false
	}
	product := Text(root, "Product")
	if product != "B4240" && product != "BlueField-4" {
		return false
	}
	for _, key := range []string{"Manufacturer", "Model", "SerialNumber", "UUID"} {
		if value, present := system[key]; present && value != nil {
			if _, valid := value.(string); !valid {
				return false
			}
		}
	}
	maker, model := Text(system, "Manufacturer"), Text(system, "Model")
	return (maker == "" || strings.EqualFold(maker, "NVIDIA")) && (model == "" || model == "BlueField-4")
}

func (c *Client) linkedResource(doc Document, key, expected string) bool {
	links, _ := Object(doc, "Links")[key].([]any)
	for _, link := range links {
		uri, err := c.guard(Link(Document{"resource": link}, "resource"))
		if err == nil && uri == expected {
			return true
		}
	}
	return false
}

// b4240Cards requires an exact board identity and explicit graph relationships.
// New firmware exposes the serial on the physical Card. Older firmware exposes
// it on the BMC Component, reciprocally contained by a serial-less physical Card.
// Both shapes retain the same serial-based host association and reject ambiguity.
func (c *Client) b4240Cards(system Resource, chassis []Resource) ([]Resource, []Resource) {
	cards, witnesses := []Resource{}, []Resource{}
	for _, ch := range chassis {
		d := ch.Document
		if Text(d, "Model") != "B4240" || !strings.EqualFold(Text(d, "Manufacturer"), "NVIDIA") || !ValidSerial(Text(d, "SerialNumber")) || !c.linkedToSystem(d, system.URI) {
			continue
		}
		serial, systemSerial := strings.TrimSpace(Text(d, "SerialNumber")), strings.TrimSpace(Text(system.Document, "SerialNumber"))
		if systemSerial != "" && (!ValidSerial(systemSerial) || serial != systemSerial) {
			continue
		}
		switch Text(d, "ChassisType") {
		case "Card":
			// The observed physical-card shape also exposes the serial on the SoC.
			if !ValidSerial(systemSerial) {
				continue
			}
			cards = append(cards, ch)
		case "Component":
			if !c.linkedResource(system.Document, "Chassis", ch.URI) {
				continue
			}
			parentURI, err := c.guard(Link(Object(d, "Links"), "ContainedBy"))
			if err != nil {
				continue
			}
			for _, parent := range chassis {
				if parent.URI != parentURI || Text(parent.Document, "ChassisType") != "Card" || !c.linkedToSystem(parent.Document, system.URI) || !c.linkedResource(parent.Document, "Contains", ch.URI) {
					continue
				}
				if !b4240Parent(parent.Document) {
					continue
				}
				parentSerial := strings.TrimSpace(Text(parent.Document, "SerialNumber"))
				if parentSerial != "" && (!ValidSerial(parentSerial) || parentSerial != serial) {
					continue
				}
				cards = append(cards, ch)
				witnesses = append(witnesses, parent)
			}
		}
	}
	return cards, witnesses
}

// A serial-less legacy Card is an unknown witness, not permission to ignore
// contradictory or malformed identity metadata when the service does expose it.
func b4240Parent(parent Document) bool {
	for _, key := range []string{"Manufacturer", "Model", "SerialNumber"} {
		if value, present := parent[key]; present && value != nil {
			if _, valid := value.(string); !valid {
				return false
			}
		}
	}
	maker, model := Text(parent, "Manufacturer"), Text(parent, "Model")
	return (maker == "" || strings.EqualFold(maker, "NVIDIA")) && (model == "" || model == "B4240")
}

// Identity-bearing fields are reread, including the graph used for legacy BF4.
func cardIdentity(doc Document) Document {
	out := Document{}
	for _, key := range []string{"Model", "Manufacturer", "SerialNumber", "ChassisType", "Links"} {
		out[key] = doc[key]
	}
	return out
}
