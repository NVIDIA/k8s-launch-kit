// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package discovery orchestrates independent management and BMC observations.
package discovery

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ProbeRunner interface {
	Run(context.Context, config.Request, ManagementClient, *inventory.Inventory) ([]inventory.ProbeResult, inventory.Cleanup, error)
}
type Collector interface {
	Collect(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error)
}
type Dependencies struct {
	Switches       SwitchCollector
	SSH            Collector
	BMC            Collector
	Probes         ProbeRunner
	HBNPermissions HBNPermissionObserver
}

func NewReport() inventory.Report {
	return inventory.Report{SchemaVersion: inventory.FormatVersion, Target: "dpf", Phase: "discover", Execution: "completed", Acceptance: "not-evaluated", Discovery: inventory.Discovery{Outcome: "invalid", Checks: []inventory.Check{}, Cleanup: inventory.Cleanup{State: "complete", Leftovers: []string{}}}, Diff: []inventory.Change{}, PendingInputs: []inventory.Problem{}, Artifacts: []inventory.Artifact{}, Errors: []inventory.Problem{}}
}
func Collect(ctx context.Context, r config.Request, runID string, deps Dependencies) inventory.Report {
	started := time.Now()
	logger := log.FromContext(ctx).WithName("dpf.discovery")
	ctx = log.IntoContext(ctx, logger)
	output := ui.FromContext(ctx)
	report := NewReport()
	logger.V(1).Info("DPF discovery started", "mode", r.Mode, "endpointCount", len(r.Targets))
	defer func() {
		logger.V(1).Info("DPF discovery completed", "outcome", report.Discovery.Outcome, "execution", report.Execution, "cleanupState", report.Discovery.Cleanup.State, "problemCount", len(report.Errors), "duration", time.Since(started))
	}()
	report.Discovery.Mode = r.Mode
	inv := &inventory.Inventory{FormatVersion: inventory.FormatVersion, Collection: inventory.Collection{RunID: runID, StartedAt: time.Now().UTC(), Mode: r.Mode, Collector: "l8k-go-discovery-v1"}, RequestedScope: []inventory.ScopeEndpoint{}, DPUs: []inventory.Device{}, Hosts: []inventory.Device{}, Associations: []inventory.Association{}, Evidence: map[string]inventory.Evidence{}}
	report.Inventory = inv
	inv.Software = r.Software
	for _, t := range r.Targets {
		inv.RequestedScope = append(inv.RequestedScope, config.Scope(t))
	}
	for _, sw := range r.Switches {
		inv.RequestedScope = append(inv.RequestedScope, config.Scope(config.Target{Role: "switches", Endpoint: sw.Endpoint, SSH: sw.Access}))
	}
	slices.SortFunc(inv.RequestedScope, func(a, b inventory.ScopeEndpoint) int {
		return strings.Compare(a.Role+a.Endpoint+a.SystemURI, b.Role+b.Endpoint+b.SystemURI)
	})
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	problem := func(p inventory.Problem) { mu.Lock(); defer mu.Unlock(); report.Errors = append(report.Errors, p) }
	var mg ManagementClient
	var provisioning *managementprobe.Snapshot
	if r.Kubeconfig != "" {
		var accessErr error
		mg, accessErr = ManagementAccess(r)
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
			if r.HBN != nil {
				output.Info("[CHECK] Management cluster: inspect DPF installation and provisioning APIs")
				snapshot := mg.ObserveProvisioning(ctx, r.DPFNamespace)
				provisioning = &snapshot
				output.Info("[DONE] Management cluster: DPF installation and provisioning API inspection")
			}
			mu.Lock()
			defer mu.Unlock()
			for id, e := range NewEvidence(time.Now().UTC()) {
				inv.Evidence[id] = e
			}
		}()
	}
	sem := make(chan struct{}, r.MaxConcurrentBMCs)
	devices := make([]inventory.Device, len(r.Targets))
	for index, target := range r.Targets {
		wg.Add(1)
		go func(i int, t config.Target) {
			defer wg.Done()
			endpointLogger := logger.WithValues("endpoint", t.DisplayName(), "role", t.Role, "sourceIndex", i+1)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				devices[i] = inventory.Device{RecordKey: inventory.Digest("dpf-access-v1", []string{t.Role, t.Endpoint, t.SystemURI}), InputRef: t.InputRef, BMCEndpoint: t.Endpoint, ObservationState: "unavailable", ReasonCode: "CANCELLED"}
				endpointLogger.V(1).Info("Endpoint observation not started", "reasonCode", "CANCELLED")
				return
			}
			endpointStarted := time.Now()
			endpointLogger.V(1).Info("Endpoint observation started")
			defer func() {
				d := devices[i]
				endpointLogger.V(1).Info("Endpoint observation completed", "identityState", d.ObservationState, "identityEstablished", d.ID != "", "reasonCode", d.ReasonCode, "duration", time.Since(endpointStarted))
			}()
			collector := deps.BMC
			if t.SSH != nil {
				collector = deps.SSH
			}
			if collector == nil {
				problem(inventory.Problem{Code: "COLLECTOR_UNAVAILABLE", Message: "selected discovery source is unavailable", InputRef: t.InputRef, ExitCode: 3})
				devices[i] = inventory.Device{RecordKey: inventory.Digest("dpf-access-v1", []string{t.Role, t.Endpoint, t.SystemURI}), InputRef: t.InputRef, ObservationState: "unavailable", ReasonCode: "COLLECTOR_UNAVAILABLE"}
				return
			}
			d, e, err := collector.Collect(ctx, t, r.RequestTimeout)
			devices[i] = d
			mu.Lock()
			for key, value := range e {
				inv.Evidence[key] = value
			}
			mu.Unlock()
			if err != nil {
				if t.SSH != nil {
					code := d.ReasonCode
					if code == "" {
						code = "SSH_OBSERVATION_FAILED"
					}
					stage := "observation"
					tool := ""
					var diagnostic *sshprobe.Error
					if errors.As(err, &diagnostic) {
						stage = diagnostic.Stage
						tool = diagnostic.Tool
					}
					problem(inventory.Problem{Code: code, Message: sshprobe.Reason(err), Tool: tool, InputRef: t.InputRef, Stage: stage, ExitCode: 3})
				} else {
					for _, diagnostic := range redfish.Diagnostics(err) {
						problem(inventory.Problem{Code: diagnostic.Code, Message: "Redfish operation failed", InputRef: t.InputRef, Method: diagnostic.Method, ResourceURI: diagnostic.URI, HTTPStatus: diagnostic.Status, Stage: diagnostic.Stage, ExitCode: 3})
					}
				}
				if d.ReasonCode == "CLEANUP_INCOMPLETE" {
					mu.Lock()
					report.Discovery.Cleanup.State = "incomplete"
					report.Discovery.Cleanup.Leftovers = append(report.Discovery.Cleanup.Leftovers, t.Endpoint+redfish.CleanupReceipt(err)+": session cleanup incomplete")
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
	requestedDPUs := false
	collectAddresses(r, inv)
	for _, t := range r.Targets {
		requestedDPUs = requestedDPUs || t.Role == "dpus"
	}
	if !requestedDPUs {
		inv.DPUs = HostCards(inv.Hosts)
	}
	output.Info("[RUNNING] Host/DPU physical association")
	inv.Associations, report.Discovery.Checks = Correlate(inv.DPUs, inv.Hosts)
	if len(inv.Associations) == len(inv.DPUs) && len(inv.DPUs) > 0 {
		output.Info("[PASS] Host/DPU physical association: %d unique matches", len(inv.Associations))
	} else {
		output.Info("[FAIL] Host/DPU physical association: %d/%d uniquely matched", len(inv.Associations), len(inv.DPUs))
	}
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
	addCheck := func(id, subject string, ok bool, reason string, refs []string, phase string) {
		state := "failed"
		if ok {
			state = "passed"
		}
		report.Discovery.Checks = append(report.Discovery.Checks, inventory.Check{ID: id, Subject: subject, Vantage: "executor", RequiredPhase: phase, State: state, ReasonCode: reason, EvidenceRefs: refs})
	}
	if inv.Management != nil {
		cp := 0
		for _, n := range inv.Management.Nodes {
			if n.ControlPlane {
				cp++
			}
		}
		phase := "generate"
		if r.Mode == "management" {
			phase = "discover"
		}
		addCheck("dpf.management.control-plane-count", "management", cp == 3, "EXACTLY_THREE_CONTROL_PLANES", inv.Management.EvidenceRefs, phase)
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
			report.Errors = append(report.Errors, inventory.Problem{Code: "MANAGEMENT_SCOPE_CHANGED", Message: "management identity, node membership or scheduling facts changed during collection", ExitCode: 3})
		}
	}
	output.Info("[RUNNING] Required discovery checks")
	report.Discovery.Checks = append(report.Discovery.Checks, ProbeChecks(inv, r.Targets)...)
	for _, d := range append(slices.Clone(inv.DPUs), inv.Hosts...) {
		addCheck("dpf.device.identity", d.RecordKey, d.ID != "" && d.ObservationState == "observed", "PHYSICAL_IDENTITY_REQUIRED", d.EvidenceRefs, "discover")
		addCheck("dpf.device.qualification", d.RecordKey, d.Qualification == "qualified" && d.InventoryCompleteness == "complete", "QUALIFIED_ADAPTER_REQUIRED", d.EvidenceRefs, "generate")
	}
	addCheck("dpf.inventory.dpus", "inventory", len(inv.DPUs) > 0, "DPU_IDENTITY_REQUIRED", nil, "discover")
	for i := range inv.DPUs {
		d := &inv.DPUs[i]
		admin := d.Observations["bmc-administrator"]
		if admin.State == "" || d.BMCEndpoint == "" || d.SSHEndpoint != "" {
			admin = inventory.Observation{State: "failed", ReasonCode: "BMC_ADMIN_NOT_VERIFIED", Message: "An explicit DPU BMC endpoint with verified administrator privileges is required; host or ARM SSH identity cannot substitute for BMC authorization."}
		}
		if d.Observations == nil {
			d.Observations = map[string]inventory.Observation{}
		}
		d.Observations["bmc-administrator"] = admin
		adminState := "failed"
		if admin.State == "observed" && admin.ReasonCode == "BMC_ADMIN_VERIFIED" && d.BMCEndpoint != "" && d.SSHEndpoint == "" {
			adminState = "passed"
		}
		report.Discovery.Checks = append(report.Discovery.Checks, inventory.Check{ID: "dpf.management.bmc-administrator", Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "discover", State: adminState, ReasonCode: admin.ReasonCode, Message: admin.Message, EvidenceRefs: d.EvidenceRefs})
		state, reason := "not-evaluated", "REDFISH_AUTHORIZATION_NOT_EVALUATED"
		if d.Authentication.PasswordRead == "verified" {
			state, reason = "passed", "INVENTORY_READ_VERIFIED"
		}
		report.Discovery.Checks = append(report.Discovery.Checks, inventory.Check{ID: "dpf.management.redfish-read", Subject: d.RecordKey, RequiredPhase: "deploy", State: state, ReasonCode: reason, EvidenceRefs: d.EvidenceRefs})
		report.Discovery.Checks = append(report.Discovery.Checks, inventory.Check{ID: "dpf.management.dpf-authentication", Subject: d.RecordKey, RequiredPhase: "deploy", State: "not-evaluated", ReasonCode: "DPF_AUTHENTICATION_NOT_EVALUATED", EvidenceRefs: []string{}})
	}
	report.Discovery.Checks = append(report.Discovery.Checks, SoftwareChecks(inv)...)
	for i := range report.Discovery.Checks {
		check := &report.Discovery.Checks[i]
		ExplainCheck(check)
		checkLogger := logger
		for index, device := range devices {
			if check.Subject != "" && (check.Subject == device.RecordKey || check.Subject == device.ID) {
				checkLogger = logger.WithValues("endpoint", r.Targets[index].DisplayName(), "role", r.Targets[index].Role, "sourceIndex", index+1)
				break
			}
		}
		checkLogger.V(1).Info("Discovery check evaluated", "check", check.ID, "state", check.State, "requiredPhase", check.RequiredPhase, "reasonCode", check.ReasonCode)
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
	collectHBN(ctx, r, &report, deps.Switches, provisioning, deps.HBNPermissions)
	if ctx.Err() != nil {
		report.Discovery.Outcome = "partial"
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
	if r.Kubeconfig != "" && inv.Management == nil {
		inv.ScopeDigest = ""
	}
	report.Diff = Diff(r.Input.Site.Inventory, inv)
	if scopeConflict(r.Input.Site.Inventory, inv) {
		report.Discovery.Outcome = "partial"
		message := "resolved scope changed; use a fresh seed and new output"
		if inventory.LegacyHostCardBindings(r.Input.Site.Inventory) {
			message = "saved inventory uses legacy Redfish card bindings that cannot protect refresh; verify endpoint ownership and use a fresh seed without inventory and a new output"
		}
		report.Errors = append(report.Errors, inventory.Problem{Code: "SCOPE_CONFLICT", Message: message, ExitCode: 2})
	}
	report.PendingInputs = []inventory.Problem{{Code: "LATER_PHASE_INPUT", Message: "DPU API allocation and selected service profile are evaluated by future generation", InputRef: "/networks/dpuAPI"}}
	slices.SortFunc(report.Errors, func(a, b inventory.Problem) int { return strings.Compare(a.InputRef+a.Code, b.InputRef+b.Code) })
	if report.Discovery.Outcome == "complete" {
		output.Info("[PASS] Required discovery checks completed; publication pending")
	} else {
		output.Info("[FAIL] Required discovery checks: incomplete evidence; complete handoff will not be published")
	}
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
		if a[i].UID != b[i].UID || a[i].Name != b[i].Name || a[i].ControlPlane != b[i].ControlPlane || a[i].Ready != b[i].Ready || a[i].Unschedulable != b[i].Unschedulable || a[i].OS != b[i].OS || a[i].Architecture != b[i].Architecture || !maps.Equal(a[i].Labels, b[i].Labels) {
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
		vendorConflict := false
		refs := slices.Clone(d.EvidenceRefs)
		for hi := range hosts {
			h := &hosts[hi]
			for ai := range h.Adapters {
				a := &h.Adapters[ai]
				if d.ObservationState == "observed" && h.ObservationState == "observed" && d.SerialNumber != "" && a.SerialNumber == d.SerialNumber {
					// Selection annotations are applied by the orchestrator after correlation.
					matches = append(matches, inventory.Association{DPUID: d.ID, HostID: h.ID, HostAdapterKey: a.Key, SerialNumber: d.SerialNumber, EvidenceRefs: append(slices.Clone(d.EvidenceRefs), a.EvidenceRefs...)})
					// Readers normalize known PCI vendors to four-digit lowercase hex.
					// Keep contradictory candidates: dropping one could turn ambiguity
					// into an apparently unique NVIDIA BlueField association.
					if a.VendorID != "" && a.VendorID != "15b3" {
						vendorConflict = true
						refs = append(refs, a.EvidenceRefs...)
					}
				}
			}
		}
		state := "failed"
		reason := "UNIQUE_PHYSICAL_MATCH_REQUIRED"
		if vendorConflict {
			reason = "HOST_ADAPTER_VENDOR_CONFLICT"
		} else if len(matches) == 1 && d.ID != "" && matches[0].HostID != "" && seenIDs[d.ID] == 1 && seenIDs[matches[0].HostID] == 1 {
			state = "passed"
			associations = append(associations, matches[0])
		}
		checks = append(checks, inventory.Check{ID: "dpf.inventory.association", Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "discover", State: state, ReasonCode: reason, EvidenceRefs: refs})
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
	// A loaded v3 inventory retains only the digest. Fresh collections supply
	// RequestedScope; do not replace a persisted guard with an empty scope.
	if inv.RequestedScope == nil && inv.ScopeDigest != "" {
		return
	}
	inv.ScopeDigest = inventory.Digest("dpf-infrastructure-scope-v1", struct {
		Mode      string
		Endpoints []inventory.ScopeEndpoint
	}{inv.Collection.Mode, inv.RequestedScope})
	if inv.Management != nil {
		inv.ScopeDigest = inventory.Digest("dpf-scope-v1", struct {
			API, UID, Trust, Mode string
			Endpoints             []inventory.ScopeEndpoint
		}{inv.Management.APIEndpoint, inv.Management.SystemNamespaceUID, inv.Management.TrustDigest, inv.Collection.Mode, inv.RequestedScope})
	}
	if inv.HBN != nil {
		inv.ScopeDigest = inventory.Digest("dpf-hbn-scope-v1", []string{inv.ScopeDigest, hbnScope(inv.HBN)})
	}
}

// Dedicated material projection excludes timestamps, readiness, credentials and aliases.
func material(inv *inventory.Inventory) map[string]any {
	fields := map[string]any{}
	if inv.HBN != nil {
		fields["hbn"] = inv.HBN
	}
	if inv.Software != nil {
		fields["software"] = releases.Summarize(*inv.Software)
	}
	if inv.AddressLookup != nil {
		fields["addressLookup"] = inv.AddressLookup
	}
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
			fields["management.node."+n.UID] = []any{n.Name, n.OS, n.Architecture, addresses, interfaces, routes, n.TenantVIPRoute}

		}
	}
	for _, group := range []struct {
		role    string
		devices []inventory.Device
	}{{"dpus", inv.DPUs}, {"hosts", inv.Hosts}} {
		for _, d := range group.devices {
			key := "device." + d.RecordKey
			if d.Runtime != nil {
				runtime := canonicalRuntime(*d.Runtime)
				d.Runtime = &runtime
			}
			w, err := inventory.DeviceToOutput(d, group.role)
			if err != nil {
				// Publication rejects malformed facts. Keep them visible in the
				// comparison as well, rather than claiming an unchanged record.
				fields[key+".invalid"] = err.Error()
				continue
			}
			fields[key+".identity"] = []string{w.ID, w.SerialNumber, w.SystemUUID, w.Manufacturer, w.Model, w.Method}
			fields[key+".endpoints"] = []string{w.SSHEndpoint, w.BMCEndpoint, w.SystemURI}
			fields[key+".psid"] = w.PSID
			fields[key+".networkFunctionMACs"] = w.NetworkFunctionMACs
			fields[key+".bmcAdministratorVerified"] = w.BMCAdministratorVerified
			fields[key+".firmwareInventoryComplete"] = w.FirmwareInventoryComplete
			fields[key+".firmware"] = w.Firmware
			fields[key+".runtime"] = w.Runtime
			fields[key+".advertisedResetTypes"] = w.AdvertisedResetTypes
			for _, a := range w.Adapters {
				a.PCIFunctions = canonicalFunctions(a.PCIFunctions)
				fields[key+".adapter."+a.Key] = a
			}
			for _, issue := range w.Issues {
				fields[key+".issue."+issue.Field+"."+issue.Code] = issue.Tool
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
	if r.Readiness != nil && r.Readiness.State != "ready" {
		return 2
	}
	return 0
}

// ProbeChecks derives required rows from frozen node/endpoint scope, never aggregate counts.
func ProbeChecks(inv *inventory.Inventory, targets []config.Target) []inventory.Check {
	if inv.Collection.Mode != "management" {
		return []inventory.Check{{ID: "dpf.management.path", Subject: "management", RequiredPhase: "generate", State: "not-requested", ReasonCode: "INVENTORY_MODE", Message: "Inventory mode omits management-network probes.", EvidenceRefs: []string{}}}
	}
	checks := []inventory.Check{}
	if inv.Management == nil {
		return checks
	}
	destinations := []string{"management-api"}
	names := map[string]string{"management-api": "management API"}
	allowUnverified := map[string]bool{}
	for _, t := range targets {
		if t.Role == "dpus" {
			id := inventory.Digest("probe-destination-v1", t.Endpoint)
			destinations = append(destinations, id)
			names[id] = t.DisplayName()
			allowUnverified[id] = t.Access.InsecureSkipVerify()
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
				state, code, message := "unknown", "PROBE_NOT_OBSERVED", "No validated probe result was collected for this path."
				refs := []string{}
				values := rows[key]
				if len(values) == 1 {
					v := values[0]
					if v.EvidenceRef != "" {
						if _, ok := inv.Evidence[v.EvidenceRef]; ok {
							refs = append(refs, v.EvidenceRef)
						}
					}
					switch {
					case len(refs) == 0:
						code, message = "PROBE_SOURCE_UNVERIFIED", "The result lacks a verified source binding."
					case v.DNS != "resolved" && v.DNS != "not-applicable":
						code, message = "PROBE_DNS_UNAVAILABLE", "The destination could not be resolved from this network namespace."
					case len(v.Addresses) == 0:
						code, message = "PROBE_ADDRESSES_UNAVAILABLE", "No destination address was available for the connection checks."
					default:
						state, code, message = "passed", "NETWORK_PATH_VERIFIED", "DNS/address resolution, TCP connectivity and TLS verification passed for every resolved address."
						expectedTLS := "verified"
						if allowUnverified[destination] {
							expectedTLS = "unverified"
						}
						for _, a := range v.Addresses {
							if a.TCP != "reachable" {
								state, code, message = "failed", "PROBE_TCP_UNREACHABLE", "TCP connectivity failed for at least one destination address."
								break
							}
							if a.TLS != expectedTLS {
								state, code, message = "failed", "PROBE_TLS_UNVERIFIED", "TLS verification failed for at least one destination address; provide trusted CA material and a matching certificate identity."
							}
						}
						if state == "passed" && allowUnverified[destination] {
							code, message = "NETWORK_PATH_REACHABLE_TLS_UNVERIFIED", "TCP and TLS handshakes succeeded with explicitly disabled BMC certificate verification; server identity remains unverified."
						}
					}
				} else if len(values) > 1 {
					state, code, message = "failed", "PROBE_COVERAGE_AMBIGUOUS", "Multiple results claim this same node, network namespace and destination."
				}
				checks = append(checks, inventory.Check{ID: "dpf.management.path", Subject: n.Name + "/" + mode + "/" + names[destination], Vantage: n.UID + "/" + mode, RequiredPhase: "discover", State: state, ReasonCode: code, Message: message, EvidenceRefs: refs})
			}
		}
	}
	return checks
}
