// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
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
	device = inventory.Device{RecordKey: inventory.Digest("dpf-access-v1", []string{target.Role, target.Endpoint, target.SystemURI}), InputRef: target.InputRef, BMCEndpoint: target.Endpoint, ObservationState: "unavailable", Qualification: "unqualified", InventoryCompleteness: "unknown", Authentication: inventory.Authentication{PasswordRead: "unknown", ClientCertificateSupport: "unknown", DPFClientAuthentication: "not-evaluated"}, Time: inventory.TimeObservation{State: "unknown", ReasonCode: "PROPERTY_NOT_EXPOSED"}, Firmware: []inventory.Firmware{}, EvidenceRefs: []string{}}
	evidence = map[string]inventory.Evidence{}
	expectedSerials := map[string]string{}
	c := New(target, timeout)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if cleanupErr := c.Close(cleanupCtx); cleanupErr != nil {
			if err == nil {
				err = cleanupErr
			} else {
				err = fmt.Errorf("%w; BMC session cleanup incomplete", err)
			}
			device.ReasonCode = "CLEANUP_INCOMPLETE"
		}
	}()
	root, err := c.Open(ctx)
	if err != nil {
		device.ReasonCode = Code(err)
		return device, evidence, err
	}
	device.Authentication.PasswordRead = "verified"
	device.Authentication.Method = "session"
	add := func(uri, property string) string {
		id := inventory.Digest("dpf-evidence-v1", []string{device.RecordKey, uri, property})
		evidence[id] = inventory.Evidence{Kind: "redfish", InputRef: target.InputRef, ResourceURI: uri, Property: property, ObservedAt: time.Now().UTC(), Vantage: "executor", Adapter: device.ReaderAdapter, Authenticated: true}
		device.EvidenceRefs = append(device.EvidenceRefs, id)
		return id
	}
	systems, err := c.Collection(ctx, Link(root, "Systems"))
	if err != nil {
		return device, evidence, err
	}
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
	adapter := ""
	maker := strings.ToLower(device.Manufacturer)
	switch {
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
		for _, ch := range chassis {
			model := Text(ch.Document, "Model")
			if strings.Contains(strings.ToLower(model), "bluefield") && ValidSerial(Text(ch.Document, "SerialNumber")) {
				if c.linkedToSystem(ch.Document, system.URI) {
					cards = append(cards, ch)
				}
			}
		}
		if len(cards) != 1 {
			return device, evidence, &Error{Code: "CARD_IDENTITY_AMBIGUOUS"}
		}
		device.SerialNumberRaw = Text(cards[0].Document, "SerialNumber")
		expectedSerials[cards[0].URI] = device.SerialNumberRaw
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
		// NetworkInterfaces link the selected System to physical NetworkAdapters.
		interfaces, er := c.Collection(ctx, Link(system.Document, "NetworkInterfaces"))
		if er != nil {
			return device, evidence, er
		}
		seen := map[string]bool{}
		for _, iface := range interfaces {
			uri := Link(Object(iface.Document, "Links"), "NetworkAdapter")
			if uri == "" {
				return device, evidence, &Error{Code: "ADAPTER_LINK_MISSING"}
			}
			uri, er = c.guard(uri)
			if er != nil {
				return device, evidence, er
			}
			if seen[uri] {
				continue
			}
			seen[uri] = true
			card, er := c.Get(ctx, uri)
			if er != nil {
				return device, evidence, er
			}
			raw := Text(card, "SerialNumber")
			expectedSerials[uri] = raw
			if !ValidSerial(raw) {
				return device, evidence, &Error{Code: "CARD_SERIAL_MISSING"}
			}
			// Retain every physical network card; PCI/vendor omission cannot prove absence.
			serial := strings.TrimSpace(raw)
			device.Adapters = append(device.Adapters, inventory.Adapter{Key: inventory.Digest("dpf-adapter-v1", []string{device.ID, serial}), SerialNumber: serial, SerialNumberRaw: raw, Model: Text(card, "Model"), Selection: "not-selected", EvidenceRefs: []string{add(uri, "SerialNumber")}})
		}
		again, er := c.Collection(ctx, Link(system.Document, "NetworkInterfaces"))
		if er != nil || !sameMembers(interfaces, again) {
			return device, evidence, &Error{Code: "INVENTORY_CHANGED"}
		}
		device.InventoryCompleteness = "unqualified"
	}
	bmcFirmware := ""
	if uri := Link(root, "Managers"); uri != "" {
		managers, er := c.Collection(ctx, uri)
		if er == nil && len(managers) == 1 {
			manager := managers[0]
			bmcFirmware = Text(manager.Document, "FirmwareVersion")
			if bmcFirmware != "" {
				device.Firmware = append(device.Firmware, inventory.Firmware{ResourceURI: manager.URI, Component: "bmc", State: "known", RawVersion: bmcFirmware, Activation: "unknown", EvidenceRefs: []string{add(manager.URI, "FirmwareVersion")}})
			}
			if npURI := Link(manager.Document, "NetworkProtocol"); npURI != "" {
				if np, readErr := c.Get(ctx, npURI); readErr == nil {
					ntp := Object(np, "NTP")
					if enabled, ok := ntp["ProtocolEnabled"].(bool); ok {
						device.Time.NTPEnabled = &enabled
					}
					if servers, ok := ntp["NTPServers"].([]any); ok {
						for _, v := range servers {
							if server, ok := v.(string); ok {
								device.Time.NTPServers = append(device.Time.NTPServers, server)
							}
						}
					}
					add(npURI, "NTP")
				}
			}

			device.Time.DateTime = Text(manager.Document, "DateTime")
			device.Time.Offset = Text(manager.Document, "DateTimeLocalOffset")
			if device.Time.DateTime != "" {
				device.Time.State = "known"
				device.Time.ReasonCode = ""
			}
			add(manager.URI, "DateTime")
		}
	}
	found := map[string]bool{}
	for _, existing := range device.Firmware {
		found[existing.Component] = true
	}
	if uri := Link(root, "UpdateService"); uri != "" {
		update, er := c.Get(ctx, uri)
		if er == nil {
			fw, er := c.Collection(ctx, Link(update, "FirmwareInventory"))
			if er == nil {
				for _, f := range fw {
					name := strings.ToLower(Text(f.Document, "Name"))
					component := "other"
					for _, candidate := range []string{"bmc", "cec", "atf", "nic", "golden"} {
						if strings.Contains(name, candidate) {
							component = candidate
							break
						}
					}
					if component == "atf" {
						component = "atf-uefi"
					}
					version := Text(f.Document, "Version")
					if version == "" {
						continue
					}
					found[component] = true
					device.Firmware = append(device.Firmware, inventory.Firmware{ResourceURI: f.URI, Component: component, State: "known", RawVersion: version, Activation: "unknown", EvidenceRefs: []string{add(f.URI, "Version")}})
				}
			}
		}
	}
	for _, component := range []string{"bmc", "cec", "atf-uefi", "nic", "dpu-os", "golden"} {
		if !found[component] {
			device.Firmware = append(device.Firmware, inventory.Firmware{Component: component, State: "unknown", ReasonCode: "PROPERTY_NOT_EXPOSED", Activation: "unknown", EvidenceRefs: []string{}})
		}
	}
	for _, q := range co.Qualifications {
		if q.Adapter == adapter && q.Model == device.Model && q.BMCFirmware == bmcFirmware && bmcFirmware != "" && q.VerifyCompleteness != nil && q.VerifyCompleteness(ctx, c, system) == nil {
			device.Qualification = "qualified"
			device.InventoryCompleteness = "complete"
		}
	}
	for uri, raw := range expectedSerials {
		current, e := c.Get(ctx, uri)
		if e != nil || Text(current, "SerialNumber") != raw {
			return device, evidence, &Error{Code: "IDENTITY_CHANGED"}
		}
	}
	refreshed, er := c.Get(ctx, system.URI)
	if er != nil || Text(refreshed, "UUID") != device.SystemUUID || Text(refreshed, "SerialNumber") != Text(system.Document, "SerialNumber") {
		return device, evidence, &Error{Code: "IDENTITY_CHANGED"}
	}
	device.ObservationState = "observed"
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
