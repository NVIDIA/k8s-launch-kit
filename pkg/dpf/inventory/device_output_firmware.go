// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"fmt"
	"slices"
	"strings"
)

type firmwareOutputSlot struct {
	component string
	versions  *[]FirmwareVersionOutput
}

func firmwareOutputSlots(w *FirmwareOutput) []firmwareOutputSlot {
	return []firmwareOutputSlot{
		{"bmc", &w.BMC}, {"nic", &w.NIC}, {"cec", &w.CEC}, {"atf", &w.ATF}, {"uefi", &w.UEFI}, {"atf-uefi", &w.ATFUEFI},
		{"dpu-bsp", &w.BSP}, {"dpu-os", &w.OS}, {"golden", &w.Golden}, {"bmc-erot", &w.BMCERoT}, {"cpu-erot", &w.CPUERoT},
	}
}

func firmwareToOutput(firmware []Firmware, observations map[string]Observation, issues *[]DeviceIssue) (*FirmwareOutput, error) {
	w := &FirmwareOutput{}
	used := false
	for _, slot := range firmwareOutputSlots(w) {
		field := "firmware." + slot.component
		addFirmwareIssue(issues, field, observations[field])
		addFirmwareIssue(issues, field+".activation", observations[field+".activation"])
		activationConflict := observations[field+".activation"].ReasonCode == "ACTIVATION_CONFLICT"
		for _, f := range firmware {
			activationConflict = activationConflict || f.Component == slot.component && !usableFirmwareVersion(f.RawVersion) && f.ActivationReasonCode == "ACTIVATION_CONFLICT"
		}
		for _, f := range firmware {
			if f.Component != slot.component {
				continue
			}
			usable := usableFirmwareVersion(f.RawVersion)
			if !usable && (f.State != "known" || f.ReasonCode != "") {
				code := f.ReasonCode
				if f.State == "conflict" {
					code = "FIRMWARE_VERSION_CONFLICT"
				}
				addFirmwareIssue(issues, field, Observation{State: "unknown", ReasonCode: code, Message: f.Message})
			}
			if !usable && f.ActivationReasonCode != "" {
				addFirmwareIssue(issues, field+".activation", Observation{State: "unknown", ReasonCode: f.ActivationReasonCode, Message: f.ActivationMessage})
			}
			if !usable {
				if f.RawVersion != "" || f.State == "known" {
					addFirmwareIssue(issues, field, Observation{State: "unknown", ReasonCode: "VERSION_INVALID", Message: "The firmware version is blank or contains unsupported characters."})
				}
				continue
			}
			activation := f.Activation
			if activation == "unknown" || activationConflict || f.ActivationReasonCode == "ACTIVATION_CONFLICT" {
				activation = ""
			}
			if activation != "" && activation != "running" && activation != "staged" {
				return nil, fmt.Errorf("%s has unsupported firmware activation", field)
			}
			entry := FirmwareVersionOutput{Version: f.RawVersion, Activation: activation}
			if !slices.Contains(*slot.versions, entry) {
				*slot.versions = append(*slot.versions, entry)
				used = true
			}
		}
		slices.SortFunc(*slot.versions, func(a, b FirmwareVersionOutput) int {
			if c := strings.Compare(a.Activation, b.Activation); c != 0 {
				return c
			}
			return strings.Compare(a.Version, b.Version)
		})
		for _, f := range firmware {
			if f.Component != slot.component || !usableFirmwareVersion(f.RawVersion) {
				continue
			}
			activation := f.Activation
			if activation == "unknown" || activationConflict || f.ActivationReasonCode == "ACTIVATION_CONFLICT" {
				activation = ""
			}
			index := slices.Index(*slot.versions, FirmwareVersionOutput{Version: f.RawVersion, Activation: activation})
			entryField := fmt.Sprintf("%s[%d]", field, index)
			if f.State == "conflict" || f.ReasonCode == "FIRMWARE_VERSION_CONFLICT" {
				addFirmwareIssue(issues, field, Observation{State: "unknown", ReasonCode: "FIRMWARE_VERSION_CONFLICT", Message: f.Message})
			} else if f.State != "known" || f.ReasonCode != "" {
				addFirmwareIssue(issues, entryField, Observation{State: "unknown", ReasonCode: f.ReasonCode, Message: f.Message})
			}
			if f.ActivationReasonCode != "" {
				addFirmwareIssue(issues, entryField+".activation", Observation{State: "unknown", ReasonCode: f.ActivationReasonCode, Message: f.ActivationMessage})
			}
		}
	}
	if !used {
		return nil, nil
	}
	return w, nil
}

func firmwareFromOutput(w *FirmwareOutput, issues []DeviceIssue) ([]Firmware, error) {
	if w == nil {
		w = &FirmwareOutput{}
	}
	if err := validateFirmwareIssues(w, issues); err != nil {
		return nil, err
	}
	var firmware []Firmware
	for _, slot := range firmwareOutputSlots(w) {
		field := "firmware." + slot.component
		nonstaged := map[string]bool{}
		conflict := false
		for _, issue := range issues {
			if issue.Field == field && issue.Code == "FIRMWARE_VERSION_CONFLICT" {
				conflict = true
			}
		}
		for index, version := range *slot.versions {
			if !usableFirmwareVersion(version.Version) || version.Activation != "" && version.Activation != "running" && version.Activation != "staged" {
				return nil, fmt.Errorf("%s requires a measured version and valid activation", field)
			}
			f := Firmware{Component: slot.component, State: "known", RawVersion: version.Version, Activation: version.Activation}
			if f.Activation == "" {
				f.Activation = "unknown"
			}
			if version.Activation != "staged" {
				nonstaged[version.Version] = true
			}
			for _, issue := range issues {
				entryField := fmt.Sprintf("%s[%d]", field, index)
				if issue.Field == entryField {
					f.State, f.ReasonCode, f.Message = "unknown", issue.Code, issue.Message
					if issue.Code == "FIRMWARE_VERSION_CONFLICT" {
						f.State = "conflict"
					}
				}
				if issue.Field == entryField+".activation" {
					if version.Activation != "" {
						return nil, fmt.Errorf("%s publishes activation with a failed activation observation", field)
					}
					f.ActivationReasonCode, f.ActivationMessage = issue.Code, issue.Message
				}
			}
			// Component conflicts veto compatibility regardless of issue order
			// or an additional per-entry read failure.
			if conflict {
				f.State, f.ReasonCode = "conflict", "FIRMWARE_VERSION_CONFLICT"
			}
			for _, issue := range issues {
				if (issue.Field == field+".activation" || issue.Field == fmt.Sprintf("%s[%d].activation", field, index)) && issue.Code == "ACTIVATION_CONFLICT" {
					f.Activation, f.ActivationReasonCode, f.ActivationMessage = "unknown", issue.Code, issue.Message
				}
			}
			firmware = append(firmware, f)
		}
		if len(nonstaged) > 1 && !conflict {
			return nil, fmt.Errorf("%s has conflicting current versions without a conflict issue", field)
		}
		if len(*slot.versions) == 0 {
			for _, issue := range issues {
				if issue.Field == field {
					state := "unknown"
					if issue.Code == "FIRMWARE_VERSION_CONFLICT" {
						state = "conflict"
					}
					firmware = append(firmware, Firmware{Component: slot.component, State: state, Activation: "unknown", ReasonCode: issue.Code, Message: issue.Message})
				}
				if issue.Field == field+".activation" && issue.Code == "ACTIVATION_CONFLICT" {
					firmware = append(firmware, Firmware{Component: slot.component, State: "unknown", ReasonCode: "VERSION_UNAVAILABLE", Activation: "unknown", ActivationReasonCode: issue.Code, ActivationMessage: issue.Message})
				}
			}
		}
	}
	return firmware, nil
}

// Raw versions are compared without normalization by software checks. Retain
// safe surrounding whitespace: trimming could turn an uncomparable value into
// compatibility evidence. Unusable optional measurements remain diagnostics.
func usableFirmwareVersion(version string) bool {
	return len(version) <= 1024 && strings.TrimSpace(version) != "" && !strings.ContainsFunc(version, func(r rune) bool {
		return r < ' ' || r == 0x7f
	})
}

// Routine absent/unknown component properties are represented by absent typed
// facts. Software checks derive the missing measurement; only actual failures
// and mismatches warrant a persisted issue.
func addFirmwareIssue(issues *[]DeviceIssue, field string, observation Observation) {
	switch observation.ReasonCode {
	case "COMPONENT_NOT_EXPOSED", "VERSION_UNAVAILABLE", "VERSION_NOT_EXPOSED", "ACTIVATION_NOT_EXPOSED":
		return
	}
	addIssue(issues, field, observation)
}
