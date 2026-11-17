// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"encoding/json"
	"fmt"
	"reflect"
)

const maxRouteNextHops = 256

// Kernel nexthop IDs are mutable references. Both dumps surround the route
// observations; only the referenced object and its flat group members must be
// identical. Unrelated nexthop churn does not invalidate the route witness.
// These are collection inputs, never persisted IDs or authority to adopt routes.
type nextHopObservation struct {
	before, after map[uint32]jsonObject
}

func parseNextHopObservation(in NVUEInputs, out *Snapshot) *nextHopObservation {
	if !in.NextHopCollectionRequested && in.OperationalNextHopsBefore == nil && in.OperationalNextHopsAfter == nil {
		return nil // Legacy snapshots do not acquire positive measurements.
	}
	return &nextHopObservation{
		before: parseNextHopTable(in.OperationalNextHopsBefore, "operationalNextHopsBefore", &out.Issues),
		after:  parseNextHopTable(in.OperationalNextHopsAfter, "operationalNextHopsAfter", &out.Issues),
	}
}

func parseNextHopTable(raw []byte, input string, issues *[]Issue) map[uint32]jsonObject {
	rows, ok := parseL3Array(raw, input, issues)
	if !ok {
		return nil
	}
	result := make(map[uint32]jsonObject, len(rows))
	for i, row := range rows {
		path := fmt.Sprintf("$[%d]", i)
		obj, ok := l3Object(row, input, path, issues)
		if !ok {
			return nil
		}
		id, ok := nextHopID(obj["id"])
		if !ok || result[id] != nil {
			addIssue(issues, input, path+".id", "invalid_nexthop_id", "Nexthop IDs must be unique positive 32-bit integers.")
			return nil
		}
		result[id] = obj
	}
	return result
}

func nextHopID(raw json.RawMessage) (uint32, bool) {
	var id uint32
	if json.Unmarshal(raw, &id) != nil || id == 0 {
		return 0, false
	}
	return id, true
}

func observedRouteNextHops(route jsonObject, observation *nextHopObservation) []RouteNextHop {
	rawID, referenced := route["nhid"]
	if !referenced {
		return routeNextHops(route)
	}
	// Never fall back to conflicting inline fields or unmodelled forwarding
	// attributes (encapsulation, blackholes, resilient groups, etc.).
	if !onlyNextHopFields(route, "dst", "nhid", "table", "protocol", "type", "metric", "flags") || !nextHopFlagsValid(route) {
		return nil
	}
	id, ok := nextHopID(rawID)
	if !ok || observation == nil {
		return nil
	}
	object := observation.stable(id)
	if object == nil || !nextHopFlagsValid(object) {
		return nil
	}
	if _, grouped := object["group"]; !grouped {
		return nextHopLeaf(object)
	}
	if !onlyNextHopFields(object, "id", "group", "protocol", "flags") {
		return nil
	}
	var members []jsonObject
	if json.Unmarshal(object["group"], &members) != nil || len(members) == 0 || len(members) > maxRouteNextHops {
		return nil
	}
	rows := make([]map[string]json.RawMessage, 0, len(members))
	seen := map[uint32]bool{id: true}
	for _, member := range members {
		child, valid := nextHopID(member["id"])
		if !valid || seen[child] || !onlyNextHopFields(member, "id", "weight") {
			return nil
		}
		seen[child] = true
		// iproute2 legacy group weights are 1..256; omitted means 1.
		// Expanded-weight and resilient group formats are not qualified here.
		if raw, present := member["weight"]; present {
			var weight uint32
			if json.Unmarshal(raw, &weight) != nil || weight == 0 || weight > 256 {
				return nil
			}
		}
		leaf := observation.stable(child)
		if !nextHopLeafFieldsValid(leaf) {
			return nil // Includes nested groups, cycles and incomplete paths.
		}
		rows = append(rows, leaf)
	}
	// The existing parser validates every gateway/interface and duplicate path,
	// and sorts the ownership projection. Weights and all other object fields
	// were compared above: no low-weight path can disappear from this witness.
	return routeNextHopRows(rows)
}

func (o *nextHopObservation) stable(id uint32) jsonObject {
	before, after := o.before[id], o.after[id]
	if before == nil || after == nil {
		return nil
	}
	// Compare complete JSON values, independent of whitespace and object-key
	// order. Group membership/order, flags, weights and unknown fields count.
	var a, b any
	x, _ := json.Marshal(before)
	y, _ := json.Marshal(after)
	if json.Unmarshal(x, &a) != nil || json.Unmarshal(y, &b) != nil || !reflect.DeepEqual(a, b) {
		return nil
	}
	return before
}

func nextHopLeaf(object jsonObject) []RouteNextHop {
	if !nextHopLeafFieldsValid(object) {
		return nil
	}
	return routeNextHops(object)
}

func nextHopLeafFieldsValid(object jsonObject) bool {
	return object != nil && onlyNextHopFields(object, "id", "dev", "gateway", "via", "scope", "protocol", "flags") && nextHopFlagsValid(object)
}

func nextHopFlagsValid(object jsonObject) bool {
	raw, exists := object["flags"]
	if !exists {
		return true
	}
	var flags []string
	if json.Unmarshal(raw, &flags) != nil || flags == nil {
		return false
	}
	for _, flag := range flags {
		if flag != "onlink" {
			return false
		}
	}
	return true
}

func onlyNextHopFields(object jsonObject, names ...string) bool {
	for key := range object {
		found := false
		for _, name := range names {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
