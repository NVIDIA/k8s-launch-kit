// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const (
	maxNetworkAdapters        = 16
	maxNetworkDeviceFunctions = 32
)

type networkFunctionMAC struct {
	adapterURI    string
	collectionURI string
	functionURI   string
	identity      Document
	isEthernet    bool
	mac           string
}

// networkFunctionMACs reads permanent Ethernet MACs from the NetworkAdapters
// graph advertised by an already identity-selected physical card. It does not
// infer port assignment or map function IDs to ARM interface names.
func networkFunctionMACs(ctx context.Context, reader Reader, card Resource) ([]string, inventory.Observation) {
	if reader == nil || card.URI == "" {
		return nil, networkFunctionUnavailable(&Error{Code: "INVALID_RESPONSE"})
	}
	initial, state := readNetworkFunctionMACGraph(ctx, reader, card)
	if state.State != "observed" {
		state.Tool = "redfish"
		return nil, state
	}
	currentCard, err := reader.Get(ctx, card.URI)
	if err != nil {
		return nil, networkFunctionUnavailable(err)
	}
	initialCollection := Link(card.Document, "NetworkAdapters")
	currentCollection := Link(currentCard, "NetworkAdapters")
	if initialCollection == "" || currentCollection == "" || !sameRedfishLink(initialCollection, currentCollection) {
		return nil, networkFunctionUnavailable(&Error{Code: "IDENTITY_CHANGED"})
	}
	current, currentState := readNetworkFunctionMACGraph(ctx, reader, Resource{URI: card.URI, Document: currentCard})
	if currentState.State != "observed" {
		currentState.Tool = "redfish"
		return nil, currentState
	}
	if !sameNetworkFunctionMACGraph(initial, current) {
		return nil, networkFunctionUnavailable(&Error{Code: "IDENTITY_CHANGED"})
	}
	macs := make([]string, 0, len(initial))
	for _, function := range initial {
		if function.isEthernet {
			macs = append(macs, function.mac)
		}
	}
	slices.Sort(macs)
	return macs, inventory.Observation{State: "observed", Values: macs, Tool: "redfish"}
}

func readNetworkFunctionMACGraph(ctx context.Context, reader Reader, card Resource) ([]networkFunctionMAC, inventory.Observation) {
	collectionURI, advertised, err := advertisedLink(card.Document, "NetworkAdapters")
	if err != nil {
		return nil, unavailable(err)
	}
	if !advertised {
		return nil, missing("LINK_NOT_EXPOSED", "The identity-selected card does not advertise NetworkAdapters; permanent Ethernet MACs are unknown.")
	}
	adapters, err := reader.Collection(ctx, collectionURI)
	if err != nil {
		return nil, unavailable(err)
	}
	if len(adapters) > maxNetworkAdapters {
		return nil, unavailable(&Error{Code: "RESOURCE_LIMIT"})
	}
	if len(adapters) == 0 {
		return nil, missing("NETWORK_ADAPTERS_EMPTY", "The identity-selected card exposes no NetworkAdapters members.")
	}

	functions := make([]networkFunctionMAC, 0)
	functionCount := 0
	seenAdapters := map[string]bool{}
	seenMACs := map[string]bool{}
	missingFunctionsLink := false
	for _, adapter := range adapters {
		adapterURI := canonicalResourceURI(adapter.URI)
		if adapterURI == "" || seenAdapters[adapterURI] {
			return nil, unavailable(&Error{Code: "DUPLICATE_MEMBER"})
		}
		seenAdapters[adapterURI] = true
		functionsURI, hasFunctions, err := advertisedLink(adapter.Document, "NetworkDeviceFunctions")
		if err != nil {
			return nil, unavailable(err)
		}
		if !hasFunctions {
			missingFunctionsLink = true
			continue
		}
		functions = append(functions, networkFunctionMAC{adapterURI: adapterURI, collectionURI: canonicalResourceURI(functionsURI)})
		members, err := reader.Collection(ctx, functionsURI)
		if err != nil {
			return nil, unavailable(err)
		}
		functionCount += len(members)
		if functionCount > maxNetworkDeviceFunctions {
			return nil, unavailable(&Error{Code: "RESOURCE_LIMIT"})
		}
		seenFunctions := map[string]bool{}
		for _, function := range members {
			functionURI := canonicalResourceURI(function.URI)
			if functionURI == "" || seenFunctions[functionURI] {
				return nil, unavailable(&Error{Code: "DUPLICATE_MEMBER"})
			}
			seenFunctions[functionURI] = true
			functionType, ok := function.Document["NetDevFuncType"].(string)
			if !ok || strings.TrimSpace(functionType) == "" {
				return nil, unavailable(&Error{Code: "INVALID_RESPONSE"})
			}
			item := networkFunctionMAC{adapterURI: adapterURI, functionURI: functionURI, identity: networkFunctionIdentity(function.Document), isEthernet: functionType == "Ethernet"}
			if !item.isEthernet {
				functions = append(functions, item)
				continue
			}
			ethernet, hasEthernet := function.Document["Ethernet"]
			if !hasEthernet || ethernet == nil {
				return nil, missing("MAC_NOT_EXPOSED", "An Ethernet NetworkDeviceFunction does not expose a permanent MAC address.")
			}
			ethernetDoc, ok := ethernet.(map[string]any)
			if !ok {
				if typed, typedOK := ethernet.(Document); typedOK {
					ethernetDoc, ok = map[string]any(typed), true
				}
			}
			if !ok {
				return nil, unavailable(&Error{Code: "INVALID_RESPONSE"})
			}
			rawMAC, exists := ethernetDoc["PermanentMACAddress"]
			if !exists || rawMAC == nil || rawMAC == "" {
				return nil, missing("MAC_NOT_EXPOSED", "An Ethernet NetworkDeviceFunction does not expose a permanent MAC address.")
			}
			macText, ok := rawMAC.(string)
			if !ok {
				return nil, unavailable(&Error{Code: "INVALID_RESPONSE"})
			}
			mac, ok := permanentUnicastMAC(macText)
			if !ok {
				return nil, unavailable(&Error{Code: "INVALID_RESPONSE"})
			}
			if seenMACs[mac] {
				return nil, unavailable(&Error{Code: "DUPLICATE_MEMBER"})
			}
			seenMACs[mac] = true
			item.mac = mac
			functions = append(functions, item)
		}
	}
	if missingFunctionsLink {
		return nil, missing("LINK_NOT_EXPOSED", "A NetworkAdapter does not advertise NetworkDeviceFunctions; permanent Ethernet MAC inventory is incomplete.")
	}
	macCount := 0
	for _, function := range functions {
		if function.isEthernet {
			macCount++
		}
	}
	if macCount == 0 {
		return nil, missing("MAC_NOT_EXPOSED", "The identity-selected card exposes no Ethernet permanent MAC addresses.")
	}
	return functions, inventory.Observation{State: "observed", Tool: "redfish"}
}

func advertisedLink(document Document, key string) (string, bool, error) {
	value, exists := document[key]
	if !exists || value == nil {
		return "", false, nil
	}
	link, ok := Object(document, key)["@odata.id"].(string)
	if !ok || strings.TrimSpace(link) == "" {
		return "", false, &Error{Code: "INVALID_RESPONSE"}
	}
	return link, true, nil
}

func permanentUnicastMAC(value string) (string, bool) {
	if len(value) != 17 {
		return "", false
	}
	for i, r := range value {
		if i%3 == 2 {
			if r != ':' {
				return "", false
			}
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return "", false
		}
	}
	parsed, err := net.ParseMAC(value)
	if err != nil || len(parsed) != 6 || parsed[0]&1 != 0 {
		return "", false
	}
	allZero := true
	for _, octet := range parsed {
		allZero = allZero && octet == 0
	}
	if allZero {
		return "", false
	}
	return strings.ToLower(value), true
}

func networkFunctionIdentity(document Document) Document {
	identity := Document{}
	for _, key := range []string{"Id", "NetDevFuncType", "Ethernet"} {
		if value, exists := document[key]; exists {
			identity[key] = value
		}
	}
	return identity
}

func sameNetworkFunctionMACGraph(a, b []networkFunctionMAC) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(item networkFunctionMAC) string {
		return item.adapterURI + "\x00" + item.functionURI
	}
	slices.SortFunc(a, func(left, right networkFunctionMAC) int { return strings.Compare(key(left), key(right)) })
	slices.SortFunc(b, func(left, right networkFunctionMAC) int { return strings.Compare(key(left), key(right)) })
	for i := range a {
		if a[i].adapterURI != b[i].adapterURI || a[i].collectionURI != b[i].collectionURI || a[i].functionURI != b[i].functionURI || a[i].isEthernet != b[i].isEthernet || a[i].mac != b[i].mac || !reflect.DeepEqual(a[i].identity, b[i].identity) {
			return false
		}
	}
	return true
}

func sameRedfishLink(a, b string) bool {
	canonical := func(value string) string {
		return canonicalResourceURI(value)
	}
	return canonical(a) == canonical(b)
}

func canonicalResourceURI(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return strings.TrimSuffix(value, "/")
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return path
}

func networkFunctionUnavailable(err error) inventory.Observation {
	state := unavailable(err)
	state.Tool = "redfish"
	return state
}
