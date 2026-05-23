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
		_ = unstructured.SetNestedMap(obj.Object, map[string]interface{}{}, "spec", "configuration")
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

func condition(t, status, reason, message string) map[string]interface{} {
	return map[string]interface{}{
		"type":    t,
		"status":  status,
		"reason":  reason,
		"message": message,
	}
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
			live := manifest.DeepCopy()
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
	// FirmwareUpdateInProgress when present.
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
			live := manifest.DeepCopy()
			c := newClient(t,
				live,
				nicDevice("dev-1", "ns", "worker-1",
					[]map[string]interface{}{port("0000:1a:00.0")},
					false, true,
					[]map[string]interface{}{
						condition(consts.ConfigUpdateInProgressCondition, "False", consts.UpdateSuccessfulReason, ""),
						condition(consts.FirmwareUpdateInProgressCondition, tc.fwStatus, tc.fwReason, ""),
					}),
				node("worker-1", map[string]string{"role": "worker"}),
			)
			v := nicTemplateValidator(templateKindConfiguration)
			res, err := v(context.Background(), c, manifest)
			require.NoError(t, err)
			assert.Equal(t, tc.wantState, res.State, "fwReason=%s", tc.fwReason)
		})
	}
}

func TestNicConfigurationTemplate_AggregatesAcrossDevices(t *testing.T) {
	// Two devices: one success, one error. Aggregate must be error.
	manifest := nicTemplateManifest(nicopKindConfigurationTemplate, "tpl", "ns", map[string]string{"role": "worker"})
	live := manifest.DeepCopy()
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
