// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/switchprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

type validationPhysicalReaders struct {
	bmc    func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error)
	host   func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error)
	fabric func(context.Context, config.SwitchTarget, time.Duration) fabric.Snapshot
	lldp   discovery.HostLLDPCapturer
}

func (o *validationObserver) physicalReaders(includeCabling bool) validationPhysicalReaders {
	r := o.physical
	if r.bmc == nil {
		r.bmc = (redfish.Collector{ProvisioningRelease: o.desired.plan.Intent().Release, IncludeNetworkFunctionMACs: includeCabling}).Collect
	}
	if r.host == nil {
		r.host = (sshprobe.Collector{}).Collect
	}
	if r.fabric == nil {
		r.fabric = (switchprobe.Collector{}).ObserveConfiguration
	}
	if r.lldp == nil {
		r.lldp = sshprobe.Collector{}
	}
	return r
}

func physicalID(kind, subject string) validation.CheckID {
	return validation.CheckID("physical/" + kind + "/" + url.PathEscape(subject))
}
func physicalSpec(kind, subject, title string) validation.CheckSpec {
	s := validationSpec(string(physicalID(kind, subject)), validation.AreaPhysical, title, false)
	s.FullOnly, s.CohortBound = true, true
	return s
}
func fabricValidationID(check inventory.Check) validation.CheckID {
	return physicalID("fabric", check.ID+"/"+check.Subject)
}

func (o *validationObserver) unavailablePhysicalSource(role, endpoint, fallback string) string {
	if !o.physicalScope.accessVerified {
		if o.physicalScope.trustProblem != "" {
			return o.physicalScope.trustProblem
		}
		return "Physical access cannot be verified against the retained trust policy because one or more trust files are unavailable. Restore the trust files and repeat validation."
	}
	sources, err := validationSources(o.request.Input.Site)
	if err != nil {
		return fallback
	}
	for _, source := range sources {
		if source.role != role || source.endpoint != endpoint {
			continue
		}
		for _, issue := range o.unavailable {
			if issue.Role == role && issue.InputRef == source.ref {
				return fallback + " " + issue.Message + "."
			}
		}
	}
	return fallback
}

// Only selected identities and intended bindings are copied into this view.
// Saved observations cannot satisfy a check in a fresh run.
func (o *validationObserver) physicalInventory() *inventory.Inventory {
	intent := o.desired.plan.Intent()
	release, _ := releases.Resolve(intent.Release)
	inv := &inventory.Inventory{Software: &release, HBN: &inventory.HBNInventory{Namespace: intent.Namespace, Profile: intent.Networking}}
	for _, d := range intent.Devices {
		inv.DPUs = append(inv.DPUs, inventory.Device{SerialNumber: d.Serial, PSID: d.PSID, ObservationState: "unavailable"})
		inv.HBN.ASNs = append(inv.HBN.ASNs, inventory.ASNAssignment{DPUSerial: d.Serial, ASN: d.ASN})
	}
	for _, sw := range o.request.Input.Site.Discovery.Switches {
		endpoint, _ := config.SSHOrigin(sw.SSH.Address) // Constructor verified the complete declaration.
		platform := "cumulus-nvue"
		if sw.Platform != nil {
			platform = *sw.Platform
		}
		inv.HBN.Switches = append(inv.HBN.Switches, inventory.FabricSwitch{Name: sw.Name, Endpoint: endpoint, Platform: platform, Ports: slices.Clone(sw.Ports)})
	}
	return inv
}

func (o *validationObserver) physicalValidationChecks() []validation.CheckSpec {
	coverage := physicalSpec("coverage", "selected", "Selected physical sources and fabric coverage")
	coverage.Coverage = true
	checks := []validation.CheckSpec{coverage}
	for _, d := range o.desired.plan.Intent().Devices {
		for _, item := range []struct{ kind, title string }{{"dpu", "DPU identity"}, {"administrator", "DPU BMC Administrator access"}, {"host", "Host identity and selected physical interface"}, {"host-prerequisites", "Host PCI, IOMMU and management separation"}, {"software", "Documented release hardware requirements"}} {
			checks = append(checks, physicalSpec(item.kind, d.Serial, item.title+" — "+d.Serial))
		}
	}
	inv := o.physicalInventory()
	for _, sw := range inv.HBN.Switches {
		checks = append(checks, physicalSpec("switch", sw.Name, "Switch observations — "+sw.Name))
	}
	for _, check := range discovery.HBNFabricConfigurationChecks(inv) {
		if check.ID == "dpf.hbn.switch-source" {
			continue
		}
		s := physicalSpec("fabric", check.ID+"/"+check.Subject, fabricValidationTitle(check)+" — "+check.Subject)
		checks = append(checks, s)
	}
	return checks
}

func (o *validationObserver) observePhysical(ctx context.Context, r *validationRecorder) error {
	if o.mode != validation.ModeFull {
		return nil
	}
	inv, err := o.observePhysicalDevices(ctx, r, true)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	read := o.physicalReaders(true)
	for i := range inv.HBN.Switches {
		sw := &inv.HBN.Switches[i]
		id := physicalID("switch", sw.Name)
		r.start(id)
		var target *config.SwitchTarget
		for j := range o.request.Switches {
			if o.request.Switches[j].Name == sw.Name && o.request.Switches[j].Endpoint == sw.Endpoint {
				target = &o.request.Switches[j]
			}
		}
		if !o.physicalScope.accessVerified || target == nil || target.Access == nil {
			sw.Snapshot.Issues = []fabric.Issue{{Input: "connection", Code: "ACCESS_UNAVAILABLE", Message: "The selected switch credential or trust reference is unavailable."}}
			r.record(id, validation.Unverified, validation.ReasonUnavailable, o.unavailablePhysicalSource("switches", sw.Endpoint, "Switch access is unavailable."), "Restore the switch credential and known-hosts reference.")
			continue
		}
		sw.Snapshot = read.fabric(ctx, *target, o.request.RequestTimeout)
	}
	if o.physicalScope.accessVerified {
		discovery.CollectHostHBNCabling(ctx, o.request, inv, read.lldp)
	}
	fabricChecks := validationFabricConfigurationChecks(inv)
	for _, sw := range inv.HBN.Switches {
		id := physicalID("switch", sw.Name)
		if _, alreadyRecorded := r.outcomes[id]; alreadyRecorded {
			continue
		}
		issues := validationRelevantSwitchIssues(sw, inv, fabricChecks)
		if len(issues) > 0 {
			state, reason := validation.Unverified, validation.ReasonUnavailable
			for _, issue := range issues {
				if issue.Code == "conflicting_observation" || issue.Code == "not_configured" || issue.Code == "interface_not_listed" {
					state, reason = validation.Failed, validation.ReasonHealth
				}
			}
			r.record(id, state, reason, "Selected switch configuration or observations need attention. "+validationSwitchProblems(issues), "Inspect the reported selected switch settings or read access.")
		} else {
			r.record(id, validation.Passed, validation.ReasonNone, "The read-only switch session completed; selected fabric requirements are checked separately.", "")
		}
	}

	for _, check := range fabricChecks {
		if check.ID != "dpf.hbn.switch-source" {
			recordPhysicalCheck(r, fabricValidationID(check), check)
		}
	}
	softwareChecks := discovery.SoftwareChecks(inv)
	for _, device := range inv.DPUs {
		var checks []inventory.Check
		for _, check := range softwareChecks {
			if check.Subject == device.RecordKey {
				checks = append(checks, check)
			}
		}
		recordPhysicalGroup(r, physicalID("software", device.SerialNumber), checks, "Observed hardware satisfies the release's documented minimum requirements; unspecified firmware minima are outside this check.")
	}
	coverage := physicalID("coverage", "selected")
	complete := true
	for _, spec := range o.physicalValidationChecks() {
		if spec.ID != coverage {
			if result, ok := r.outcomes[spec.ID]; !ok || result.State != validation.Passed {
				complete = false
			}
		}
	}
	if complete {
		r.record(coverage, validation.Passed, validation.ReasonNone, "Every selected physical source and configured fabric prerequisite was checked.", "")
	} else {
		r.record(coverage, validation.Unverified, validation.ReasonIncomplete, "Physical or fabric validation is incomplete; see the individual checks.", "Resolve failed or unavailable observations and validate again.")
	}
	return err
}

func (o *validationObserver) observePhysicalDevices(ctx context.Context, r *validationRecorder, includeCabling bool) (*inventory.Inventory, error) {
	inv := o.physicalInventory()
	selected, err := validationSelectedHosts(o.desired, o.request, o.physicalScope.expected)
	if err != nil {
		return inv, &validation.ObservationError{Category: validation.FatalInput, Cause: err}
	}
	read := o.physicalReaders(includeCabling)
	var fatal error
	for _, item := range selected {
		serial := item.retained.Serial
		bmcID, adminID, hostID := physicalID("dpu", serial), physicalID("administrator", serial), physicalID("host", serial)
		r.start(bmcID)
		var target *config.Target
		for i := range o.request.Targets {
			candidate := &o.request.Targets[i]
			if candidate.Role == "dpus" && candidate.SSH == nil && candidate.Endpoint == item.device.BMCEndpoint {
				target = candidate
			}
		}
		if !o.physicalScope.accessVerified || target == nil {
			r.record(bmcID, validation.Unverified, validation.ReasonUnavailable, o.unavailablePhysicalSource("dpus", item.device.BMCEndpoint, "Selected DPU BMC access is unavailable."), "Restore its credential and TLS trust references.")
			r.record(adminID, validation.Unverified, validation.ReasonDependency, "Administrator access could not be checked without a BMC session.", "Restore selected BMC access.")
		} else {
			observed, _, readErr := read.bmc(ctx, *target, o.request.RequestTimeout)
			switch {
			case redfish.Code(readErr) == "IDENTITY_CHANGED":
				r.record(bmcID, validation.Failed, validation.ReasonIdentityDrift, "The selected DPU identity changed during the BMC observation.", "Inspect the selected BMC and repeat discovery and generation.")
				fatal = &validation.ObservationError{Category: validation.FatalIdentity}
				r.record(adminID, validation.Unverified, validation.ReasonDependency, "Administrator access cannot qualify an identity that changed during collection.", "Resolve the identity change.")
			case readErr != nil:
				r.record(bmcID, validation.Unverified, validation.ReasonUnavailable, "BMC identity could not be fully observed or its session closed. "+safeBMCDiagnostics(readErr), "Verify BMC reachability, credentials and certificate policy.")
				r.record(adminID, validation.Unverified, validation.ReasonDependency, "Administrator access cannot qualify an incomplete BMC session.", "Resolve the BMC observation failure.")
			case validationDPUIdentityConflict(observed, item.device.ID, serial, item.retained.PSID):
				r.record(bmcID, validation.Failed, validation.ReasonIdentityDrift, "The current DPU physical identity conflicts with the retained selection.", "Inspect the selected BMC and repeat discovery and generation.")
				fatal = &validation.ObservationError{Category: validation.FatalIdentity}
				r.record(adminID, validation.Unverified, validation.ReasonDependency, "Administrator access cannot qualify a different device.", "Resolve the identity mismatch.")
			case observed.ObservationState != "observed" || observed.ID == "" || observed.SerialNumber == "" || !enrollmentPSIDMatches(observed, item.retained.PSID):
				r.record(bmcID, validation.Unverified, validation.ReasonUnavailable, "The current BMC observation does not establish the selected DPU identity or PSID. Missing or malformed identity facts do not establish a device replacement.", "Restore the BMC identity and PSID observations, then validate again.")
				r.record(adminID, validation.Unverified, validation.ReasonDependency, "Administrator access cannot qualify an incompletely identified device.", "Resolve the BMC identity observation failure.")
			default:
				for i := range inv.DPUs {
					if inv.DPUs[i].SerialNumber == serial {
						inv.DPUs[i] = observed
					}
				}
				r.record(bmcID, validation.Passed, validation.ReasonNone, "Current BMC serial, physical identity and reported PSID agree with the selected DPU.", "")
				admin := observed.Observations["bmc-administrator"]
				if admin.State == "observed" && admin.ReasonCode == "BMC_ADMIN_VERIFIED" {
					r.record(adminID, validation.Passed, validation.ReasonNone, "The current BMC session has verified Administrator privileges.", "")
				} else {
					r.record(adminID, validation.Failed, validation.ReasonDenied, "The current BMC account does not have verified Administrator privileges.", "Use an enabled, unlocked Administrator account with no pending password change.")
				}
			}
		}
		r.start(hostID)
		if !o.physicalScope.accessVerified || item.hostTarget.SSH == nil {
			r.record(hostID, validation.Unverified, validation.ReasonUnavailable, o.unavailablePhysicalSource("hosts", item.hostTarget.Endpoint, "The selected host's SSH access is unavailable."), "Restore its credential and known-hosts references.")
			continue
		}
		host, _, readErr := read.host(ctx, item.hostTarget, o.request.RequestTimeout)
		if sshprobe.Code(readErr) == "INVENTORY_CHANGED" {
			r.record(hostID, validation.Failed, validation.ReasonIdentityDrift, "The selected host identity or PCI binding changed during hardware inspection.", "Inspect the host and repeat discovery and generation.")
			fatal = &validation.ObservationError{Category: validation.FatalIdentity}
			continue
		}
		if readErr != nil || host.ObservationState != "observed" {
			r.record(hostID, validation.Unverified, validation.ReasonUnavailable, "Host hardware inspection did not complete: "+sshprobe.Reason(readErr)+" ("+sshprobe.Code(readErr)+").", "Check SSH access, sudo permissions and the reported inspection tool.")
			continue
		}
		oldIdentity, oldErr := sshprobe.SelectHostWorkloadNetworkIdentity(item.host, item.adapter.Key, serial)
		newIdentity, newErr := sshprobe.SelectHostWorkloadNetworkIdentity(host, item.adapter.Key, serial)
		if oldErr != nil {
			r.record(hostID, validation.Unverified, validation.ReasonInvalidInput, "The retained inventory does not establish the selected host interface identity.", "Repeat discovery and generation to retain a complete host binding.")
			return inv, &validation.ObservationError{Category: validation.FatalInput, Cause: oldErr}
		}
		if validationHostIdentityConflict(host, item.host, item.adapter, oldIdentity) || newErr == nil && oldIdentity != newIdentity {
			r.record(hostID, validation.Failed, validation.ReasonIdentityDrift, "The host UUID, selected card, PCI function or permanent MAC differs from the retained binding.", "Inspect the host and repeat discovery and generation.")
			fatal = &validation.ObservationError{Category: validation.FatalIdentity}
			continue
		}
		if newErr != nil || host.ID == "" {
			message := "The current host observation does not establish the selected physical interface identity."
			if newErr != nil {
				// This pure selector returns finite diagnostics, never command output.
				message += " " + newErr.Error() + "."
			}
			r.record(hostID, validation.Unverified, validation.ReasonUnavailable, message, "Restore the required host PCI, network and permanent-MAC reads, then validate again.")
			continue
		}
		inv.Hosts = append(inv.Hosts, host)
		for _, dpu := range inv.DPUs {
			if dpu.SerialNumber == serial && dpu.ID != "" {
				inv.Associations = append(inv.Associations, inventory.Association{DPUID: dpu.ID, HostID: host.ID, HostAdapterKey: item.adapter.Key, SerialNumber: serial})
			}
		}
		r.record(hostID, validation.Passed, validation.ReasonNone, "The current host UUID, selected physical p0 and permanent MAC match the retained association.", "")
		if ctx.Err() != nil {
			return inv, ctx.Err()
		}
	}
	hostChecks := discovery.HostHBNChecks(inv, nil)
	for _, device := range inv.DPUs {
		var checks []inventory.Check
		for _, check := range hostChecks {
			if (check.ID == "dpf.hbn.host-pf" || check.ID == "dpf.hbn.host-iommu-group" || check.ID == "dpf.hbn.host-management-physical") && (check.Subject == device.SerialNumber || strings.HasPrefix(check.Subject, device.SerialNumber+" @ ")) {
				checks = append(checks, check)
			}
		}
		recordPhysicalGroup(r, physicalID("host-prerequisites", device.SerialNumber), checks, "Selected PCI functions, IOMMU groups and management physical separation are valid.")
	}
	return inv, fatal
}

// Only positively observed conflicting values establish replacement. Optional
// collector reads can be unavailable even after the physical session succeeds.
func validationDPUIdentityConflict(current inventory.Device, id, serial, psid string) bool {
	if current.ID != "" && current.ID != id || current.SerialNumber != "" && current.SerialNumber != serial || current.PSID != "" && !strings.EqualFold(current.PSID, psid) {
		return true
	}
	observed := current.Observations["psid"]
	return observed.ReasonCode == "PSID_CHANGED" || observed.State == "observed" && len(observed.Values) == 1 && observed.Values[0] != "" && !strings.EqualFold(observed.Values[0], psid)
}

func validationHostIdentityConflict(current, expected inventory.Device, adapter inventory.Adapter, selected sshprobe.HostNetworkIdentity) bool {
	if current.ID != "" && current.ID != expected.ID || current.SystemUUID != "" && !strings.EqualFold(current.SystemUUID, expected.SystemUUID) {
		return true
	}
	for _, observed := range current.Adapters {
		if observed.IdentityState != "known" || observed.SerialNumber == "" || observed.Key == "" {
			continue
		}
		if observed.Key == adapter.Key && observed.SerialNumber != adapter.SerialNumber || observed.SerialNumber == adapter.SerialNumber && observed.Key != adapter.Key {
			return true
		}
	}
	if current.Runtime == nil {
		return false
	}
	if current.Runtime.Observations["host-interface:"+selected.InterfaceName+":permanent-mac"].ReasonCode == "INVENTORY_CHANGED" {
		return true
	}
	if current.Runtime.Network != nil {
		for _, link := range current.Runtime.Network.Links {
			if !strings.EqualFold(link.PCIAddress, selected.PCIAddress) || link.PhysicalPortName != selected.PhysicalPort {
				continue
			}
			// A valid measurement for the selected physical function remains a
			// contradiction even if another link's optional fields are missing.
			mac, err := net.ParseMAC(link.PermanentMAC)
			if err == nil && len(mac) == 6 && mac[0]&1 == 0 && mac.String() != "00:00:00:00:00:00" && mac.String() != selected.PermanentMAC {
				return true
			}
		}
	}
	return false
}

func recordPhysicalCheck(r *validationRecorder, id validation.CheckID, check inventory.Check) {
	r.start(id)
	state, reason, action := validation.Unverified, validation.ReasonIncomplete, "Restore the stated prerequisite and repeat validation."
	switch check.State {
	case "passed":
		state, reason, action = validation.Passed, validation.ReasonNone, ""
	case "failed":
		state, reason = validation.Failed, validation.ReasonHealth
	}
	r.record(id, state, reason, check.Message, action)
}

func recordPhysicalGroup(r *validationRecorder, id validation.CheckID, checks []inventory.Check, success string) {
	r.start(id)
	if len(checks) == 0 {
		r.record(id, validation.Unverified, validation.ReasonDependency, "Current prerequisite observations are unavailable.", "Resolve the physical identity and access checks.")
		return
	}
	var failures []string
	state := validation.Unverified
	for _, check := range checks {
		if check.State != "passed" {
			failures = append(failures, check.Message)
			if check.State == "failed" {
				state = validation.Failed
			}
		}
	}
	if len(failures) == 0 {
		r.record(id, validation.Passed, validation.ReasonNone, success, "")
		return
	}
	r.record(id, state, validation.ReasonHealth, strings.Join(failures, " "), "Resolve the stated host or software prerequisites and validate again.")
}

// Runtime phase boundaries recheck the complete physical cohort. Diagnostic
// measurements fence Kubernetes objects individually; this more expensive pass
// runs at runtime entry and final acceptance only.
func (o *validationObserver) currentPhysicalBoots(ctx context.Context) (map[string]string, error) {
	r := newValidationRecorder(o.physicalValidationChecks(), nil)
	inv, err := o.observePhysicalDevices(ctx, r, false)
	if err != nil {
		return nil, err
	}
	boots := map[string]string{}
	for _, device := range o.desired.plan.Intent().Devices {
		for _, kind := range []string{"dpu", "administrator", "host", "host-prerequisites"} {
			out, ok := r.outcomes[physicalID(kind, device.Serial)]
			if !ok || out.State != validation.Passed {
				category := validation.FatalObservation
				if !ok || out.Reason == validation.ReasonDenied || out.Reason == validation.ReasonUnavailable || out.Reason == validation.ReasonDependency {
					category = validation.FatalAccess
				}
				return nil, &validation.ObservationError{Category: category, Cause: fmt.Errorf("selected physical identity, administrator access or host prerequisites could not be verified")}
			}
		}
		for _, association := range inv.Associations {
			if association.SerialNumber != device.Serial {
				continue
			}
			for _, host := range inv.Hosts {
				if host.ID == association.HostID && host.Runtime != nil {
					boots[device.Serial] = host.Runtime.BootID
				}
			}
		}
		if boots[device.Serial] == "" {
			return nil, &validation.ObservationError{Category: validation.FatalObservation, Cause: fmt.Errorf("current host boot identity is unavailable")}
		}
	}
	return boots, ctx.Err()
}

func (o *validationObserver) openValidationPhysicalFence(ctx context.Context) (func(context.Context) error, error) {
	boots, err := o.currentPhysicalBoots(ctx)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		current, err := o.currentPhysicalBoots(ctx)
		if err != nil {
			return err
		}
		if !maps.Equal(boots, current) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: fmt.Errorf("selected host rebooted during runtime validation")}
		}
		return nil
	}, nil
}
