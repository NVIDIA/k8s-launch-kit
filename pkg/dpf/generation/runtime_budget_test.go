// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
)

func TestNativeGenerationRejectsProvablyImpossibleMatrix(t *testing.T) {
	for _, count := range []int{2, 42, 43, 44} {
		input := generationInputs(t)
		input.Workload.Profile.DPUASNStride = 1
		serials := make([]string, count)
		for i := range serials {
			serials[i] = fmt.Sprintf("SERIAL-%03d", i)
		}
		asns, err := input.Workload.Profile.AllocateASNs(serials)
		if err != nil {
			t.Fatal(err)
		}
		input.Workload.Devices = nil
		input.Operator.Bootstrap.Devices = nil
		for i, serial := range serials {
			input.Workload.Devices = append(input.Workload.Devices, hbnrender.Device{Serial: serial, PSID: "MT_0000001774", SelectorLabels: map[string]string{"inventory.example/serial-hash": strings.ToLower(serial)}, ASN: asns[serial]})
			input.Operator.Bootstrap.Devices = append(input.Operator.Bootstrap.Devices, bootstrap.Device{Serial: serial, BMCIP: fmt.Sprintf("192.0.2.%d", 30+i), BMCPort: 443})
		}
		before := cloneInputs(input)
		retained, err := Generate(input)
		if !reflect.DeepEqual(input, before) {
			t.Fatal("budget validation mutated generation inputs")
		}
		if count >= 43 {
			if err == nil || !strings.Contains(err.Error(), "native HBN runtime budget") || retained != nil {
				t.Fatalf("impossible %d-card plan generated: err=%v", count, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("possible %d-card plan rejected: %v", count, err)
		}
		plan, err := LoadPlan(retained)
		if err != nil || len(plan.Intent().Devices) != count {
			t.Fatalf("possible cohort lost in retained plan: %v", err)
		}
	}
}
