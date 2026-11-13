// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var firmwareIssueField = regexp.MustCompile(`^firmware\.([a-z][a-z0-9-]*)(?:\[([0-9]+)\])?(\.activation)?$`)
var adapterIssueField = regexp.MustCompile(`^adapters\[([0-9]+)\]\.identity$`)
var cardIssueField = regexp.MustCompile(`^runtime\.hardware\.cards\[([0-9]+)\]\.(firmwareVersion|psid|hostPrivileges|nextBoot\.(internalCPUOffloadEngine|internalCPURShim|numVFs))$`)

func issueIndex(text string, length int) (int, bool) {
	index, err := strconv.Atoi(text)
	return index, err == nil && index >= 0 && index < length && strconv.Itoa(index) == text
}

// A diagnostic must address the actual typed fact that its decoder consumes.
// Otherwise a misspelled scope or absent row could silently discard a failure.
func validateFirmwareIssues(w *FirmwareOutput, issues []DeviceIssue) error {
	for _, issue := range issues {
		if !strings.HasPrefix(issue.Field, "firmware.") {
			continue
		}
		match := firmwareIssueField.FindStringSubmatch(issue.Field)
		if match == nil {
			return fmt.Errorf("invalid firmware issue field %q", issue.Field)
		}
		var versions *[]FirmwareVersionOutput
		for _, slot := range firmwareOutputSlots(w) {
			if slot.component == match[1] {
				versions = slot.versions
				break
			}
		}
		if versions == nil {
			return fmt.Errorf("firmware issue names an unsupported component %q", match[1])
		}
		indexed, activation := match[2] != "", match[3] != ""
		if indexed {
			if _, valid := issueIndex(match[2], len(*versions)); !valid {
				return fmt.Errorf("firmware issue names an absent version %q", issue.Field)
			}
		} else if activation && len(*versions) != 0 && issue.Code != "ACTIVATION_CONFLICT" {
			return fmt.Errorf("firmware activation issue must name its version: %q", issue.Field)
		}
		if issue.Code == "ACTIVATION_CONFLICT" && !activation {
			return fmt.Errorf("activation conflict requires an activation field: %q", issue.Field)
		}
		if issue.Code == "FIRMWARE_VERSION_CONFLICT" && (indexed || activation) {
			return fmt.Errorf("firmware version conflict requires its component field: %q", issue.Field)
		}
	}
	return nil
}

func validateHardwareIssueFields(w DeviceOutput) error {
	for _, issue := range w.Issues {
		if strings.HasPrefix(issue.Field, "adapters[") {
			match := adapterIssueField.FindStringSubmatch(issue.Field)
			if match == nil {
				return fmt.Errorf("invalid adapter issue field %q", issue.Field)
			}
			if _, valid := issueIndex(match[1], len(w.Adapters)); !valid {
				return fmt.Errorf("adapter issue names an absent adapter %q", issue.Field)
			}
		}
		if !strings.HasPrefix(issue.Field, "runtime.hardware.") {
			continue
		}
		if w.Runtime == nil || w.Runtime.Hardware == nil {
			return fmt.Errorf("hardware issue has no hardware context: %q", issue.Field)
		}
		if issue.Field == "runtime.hardware.rshim" {
			continue
		}
		match := cardIssueField.FindStringSubmatch(issue.Field)
		if match == nil {
			return fmt.Errorf("invalid hardware issue field %q", issue.Field)
		}
		if _, valid := issueIndex(match[1], len(w.Runtime.Hardware.Cards)); !valid {
			return fmt.Errorf("hardware issue names an absent card %q", issue.Field)
		}
	}
	return nil
}
