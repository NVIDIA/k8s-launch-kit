// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// missing and unavailable deliberately describe our observation, never a guessed
// device capability. Response bodies and error strings are not diagnostic inputs.
func missing(code, message string) inventory.Observation {
	return inventory.Observation{State: "unknown", ReasonCode: code, Message: message}
}
func unavailable(err error) inventory.Observation {
	reason := Code(err)
	message := "The Redfish observation failed; inspect the reason code."
	switch reason {
	case "ACCESS_DENIED":
		message = "The account is not authorized to read this Redfish resource."
	case "AUTHENTICATION_FAILED":
		message = "The Redfish service rejected authentication for this read."
	case "RESOURCE_NOT_FOUND":
		message = "The advertised Redfish resource was not found."
	case "BMC_TIMEOUT":
		message = "The Redfish read exceeded its timeout."
	case "CANCELLED":
		message = "The Redfish read was cancelled."
	case "TRANSPORT_FAILED":
		message = "The Redfish connection could not complete."
	case "TLS_VERIFICATION_FAILED":
		message = "The Redfish server certificate could not be verified."
	case "INVALID_RESPONSE", "INCOMPLETE_COLLECTION", "RESPONSE_READ_FAILED":
		message = "The Redfish response was malformed or incomplete."
	case "RESOURCE_LIMIT", "RESPONSE_LIMIT", "PAGINATION_LIMIT":
		message = "The Redfish read exceeded its resource, size or pagination limit."
	}
	var remote *Error
	if errors.As(err, &remote) && remote.Status != 0 {
		message += fmt.Sprintf(" HTTP %d.", remote.Status)
	}
	return inventory.Observation{State: "unavailable", ReasonCode: reason, Message: message}
}

func collectSoftware(ctx context.Context, reader Reader, root Document, system Resource, device *inventory.Device) string {
	if device.Observations == nil {
		device.Observations = map[string]inventory.Observation{}
	}
	device.Observations["client-certificate-support"] = missing("NOT_EVALUATED", "Inventory collection does not test DPF client-certificate authentication.")
	manager, managerState := softwareManager(ctx, reader, root)
	device.Observations["managers"] = managerState
	device.Time = inventory.TimeObservation{State: "unknown", ReasonCode: managerState.ReasonCode, Message: managerState.Message}
	if managerState.State == "observed" {
		collectManager(ctx, reader, manager, device)
	} else {
		device.Observations["bmc-time"], device.Observations["bmc-ntp"] = managerState, managerState
	}
	records, state := softwareInventory(ctx, reader, root)
	device.Observations["firmware-inventory"] = state
	for _, resource := range records {
		firmware := firmwareRecord(resource, system.URI, manager.URI, device.BMCEndpoint)
		device.Firmware = append(device.Firmware, firmware)
	}
	for _, component := range []string{"bmc", "cec", "atf", "uefi", "nic", "dpu-os", "dpu-bsp", "golden"} {
		found := false
		for _, f := range device.Firmware {
			found = found || f.Component == component
		}
		if !found {
			reason := state
			if state.State == "observed" {
				reason = missing("COMPONENT_NOT_EXPOSED", "The firmware inventory does not expose this component.")
			}
			device.Firmware = append(device.Firmware, inventory.Firmware{Component: component, State: "unknown", ReasonCode: reason.ReasonCode, Message: reason.Message, Activation: "unknown", ActivationReasonCode: "VERSION_UNAVAILABLE", ActivationMessage: "Activation cannot be established without the component observation."})
		}
	}
	return reconcileFirmware(device)
}

func softwareManager(ctx context.Context, reader Reader, root Document) (Resource, inventory.Observation) {
	uri := Link(root, "Managers")
	if uri == "" {
		return Resource{}, missing("LINK_NOT_EXPOSED", "The service root does not advertise Managers.")
	}
	managers, err := reader.Collection(ctx, uri)
	if err != nil {
		return Resource{}, unavailable(err)
	}
	if len(managers) != 1 {
		return Resource{}, missing("MANAGER_SELECTION_AMBIGUOUS", "A unique BMC manager could not be selected; manager firmware and time remain unknown.")
	}
	return managers[0], inventory.Observation{State: "observed"}
}

func collectManager(ctx context.Context, reader Reader, manager Resource, device *inventory.Device) {
	if version := Text(manager.Document, "FirmwareVersion"); strings.TrimSpace(version) != "" {
		device.Firmware = append(device.Firmware, inventory.Firmware{ResourceURI: manager.URI, Component: "bmc", State: "known", RawVersion: version, Activation: "unknown", ActivationReasonCode: "ACTIVATION_NOT_EXPOSED", ActivationMessage: "Manager firmware metadata does not identify an active software inventory image."})
	}
	datetime := Text(manager.Document, "DateTime")
	state := missing("PROPERTY_NOT_EXPOSED", "The manager does not expose DateTime.")
	if value, exists := manager.Document["DateTime"]; exists && value != nil && datetime == "" {
		state = missing("INVALID_DATETIME", "The manager DateTime is not a nonempty RFC3339 timestamp.")
	}
	if datetime != "" {
		if _, err := time.Parse(time.RFC3339, datetime); err != nil {
			state = missing("INVALID_DATETIME", "The manager DateTime is not a valid RFC3339 timestamp.")
		} else {
			device.Time.DateTime, device.Time.Offset = datetime, Text(manager.Document, "DateTimeLocalOffset")
			state = inventory.Observation{State: "observed"}
		}
	}
	device.Time.State, device.Time.ReasonCode, device.Time.Message = state.State, state.ReasonCode, state.Message
	if state.State == "observed" {
		device.Time.State = "known"
	}
	device.Observations["bmc-time"] = state
	uri := Link(manager.Document, "NetworkProtocol")
	state = missing("LINK_NOT_EXPOSED", "The manager does not advertise NetworkProtocol; NTP configuration is unknown.")
	if uri != "" {
		np, err := reader.Get(ctx, uri)
		if err != nil {
			state = unavailable(err)
		} else {
			ntp := Object(np, "NTP")
			enabled, hasEnabled := ntp["ProtocolEnabled"].(bool)
			servers, hasServers := ntp["NTPServers"].([]any)
			validServers := hasServers
			for _, value := range servers {
				server, ok := value.(string)
				if !ok {
					validServers = false
					break
				}
				device.Time.NTPServers = append(device.Time.NTPServers, server)
			}
			if !validServers {
				device.Time.NTPServers = nil
			}
			if hasEnabled {
				device.Time.NTPEnabled = &enabled
			}
			state = missing("PROPERTY_NOT_EXPOSED", "NTP enablement or server configuration is not exposed.")
			if (ntp["ProtocolEnabled"] != nil && !hasEnabled) || (ntp["NTPServers"] != nil && !validServers) {
				state = missing("PROPERTY_INVALID", "The NTP enablement or server configuration has an invalid type.")
			}
			if hasEnabled && validServers {
				state = inventory.Observation{State: "observed"}
			}
		}
	}
	device.Observations["bmc-ntp"] = state
}

func softwareInventory(ctx context.Context, reader Reader, root Document) ([]Resource, inventory.Observation) {
	uri := Link(root, "UpdateService")
	if uri == "" {
		return nil, missing("LINK_NOT_EXPOSED", "The service root does not advertise UpdateService.")
	}
	update, err := reader.Get(ctx, uri)
	if err != nil {
		return nil, unavailable(err)
	}
	uri = Link(update, "FirmwareInventory")
	if uri == "" {
		return nil, missing("LINK_NOT_EXPOSED", "UpdateService does not advertise FirmwareInventory.")
	}
	records, err := reader.Collection(ctx, uri)
	if err != nil {
		return records, unavailable(err)
	}
	return records, inventory.Observation{State: "observed"}
}

func componentName(id, name string) string {
	normalize := func(s string) string {
		return strings.Trim(strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return '-'
		}, strings.ToLower(s)), "-")
	}
	// Prefer a recognizable stable ID, then the display name; golden must precede
	// component aliases because a golden BMC image is not the running BMC image.
	// Keep explicitly identified BMC and CPU ERoTs separate from each other
	// and from BMC firmware. An ERoT name alone does not identify CEC firmware.
	for _, raw := range []string{id, name} {
		s := normalize(raw)
		words := "-" + s + "-"
		switch {
		case strings.Contains(words, "-golden-"):
			return "golden"
		case strings.Contains(words, "-erot-") && strings.Contains(words, "-bmc-") && !strings.Contains(words, "-cpu-"):
			return "bmc-erot"
		case strings.Contains(words, "-erot-") && strings.Contains(words, "-cpu-") && !strings.Contains(words, "-bmc-"):
			return "cpu-erot"
		case strings.Contains(words, "-erot-"):
			return "erot"
		case strings.Contains(words, "-bmc-"):
			return "bmc"
		case strings.Contains(words, "-cec-"):
			return "cec"
		case strings.Contains(words, "-atf-") && strings.Contains(words, "-uefi-"):
			return "atf-uefi"
		case strings.Contains(words, "-atf-"):
			return "atf"
		case strings.Contains(words, "-uefi-"):
			return "uefi"
		case strings.Contains(words, "-nic-"):
			return "nic"
		case s == "dpu-os" || s == "dpu-operating-system":
			return "dpu-os"
		case s == "dpu-bsp" || s == "bsp":
			return "dpu-bsp"
		}
	}
	return "other"
}

func firmwareRecord(resource Resource, system, manager, origin string) inventory.Firmware {
	f := inventory.Firmware{ResourceURI: resource.URI, RawID: Text(resource.Document, "Id"), RawName: Text(resource.Document, "Name"), RawVersion: Text(resource.Document, "Version"), State: "known", Activation: "unknown", ActivationReasonCode: "ACTIVATION_NOT_EXPOSED", ActivationMessage: "No explicit active or staged target link identifies the selected device."}
	f.Component = componentName(f.RawID, f.RawName)
	if strings.TrimSpace(f.RawVersion) == "" {
		f.State, f.ReasonCode, f.Message = "unknown", "VERSION_NOT_EXPOSED", "This firmware entry does not expose a nonempty Version."
		if value := resource.Document["Version"]; value != nil {
			if _, ok := value.(string); !ok {
				f.ReasonCode, f.Message = "VERSION_INVALID", "The firmware Version property is not a string."
			}
		}
	} else if f.Component == "other" {
		f.ReasonCode, f.Message = "COMPONENT_UNRECOGNIZED", "The raw firmware identity is retained; its component is not recognized."
	}
	// DMTF SoftwareInventory v1.12 Links.ActiveTargets/StagedTargets describe
	// actual activation, unlike Updateable, Status.Enabled, or an image's name.
	links := Object(resource.Document, "Links")
	// Reuse the transport's origin/path rules without performing a request.
	// Target identity links cannot carry even the pagination queries accepted
	// by the general reader. Normalize trailing slash only after validation.
	validator := Client{origin: origin}
	normalize := func(ref string) (string, error) {
		uri, err := validator.guard(ref)
		if err != nil {
			return "", err
		}
		if strings.Contains(uri, "?") {
			return "", &Error{Code: "UNSAFE_QUERY"}
		}
		return strings.TrimSuffix(uri, "/"), nil
	}
	selected := map[string]bool{}
	for _, ref := range []string{system, manager} {
		if ref == "" {
			continue
		}
		uri, err := normalize(ref)
		if err != nil {
			f.ActivationReasonCode, f.ActivationMessage = Code(err), "The selected resource link is invalid; firmware activation cannot be established."
			return f
		}
		selected[uri] = true
	}
	matches := func(key string) (bool, error) {
		value, exists := links[key]
		if !exists || value == nil {
			return false, nil
		}
		targets, ok := value.([]any)
		if !ok {
			return false, &Error{Code: "ACTIVATION_LINK_INVALID"}
		}
		found := false
		for _, target := range targets {
			uri, err := normalize(Link(Document{"target": target}, "target"))
			if err != nil {
				return false, err
			}
			found = found || selected[uri]
		}
		return found, nil
	}
	active, activeErr := matches("ActiveTargets")
	staged, stagedErr := matches("StagedTargets")
	if err := errors.Join(activeErr, stagedErr); err != nil {
		f.ActivationReasonCode, f.ActivationMessage = Code(err), "ActiveTargets or StagedTargets contains an invalid or unsafe resource link; activation remains unknown."
		return f
	}
	if active != staged {
		f.Activation = "running"
		if staged {
			f.Activation = "staged"
		}
		f.ActivationReasonCode, f.ActivationMessage = "", ""
	} else if active {
		f.ActivationReasonCode, f.ActivationMessage = "ACTIVATION_CONFLICT", "The selected target is reported as both active and staged."
	}
	return f
}

func reconcileFirmware(device *inventory.Device) string {
	versions := map[string]map[string]bool{}
	for _, f := range device.Firmware {
		// Generic identities cannot establish that entries describe one component.
		if f.State != "known" || f.Activation == "staged" || f.Component == "other" || f.Component == "erot" {
			continue
		}
		if versions[f.Component] == nil {
			versions[f.Component] = map[string]bool{}
		}
		versions[f.Component][strings.TrimSpace(f.RawVersion)] = true
	}
	bmc := ""
	for i := range device.Firmware {
		f := &device.Firmware[i]
		if len(versions[f.Component]) > 1 && f.Activation != "staged" {
			f.State, f.ReasonCode, f.Message = "conflict", "FIRMWARE_VERSION_CONFLICT", "Sources report different versions for this component without an explicit staged-image distinction."
			device.Observations["firmware."+f.Component] = missing(f.ReasonCode, f.Message)
		}
		if f.Component == "bmc" && f.State == "known" && f.Activation != "staged" {
			bmc = f.RawVersion
		}
	}
	return bmc
}
