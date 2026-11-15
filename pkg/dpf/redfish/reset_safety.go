// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// ResetSafetyRequest binds a transient shutdown read to the current DPU round.
// NotBefore is the current agent startup; unmeasured pre-OS resets are unsupported.
type ResetSafetyRequest struct {
	SerialNumber string
	PSID         string
	NotBefore    time.Time
}

// ResetSafetyObservation is deliberately separate from discovery inventory.
// NICCopyState documents a qualified missing signal, never an idle-slot claim.
type ResetSafetyObservation struct {
	SerialNumber   string
	SystemURI      string
	CardChassisURI string
	CardPowerState string
	PowerState     string
	BootState      string
	BootStateTime  time.Time
	ObservedAt     time.Time
	TaskCount      int
	NICCopyState   string
}

// ObserveResetSafety performs only bounded reads and owned session operations.
// It qualifies modern BF4 26.10 only. External hardware writers must be excluded:
// Redfish does not expose independent NIC programming-idle state on this model.
func (co Collector) ObserveResetSafety(ctx context.Context, target config.Target, timeout time.Duration, request ResetSafetyRequest) (ResetSafetyObservation, error) {
	if ctx == nil || target.Role != "dpus" || !ValidSerial(request.SerialNumber) || request.PSID == "" || request.NotBefore.IsZero() ||
		timeout < time.Second || timeout > 2*time.Minute {
		return ResetSafetyObservation{}, &Error{Code: "RESET_SAFETY_INPUT_INVALID"}
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return ResetSafetyObservation{}, &Error{Code: "RESET_SAFETY_DEADLINE_MISSING"}
	}
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	read := &resetSafetyRead{request: request}
	device, _, err := co.collect(ctx, target, timeout, read)
	if err != nil {
		return ResetSafetyObservation{}, err
	}
	psid := device.Observations["psid"]
	if device.SerialNumber != request.SerialNumber || device.ObservationState != "observed" || read.reads != 2 {
		return ResetSafetyObservation{}, &Error{Code: "RESET_IDENTITY_CHANGED"}
	}
	if psid.State != "observed" || len(psid.Values) != 1 {
		return ResetSafetyObservation{}, &Error{Code: "RESET_PSID_UNVERIFIED"}
	}
	if !strings.EqualFold(psid.Values[0], request.PSID) || device.PSID != "" && !strings.EqualFold(device.PSID, request.PSID) {
		return ResetSafetyObservation{}, &Error{Code: "RESET_PSID_CHANGED"}
	}
	// Bound every contributing read, including System/Card identity before the
	// two task/firmware captures. Completing collection never refreshes this
	// in-process proof's age; preserve the local monotonic clock component.
	read.observed.ObservedAt = startedAt
	return read.observed, nil
}

type resetSafetyRead struct {
	request  ResetSafetyRequest
	observed ResetSafetyObservation
	fence    [32]byte
	reads    int
}

func (r *resetSafetyRead) capture(ctx context.Context, c *Client, root Document, system Resource, card Resource, device inventory.Device) error {
	if device.SerialNumber != r.request.SerialNumber {
		return &Error{Code: "RESET_IDENTITY_CHANGED"}
	}
	admin := device.Observations["bmc-administrator"]
	if admin.State != "observed" || admin.ReasonCode != "BMC_ADMIN_VERIFIED" {
		return &Error{Code: "RESET_BMC_ADMIN_UNVERIFIED"}
	}
	if !b4240Service(root, system.Document) || Text(system.Document, "Model") != "BlueField-4" {
		return &Error{Code: "RESET_PLATFORM_UNQUALIFIED"}
	}
	// This is the management card chassis, not the Arm power domain. It
	// remains powered and healthy while the identity-linked Arm is shut down.
	cardStatus := Object(card.Document, "Status")
	if card.URI == "" || Text(card.Document, "SerialNumber") != r.request.SerialNumber || Text(card.Document, "PowerState") != "On" || Text(cardStatus, "State") != "Enabled" || Text(cardStatus, "Health") != "OK" {
		return &Error{Code: "RESET_CARD_CHASSIS_UNVERIFIED", Method: "GET", URI: card.URI}
	}
	if err := resetHealthError(cardStatus, card.URI); err != nil {
		return err
	}
	if err := resetHealthError(Object(system.Document, "Status"), system.URI); err != nil {
		return err
	}
	observed, err := resetArmOff(system.Document, r.request.NotBefore)
	if err != nil {
		return err
	}
	observed.SerialNumber, observed.SystemURI = device.SerialNumber, system.URI
	observed.CardChassisURI, observed.CardPowerState = card.URI, Text(card.Document, "PowerState")
	tasks, err := resetTasks(ctx, c, root)
	if err != nil {
		return err
	}
	firmware, err := resetFirmware(ctx, c, root)
	if err != nil {
		return err
	}
	observed.TaskCount, observed.NICCopyState = len(tasks), "NIC_COPY_STATE_NOT_EXPOSED"
	// Compare the bounded semantic projection, never task Payload/headers or raw
	// account/firmware documents. Order in advertised collections is immaterial.
	data, err := json.Marshal([]any{observed, tasks, firmware})
	if err != nil {
		return &Error{Code: "RESET_SAFETY_STATE_UNVERIFIED"}
	}
	fence := sha256.Sum256(data)
	if r.reads != 0 && fence != r.fence {
		return &Error{Code: "RESET_SAFETY_STATE_CHANGED"}
	}
	r.fence, r.reads = fence, r.reads+1
	r.observed = observed
	return nil
}

func resetArmOff(system Document, notBefore time.Time) (ResetSafetyObservation, error) {
	status, progress := Object(system, "Status"), Object(system, "BootProgress")
	power, state, boot := Text(system, "PowerState"), Text(status, "State"), Text(progress, "LastState")
	when, err := time.Parse(time.RFC3339Nano, Text(progress, "LastStateTime"))
	if power == "" || state == "" || boot == "" || err != nil || when.Before(notBefore) || when.After(time.Now()) || Text(status, "Health") != "OK" {
		return ResetSafetyObservation{}, &Error{Code: "RESET_ARM_STATE_UNVERIFIED"}
	}
	// Off/Disabled alone was observed while UEFI PCIResourceConfigStarted was
	// executing. Missing or unfamiliar progress can never establish shutdown.
	if power != "Off" || state != "Disabled" || boot != "None" {
		return ResetSafetyObservation{}, &Error{Code: "RESET_ARM_NOT_OFF"}
	}
	return ResetSafetyObservation{PowerState: power, BootState: boot, BootStateTime: when}, nil
}

// Optional health fields cannot contradict an otherwise positive shutdown or
// copy state. Their absence is supported; malformed or unhealthy values are not.
func resetHealthError(status Document, uri string) error {
	for _, check := range []struct{ key, code string }{{"Health", "RESET_HEALTH_UNVERIFIED"}, {"HealthRollup", "RESET_HEALTH_ROLLUP_UNVERIFIED"}} {
		if value, present := status[check.key]; present && value != "OK" {
			return &Error{Code: check.code, Method: "GET", URI: uri}
		}
	}
	return nil
}

func resetTasks(ctx context.Context, c *Client, root Document) (map[string]any, error) {
	service, err := c.Get(ctx, Link(root, "TaskService"))
	if err != nil {
		return nil, err
	}
	base := Link(service, "Tasks")
	members, err := c.Collection(ctx, base)
	if err != nil {
		return nil, err
	}
	if len(members) > 64 {
		return nil, &Error{Code: "RESET_TASKS_UNVERIFIED"}
	}
	projection := map[string]any{}
	for _, member := range members {
		d := member.Document
		if !resetMember(base, member) {
			return nil, &Error{Code: "RESET_TASKS_UNVERIFIED"}
		}
		if Text(d, "TaskState") != "Completed" || Text(d, "TaskStatus") != "OK" || d["PercentComplete"] != float64(100) {
			return nil, &Error{Code: "RESET_TASKS_NOT_IDLE"}
		}
		if raw, present := d["Messages"]; present {
			messages, ok := raw.([]any)
			if !ok {
				return nil, &Error{Code: "RESET_TASKS_UNVERIFIED"}
			}
			for _, raw := range messages {
				message, ok := raw.(map[string]any)
				if !ok || !resetMessageOK(message) {
					return nil, &Error{Code: "RESET_TASKS_UNVERIFIED"}
				}
			}
		}
		projection[member.URI] = []any{Text(d, "TaskState"), Text(d, "TaskStatus"), d["PercentComplete"]}
	}
	return projection, nil
}

func resetMessageOK(message map[string]any) bool {
	seen := false
	for _, key := range []string{"Severity", "MessageSeverity"} {
		if value, present := message[key]; present {
			if value != "OK" {
				return false
			}
			seen = true
		}
	}
	return seen
}

func resetMember(base string, member Resource) bool {
	return strings.HasPrefix(member.URI, strings.TrimSuffix(base, "/")+"/") &&
		strings.TrimSuffix(Text(member.Document, "@odata.id"), "/") == strings.TrimSuffix(member.URI, "/")
}

func resetFirmware(ctx context.Context, c *Client, root Document) (map[string]any, error) {
	updateURI := Link(root, "UpdateService")
	update, err := c.Get(ctx, updateURI)
	if err != nil {
		return nil, err
	}
	if err := resetHealthError(Object(update, "Status"), updateURI); err != nil {
		return nil, err
	}
	if Text(Object(update, "Status"), "State") != "Enabled" {
		return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED"}
	}
	base := Link(update, "FirmwareInventory")
	members, err := c.Collection(ctx, base)
	if err != nil {
		return nil, err
	}
	required := map[string]bool{"BlueField_FW_BMC_0": false, "BlueField_FW_CPU_0": false, "BlueField_FW_ERoT_BMC_0": false, "BlueField_FW_ERoT_CPU_0": false, "BlueField_FW_NIC_0": false}
	pending := map[string]bool{}
	projection := map[string]any{}
	for _, member := range members {
		if !resetMember(base, member) {
			return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED"}
		}
		d := member.Document
		id := Text(d, "Id")
		if strings.HasSuffix(id, "_pending") {
			if pending[strings.TrimSuffix(id, "_pending")] {
				return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED", Method: "GET", URI: member.URI}
			}
			if version, ok := d["Version"].(string); !ok || version != "" {
				return nil, &Error{Code: "RESET_FIRMWARE_NOT_IDLE"}
			}
			projection[member.URI] = ""
			pending[strings.TrimSuffix(id, "_pending")] = true
		}
		if _, wanted := required[id]; !wanted {
			nvidia := Object(Object(d, "Oem"), "Nvidia")
			for _, key := range []string{"ActiveFirmwareSlot", "InactiveFirmwareSlot"} {
				if _, exposed := nvidia[key]; exposed {
					return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED", Method: "GET", URI: member.URI}
				}
			}
			continue
		}
		if required[id] || Text(d, "Version") == "" {
			return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED", Method: "GET", URI: member.URI}
		}
		required[id] = true
		nvidia := Object(Object(d, "Oem"), "Nvidia")
		active, inactive := Object(nvidia, "ActiveFirmwareSlot"), Object(nvidia, "InactiveFirmwareSlot")
		if id == "BlueField_FW_NIC_0" {
			for _, slot := range []Document{active, inactive} {
				if Text(slot, "FirmwareState") != "Unknown" || Text(slot, "Version") != "NA" || Text(slot, "BuildType") != "Development" {
					return nil, &Error{Code: "RESET_NIC_COPY_STATE_UNQUALIFIED"}
				}
			}
		} else if Text(active, "FirmwareState") != "Activated" || Text(inactive, "FirmwareState") != "Inactive" ||
			Text(active, "Version") != Text(d, "Version") || Text(active, "BuildType") != "Release" || Text(inactive, "BuildType") != "Release" {
			return nil, &Error{Code: "RESET_FIRMWARE_NOT_IDLE"}
		}
		if id == "BlueField_FW_BMC_0" && Text(d, "Version") != "BF4-26.10-0001" {
			return nil, &Error{Code: "RESET_PLATFORM_UNQUALIFIED"}
		}
		cpuOffline := resetQualifiedCPUOff(id, d)
		if !cpuOffline {
			if id == "BlueField_FW_CPU_0" && Text(Object(d, "Status"), "State") == "StandbyOffline" {
				return nil, &Error{Code: "RESET_CPU_OFF_STATE_UNQUALIFIED", Method: "GET", URI: member.URI}
			}
			if err := resetHealthError(Object(d, "Status"), member.URI); err != nil {
				return nil, err
			}
			if Text(Object(d, "Status"), "State") != "Enabled" {
				return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED", Method: "GET", URI: member.URI}
			}
		}
		projection[member.URI] = []any{Text(d, "Version"), active, inactive}
	}
	for id, found := range required {
		if !found || !pending[id] {
			return nil, &Error{Code: "RESET_FIRMWARE_STATE_UNVERIFIED"}
		}
	}
	return projection, nil
}

// resetQualifiedCPUOff recognizes the measured powered-off CPU representation
// on this BF4 BMC build, not a general exception for Critical firmware health.
// resetFirmware is called only after System Off/Disabled/BootProgress None is
// proven, and independently requires the matching qualified BMC version. The
// CPU slots must remain the exact activated/inactive release pair without copy.
func resetQualifiedCPUOff(id string, d Document) bool {
	if id != "BlueField_FW_CPU_0" || Text(d, "Version") != "26.10-0001" {
		return false
	}
	status := Object(d, "Status")
	conditions, ok := status["Conditions"].([]any)
	if !ok || len(conditions) != 0 || Text(status, "State") != "StandbyOffline" || Text(status, "Health") != "Critical" || Text(status, "HealthRollup") != "OK" {
		return false
	}
	nvidia := Object(Object(d, "Oem"), "Nvidia")
	for key, state := range map[string]string{"ActiveFirmwareSlot": "Activated", "InactiveFirmwareSlot": "Inactive"} {
		slot := Object(nvidia, key)
		if Text(slot, "Version") != "26.10-0001" || Text(slot, "BuildType") != "Release" || Text(slot, "FirmwareState") != state {
			return false
		}
	}
	return true
}
