// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package discovery orchestrates independent management and BMC observations.
package discovery

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
)

type ProbeRunner interface {
	Run(context.Context, config.Request, ManagementClient, *inventory.Inventory) ([]inventory.ProbeResult, inventory.Cleanup, error)
}
type Dependencies struct {
	BMC    redfish.Collector
	Probes ProbeRunner
}

func NewReport() inventory.Report {
	return inventory.Report{SchemaVersion: inventory.FormatVersion, Target: "dpf", Phase: "discover", Execution: "completed", Acceptance: "not-evaluated", Discovery: inventory.Discovery{Outcome: "invalid", Checks: []inventory.Check{}, Cleanup: inventory.Cleanup{State: "complete", Leftovers: []string{}}}, Diff: []inventory.Change{}, PendingInputs: []inventory.Problem{}, Artifacts: []inventory.Artifact{}, Errors: []inventory.Problem{}}
}
func Collect(ctx context.Context, r config.Request, runID string, deps Dependencies) inventory.Report {
	report := NewReport()
	report.Discovery.Mode = r.Mode
	inv := &inventory.Inventory{FormatVersion: inventory.FormatVersion, Collection: inventory.Collection{RunID: runID, StartedAt: time.Now().UTC(), Mode: r.Mode, Collector: "l8k-go-discovery-v1"}, RequestedScope: []inventory.ScopeEndpoint{}, DPUs: []inventory.Device{}, Hosts: []inventory.Device{}, Associations: []inventory.Association{}, Evidence: map[string]inventory.Evidence{}}
	report.Inventory = inv
	for _, t := range r.Targets {
		inv.RequestedScope = append(inv.RequestedScope, inventory.ScopeEndpoint{Role: t.Role, Endpoint: t.Endpoint, SystemURI: t.SystemURI})
	}
	slices.SortFunc(inv.RequestedScope, func(a, b inventory.ScopeEndpoint) int {
		return strings.Compare(a.Role+a.Endpoint+a.SystemURI, b.Role+b.Endpoint+b.SystemURI)
	})
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	problem := func(p inventory.Problem) { mu.Lock(); defer mu.Unlock(); report.Errors = append(report.Errors, p) }
	mg, accessErr := ManagementAccess(r)
	if accessErr != nil {
		report.Execution = "failed"
		report.Errors = append(report.Errors, inventory.Problem{Code: "INVALID_MANAGEMENT_ACCESS", Message: accessErr.Error(), ExitCode: 2})
		return report
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		inv.Management, err = mg.Observe(ctx)
		if err != nil {
			problem(inventory.Problem{Code: "MANAGEMENT_INSPECTION_FAILED", Message: err.Error(), InputRef: "/management", ExitCode: 3})
			return
		}
		mu.Lock()
		for id, e := range NewEvidence(time.Now().UTC()) {
			inv.Evidence[id] = e
		}
		mu.Unlock()
	}()
	sem := make(chan struct{}, r.MaxConcurrentBMCs)
	devices := make([]inventory.Device, len(r.Targets))
	for index, target := range r.Targets {
		wg.Add(1)
		go func(i int, t config.Target) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				devices[i] = inventory.Device{RecordKey: inventory.Digest("dpf-access-v1", []string{t.Role, t.Endpoint, t.SystemURI}), InputRef: t.InputRef, BMCEndpoint: t.Endpoint, ObservationState: "unavailable", ReasonCode: "CANCELLED"}
				return
			}
			d, e, err := deps.BMC.Collect(ctx, t, r.RequestTimeout)
			devices[i] = d
			mu.Lock()
			for key, value := range e {
				inv.Evidence[key] = value
			}
			mu.Unlock()
			if err != nil {
				problem(inventory.Problem{Code: redfish.Code(err), Message: "BMC observation failed; inspect record reason and field path", InputRef: t.InputRef, ExitCode: 3})
				if d.ReasonCode == "CLEANUP_INCOMPLETE" {
					mu.Lock()
					report.Discovery.Cleanup.State = "incomplete"
					report.Discovery.Cleanup.Leftovers = append(report.Discovery.Cleanup.Leftovers, t.Endpoint+": session outcome unknown")
					mu.Unlock()
				}
			}
		}(index, target)
	}
	wg.Wait()
	for i, d := range devices {
		if r.Targets[i].Role == "dpus" {
			inv.DPUs = append(inv.DPUs, d)
		} else {
			inv.Hosts = append(inv.Hosts, d)
		}
	}
	inv.Associations, report.Discovery.Checks = Correlate(inv.DPUs, inv.Hosts)
	selected := map[string]bool{}
	for _, a := range inv.Associations {
		selected[a.HostAdapterKey] = true
	}
	for hi := range inv.Hosts {
		for ai := range inv.Hosts[hi].Adapters {
			if selected[inv.Hosts[hi].Adapters[ai].Key] {
				inv.Hosts[hi].Adapters[ai].Selection = "selected"
			}
		}
	}
	addCheck := func(id, subject string, ok bool, reason string, refs []string) {
		state := "failed"
		if ok {
			state = "passed"
		}
		report.Discovery.Checks = append(report.Discovery.Checks, inventory.Check{ID: id, Subject: subject, Vantage: "executor", RequiredPhase: "discover", State: state, ReasonCode: reason, EvidenceRefs: refs})
	}
	if inv.Management != nil {
		cp := 0
		for _, n := range inv.Management.Nodes {
			if n.ControlPlane {
				cp++
			}
		}
		addCheck("dpf.management.control-plane-count", "management", cp == 3, "EXACTLY_THREE_CONTROL_PLANES", inv.Management.EvidenceRefs)
		if r.Mode == "management" && cp == 3 {
			if deps.Probes == nil {
				report.Errors = append(report.Errors, inventory.Problem{Code: "PROBE_ASSETS_UNAVAILABLE", Message: "management probes require qualified installed probe assets", ExitCode: 2})
			} else {
				rows, cleanup, err := deps.Probes.Run(ctx, r, mg, inv)
				inv.Probes = rows
				if cleanup.State != "complete" {
					report.Discovery.Cleanup.State = cleanup.State
				}
				report.Discovery.Cleanup.Leftovers = append(report.Discovery.Cleanup.Leftovers, cleanup.Leftovers...)
				if err != nil {
					report.Errors = append(report.Errors, inventory.Problem{Code: "PROBE_FAILED", Message: err.Error(), ExitCode: 3})
				}
			}
		}
		fresh, err := mg.Observe(ctx)
		if err != nil {
			report.Errors = append(report.Errors, inventory.Problem{Code: "MANAGEMENT_RECHECK_FAILED", Message: "management identity recheck unavailable", InputRef: "/management", ExitCode: 3})
		} else if fresh.SystemNamespaceUID != inv.Management.SystemNamespaceUID || !sameNodes(fresh.Nodes, inv.Management.Nodes) {
			report.Errors = append(report.Errors, inventory.Problem{Code: "MANAGEMENT_SCOPE_CHANGED", Message: "management identity or node membership changed during collection", ExitCode: 3})
		}
	}
	report.Discovery.Checks = append(report.Discovery.Checks, ProbeChecks(inv, r.Targets)...)
	for _, d := range append(slices.Clone(inv.DPUs), inv.Hosts...) {
		addCheck("dpf.device.identity", d.RecordKey, d.ID != "" && d.ObservationState == "observed", "PHYSICAL_IDENTITY_REQUIRED", d.EvidenceRefs)
		addCheck("dpf.device.qualification", d.RecordKey, d.Qualification == "qualified" && d.InventoryCompleteness == "complete", "QUALIFIED_ADAPTER_REQUIRED", d.EvidenceRefs)
	}
	report.Discovery.Outcome = "complete"
	for _, c := range report.Discovery.Checks {
		if c.RequiredPhase == "discover" && c.State != "passed" {
			report.Discovery.Outcome = "partial"
		}
	}
	if len(report.Errors) > 0 || report.Discovery.Cleanup.State != "complete" {
		report.Discovery.Outcome = "partial"
	}
	if ctx.Err() != nil {
		report.Execution = "failed"
		code := 3
		if ctx.Err() == context.Canceled {
			report.Execution = "cancelled"
			code = 1
		}
		report.Errors = append(report.Errors, inventory.Problem{Code: "COLLECTION_INTERRUPTED", Message: "discovery cancelled or timed out", ExitCode: code})
	}
	inv.Collection.FinishedAt = time.Now().UTC()
	Canonicalize(inv)
	report.Diff = Diff(r.Input.Site.Inventory, inv)
	if scopeConflict(r.Input.Site.Inventory, inv) {
		report.Discovery.Outcome = "partial"
		report.Errors = append(report.Errors, inventory.Problem{Code: "SCOPE_CONFLICT", Message: "resolved scope changed; use a fresh seed and new output", ExitCode: 2})
	}
	report.PendingInputs = []inventory.Problem{{Code: "LATER_PHASE_INPUT", Message: "DPU API allocation and selected service profile are evaluated by future generation", InputRef: "/networks/dpuAPI"}}
	slices.SortFunc(report.Errors, func(a, b inventory.Problem) int { return strings.Compare(a.InputRef+a.Code, b.InputRef+b.Code) })
	return report
}
func scopeConflict(old, current *inventory.Inventory) bool {
	return old != nil && ((old.ScopeDigest != "" && current.ScopeDigest != "" && old.ScopeDigest != current.ScopeDigest) || inventory.IdentityConflict(old, current))
}
func sameNodes(a, b []inventory.Node) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].UID != b[i].UID || a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}
func Correlate(dpus, hosts []inventory.Device) ([]inventory.Association, []inventory.Check) {
	associations := []inventory.Association{}
	checks := []inventory.Check{}
	seenIDs := map[string]int{}
	for _, d := range append(slices.Clone(dpus), hosts...) {
		if d.ID != "" {
			seenIDs[d.ID]++
		}
	}
	for _, d := range dpus {
		matches := []inventory.Association{}
		for hi := range hosts {
			h := &hosts[hi]
			for ai := range h.Adapters {
				a := &h.Adapters[ai]
				if d.SerialNumber != "" && a.SerialNumber == d.SerialNumber {
					// Selection annotations are applied by the orchestrator after correlation.
					matches = append(matches, inventory.Association{DPUID: d.ID, HostID: h.ID, HostAdapterKey: a.Key, SerialNumber: d.SerialNumber, EvidenceRefs: append(slices.Clone(d.EvidenceRefs), a.EvidenceRefs...)})
				}
			}
		}
		state := "failed"
		if len(matches) == 1 && d.ID != "" && matches[0].HostID != "" && seenIDs[d.ID] == 1 && seenIDs[matches[0].HostID] == 1 {
			state = "passed"
			associations = append(associations, matches[0])
		}
		checks = append(checks, inventory.Check{ID: "dpf.inventory.association", Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "discover", State: state, ReasonCode: "UNIQUE_PHYSICAL_MATCH_REQUIRED", EvidenceRefs: d.EvidenceRefs})
	}
	return associations, checks
}
func Canonicalize(inv *inventory.Inventory) {
	sortDevices := func(devices []inventory.Device) {
		slices.SortFunc(devices, func(a, b inventory.Device) int { return strings.Compare(a.RecordKey, b.RecordKey) })
		for i := range devices {
			slices.SortFunc(devices[i].Adapters, func(a, b inventory.Adapter) int { return strings.Compare(a.Key, b.Key) })
			slices.SortFunc(devices[i].Firmware, func(a, b inventory.Firmware) int {
				return strings.Compare(a.Component+a.RawVersion, b.Component+b.RawVersion)
			})
		}
	}
	sortDevices(inv.DPUs)
	sortDevices(inv.Hosts)
	slices.SortFunc(inv.Associations, func(a, b inventory.Association) int { return strings.Compare(a.DPUID, b.DPUID) })
	inv.Fingerprint = inventory.Digest("dpf-facts-v1", material(inv))
	if inv.Management != nil {
		inv.ScopeDigest = inventory.Digest("dpf-scope-v1", struct {
			API, UID, Trust, Mode string
			Endpoints             []inventory.ScopeEndpoint
		}{inv.Management.APIEndpoint, inv.Management.SystemNamespaceUID, inv.Management.TrustDigest, inv.Collection.Mode, inv.RequestedScope})
	}
}

// Dedicated material projection excludes timestamps, readiness, credentials and aliases.
func material(inv *inventory.Inventory) map[string]any {
	fields := map[string]any{}
	if inv.Management != nil {
		fields["management.identity"] = []string{inv.Management.APIEndpoint, inv.Management.SystemNamespaceUID, inv.Management.TrustDigest, inv.Management.KubernetesVersion}
		for _, n := range inv.Management.Nodes {
			addresses := slices.Clone(n.Addresses)
			slices.Sort(addresses)
			interfaces := slices.Clone(n.Interfaces)
			for i := range interfaces {
				interfaces[i].Addresses = slices.Clone(interfaces[i].Addresses)
				slices.Sort(interfaces[i].Addresses)
			}
			slices.SortFunc(interfaces, func(a, b inventory.Interface) int {
				return cmp.Or(cmp.Compare(a.Index, b.Index), strings.Compare(a.Name, b.Name))
			})
			routes := slices.Clone(n.Routes)
			slices.SortFunc(routes, func(a, b inventory.Route) int {
				return cmp.Or(cmp.Compare(a.Table, b.Table), strings.Compare(a.Destination, b.Destination), strings.Compare(a.Gateway, b.Gateway), cmp.Compare(a.InterfaceIndex, b.InterfaceIndex))
			})
			fields["management.node."+n.UID] = []any{n.Name, n.OS, n.Architecture, addresses, interfaces, routes}

		}
	}
	for _, devices := range [][]inventory.Device{inv.DPUs, inv.Hosts} {
		for _, d := range devices {
			key := "device." + d.RecordKey
			fields[key+".identity"] = []any{d.ID, d.IdentityKey, d.Model, d.ReaderAdapter, d.Qualification, d.InventoryCompleteness}
			for _, a := range d.Adapters {
				fields[key+".adapter."+a.Key] = []string{a.SerialNumber, a.Model, a.Slot}
			}
			for _, fw := range d.Firmware {
				fields[key+".firmware."+fw.Component+"."+fw.ResourceURI] = []string{fw.State, fw.RawVersion, fw.Activation}
			}
		}
	}
	for _, a := range inv.Associations {
		fields["association."+a.DPUID] = []string{a.HostID, a.HostAdapterKey, a.SerialNumber}
	}
	return fields
}
func Diff(old, current *inventory.Inventory) []inventory.Change {
	out := []inventory.Change{}
	if old == nil {
		return append(out, inventory.Change{Path: "inventory", State: "baseline-absent", RequiredPhase: "generate"})
	}
	before, after := material(old), material(current)
	keys := []string{}
	for k := range before {
		keys = append(keys, k)
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		a, aok := before[k]
		b, bok := after[k]
		if reflect.DeepEqual(a, b) {
			continue
		}
		state := "changed"
		if !aok {
			state = "added"
		}
		if !bok {
			state = "unknown"
		}
		for _, group := range [][]inventory.Device{current.DPUs, current.Hosts} {
			for _, d := range group {
				if d.ObservationState != "observed" && strings.HasPrefix(k, "device."+d.RecordKey+".") {
					state = "unknown"
				}
			}
		}
		out = append(out, inventory.Change{Path: k, State: state, Previous: a, Current: b, RequiredPhase: "generate"})
	}
	return out
}
func ExitCode(r inventory.Report) int {
	code := 0
	for _, e := range r.Errors {
		if e.ExitCode == 1 {
			return 1
		}
		if e.ExitCode == 3 {
			code = 3
		} else if code == 0 {
			code = e.ExitCode
		}
	}
	if code != 0 {
		return code
	}
	if r.Discovery.Outcome != "complete" {
		return 2
	}
	return 0
}

// ProbeChecks derives required rows from frozen node/endpoint scope, never aggregate counts.
func ProbeChecks(inv *inventory.Inventory, targets []config.Target) []inventory.Check {
	if inv.Collection.Mode != "management" {
		return []inventory.Check{{ID: "dpf.management.path", Subject: "management", RequiredPhase: "generate", State: "not-requested", ReasonCode: "INVENTORY_MODE", EvidenceRefs: []string{}}}
	}
	checks := []inventory.Check{}
	if inv.Management == nil {
		return checks
	}
	destinations := []string{"management-api"}
	for _, t := range targets {
		if t.Role == "dpus" {
			destinations = append(destinations, inventory.Digest("probe-destination-v1", t.Endpoint))
		}
	}
	rows := map[string][]inventory.ProbeResult{}
	for _, r := range inv.Probes {
		key := r.NodeUID + "/" + r.NetworkMode + "/" + r.Destination
		rows[key] = append(rows[key], r)
	}
	for _, n := range inv.Management.Nodes {
		for _, mode := range []string{"pod-network", "node-network"} {
			for _, destination := range destinations {
				key := n.UID + "/" + mode + "/" + destination
				state := "unknown"
				refs := []string{}
				values := rows[key]
				if len(values) == 1 {
					v := values[0]
					if v.EvidenceRef != "" {
						if _, ok := inv.Evidence[v.EvidenceRef]; ok {
							refs = append(refs, v.EvidenceRef)
						}
					}
					state = "passed"
					if len(refs) == 0 || len(v.Addresses) == 0 || v.DNS == "unavailable" {
						state = "unknown"
					}
					for _, a := range v.Addresses {
						if a.TCP != "reachable" || a.TLS != "verified" {
							state = "failed"
						}
					}
				} else if len(values) > 1 {
					state = "failed"
				}
				checks = append(checks, inventory.Check{ID: "dpf.management.path", Subject: key, Vantage: n.UID + "/" + mode, RequiredPhase: "discover", State: state, ReasonCode: "EXACT_NETWORK_COVERAGE_REQUIRED", EvidenceRefs: refs})
			}
		}
	}
	return checks
}
