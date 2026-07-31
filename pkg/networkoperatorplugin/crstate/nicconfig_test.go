// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"testing"

	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nicopAPIVersion = nicopGroup + "/" + nicopVersion

func nicTemplateManifest(kind, name, ns string, nodeSelector map[string]string) *unstructured.Unstructured {
	obj := makeUnstructured(nicopAPIVersion, kind, ns, name, nil)
	if len(nodeSelector) > 0 {
		nsRaw := make(map[string]interface{}, len(nodeSelector))
		for k, v := range nodeSelector {
			nsRaw[k] = v
		}
		_ = unstructured.SetNestedMap(obj.Object, nsRaw, "spec", "nodeSelector")
	}
	switch kind {
	case nicopKindConfigurationTemplate:
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{"nicType": "101d"}, "spec", "nicSelector")
		_ = unstructured.SetNestedMap(obj.Object, configurationTemplatePayload(), "spec", "template")
	case nicopKindFirmwareTemplate:
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{"nicType": "101d"}, "spec", "nicSelector")
		_ = unstructured.SetNestedMap(obj.Object, firmwareTemplatePayload(), "spec", "template")
	}
	return obj
}

func configurationTemplatePayload() map[string]interface{} {
	return map[string]interface{}{
		"numVfs":   int64(1),
		"linkType": "Ethernet",
	}
}

func firmwareTemplatePayload() map[string]interface{} {
	return map[string]interface{}{
		"nicFirmwareSourceRef": "fw-source",
		"updatePolicy":         "Update",
	}
}

func withMatchedDevices(obj *unstructured.Unstructured, names ...string) *unstructured.Unstructured {
	_ = unstructured.SetNestedStringSlice(obj.Object, names, "status", "nicDevices")
	return obj
}

// nicDevice constructs a NicDevice CR for fake-client seeding. Ports is
// a list of (pci, networkInterface) pairs; portsTargeted controls
// whether spec.{interfaceNameTemplate|configuration} is set so the
// validator's deviceTargetedByTemplate filter keeps the device.
func nicDevice(name, ns, node string, ports []map[string]interface{}, interfaceTargeted, configTargeted bool, conds []map[string]interface{}) *unstructured.Unstructured {
	obj := makeUnstructured(nicopAPIVersion, nicopKindDevice, ns, name, nil)
	if node != "" {
		_ = unstructured.SetNestedField(obj.Object, node, "status", "node")
	}
	_ = unstructured.SetNestedField(obj.Object, "101d", "status", "type")
	if len(ports) > 0 {
		raw := make([]interface{}, len(ports))
		for i, p := range ports {
			raw[i] = p
		}
		_ = unstructured.SetNestedSlice(obj.Object, raw, "status", "ports")
	}
	if interfaceTargeted {
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
			"nicIndex":         int64(0),
			"railIndex":        int64(0),
			"netDevicePrefix":  "eth_r",
			"rdmaDevicePrefix": "rdma_r",
		}, "spec", "interfaceNameTemplate")
	}
	if configTargeted {
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{
			"template": configurationTemplatePayload(),
		}, "spec", "configuration")
	}
	if len(conds) > 0 {
		raw := make([]interface{}, len(conds))
		for i, c := range conds {
			raw[i] = c
		}
		_ = unstructured.SetNestedSlice(obj.Object, raw, "status", "conditions")
	}
	return obj
}

func withFirmwareSpec(obj *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(obj.Object, firmwareTemplatePayload(), "spec", "firmware")
	return obj
}

func withDeviceType(obj *unstructured.Unstructured, deviceType string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, deviceType, "status", "type")
	return obj
}

func withSerialNumber(obj *unstructured.Unstructured, serialNumber string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, serialNumber, "status", "serialNumber")
	return obj
}

func withPartNumber(obj *unstructured.Unstructured, partNumber string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, partNumber, "status", "partNumber")
	return obj
}

func condition(t, status, reason, message string) map[string]interface{} {
	return map[string]interface{}{
		"type":    t,
		"status":  status,
		"reason":  reason,
		"message": message,
	}
}

func conditionWithGeneration(t, status, reason, message string, observedGeneration int64) map[string]interface{} {
	cond := condition(t, status, reason, message)
	cond["observedGeneration"] = observedGeneration
	return cond
}

func port(pci string) map[string]interface{} {
	return map[string]interface{}{"pci": pci}
}

func TestNicInterfaceNameTemplate_NotDeployed(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindInterfaceNameTemplate, "tpl", "ns", nil)
	c := newClient(t)
	v := nicTemplateValidator(templateKindInterfaceName)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateNotDeployed, res.State)
}

func TestNicInterfaceNameTemplate_NoTargetedDevicesIsInProgress(t *testing.T) {
	// Devices exist on the target nodes but the operator hasn't
	// reconciled the template yet (spec.interfaceNameTemplate still
	// nil). This is the *normal* initial state right after apply —
	// must be in-progress, not error, or the deploy fails the
	// instant phase 4 runs.
	manifest := nicTemplateManifest(nicopKindInterfaceNameTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			false, false, nil),
		node("worker-1", map[string]string{"role": "worker"}),
	)
	v := nicTemplateValidator(templateKindInterfaceName)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "1 NicDevice")
	assert.Contains(t, res.Reason, "spec.interfaceNameTemplate")
}

func TestNicInterfaceNameTemplate_NoDevicesOnTargetNodesIsInProgress(t *testing.T) {
	// No NicDevice CRs on the target nodes at all — also in-progress
	// because the nic-configuration-operator daemon may not yet
	// have discovered the hardware.
	manifest := nicTemplateManifest(nicopKindInterfaceNameTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		node("worker-1", map[string]string{"role": "worker"}),
	)
	v := nicTemplateValidator(templateKindInterfaceName)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "no NicDevice CRs on target nodes yet")
}

func TestNicInterfaceNameTemplate_AppliedSuccessfully(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindInterfaceNameTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			true, false,
			[]map[string]interface{}{
				condition(consts.InterfaceNameCondition, "True", consts.InterfaceNameAppliedReason, "ok"),
			}),
		node("worker-1", map[string]string{"role": "worker"}),
	)
	v := nicTemplateValidator(templateKindInterfaceName)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateSuccess, res.State)
}

func TestNicInterfaceNameTemplate_MismatchIsError(t *testing.T) {
	// Silent-failure case: udev rules didn't apply, names didn't take.
	manifest := nicTemplateManifest(nicopKindInterfaceNameTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			true, false,
			[]map[string]interface{}{
				condition(consts.InterfaceNameCondition, "False", consts.InterfaceNameMismatchReason,
					"interface name mismatch for ports: [net:0000:1a:00.0 expected=eth_r0 actual=enp1s0f0]"),
			}),
		node("worker-1", map[string]string{"role": "worker"}),
	)
	v := nicTemplateValidator(templateKindInterfaceName)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Contains(t, res.Reason, "interface name mismatch")
}

func TestNicInterfaceNameTemplate_ConditionAbsentIsInProgress(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindInterfaceNameTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			true, false, nil),
		node("worker-1", map[string]string{"role": "worker"}),
	)
	v := nicTemplateValidator(templateKindInterfaceName)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
}

func TestNicConfigurationTemplate_WaitsForMatchedDeviceStatus(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			false, true,
			[]map[string]interface{}{
				condition(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, ""),
			}),
	)

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "status.nicDevices")
}

func TestNicConfigurationTemplate_UsesOnlyStatusMatchedDevices(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	live := withMatchedDevices(manifest.DeepCopy(), "dev-matched")
	c := newClient(t,
		live,
		nicDevice("dev-matched", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			false, true,
			[]map[string]interface{}{
				condition(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, ""),
			}),
		withDeviceType(nicDevice("dev-unmatched", "ns", "worker-1",
			[]map[string]interface{}{port("0000:2a:00.0")},
			false, true,
			[]map[string]interface{}{
				condition(consts.ConfigUpdateInProgressCondition, "False", consts.NonVolatileConfigUpdateFailedReason, "must be ignored"),
			}), "other"),
	)

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateSuccess, res.State)
	assert.Equal(t, "1 matched device(s) reconciled", res.Reason)
	assert.Len(t, res.Details, 1)
	assert.NotContains(t, res.Details, "worker-1/0000:2a:00.0")
}

func TestNicConfigurationTemplate_MatchedDeviceSpecPending(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			false, false, nil),
	)

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "waiting for spec.configuration")
}

func TestNicConfigurationTemplate_MatchedDeviceMissing(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), newClient(t, live), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Equal(t, "listed in template status but NicDevice not found yet", res.Details["dev-1"])
}

func TestNicConfigurationTemplate_WaitsForCurrentTemplatePayload(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	_ = unstructured.SetNestedField(manifest.Object, int64(8), "spec", "template", "numVfs")
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	device := nicDevice("dev-1", "ns", "worker-1",
		[]map[string]interface{}{port("0000:1a:00.0")},
		false, true,
		[]map[string]interface{}{
			condition(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, ""),
		})

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), newClient(t, live, device), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "reflect the current template")
}

func TestNicConfigurationTemplate_WaitsForCurrentDeviceGeneration(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	device := nicDevice("dev-1", "ns", "worker-1",
		[]map[string]interface{}{port("0000:1a:00.0")},
		false, true,
		[]map[string]interface{}{
			conditionWithGeneration(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, "", 1),
		})
	device.SetGeneration(2)

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), newClient(t, live, device), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "has not observed NicDevice generation 2")
}

func TestNicConfigurationTemplate_AcceptsCurrentDeviceGeneration(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", nil)
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	device := nicDevice("dev-1", "ns", "worker-1",
		[]map[string]interface{}{port("0000:1a:00.0")},
		false, true,
		[]map[string]interface{}{
			conditionWithGeneration(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, "", 2),
		})
	device.SetGeneration(2)

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), newClient(t, live, device), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateSuccess, res.State)
}

func TestNicTemplates_WaitForStatusMatchingCurrentSelectors(t *testing.T) {
	tests := []struct {
		name           string
		kind           string
		typeOfTemplate templateKind
		device         func(*unstructured.Unstructured) *unstructured.Unstructured
	}{
		{
			name:           "configuration selector broadened",
			kind:           nicopKindConfigurationTemplate,
			typeOfTemplate: templateKindConfiguration,
			device: func(device *unstructured.Unstructured) *unstructured.Unstructured {
				return device
			},
		},
		{
			name:           "firmware selector broadened",
			kind:           nicopKindFirmwareTemplate,
			typeOfTemplate: templateKindFirmware,
			device:         withFirmwareSpec,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := nicTemplateManifest(tc.kind, "tpl", "ns", nil)
			_ = unstructured.SetNestedField(manifest.Object, "101d", "spec", "nicSelector", "nicType")
			live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
			conditionType := consts.ConfigUpdateInProgressCondition
			reason := consts.UpdateSuccessfulReason
			if tc.typeOfTemplate == templateKindFirmware {
				conditionType = consts.FirmwareUpdateInProgressCondition
				reason = consts.DeviceFwMatchReason
			}
			devices := []client.Object{live}
			for _, name := range []string{"dev-1", "dev-2"} {
				device := nicDevice(name, "ns", "worker-1",
					[]map[string]interface{}{port("0000:1a:00.0")},
					false, tc.typeOfTemplate == templateKindConfiguration,
					[]map[string]interface{}{condition(conditionType, "False", reason, "")})
				devices = append(devices, tc.device(withDeviceType(device, "101d")))
			}

			res, err := nicTemplateValidator(tc.typeOfTemplate)(context.Background(), newClient(t, devices...), manifest)
			require.NoError(t, err)
			assert.Equal(t, StateInProgress, res.State)
			assert.Contains(t, res.Reason, "status.nicDevices to reflect current template selectors")
			assert.Contains(t, res.Reason, "dev-2")
		})
	}
}

func TestDevicesMatchingCurrentSelectors(t *testing.T) {
	template := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	_ = unstructured.SetNestedStringSlice(template.Object, []string{"0000:1a:00.0"}, "spec", "nicSelector", "pciAddresses")
	_ = unstructured.SetNestedStringSlice(template.Object, []string{"serial-1"}, "spec", "nicSelector", "serialNumbers")
	_ = unstructured.SetNestedStringSlice(template.Object, []string{"part-1"}, "spec", "nicSelector", "partNumbers")

	devices := []unstructured.Unstructured{
		*withPartNumber(withSerialNumber(nicDevice("matched", "ns", "worker-1", []map[string]interface{}{port("0000:1a:00.0")}, false, true, nil), "serial-1"), "part-1"),
		*withPartNumber(withSerialNumber(nicDevice("wrong-node", "ns", "worker-2", []map[string]interface{}{port("0000:1a:00.0")}, false, true, nil), "serial-1"), "part-1"),
		*withPartNumber(withSerialNumber(withDeviceType(nicDevice("wrong-type", "ns", "worker-1", []map[string]interface{}{port("0000:1a:00.0")}, false, true, nil), "other"), "serial-1"), "part-1"),
		*withPartNumber(withSerialNumber(nicDevice("wrong-pci", "ns", "worker-1", []map[string]interface{}{port("0000:2a:00.0")}, false, true, nil), "serial-1"), "part-1"),
		*withPartNumber(withSerialNumber(nicDevice("wrong-serial", "ns", "worker-1", []map[string]interface{}{port("0000:1a:00.0")}, false, true, nil), "serial-2"), "part-1"),
		*withPartNumber(withSerialNumber(nicDevice("wrong-part", "ns", "worker-1", []map[string]interface{}{port("0000:1a:00.0")}, false, true, nil), "serial-1"), "part-2"),
	}
	c := newClient(t,
		node("worker-1", map[string]string{"role": "worker"}),
		node("worker-2", map[string]string{"role": "infra"}),
	)

	matches, err := devicesMatchingCurrentSelectors(context.Background(), c, template, devices)
	require.NoError(t, err)
	assert.Equal(t, []string{"matched"}, matches)
}

func TestNicTemplates_PartNumberSelectorsExcludeUnmatchedDevices(t *testing.T) {
	tests := []struct {
		name            string
		kind            string
		templateKind    templateKind
		prepareDevice   func(*unstructured.Unstructured) *unstructured.Unstructured
		conditionType   string
		conditionReason string
	}{
		{
			name:            "configuration",
			kind:            nicopKindConfigurationTemplate,
			templateKind:    templateKindConfiguration,
			prepareDevice:   func(device *unstructured.Unstructured) *unstructured.Unstructured { return device },
			conditionType:   consts.ConfigUpdateInProgressCondition,
			conditionReason: consts.UpdateSuccessfulReason,
		},
		{
			name:            "firmware",
			kind:            nicopKindFirmwareTemplate,
			templateKind:    templateKindFirmware,
			prepareDevice:   withFirmwareSpec,
			conditionType:   consts.FirmwareUpdateInProgressCondition,
			conditionReason: consts.DeviceFwMatchReason,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := nicTemplateManifest(tc.kind, "tpl", "ns", nil)
			_ = unstructured.SetNestedStringSlice(manifest.Object, []string{"part-1"}, "spec", "nicSelector", "partNumbers")
			live := withMatchedDevices(manifest.DeepCopy(), "matched")

			matched := nicDevice("matched", "ns", "worker-1",
				[]map[string]interface{}{port("0000:1a:00.0")}, false,
				tc.templateKind == templateKindConfiguration,
				[]map[string]interface{}{condition(tc.conditionType, "False", tc.conditionReason, "")})
			matched = tc.prepareDevice(withPartNumber(matched, "part-1"))
			unmatched := withPartNumber(nicDevice("unmatched", "ns", "worker-1",
				[]map[string]interface{}{port("0000:2a:00.0")}, false, false, nil), "part-2")

			res, err := nicTemplateValidator(tc.templateKind)(context.Background(), newClient(t, live, matched, unmatched), manifest)
			require.NoError(t, err)
			assert.Equal(t, StateSuccess, res.State)
			assert.Equal(t, "1 matched device(s) reconciled", res.Reason)
		})
	}
}

func TestNicConfigurationTemplate_AllReasonsClassified(t *testing.T) {
	// Walk every Reason listed in §1.2b for ConfigUpdateInProgress and
	// confirm the validator's classification matches the plan's
	// authoritative table.
	cases := []struct {
		name   string
		reason string
		status string
		want   CRState
	}{
		{"UpdateSuccessful→success", consts.UpdateSuccessfulReason, "False", StateSuccess},
		{"UpdateStarted→in-progress", consts.UpdateStartedReason, "True", StateInProgress},
		{"PendingReboot→in-progress", consts.PendingRebootReason, "True", StateInProgress},
		{"PendingFirmwareUpdate→in-progress", consts.PendingFirmwareUpdateReason, "False", StateInProgress},
		{"NonVolatileConfigUpdateFailed→error", consts.NonVolatileConfigUpdateFailedReason, "False", StateError},
		{"RuntimeConfigUpdateFailed→error", consts.RuntimeConfigUpdateFailedReason, "False", StateError},
		{"SpecValidationFailed→error", consts.SpecValidationFailed, "False", StateError},
		{"IncorrectSpec→error", consts.IncorrectSpecReason, "False", StateError},
		{"FirmwareError→error", consts.FirmwareError, "False", StateError},
		{"DeviceConfigSpecEmpty→in-progress", consts.DeviceConfigSpecEmptyReason, "False", StateInProgress},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
			live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
			c := newClient(t,
				live,
				nicDevice("dev-1", "ns", "worker-1",
					[]map[string]interface{}{port("0000:1a:00.0")},
					false, true,
					[]map[string]interface{}{
						condition(consts.ConfigUpdateInProgressCondition, tc.status, tc.reason, "msg"),
					}),
				node("worker-1", map[string]string{"role": "worker"}),
			)
			v := nicTemplateValidator(templateKindConfiguration)
			res, err := v(context.Background(), c, manifest)
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.State, "reason=%s", tc.reason)
		})
	}
}

func TestNicConfigurationTemplate_FirmwareGate(t *testing.T) {
	// ConfigUpdateInProgress=False/UpdateSuccessful gates on
	// FirmwareUpdateInProgress when spec.firmware is present.
	cases := []struct {
		name       string
		fwReason   string
		fwStatus   string
		wantState  CRState
		wantReason string
	}{
		{"firmware matched", consts.DeviceFwMatchReason, "False", StateSuccess, ""},
		{"firmware pending maintenance", consts.PendingNodeMaintenanceReason, "True", StateInProgress, ""},
		{"firmware update failed", consts.FirmwareUpdateFailedReason, "False", StateError, ""},
		{"firmware source failed", consts.FirmwareSourceFailedReason, "False", StateError, ""},
		{"device firmware mismatch", consts.DeviceFwMismatchReason, "False", StateError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
			live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
			device := withFirmwareSpec(nicDevice("dev-1", "ns", "worker-1",
				[]map[string]interface{}{port("0000:1a:00.0")},
				false, true,
				[]map[string]interface{}{
					condition(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, ""),
					condition(consts.FirmwareUpdateInProgressCondition, tc.fwStatus, tc.fwReason, ""),
				}))
			c := newClient(t,
				live,
				device,
				node("worker-1", map[string]string{"role": "worker"}),
			)
			v := nicTemplateValidator(templateKindConfiguration)
			res, err := v(context.Background(), c, manifest)
			require.NoError(t, err)
			assert.Equal(t, tc.wantState, res.State, "fwReason=%s", tc.fwReason)
		})
	}
}

func TestNicConfigurationTemplate_IgnoresStaleFirmwareConditionWithoutFirmwareSpec(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	device := nicDevice("dev-1", "ns", "worker-1",
		[]map[string]interface{}{port("0000:1a:00.0")},
		false, true,
		[]map[string]interface{}{
			conditionWithGeneration(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, "", 3),
			conditionWithGeneration(consts.FirmwareUpdateInProgressCondition, "False", consts.DeviceFwMatchReason, "", 2),
		})
	device.SetGeneration(3)

	res, err := nicTemplateValidator(templateKindConfiguration)(context.Background(), newClient(t,
		live,
		device,
		node("worker-1", map[string]string{"role": "worker"}),
	), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateSuccess, res.State)
	assert.Contains(t, res.Details["worker-1/0000:1a:00.0"], "no firmware spec")
}

func TestNicConfigurationTemplate_AggregatesAcrossDevices(t *testing.T) {
	// Two devices: one success, one error. Aggregate must be error.
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1", "dev-2")
	c := newClient(t,
		live,
		nicDevice("dev-1", "ns", "worker-1",
			[]map[string]interface{}{port("0000:1a:00.0")},
			false, true,
			[]map[string]interface{}{
				condition(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, ""),
			}),
		nicDevice("dev-2", "ns", "worker-2",
			[]map[string]interface{}{port("0000:2a:00.0")},
			false, true,
			[]map[string]interface{}{
				condition(consts.ConfigUpdateInProgressCondition, "False", consts.NonVolatileConfigUpdateFailedReason, "nvconfig write failed"),
			}),
		node("worker-1", map[string]string{"role": "worker"}),
		node("worker-2", map[string]string{"role": "worker"}),
	)
	v := nicTemplateValidator(templateKindConfiguration)
	res, err := v(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Len(t, res.Details, 2)
}

func TestNicFirmwareTemplate_WaitsForMatchedDeviceStatus(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindFirmwareTemplate, "tpl", "ns", nil)
	live := manifest.DeepCopy()

	res, err := nicTemplateValidator(templateKindFirmware)(context.Background(), newClient(t, live), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "status.nicDevices")
}

func TestNicFirmwareTemplate_WaitsForCurrentTemplatePayload(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindFirmwareTemplate, "tpl", "ns", nil)
	_ = unstructured.SetNestedField(manifest.Object, "new-fw-source", "spec", "template", "nicFirmwareSourceRef")
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	device := withFirmwareSpec(nicDevice("dev-1", "ns", "worker-1",
		[]map[string]interface{}{port("0000:1a:00.0")},
		false, false,
		[]map[string]interface{}{
			condition(consts.FirmwareUpdateInProgressCondition, "False", consts.DeviceFwMatchReason, ""),
		}))

	res, err := nicTemplateValidator(templateKindFirmware)(context.Background(), newClient(t, live, device), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "reflect the current template")
}

func TestNicFirmwareTemplate_WaitsForCurrentDeviceGeneration(t *testing.T) {
	manifest := nicTemplateManifest(nicopKindFirmwareTemplate, "tpl", "ns", nil)
	live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
	device := withFirmwareSpec(nicDevice("dev-1", "ns", "worker-1",
		[]map[string]interface{}{port("0000:1a:00.0")},
		false, false,
		[]map[string]interface{}{
			conditionWithGeneration(consts.FirmwareUpdateInProgressCondition, "False", consts.DeviceFwMatchReason, "", 3),
		}))
	device.SetGeneration(4)

	res, err := nicTemplateValidator(templateKindFirmware)(context.Background(), newClient(t, live, device), manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Reason, "has not observed NicDevice generation 4")
}

func TestNicFirmwareTemplate_AllReasonsClassified(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		status string
		want   CRState
	}{
		{"firmware matched→success", consts.DeviceFwMatchReason, "False", StateSuccess},
		{"update started→in-progress", consts.FirmwareUpdateStartedReason, "True", StateInProgress},
		{"pending maintenance→in-progress", consts.PendingNodeMaintenanceReason, "True", StateInProgress},
		{"update failed→error", consts.FirmwareUpdateFailedReason, "False", StateError},
		{"source not ready→error", consts.FirmwareSourceNotReadyReason, "False", StateError},
		{"source failed→error", consts.FirmwareSourceFailedReason, "False", StateError},
		{"firmware mismatch→error", consts.DeviceFwMismatchReason, "False", StateError},
		{"empty firmware spec→in-progress", consts.DeviceFirmwareSpecEmptyReason, "False", StateInProgress},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := nicTemplateManifest(nicopKindFirmwareTemplate, "tpl", "ns", nil)
			live := withMatchedDevices(manifest.DeepCopy(), "dev-1")
			device := withFirmwareSpec(nicDevice("dev-1", "ns", "worker-1",
				[]map[string]interface{}{port("0000:1a:00.0")},
				false, false,
				[]map[string]interface{}{
					condition(consts.FirmwareUpdateInProgressCondition, tc.status, tc.reason, "msg"),
				}))

			res, err := nicTemplateValidator(templateKindFirmware)(context.Background(), newClient(t, live, device), manifest)
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.State, "reason=%s", tc.reason)
		})
	}
}

func TestNicTemplateValidators_RegisteredKinds(t *testing.T) {
	r := NewDefault()
	for _, kind := range []string{
		nicopKindInterfaceNameTemplate,
		nicopKindConfigurationTemplate,
		nicopKindFirmwareTemplate,
	} {
		gvk := schema.GroupVersionKind{Group: nicopGroup, Version: nicopVersion, Kind: kind}
		_, ok := r.validators[gvk]
		assert.Truef(t, ok, "%s not registered", kind)
	}
}
