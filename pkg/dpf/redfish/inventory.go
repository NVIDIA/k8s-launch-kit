// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Qualification is supplied by the installed, reviewed hardware catalog, never site YAML.
// An empty catalog collects candidate facts while keeping completeness unqualified.
type Qualification struct {
	VerifyCompleteness func(context.Context, Reader, Resource) error
	Adapter            string
	Model              string
	BMCFirmware        string
}
type Collector struct{ Qualifications []Qualification }

func ValidSerial(raw string) bool {
	s := strings.TrimSpace(raw)
	switch strings.ToLower(s) {
	case "", "unknown", "n/a", "none", "to be filled by o.e.m.", "default string":
		return false
	}
	return strings.Trim(s, "0- ") != ""
}
func (co Collector) Collect(ctx context.Context, target config.Target, timeout time.Duration) (device inventory.Device, evidence map[string]inventory.Evidence, err error) {
	started := time.Now()
	label := target.DisplayName()
	logger := log.FromContext(ctx).WithName("redfish").WithValues("endpoint", label, "role", target.Role)
	ctx = log.IntoContext(ctx, logger)
	output := ui.FromContext(ctx)
	stage := ""
	begin := func(name string) {
		stage = name
		output.Info("[RUNNING] %s: %s", label, stage)
		logger.V(1).Info("Redfish stage started", "stage", stage)
	}
	pass := func(detail string) {
		output.Info("[PASS] %s: %s — %s", label, stage, detail)
		stage = ""
	}
	device = inventory.Device{RecordKey: inventory.Digest("dpf-access-v1", []string{target.Role, target.Endpoint, target.SystemURI}), InputRef: target.InputRef, BMCEndpoint: target.Endpoint, ObservationState: "unavailable", Qualification: "unqualified", InventoryCompleteness: "unknown", Authentication: inventory.Authentication{PasswordRead: "unknown", ClientCertificateSupport: "unknown", DPFClientAuthentication: "not-evaluated"}, Time: inventory.TimeObservation{State: "unknown", ReasonCode: "PROPERTY_NOT_EXPOSED"}, Firmware: []inventory.Firmware{}, EvidenceRefs: []string{}}
	evidence = map[string]inventory.Evidence{}
	expectedSerials := map[string]string{}
	bf4Identities := map[string]Document{}
	var psidCard Resource
	var currentPSIDCard Document
	c := New(target, timeout)
	defer func() {
		if stage != "" {
			output.Info("[FAIL] %s: %s (%s)", label, stage, Code(err))
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if cleanupErr := c.Close(cleanupCtx); cleanupErr != nil {
			err = &ObservationError{Primary: err, Cleanup: cleanupErr, SessionURI: c.session}
			device.ReasonCode = "CLEANUP_INCOMPLETE"
		}
		if err != nil && psidCard.URI != "" {
			state := device.Observations["psid"]
			if state.State == "observed" || state.ReasonCode == "PSID_NOT_VERIFIED" {
				device.Observations["psid"] = psidUnavailable("PSID_RECHECK_FAILED", "A required identity, source or cleanup recheck failed; the Redfish PSID is ineligible.")
			}
		}
		if err != nil && device.ReasonCode == "" {
			device.ReasonCode = Code(err)
		}
		logger.V(1).Info("Redfish collection completed", "identityState", device.ObservationState, "identityEstablished", device.ID != "", "reasonCode", device.ReasonCode, "duration", time.Since(started))
	}()
	begin("Redfish connection/session")
	root, err := c.Open(ctx)
	if err != nil {
		device.ReasonCode = Code(err)
		if target.Role == "dpus" {
			state := unavailable(err)
			if Code(err) == "PASSWORD_CHANGE_REQUIRED" {
				state.Message = "The BMC requires a password change; discovery does not change passwords."
			}
			device.Observations = map[string]inventory.Observation{"bmc-administrator": state}
		}
		return device, evidence, err
	}
	pass("session established")
	device.Authentication.Method = "session"
	add := func(uri, property string) string {
		id := inventory.Digest("dpf-evidence-v1", []string{device.RecordKey, uri, property})
		evidence[id] = inventory.Evidence{Kind: "redfish", InputRef: target.InputRef, ResourceURI: uri, Property: property, ObservedAt: time.Now().UTC(), Vantage: "executor", Adapter: device.ReaderAdapter, Authenticated: true}
		device.EvidenceRefs = append(device.EvidenceRefs, id)
		return id
	}
	if target.Role == "dpus" {
		begin("DPU BMC administrator privileges")
		admin := c.administrator(ctx, root, add)
		device.Observations = map[string]inventory.Observation{"bmc-administrator": admin}
		logger.V(1).Info("BMC administrator verification completed", "state", admin.State, "reasonCode", admin.ReasonCode)
		if admin.State == "observed" && admin.ReasonCode == "BMC_ADMIN_VERIFIED" {
			pass("verified")
		} else {
			output.Info("[FAIL] %s: %s (%s)", label, stage, admin.ReasonCode)
			stage = ""
		}
	}
	begin("Physical inventory")
	systems, err := c.Collection(ctx, Link(root, "Systems"))
	if err != nil {
		return device, evidence, err
	}
	device.Authentication.PasswordRead = "verified"
	var system Resource
	if target.SystemURI != "" {
		for _, s := range systems {
			if s.URI == target.SystemURI {
				system = s
			}
		}
	} else if len(systems) == 1 {
		system = systems[0]
	}
	if system.URI == "" {
		device.ObservationState = "ambiguous"
		return device, evidence, &Error{Code: "SYSTEM_SELECTION_REQUIRED"}
	}
	device.SystemURI = system.URI
	device.Manufacturer = Text(system.Document, "Manufacturer")
	device.Model = Text(system.Document, "Model")
	device.SystemUUID = Text(system.Document, "UUID")
	bf4 := target.Role == "dpus" && b4240Service(root, nil)
	if target.Role == "dpus" {
		device.Observations["psid"] = inventory.Observation{State: "unknown", ReasonCode: "PSID_READER_UNSUPPORTED", Message: "This device is outside the supported BF4 AssetTag PSID reader.", Tool: "redfish"}
	}
	if bf4 && !b4240Service(root, system.Document) {
		return device, evidence, &Error{Code: "DPU_IDENTITY_CONFLICT"}
	}
	adapter := ""
	maker := strings.ToLower(device.Manufacturer)
	switch {
	case bf4:
		adapter = "bluefield-card-v1"
	case strings.Contains(maker, "nvidia") || strings.Contains(maker, "mellanox"):
		adapter = "bluefield-card-v1"
	case strings.Contains(maker, "hpe") || strings.Contains(maker, "hewlett"):
		adapter = "hpe-standard-v1"
	case strings.Contains(maker, "dell"):
		adapter = "dell-standard-v1"
	}
	device.ReaderAdapter = adapter
	action := Object(Object(system.Document, "Actions"), "#ComputerSystem.Reset")
	if values, ok := action["ResetType@Redfish.AllowableValues"].([]any); ok {
		for _, v := range values {
			if value, ok := v.(string); ok {
				device.AdvertisedResetTypes = append(device.AdvertisedResetTypes, value)
			}
		}
	}
	add(system.URI, "UUID")
	if target.Role == "dpus" {
		if adapter != "bluefield-card-v1" {
			return device, evidence, &Error{Code: "UNSUPPORTED_DPU_IDENTITY"}
		}
		chassis, er := c.Collection(ctx, Link(root, "Chassis"))
		if er != nil {
			return device, evidence, er
		}
		var cards []Resource
		if bf4 {
			var witnesses []Resource
			cards, witnesses = c.b4240Cards(system, chassis)
			for _, resources := range [][]Resource{cards, witnesses} {
				for _, resource := range resources {
					bf4Identities[resource.URI] = cardIdentity(resource.Document)
				}
			}
		} else {
			for _, ch := range chassis {
				model := Text(ch.Document, "Model")
				if strings.Contains(strings.ToLower(model), "bluefield") && ValidSerial(Text(ch.Document, "SerialNumber")) && c.linkedToSystem(ch.Document, system.URI) {
					cards = append(cards, ch)
				}
			}
		}
		if len(cards) != 1 {
			return device, evidence, &Error{Code: "CARD_IDENTITY_AMBIGUOUS"}
		}
		if bf4 {
			psidCard = cards[0]
			device.Observations["psid"] = inventory.Observation{State: "unknown", ReasonCode: "PSID_NOT_VERIFIED", Message: "Redfish PSID eligibility awaits successful identity and source rechecks.", Tool: "redfish"}
		}
		device.SerialNumberRaw = Text(cards[0].Document, "SerialNumber")
		if !bf4 {
			expectedSerials[cards[0].URI] = device.SerialNumberRaw
		}
		device.SerialNumber = strings.TrimSpace(device.SerialNumberRaw)
		device.Model = Text(cards[0].Document, "Model")
		device.IdentityKey = []string{"nvidia", device.SerialNumber}
		device.ID = inventory.PhysicalID("dpu", device.IdentityKey)
		add(cards[0].URI, "SerialNumber")
	} else {
		if adapter != "hpe-standard-v1" && adapter != "dell-standard-v1" {
			return device, evidence, &Error{Code: "UNSUPPORTED_HOST_ADAPTER"}
		}
		if !validHostUUID(device.SystemUUID) {
			return device, evidence, &Error{Code: "HOST_IDENTITY_MISSING"}
		}
		device.IdentityKey = []string{"system-uuid", strings.ToLower(device.SystemUUID)}
		device.ID = inventory.PhysicalID("host", device.IdentityKey)
		device.Adapters, err = collectHostAdapters(ctx, c, system, device.ID, adapter == "hpe-standard-v1", add)
		if err != nil {
			return device, evidence, err
		}
		device.InventoryCompleteness = "unqualified"
	}
	output.Info("[RUNNING] %s: Physical inventory read completed; identity recheck pending", label)
	stage = ""
	begin("Optional BMC firmware/time/settings observations")
	bmcFirmware := collectSoftware(ctx, c, root, system, &device, add)
	for _, q := range co.Qualifications {
		if q.Adapter == adapter && q.Model == device.Model && q.BMCFirmware == bmcFirmware && bmcFirmware != "" && q.VerifyCompleteness != nil && q.VerifyCompleteness(ctx, c, system) == nil {
			device.Qualification = "qualified"
			device.InventoryCompleteness = "complete"
		}
	}
	observed, unavailableCount := 0, 0
	for _, key := range []string{"managers", "firmware-inventory", "bmc-time", "bmc-ntp"} {
		if device.Observations[key].State == "observed" {
			observed++
		} else {
			unavailableCount++
		}
	}
	for _, firmware := range device.Firmware {
		if firmware.State == "known" {
			observed++
		} else {
			unavailableCount++
		}
	}
	if unavailableCount > 0 {
		output.Info("[UNAVAILABLE] %s: Optional BMC firmware/time/settings reads completed; %d observed, %d unavailable or unknown; inspect the detailed report for reasons, compatibility not evaluated", label, observed, unavailableCount)
		stage = ""
	} else {
		pass("reads completed; compatibility not evaluated")
	}
	logger.V(1).Info("Optional Redfish observations completed", "observedSections", observed, "unavailableObservations", unavailableCount)
	begin("Physical identity recheck")
	for uri, identity := range bf4Identities {
		current, e := c.Get(ctx, uri)
		if e != nil {
			return device, evidence, e
		}
		if !reflect.DeepEqual(cardIdentity(current), identity) {
			return device, evidence, &Error{Code: "IDENTITY_CHANGED", URI: uri}
		}
		if uri == psidCard.URI {
			currentPSIDCard = current
		}
		add(uri, "Model,Manufacturer,SerialNumber,ChassisType,Links")
	}
	for uri, raw := range expectedSerials {
		current, e := c.Get(ctx, uri)
		if e != nil {
			return device, evidence, e
		}
		if Text(current, "SerialNumber") != raw {
			return device, evidence, &Error{Code: "IDENTITY_CHANGED", URI: uri}
		}
	}
	refreshed, er := c.Get(ctx, system.URI)
	if er != nil {
		return device, evidence, er
	}
	if Text(refreshed, "UUID") != device.SystemUUID || Text(refreshed, "SerialNumber") != Text(system.Document, "SerialNumber") || len(bf4Identities) > 0 && !reflect.DeepEqual(cardIdentity(refreshed), cardIdentity(system.Document)) {
		return device, evidence, &Error{Code: "IDENTITY_CHANGED", URI: system.URI}
	}
	if bf4 {
		device.Observations["psid"] = bf4PSID(system, psidCard, refreshed, currentPSIDCard, add)
	}
	if target.Role == "dpus" && c.passwordChangeRequired {
		device.Observations["bmc-administrator"] = inventory.Observation{State: "failed", ReasonCode: "BMC_PASSWORD_CHANGE_REQUIRED", Message: "The BMC requires a password change; discovery does not change passwords.", Tool: "redfish"}
		output.Info("[FAIL] %s: DPU BMC administrator privileges (BMC_PASSWORD_CHANGE_REQUIRED)", label)
	}
	device.ObservationState = "observed"
	pass("verified; session cleanup pending")
	return device, evidence, nil
}

func sameMembers(a, b []Resource) bool {
	keys := func(r []Resource) []string {
		s := make([]string, 0, len(r))
		for _, v := range r {
			s = append(s, v.URI)
		}
		slices.Sort(s)
		return s
	}
	return reflect.DeepEqual(keys(a), keys(b))
}

func (c *Client) linkedToSystem(card Document, system string) bool {
	links := Object(card, "Links")
	if values, ok := links["ComputerSystems"].([]any); ok {
		for _, value := range values {
			uri, err := c.guard(Link(Document{"v": value}, "v"))
			if err == nil && uri == system {
				return true
			}
		}
	}
	return false
}

var hostUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validHostUUID(s string) bool {
	return hostUUIDPattern.MatchString(s) && strings.Trim(s, "0-") != "" && strings.Trim(strings.ToLower(s), "f-") != ""
}
