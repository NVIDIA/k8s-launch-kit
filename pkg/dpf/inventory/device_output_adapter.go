// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// DeviceToOutput is the explicit boundary between collector observations and
// public facts. It does not mutate the collector's retained result.
func DeviceToOutput(d Device, role string) (DeviceOutput, error) {
	role, err := deviceRole(role)
	if err != nil {
		return DeviceOutput{}, err
	}
	w := DeviceOutput{RecordKey: d.RecordKey, InputRef: d.InputRef, BMCEndpoint: d.BMCEndpoint, SSHEndpoint: d.SSHEndpoint,
		SystemURI: d.SystemURI, SerialNumber: d.SerialNumber, SystemUUID: d.SystemUUID, Manufacturer: d.Manufacturer,
		Model: d.Model, AdvertisedResetTypes: slices.Clone(d.AdvertisedResetTypes)}
	switch {
	case d.SSHEndpoint != "":
		w.Method = "ssh"
	case d.BMCEndpoint != "":
		w.Method = "redfish"
	case d.ReaderAdapter == "host-card-v1":
		w.Method = "host-inventory"
	}
	if d.ObservationState == "observed" && d.ReasonCode == "" {
		w.ID = d.ID
	}
	if w.ID == "" {
		code := d.ReasonCode
		if code == "" {
			code = "PHYSICAL_IDENTITY_UNAVAILABLE"
		}
		w.Issues = append(w.Issues, DeviceIssue{Field: "identity", Code: code, Message: d.Message})
	}
	if w.ID != "" && role == "dpus" && w.Method == "redfish" && w.Model == "B4240" && d.ReaderAdapter != "bluefield-card-v1" {
		return DeviceOutput{}, fmt.Errorf("device %s B4240 identity is outside the supported Redfish reader", d.RecordKey)
	}
	for _, key := range sortedObservationKeys(d.Observations) {
		o := d.Observations[key]
		switch key {
		case "psid":
			if o.State == "observed" {
				if o.ReasonCode != "PSID_OBSERVED" || o.Tool != "redfish" {
					return DeviceOutput{}, fmt.Errorf("device %s has inconsistent Redfish PSID evidence", d.RecordKey)
				}
				value, e := observedValue(o, "psid")
				if e != nil {
					return DeviceOutput{}, e
				}
				w.PSID = value
			} else {
				addIssue(&w.Issues, "psid", o)
			}
		case "bmc-administrator":
			w.BMCAdministratorVerified = o.State == "observed" && o.ReasonCode == "BMC_ADMIN_VERIFIED"
			if !w.BMCAdministratorVerified {
				addIssue(&w.Issues, "bmcAdministratorVerified", o)
			}
		case "firmware-inventory":
			if role == "dpus" {
				w.FirmwareInventoryComplete = o.State == "observed"
				addIssue(&w.Issues, "firmwareInventoryComplete", o)
			}
		case "managers", "bmc-time", "bmc-ntp", "client-certificate-support":
			// These reads have no public fact consumer.
		default:
			if !strings.HasPrefix(key, "firmware.") {
				addIssue(&w.Issues, key, o)
			}
		}
	}
	if d.PSID != "" {
		if w.PSID != "" && w.PSID != d.PSID || hasIssue(w.Issues, "psid") {
			return DeviceOutput{}, fmt.Errorf("device %s has conflicting PSID evidence", d.RecordKey)
		}
		w.PSID = d.PSID
	}
	if d.Authentication.BMCAdministrator != "" {
		if d.Authentication.BMCAdministrator != "verified" || hasIssue(w.Issues, "bmcAdministratorVerified") {
			return DeviceOutput{}, fmt.Errorf("device %s has conflicting BMC administrator evidence", d.RecordKey)
		}
		w.BMCAdministratorVerified = true
	}
	if d.FirmwareInventoryComplete {
		if hasIssue(w.Issues, "firmwareInventoryComplete") {
			return DeviceOutput{}, fmt.Errorf("device %s has conflicting firmware inventory completeness", d.RecordKey)
		}
		w.FirmwareInventoryComplete = role == "dpus"
	}
	// Positive source claims cannot outlive the identity/source recheck.
	if w.ID == "" {
		w.PSID, w.BMCAdministratorVerified, w.FirmwareInventoryComplete = "", false, false
	}
	if w.PSID != "" && (d.ReaderAdapter != "bluefield-card-v1" || w.Model != "B4240") {
		return DeviceOutput{}, fmt.Errorf("device %s PSID lacks supported identity-bound Redfish evidence", d.RecordKey)
	}
	if role == "dpus" {
		w.Firmware, err = firmwareToOutput(d.Firmware, d.Observations, &w.Issues)
		if err != nil {
			return DeviceOutput{}, err
		}
	}
	if d.Runtime != nil {
		w.Runtime, err = runtimeToOutput(d.Runtime, role, &w.Issues)
		if err != nil {
			return DeviceOutput{}, err
		}
	}
	for i, a := range d.Adapters {
		serial := a.SerialNumber
		if serial == "" && a.IdentityState != "known" {
			serial = a.SerialNumberRaw
		}
		w.Adapters = append(w.Adapters, AdapterOutput{Key: a.Key, SerialNumber: serial, Model: a.Model, Slot: a.Slot,
			PCIAddress: a.PCIAddress, VendorID: a.VendorID, DeviceID: a.DeviceID, PCIFunctions: cloneFunctions(a.PCIFunctions)})
		if a.IdentityState != "known" {
			code := a.ReasonCode
			if code == "" {
				code = "CARD_SERIAL_UNVERIFIED"
			}
			w.Issues = append(w.Issues, DeviceIssue{Field: fmt.Sprintf("adapters[%d].identity", i), Code: code})
		}
	}
	canonicalIssues(w.Issues)
	if _, err = DeviceFromOutput(w, role); err != nil {
		return DeviceOutput{}, err
	}
	return w, nil
}

// DeviceFromOutput restores private facts under the v3 contract. Legacy objects
// must first pass their own versioned decoder; absence never upgrades them here.
func DeviceFromOutput(w DeviceOutput, role string) (Device, error) {
	role, err := deviceRole(role)
	if err != nil {
		return Device{}, err
	}
	if err = validateDeviceOutput(w, role); err != nil {
		return Device{}, err
	}
	d := Device{RecordKey: w.RecordKey, InputRef: w.InputRef, ID: w.ID, BMCEndpoint: w.BMCEndpoint, SSHEndpoint: w.SSHEndpoint,
		SystemURI: w.SystemURI, SerialNumber: w.SerialNumber, SerialNumberRaw: w.SerialNumber, SystemUUID: w.SystemUUID,
		Manufacturer: w.Manufacturer, Model: w.Model, AdvertisedResetTypes: slices.Clone(w.AdvertisedResetTypes),
		ObservationState: "unknown", Observations: map[string]Observation{}}
	if w.ID != "" {
		d.ObservationState = "observed"
		if role == "hosts" && w.SystemUUID != "" {
			d.IdentityKey = []string{"system-uuid", strings.ToLower(w.SystemUUID)}
		} else if role == "dpus" && w.SerialNumber != "" {
			d.IdentityKey = []string{"nvidia", w.SerialNumber}
		}
	}
	switch w.Method {
	case "ssh":
		d.ReaderAdapter = "linux-host-ssh-v1"
		if role == "dpus" {
			d.ReaderAdapter = "bluefield-arm-ssh-v1"
		}
	case "redfish":
		if role == "dpus" {
			d.ReaderAdapter = "bluefield-card-v1"
		}
	case "host-inventory":
		d.ReaderAdapter = "host-card-v1"
	}
	if w.PSID != "" {
		d.Observations["psid"] = Observation{State: "observed", Values: []string{w.PSID}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"}
	}
	if w.BMCAdministratorVerified {
		d.Observations["bmc-administrator"] = Observation{State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Tool: "redfish"}
	}
	if w.FirmwareInventoryComplete {
		d.Observations["firmware-inventory"] = Observation{State: "observed"}
	}
	d.Firmware, err = firmwareFromOutput(w.Firmware, w.Issues)
	if err != nil {
		return Device{}, err
	}
	if w.Runtime != nil {
		d.Runtime, err = runtimeFromOutput(w.Runtime, role, w.Issues)
		if err != nil {
			return Device{}, err
		}
	}
	for i, a := range w.Adapters {
		adapter := Adapter{Key: a.Key, SerialNumber: a.SerialNumber, SerialNumberRaw: a.SerialNumber, Model: a.Model, Slot: a.Slot,
			PCIAddress: a.PCIAddress, VendorID: a.VendorID, DeviceID: a.DeviceID, PCIFunctions: cloneFunctions(a.PCIFunctions), Selection: "not-selected", IdentityState: "unknown"}
		field := fmt.Sprintf("adapters[%d].identity", i)
		if a.SerialNumber != "" && !hasIssue(w.Issues, field) {
			adapter.IdentityState = "known"
		} else {
			// Correlation and HostCards consume SerialNumber directly. Preserve
			// a failed candidate only as context, never as association authority.
			adapter.SerialNumber = ""
		}
		for _, issue := range w.Issues {
			if issue.Field == field {
				adapter.ReasonCode = issue.Code
			}
		}
		d.Adapters = append(d.Adapters, adapter)
	}
	for _, issue := range w.Issues {
		switch issue.Field {
		case "identity":
			d.ReasonCode, d.Message = issue.Code, issue.Message
		case "psid":
			d.Observations["psid"] = issueObservation(issue)
		case "bmcAdministratorVerified":
			d.Observations["bmc-administrator"] = issueObservation(issue)
		case "firmwareInventoryComplete":
			d.Observations["firmware-inventory"] = issueObservation(issue)
		default:
			if strings.HasPrefix(issue.Field, "firmware.") {
				d.Observations[issue.Field] = issueObservation(issue)
			}
			if !strings.HasPrefix(issue.Field, "runtime.") && !strings.HasPrefix(issue.Field, "adapters[") && !strings.HasPrefix(issue.Field, "firmware.") {
				d.Observations[issue.Field] = issueObservation(issue)
			}
		}
	}
	return d, nil
}

func validateDeviceOutput(w DeviceOutput, role string) error {
	if err := validateHardwareIssueFields(w); err != nil {
		return err
	}
	securityIssues := map[string]bool{}
	for _, issue := range w.Issues {
		if issue.Field == "" || issue.Code == "" || !safeFact(issue.Field) || !safeFact(issue.Code) {
			return fmt.Errorf("device %s has an invalid issue", w.RecordKey)
		}
		if issue.Field == "identity" || issue.Field == "psid" || issue.Field == "bmcAdministratorVerified" || issue.Field == "firmwareInventoryComplete" {
			if securityIssues[issue.Field] {
				return fmt.Errorf("device %s has ambiguous %s issues", w.RecordKey, issue.Field)
			}
			securityIssues[issue.Field] = true
		}
	}
	if w.ID != "" && hasIssue(w.Issues, "identity") {
		return fmt.Errorf("device %s publishes identity with a failed recheck", w.RecordKey)
	}
	switch w.Method {
	case "redfish":
		if w.BMCEndpoint == "" || w.SSHEndpoint != "" {
			return fmt.Errorf("device %s Redfish method requires only a BMC endpoint", w.RecordKey)
		}
	case "ssh":
		if w.SSHEndpoint == "" {
			return fmt.Errorf("device %s SSH method requires an SSH endpoint", w.RecordKey)
		}
	case "host-inventory":
		if role != "dpus" || w.BMCEndpoint != "" || w.SSHEndpoint != "" {
			return fmt.Errorf("device %s host inventory cannot assert an endpoint", w.RecordKey)
		}
	case "":
		if w.ID != "" || w.BMCEndpoint != "" || w.SSHEndpoint != "" {
			return fmt.Errorf("device %s requires its discovery method", w.RecordKey)
		}
	default:
		return fmt.Errorf("device %s has unsupported method %q", w.RecordKey, w.Method)
	}
	if w.PSID != "" && (role != "dpus" || w.ID == "" || w.Method != "redfish" || w.Model != "B4240" || !validPSID(w.PSID) || hasIssue(w.Issues, "psid")) {
		return fmt.Errorf("device %s has inconsistent verified Redfish PSID", w.RecordKey)
	}
	if w.BMCAdministratorVerified && (role != "dpus" || w.ID == "" || w.Method != "redfish" || hasIssue(w.Issues, "bmcAdministratorVerified")) {
		return fmt.Errorf("device %s has inconsistent BMC administrator verification", w.RecordKey)
	}
	if (w.Firmware != nil || w.FirmwareInventoryComplete) && (role != "dpus" || w.Method != "redfish") {
		return fmt.Errorf("device %s firmware inventory requires a DPU Redfish source", w.RecordKey)
	}
	if w.FirmwareInventoryComplete && (w.ID == "" || hasIssue(w.Issues, "firmwareInventoryComplete")) {
		return fmt.Errorf("device %s has inconsistent firmware inventory completeness", w.RecordKey)
	}
	return nil
}

func deviceRole(role string) (string, error) {
	switch role {
	case "host", "hosts":
		return "hosts", nil
	case "dpu", "dpus":
		return "dpus", nil
	default:
		return "", fmt.Errorf("unknown inventory device role %q", role)
	}
}

func sortedObservationKeys(observations map[string]Observation) []string {
	keys := make([]string, 0, len(observations))
	for key := range observations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func addIssue(issues *[]DeviceIssue, field string, observation Observation) {
	if observation.State == "observed" || observation.ReasonCode == "NOT_APPLICABLE" || observation.State == "" && observation.ReasonCode == "" {
		return
	}
	code := observation.ReasonCode
	if code == "" {
		code = "FACT_UNAVAILABLE"
	}
	issue := DeviceIssue{Field: field, Code: code, Message: observation.Message, Tool: observation.Tool}
	if !slices.Contains(*issues, issue) {
		*issues = append(*issues, issue)
	}
}

func hasIssue(issues []DeviceIssue, field string) bool {
	return slices.ContainsFunc(issues, func(issue DeviceIssue) bool { return issue.Field == field })
}

func canonicalIssues(issues []DeviceIssue) {
	slices.SortFunc(issues, func(a, b DeviceIssue) int {
		if c := strings.Compare(a.Field, b.Field); c != 0 {
			return c
		}
		if c := strings.Compare(a.Code, b.Code); c != 0 {
			return c
		}
		if c := strings.Compare(a.Message, b.Message); c != 0 {
			return c
		}
		return strings.Compare(a.Tool, b.Tool)
	})
}

func issueObservation(issue DeviceIssue) Observation {
	return Observation{State: "unavailable", ReasonCode: issue.Code, Message: issue.Message, Tool: issue.Tool}
}

func observedValue(o Observation, field string) (string, error) {
	if o.State != "observed" {
		return "", nil
	}
	if len(o.Values) != 1 || !safeFact(o.Values[0]) {
		return "", fmt.Errorf("%s requires exactly one nonempty measured value", field)
	}
	return o.Values[0], nil
}

func safeFact(value string) bool {
	return value != "" && len(value) <= 1024 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validPSID(value string) bool {
	return len(value) <= 32 && strings.HasPrefix(value, "MT_") && strings.IndexFunc(value, func(r rune) bool {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-'
		return !allowed
	}) < 0
}

func cloneFunctions(functions []PCIFunction) []PCIFunction {
	out := slices.Clone(functions)
	for i := range out {
		out[i].Netdevs = slices.Clone(out[i].Netdevs)
	}
	return out
}
