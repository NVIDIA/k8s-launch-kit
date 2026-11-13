// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// inventoryWire avoids recursively invoking the Inventory codecs. Device itself
// keeps its ordinary encoding for consumers with their own versioned contracts.
type inventoryWire Inventory

func (i Inventory) MarshalJSON() ([]byte, error) {
	wire, err := i.compact()
	if err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

func (i Inventory) MarshalYAML() (any, error) { return i.compact() }

func (i *Inventory) UnmarshalJSON(data []byte) error {
	var wire inventoryWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	result := Inventory(wire)
	if err := NormalizeInventory(&result); err != nil {
		return err
	}
	*i = result
	return nil
}

func (i Inventory) compact() (inventoryWire, error) {
	wire := inventoryWire(i)
	if i.FormatVersion != FormatVersion {
		return wire, nil
	}
	var err error
	wire.Hosts, err = compactDevices(i.Hosts)
	if err != nil {
		return inventoryWire{}, err
	}
	wire.DPUs, err = compactDevices(i.DPUs)
	return wire, err
}

// compactDevices projects diagnostics without changing the retained observations
// used by discovery checks. Typed facts retain successful evidence needed later.
func compactDevices(devices []Device) ([]Device, error) {
	out := slices.Clone(devices)
	for index := range out {
		d := &out[index]
		d.Observations = maps.Clone(d.Observations)
		if err := restoreFacts(d); err != nil {
			return nil, err
		}
		switch d.ObservationState {
		case "observed":
			d.ObservationState = ""
		case "":
			// Absence has a success meaning only in the compact format. An
			// unknown in-memory record must therefore remain explicit.
			d.ObservationState = "unknown"
		}
		for key, observation := range d.Observations {
			if observation.State != "observed" {
				if key == "client-certificate-support" && observation.State == "unknown" && observation.ReasonCode == "NOT_EVALUATED" {
					delete(d.Observations, key)
				}
				continue
			}
			switch key {
			case "psid":
				if len(observation.Values) != 1 || observation.Values[0] == "" || observation.ReasonCode != "PSID_OBSERVED" || observation.Tool != "redfish" {
					return nil, fmt.Errorf("device %s has invalid verified PSID evidence", d.RecordKey)
				}
				d.PSID = observation.Values[0]
			case "bmc-administrator":
				if observation.ReasonCode == "BMC_ADMIN_VERIFIED" {
					d.Authentication.BMCAdministrator = "verified"
				}
			case "firmware-inventory":
				d.FirmwareInventoryComplete = true
			}
			delete(d.Observations, key)
		}
	}
	return out, nil
}

// NormalizeInventory restores the internal observation model from the compact
// v1alpha2 wire contract. YAML callers must invoke it after their strict decode;
// keeping YAML decoding outside this type preserves KnownFields validation.
// Legacy records retain their exact state, including absent/unknown evidence.
func NormalizeInventory(i *Inventory) error {
	if i == nil || i.FormatVersion != FormatVersion {
		return nil
	}
	for _, devices := range [][]Device{i.Hosts, i.DPUs} {
		for index := range devices {
			d := &devices[index]
			if d.ObservationState == "" {
				d.ObservationState = "unknown"
				if d.ID != "" {
					d.ObservationState = "observed"
				}
			}
			if err := restoreFacts(d); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreFacts is shared by reading and writing so contradictory wire facts are
// rejected even when a caller constructs an Inventory directly for encoding.
func restoreFacts(d *Device) error {
	if d.Authentication.BMCAdministrator != "" {
		if d.Authentication.BMCAdministrator != "verified" {
			return fmt.Errorf("device %s has invalid BMC administrator evidence", d.RecordKey)
		}
		if err := restoreObservation(d, "bmc-administrator", Observation{State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}); err != nil {
			return err
		}
		d.Authentication.BMCAdministrator = ""
	}
	if d.PSID != "" {
		if err := restoreObservation(d, "psid", Observation{State: "observed", Values: []string{d.PSID}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"}); err != nil {
			return err
		}
		d.PSID = ""
	}
	if d.FirmwareInventoryComplete {
		if err := restoreObservation(d, "firmware-inventory", Observation{State: "observed"}); err != nil {
			return err
		}
		d.FirmwareInventoryComplete = false
	}
	return nil
}

func restoreObservation(d *Device, key string, fact Observation) error {
	if old, exists := d.Observations[key]; exists {
		if old.State != fact.State || old.ReasonCode != fact.ReasonCode || !slices.Equal(old.Values, fact.Values) || (key == "psid" && old.Tool != fact.Tool) {
			return fmt.Errorf("device %s has conflicting %s evidence", d.RecordKey, key)
		}
		return nil
	}
	if d.Observations == nil {
		d.Observations = map[string]Observation{}
	}
	d.Observations[key] = fact
	return nil
}
