// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
)

func TestEnrollmentCheckUsesAllObservedIdentitiesAcrossNamespaces(t *testing.T) {
	selected := &inventory.Inventory{DPUs: []inventory.Device{{SerialNumber: "SELECTED"}}}
	for _, kind := range []string{"DPUDevice", "DPU"} {
		t.Run(kind, func(t *testing.T) {
			s := freshAbsentHBNManagementSnapshot()
			s.DPUDevices = managementprobe.ResourceSet[managementprobe.DPUDevice]{State: managementprobe.CollectionComplete, Items: []managementprobe.DPUDevice{{ObjectSummary: managementprobe.ObjectSummary{Name: "unrelated", Namespace: "other"}, SpecSerialNumber: stringPointer("UNRELATED")}}}
			s.DPUs = managementprobe.ResourceSet[managementprobe.DPU]{State: managementprobe.CollectionComplete}
			if kind == "DPUDevice" {
				s.DPUDevices.Items = append(s.DPUDevices.Items, managementprobe.DPUDevice{ObjectSummary: managementprobe.ObjectSummary{Name: "selected", Namespace: "elsewhere"}, SpecSerialNumber: stringPointer("SELECTED")})
			} else {
				s.DPUs.Items = []managementprobe.DPU{{ObjectSummary: managementprobe.ObjectSummary{Name: "selected", Namespace: "elsewhere"}, SerialNumber: stringPointer("SELECTED")}}
			}
			got := enrollmentCheck(&s, selected)
			if got.state != "failed" || got.reason != "DPU_ALREADY_ENROLLED" || !strings.Contains(got.message, "elsewhere/selected") {
				t.Fatalf("existing selected enrollment was missed: %#v", got)
			}
			facts := projectEnrollments(&s, selectedSerials(selected))
			if len(facts) != 1 || facts[0].Namespace != "elsewhere" || facts[0].SerialNumber != "SELECTED" {
				t.Fatalf("projection must retain only target conflict: %#v", facts)
			}
			// Known conflicts remain failures even if another page was denied.
			s.DPUDevices.State = managementprobe.CollectionIncomplete
			if got := enrollmentCheck(&s, selected); got.state != "failed" {
				t.Fatalf("incomplete list lost known conflict: %#v", got)
			}
		})
	}
}

func TestEnrollmentCheckMissingIdentityAndCoverageRemainUnknown(t *testing.T) {
	selected := &inventory.Inventory{DPUs: []inventory.Device{{SerialNumber: "SELECTED"}}}
	for _, state := range []managementprobe.CollectionState{"", managementprobe.CollectionUnavailable, managementprobe.CollectionIncomplete} {
		s := freshAbsentHBNManagementSnapshot()
		s.DPUDevices.State = state
		if got := enrollmentCheck(&s, selected); got.state != "unknown" {
			t.Fatalf("coverage %q passed: %#v", state, got)
		}
	}
	s := freshAbsentHBNManagementSnapshot()
	s.DPUDevices = managementprobe.ResourceSet[managementprobe.DPUDevice]{State: managementprobe.CollectionComplete, Items: []managementprobe.DPUDevice{{ObjectSummary: managementprobe.ObjectSummary{Name: "unknown", Namespace: "elsewhere", UID: "private"}}}}
	if got := enrollmentCheck(&s, selected); got.state != "unknown" || got.reason != "DPU_ENROLLMENT_IDENTITY_UNKNOWN" {
		t.Fatalf("unknown serial passed: %#v", got)
	}
	want := []inventory.HBNManagementEnrollment{{Kind: "DPUDevice", Namespace: "elsewhere", Name: "unknown"}}
	if got := projectEnrollments(&s, selectedSerials(selected)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown projection %#v != %#v", got, want)
	}
	s.DPUDevices.Items[0].SpecSerialNumber, s.DPUDevices.Items[0].StatusSerialNumber = stringPointer("OTHER-A"), stringPointer("OTHER-B")
	if got := enrollmentCheck(&s, selected); got.state != "passed" {
		t.Fatalf("unrelated conflicting enrollment blocked target: %#v", got)
	}
	if got := projectEnrollments(&s, selectedSerials(selected)); len(got) != 0 {
		t.Fatalf("unrelated conflicting enrollment leaked: %#v", got)
	}
	s.DPUDevices.Items[0].SpecSerialNumber, s.DPUDevices.Items[0].StatusSerialNumber = stringPointer("SELECTED"), stringPointer("CONFLICT")
	if got := enrollmentCheck(&s, selected); got.state != "failed" || got.reason != "DPU_ENROLLMENT_IDENTITY_CONFLICT" {
		t.Fatalf("serial conflict passed: %#v", got)
	}
}
