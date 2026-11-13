// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

var _ = Describe("resolved DPF generation", func() {
	It("rejects retained PSID selection that contradicts the Redfish observation", func() {
		generation := resolvedGeneration()
		d := &generation.Devices[0]
		d.Model, d.ReaderAdapter = "B4240", "bluefield-card-v1"
		d.Observations["psid"] = inventory.Observation{State: "observed", ReasonCode: "PSID_OBSERVED", Tool: "redfish", Values: []string{"MT_0000000011"}}
		Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("differs from its retained measurements")))
		d.Observations["psid"] = inventory.Observation{State: "unavailable", ReasonCode: "PSID_CHANGED", Tool: "redfish"}
		Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("rejected Redfish PSID evidence")))
	})

	It("compares all retained serial-bound measurements before accepting firmware selection", func() {
		generation := resolvedGeneration()
		d := &generation.Devices[0]
		d.Model, d.ReaderAdapter = "B4240", "bluefield-card-v1"
		d.Observations["psid"] = inventory.Observation{State: "observed", ReasonCode: "PSID_OBSERVED", Tool: "redfish", Values: []string{generationPSID}}
		d.Runtime.Hardware.Cards[0].PSID.Values = []string{"MT_0000000011"}
		Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("differs from its retained measurements")))
		_, err := EncodeResolved(generation)
		Expect(err).To(HaveOccurred())
		encoded, err := yaml.Marshal(Resolved{FormatVersion: "v1alpha1", Target: "dpf", Config: generation})
		Expect(err).NotTo(HaveOccurred())
		_, err = DecodeResolved(encoded)
		Expect(err).To(HaveOccurred())

		// An unrelated card's measurement must not participate in this binding.
		d.Runtime.Hardware.Cards[0].SerialNumber = "OTHER-CARD"
		Expect(ValidateGeneration(generation)).To(Succeed())
	})

	It("rejects incomplete Redfish verification even when retained SSH has a PSID", func() {
		for _, reason := range []string{"PSID_RECHECK_FAILED", "PSID_NOT_VERIFIED"} {
			generation := resolvedGeneration()
			generation.Devices[0].Observations["psid"] = inventory.Observation{State: "unavailable", ReasonCode: reason, Tool: "redfish"}
			Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("rejected Redfish PSID evidence")))
		}
	})

	It("rejects legacy or failed administrator proof in retained deployment metadata", func() {
		for _, admin := range []inventory.Observation{{}, {State: "failed", ReasonCode: "BMC_ADMIN_REQUIRED"}, {State: "failed", ReasonCode: "BMC_ADMIN_VERIFIED"}, {State: "observed", ReasonCode: "BMC_ADMIN_UNVERIFIABLE"}} {
			generation := resolvedGeneration()
			generation.Devices[0].Observations = map[string]inventory.Observation{"bmc-administrator": admin}
			Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("rerun discovery")))
			_, err := EncodeResolved(generation)
			Expect(err).To(MatchError(ContainSubstring("rerun discovery")))
			// Marshal directly to represent metadata produced before this requirement.
			legacy, err := yaml.Marshal(Resolved{FormatVersion: "v1alpha1", Target: "dpf", Config: generation})
			Expect(err).NotTo(HaveOccurred())
			_, err = DecodeResolved(legacy)
			Expect(err).To(MatchError(ContainSubstring("rerun discovery")))
		}
	})

	It("round-trips a validated deployment config and returns independent values", func() {
		generation := resolvedGeneration()
		encoded, err := EncodeResolved(generation)
		Expect(err).NotTo(HaveOccurred())

		decoded, err := DecodeResolved(encoded)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(Equal(generation))

		decoded.Provisioning.Software.PLDM[generationPSID] = testArtifact("mutated.pldm")
		decoded.Provisioning.Devices[0].DPUNode = "mutated-node"
		decoded.Devices[0].SerialNumber = "mutated-serial"
		Expect(generation.Provisioning.Software.PLDM[generationPSID]).To(Equal(testArtifact("bf4.pldm")))
		Expect(generation.Provisioning.Devices[0].DPUNode).To(Equal("dpu-node-1"))
		Expect(generation.Devices[0].SerialNumber).To(Equal("BF4-SERIAL-1"))
	})

	It("strictly checks the resolved format, fields, duplicate keys and document count", func() {
		encoded, err := EncodeResolved(resolvedGeneration())
		Expect(err).NotTo(HaveOccurred())
		document := string(encoded)
		for _, tc := range []struct {
			name string
			edit func(string) string
			want string
		}{
			{name: "unknown format", edit: func(s string) string { return strings.Replace(s, "formatVersion: v1alpha1", "formatVersion: v9", 1) }, want: "unsupported resolved DPF format or target"},
			{name: "wrong target", edit: func(s string) string { return strings.Replace(s, "target: dpf", "target: host", 1) }, want: "unsupported resolved DPF format or target"},
			{name: "unknown field", edit: func(s string) string { return s + "unexpectedField: true\n" }, want: "field unexpectedField not found"},
			{name: "duplicate key", edit: func(s string) string { return strings.Replace(s, "target: dpf\n", "target: dpf\ntarget: host\n", 1) }, want: "duplicate"},
			{name: "extra document", edit: func(s string) string { return s + "---\n{}\n" }, want: "one document"},
		} {
			By(tc.name)
			_, err := DecodeResolved([]byte(tc.edit(document)))
			Expect(err).To(MatchError(ContainSubstring(tc.want)))
		}
	})

	It("validates without reading local reference paths or mutating resolved defaults", func() {
		generation := resolvedGeneration()
		generation.KubeconfigRef.File = filepath.Join("/missing", "management.kubeconfig")
		generation.Sentry.ChartDirectory = filepath.Join("/missing", "charts", "sentry")
		before := cloneResolvedGeneration(generation)

		Expect(ValidateGeneration(generation)).To(Succeed())
		Expect(generation).To(Equal(before))
	})

	It("rejects incomplete selected-device membership and duplicate selected serials", func() {
		generation := resolvedGeneration()
		generation.Provisioning.Devices = nil
		Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("selected-device membership is incomplete")))
		_, err := EncodeResolved(generation)
		Expect(err).To(MatchError(ContainSubstring("selected-device membership is incomplete")))

		generation = resolvedGeneration()
		generation.Devices = append(generation.Devices, generation.Devices[0])
		generation.Devices[1].ID = "dpu-2"
		generation.Provisioning.Devices = append(generation.Provisioning.Devices, DeviceBinding{
			ID: "dpu-2", DPUDevice: "dpu-device-2", DPUNode: "dpu-node-2",
		})
		generation.DevicePSIDs["dpu-2"] = generationPSID
		Expect(ValidateGeneration(generation)).To(MatchError(ContainSubstring("resolved device identities or bindings conflict")))
	})

	It("rejects invalid persisted DPUCluster selector labels", func() {
		generation := resolvedGeneration()
		generation.Provisioning.DPUCluster.Selector = map[string]string{"Invalid Key": "sentry"}
		encoded, err := EncodeResolved(generation)
		if err == nil {
			_, err = DecodeResolved(encoded)
		}
		Expect(err).To(MatchError(ContainSubstring("selector")))
	})

	It("ignores fresh observation diagnostics while comparing desired intent", func() {
		original := resolvedGeneration()
		fresh := cloneResolvedGeneration(original)
		fresh.Devices[0].Message = "fresh read diagnostic"
		fresh.Devices[0].ReasonCode = "NEW_OBSERVATION"
		fresh.Devices[0].Firmware = append(fresh.Devices[0].Firmware, inventoryFirmwareFixture())
		fresh.Devices[0].EvidenceRefs = []string{"new-evidence"}
		fresh.Devices[0].Time.DateTime = "2099-01-02T03:04:05Z"
		fresh.Devices[0].Runtime.Hardware.Cards[0].Firmware = inventoryObservationFixture()
		Expect(SameIntent(original, fresh)).To(BeTrue())
	})

	It("detects changes to target identity, cluster identity, names, software and namespaces", func() {
		for _, tc := range []struct {
			name   string
			change func(*Generation)
		}{
			{name: "selected serial", change: func(g *Generation) { g.Devices[0].SerialNumber = "BF4-SERIAL-2" }},
			{name: "selected PSID", change: func(g *Generation) { g.DevicePSIDs["dpu-1"] = "MT_0000000011" }},
			{name: "management UID", change: func(g *Generation) { g.ManagementUID = "other-management-uid" }},
			{name: "management Kubernetes version", change: func(g *Generation) { g.ManagementKubernetesVersion = "v1.36.2" }},
			{name: "tenant cluster UID", change: func(g *Generation) { g.Provisioning.DPUCluster.ClusterUID = "other-tenant-uid" }},
			{name: "tenant Kubernetes version", change: func(g *Generation) { g.Provisioning.DPUCluster.KubernetesVersion = "v1.36.2" }},
			{name: "DPUCluster resource UID", change: func(g *Generation) { g.Provisioning.DPUCluster.ResourceUID = "other-resource-uid" }},
			{name: "generation name", change: func(g *Generation) { g.Name = "other-sentry" }},
			{name: "DPU resource name", change: func(g *Generation) { g.Provisioning.Devices[0].DPUDevice = "other-device" }},
			{name: "DPU node name", change: func(g *Generation) { g.Provisioning.Devices[0].DPUNode = "other-node" }},
			{name: "Sentry namespace", change: func(g *Generation) { g.Sentry.Namespace = stringPointer("other-services") }},
			{name: "DPF namespace", change: func(g *Generation) { g.Namespace = "other-operator" }},
			{name: "software digest", change: func(g *Generation) { g.Provisioning.Software.OSISO.SHA256 = strings.Repeat("b", 64) }},
		} {
			By(tc.name)
			original := resolvedGeneration()
			changed := cloneResolvedGeneration(original)
			tc.change(&changed)
			Expect(SameIntent(original, changed)).To(BeFalse())
		}
	})

	It("allows a kubeconfig path move only while the management cluster identity stays fixed", func() {
		original := resolvedGeneration()
		moved := cloneResolvedGeneration(original)
		moved.KubeconfigRef.File = "/new/location/management.kubeconfig"
		Expect(SameIntent(original, moved)).To(BeTrue())

		moved.ManagementUID = "different-management-uid"
		Expect(SameIntent(original, moved)).To(BeFalse())
	})
})

func resolvedGeneration() Generation {
	generation, err := ResolveGeneration(validGenerationInput(GinkgoT().TempDir()))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return generation
}

func cloneResolvedGeneration(g Generation) Generation {
	encoded, err := EncodeResolved(g)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	cloned, err := DecodeResolved(encoded)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return cloned
}

func inventoryFirmwareFixture() inventory.Firmware {
	return inventory.Firmware{Component: "bf4", State: "observed", RawVersion: "new-version", EvidenceRefs: []string{"fresh-firmware"}}
}

func inventoryObservationFixture() inventory.Observation {
	return inventory.Observation{State: "observed", Values: []string{"fresh-psid-observation"}}
}
