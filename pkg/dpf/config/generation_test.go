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

const generationPSID = "MT_0000000010"

var _ = Describe("DPF generation configuration", func() {
	It("preserves the chart directory through a relocated discovery output", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Sentry.ChartDirectory = "charts/sentry"
		in = decodeGenerationInput(in)
		before, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		destination := filepath.Join(filepath.Dir(in.Path), "refreshed", "site.yaml")
		data, err := Saved(in, in.Site.Inventory, destination)
		Expect(err).NotTo(HaveOccurred())
		moved, err := Decode(data, destination)
		Expect(err).NotTo(HaveOccurred())
		after, err := ResolveGeneration(moved)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Sentry.ChartDirectory).To(Equal(before.Sentry.ChartDirectory))
	})

	It("resolves explicit release, context, management identity and only selected observed BF4 devices", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		// Extra inventory records are not implicitly selected or required to be usable.
		in.Site.Inventory.DPUs = append(in.Site.Inventory.DPUs, inventory.Device{
			ID: "unselected-bf3", Model: "BlueField-3", ObservationState: "failed",
		})

		resolved, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.Release).To(Equal("26.8.0"))
		Expect(resolved.Name).To(Equal("sentry"))
		Expect(resolved.Profile).To(Equal("sentry-bf4"))
		Expect(resolved.Namespace).To(Equal("dpf-operator-system"))
		Expect(resolved.ManagementUID).To(Equal("management-uid-1"))
		Expect(resolved.ManagementKubernetesVersion).To(Equal("v1.35.4"))
		Expect(resolved.Context).To(Equal("management-context"))
		Expect(resolved.Provisioning.DPUCluster.Selector).To(Equal(map[string]string{"site.example.com/tenant": "sentry"}))
		Expect(resolved.Provisioning.DPUCluster.ResourceUID).To(Equal("resource-uid-1"))
		Expect(resolved.Provisioning.DPUCluster.KubernetesVersion).To(Equal("v1.35.6"))
		Expect(resolved.DevicePSIDs).To(Equal(map[string]string{"dpu-1": generationPSID}))
		Expect(resolved.Devices).To(HaveLen(1))
		Expect(resolved.Devices[0].ID).To(Equal("dpu-1"))
		Expect(resolved.Sentry.PolicyServerNodePort).NotTo(BeNil())
		Expect(*resolved.Sentry.PolicyServerNodePort).To(Equal(30080))
		Expect(resolved.Sentry.Namespace).NotTo(BeNil())
		Expect(*resolved.Sentry.Namespace).To(Equal("sentry-services"))
		Expect(resolved.KubeconfigRef.File).To(Equal(filepath.Join(filepath.Dir(in.Path), "missing", "management.kubeconfig")))
		Expect(resolved.Sentry.ChartDirectory).To(Equal(filepath.Join(filepath.Dir(in.Path), "charts", "sentry")))
	})

	It("does not read kubeconfig, credential, or known-host references during local generation", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		missing := filepath.Join(filepath.Dir(in.Path), "definitely-not-present")
		passwordPath := missing + ".password"
		knownHostsPath := missing + ".known_hosts"
		in.Site.Management.KubeconfigRef.File = "definitely-not-present/kubeconfig"
		in.Site.Credentials = map[string]Credential{
			"remote": {Username: "user", PasswordRef: PasswordRef{File: &passwordPath}},
		}
		in.Site.Discovery.Hosts = []Endpoint{{
			CredentialRef: "remote",
			SSH:           &SSH{Address: "192.0.2.12", KnownHostsFile: knownHostsPath},
		}}

		resolved, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.KubeconfigRef.File).To(Equal(filepath.Join(filepath.Dir(in.Path), "definitely-not-present", "kubeconfig")))
		Expect(resolved.Sentry.ChartDirectory).To(Equal(filepath.Join(filepath.Dir(in.Path), "charts", "sentry")))
	})

	It("keeps omitted values distinct from explicit empty or zero values", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		encoded, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		decoded, err := Decode(encoded, in.Path)
		Expect(err).NotTo(HaveOccurred())
		resolved, err := ResolveGeneration(decoded)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.Name).To(Equal("sentry"))
		Expect(resolved.Profile).To(Equal("sentry-bf4"))
		Expect(resolved.Namespace).To(Equal("dpf-operator-system"))
		Expect(*resolved.Sentry.PolicyServerNodePort).To(Equal(30080))

		for _, tc := range []struct {
			name string
			edit func(*Input)
			want string
		}{
			{name: "explicit empty name", edit: func(i *Input) { v := ""; i.Site.DPF.Name = &v }, want: "/dpf/name must not be empty"},
			{name: "explicit empty profile", edit: func(i *Input) { v := ""; i.Site.DPF.Profile = &v }, want: "/dpf/profile must not be empty"},
			{name: "explicit empty namespace", edit: func(i *Input) { v := ""; i.Site.DPF.Namespace = &v }, want: "/dpf/namespace must not be empty"},
			{name: "omitted release", edit: func(i *Input) { i.Site.DPF.Release = nil }, want: "/dpf/release is required"},
			{name: "explicit empty release", edit: func(i *Input) { v := ""; i.Site.DPF.Release = &v }, want: `DPF release "" has no Sentry BF4 generation contract`},
			{name: "zero node port", edit: func(i *Input) { v := 0; i.Site.Sentry.PolicyServerNodePort = &v }, want: "/sentry/policyServerNodePort must be within 30000-32767"},
			{name: "empty chart path", edit: func(i *Input) { i.Site.Sentry.ChartDirectory = "" }, want: "/sentry/chartDirectory requires local pinned Helm chart archives"},
			{name: "empty Sentry namespace", edit: func(i *Input) { v := ""; i.Site.Sentry.Namespace = &v }, want: "/sentry/namespace must be a dedicated DNS namespace"},
			{name: "Sentry namespace collides with DPF namespace", edit: func(i *Input) { v := "dpf-operator-system"; i.Site.Sentry.Namespace = &v }, want: "/sentry/namespace must be a dedicated DNS namespace"},
		} {
			By(tc.name)
			copy := validGenerationInput(GinkgoT().TempDir())
			tc.edit(&copy)
			copy = decodeGenerationInput(copy)
			_, err := ResolveGeneration(copy)
			Expect(err).To(MatchError(ContainSubstring(tc.want)))
		}

		in = validGenerationInput(GinkgoT().TempDir())
		encoded, err = yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		withNullRelease := strings.Replace(string(encoded), "release: 26.8.0", "release: null", 1)
		decoded, err = Decode([]byte(withNullRelease), in.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded.Site.DPF.Release).To(BeNil())
		_, err = ResolveGeneration(decoded)
		Expect(err).To(MatchError(ContainSubstring("/dpf/release is required")))
	})

	It("strictly decodes generation fields and rejects unknown configuration keys", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		encoded, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		_, err = Decode(append(encoded, []byte("unexpectedGenerationField: true\n")...), in.Path)
		Expect(err).To(HaveOccurred())
	})

	It("rejects unsupported release lines and non-BF4 selected hardware", func() {
		for _, tc := range []struct {
			name  string
			setup func(*Input)
			want  string
		}{
			{name: "26.4 release", setup: func(i *Input) { v := "26.4.0"; i.Site.DPF.Release = &v }, want: `DPF release "26.4.0" has no Sentry BF4 generation contract`},
			{name: "BF3 device", setup: func(i *Input) { i.Site.Inventory.DPUs[0].Model = "BlueField-3" }, want: "requires observed BlueField-4 identity"},
		} {
			By(tc.name)
			in := validGenerationInput(GinkgoT().TempDir())
			tc.setup(&in)
			_, err := ResolveGeneration(in)
			Expect(err).To(MatchError(ContainSubstring(tc.want)))
		}
	})

	It("retains the exact B4240 model only from the reviewed BlueField reader", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Inventory.DPUs[0].Model = "B4240"
		in.Site.Inventory.DPUs[0].ReaderAdapter = "bluefield-card-v1"
		resolved, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.Devices[0].Model).To(Equal("B4240"))
		Expect(in.Site.Inventory.DPUs[0].Model).To(Equal("B4240"))
		encoded, err := EncodeResolved(resolved)
		Expect(err).NotTo(HaveOccurred())
		decoded, err := DecodeResolved(encoded)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded.Devices[0].Model).To(Equal("B4240"))

		for _, reader := range []string{"", "host-card-v1", "ssh"} {
			By(reader)
			in.Site.Inventory.DPUs[0].ReaderAdapter = reader
			_, err = ResolveGeneration(in)
			Expect(err).To(MatchError(ContainSubstring("B4240 requires the bluefield-card-v1 reader")))
		}
		in.Site.Inventory.DPUs[0].ReaderAdapter = "bluefield-card-v1"
		in.Site.Inventory.DPUs[0].Model = "B4240X"
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("requires observed BlueField-4 identity")))
	})

	It("requires successful BMC administrator discovery evidence for each selected device", func() {
		for _, tc := range []struct {
			name string
			edit func(*inventory.Device)
		}{
			{"legacy inventory", func(d *inventory.Device) { d.Observations = nil }},
			{"missing observation", func(d *inventory.Device) { delete(d.Observations, "bmc-administrator") }},
			{"failed observation", func(d *inventory.Device) {
				d.Observations["bmc-administrator"] = inventory.Observation{State: "failed", ReasonCode: "BMC_ADMIN_REQUIRED"}
			}},
			{"conflicting state", func(d *inventory.Device) {
				d.Observations["bmc-administrator"] = inventory.Observation{State: "failed", ReasonCode: "BMC_ADMIN_VERIFIED"}
			}},
			{"unverified reason", func(d *inventory.Device) {
				d.Observations["bmc-administrator"] = inventory.Observation{State: "observed", ReasonCode: "BMC_ADMIN_UNVERIFIABLE"}
			}},
			{"missing BMC", func(d *inventory.Device) { d.BMCEndpoint = "" }},
			{"ARM SSH substitute", func(d *inventory.Device) { d.SSHEndpoint = "192.0.2.21:22" }},
		} {
			By(tc.name)
			in := validGenerationInput(GinkgoT().TempDir())
			tc.edit(&in.Site.Inventory.DPUs[0])
			_, err := ResolveGeneration(in)
			Expect(err).To(MatchError(ContainSubstring("rerun discovery with an explicit DPU BMC endpoint and administrator credentials")))
		}

		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Inventory.DPUs = append(in.Site.Inventory.DPUs, inventory.Device{ID: "dpu-2", SerialNumber: "BF4-SERIAL-2", Model: "BlueField-4", ObservationState: "observed", BMCEndpoint: "https://192.0.2.22"})
		in.Site.Provisioning.Devices = append(in.Site.Provisioning.Devices, DeviceBinding{ID: "dpu-2", DPUDevice: "dpu-device-2", DPUNode: "dpu-node-2"})
		_, err := ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("device dpu-2 requires verified BMC administrator discovery evidence")))
	})

	It("requires explicit management context and either explicit or observed management UID", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Management.Context = nil
		_, err := ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("/management/context is required")))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Management.Context = stringPointer("")
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("/management/context is required")))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Management.ClusterUID = nil
		in.Site.Inventory.Management = nil
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("management cluster UID is unobserved")))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Inventory.Management = &inventory.Management{SystemNamespaceUID: "other-uid"}
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("management cluster UID conflicts")))
	})

	It("uses explicit or observed management Kubernetes version and rejects missing or invalid versions", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Management.KubernetesVersion = nil
		resolved, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.ManagementKubernetesVersion).To(Equal("v1.35.4"))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Management.KubernetesVersion = stringPointer("v1.36.2")
		resolved, err = ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.ManagementKubernetesVersion).To(Equal("v1.36.2"))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Management.KubernetesVersion = nil
		in.Site.Inventory.Management.KubernetesVersion = ""
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("management Kubernetes version")))

		for _, tc := range []struct {
			name  string
			setup func(*Input)
		}{
			{name: "invalid management version", setup: func(i *Input) { i.Site.Management.KubernetesVersion = stringPointer("v2.0.0") }},
			{name: "missing tenant version", setup: func(i *Input) { i.Site.Provisioning.DPUCluster.KubernetesVersion = "" }},
			{name: "invalid tenant version", setup: func(i *Input) { i.Site.Provisioning.DPUCluster.KubernetesVersion = "v1.35" }},
		} {
			By(tc.name)
			in := validGenerationInput(GinkgoT().TempDir())
			tc.setup(&in)
			_, err := ResolveGeneration(in)
			Expect(err).To(HaveOccurred())
		}
	})

	It("requires one association whose serial and host mapping match the selected device", func() {
		for _, tc := range []struct {
			name  string
			setup func(*Input)
			want  string
		}{
			{name: "duplicate association", setup: func(i *Input) {
				i.Site.Inventory.Associations = append(i.Site.Inventory.Associations, i.Site.Inventory.Associations[0])
			}, want: "one unambiguous host association"},
			{name: "association serial mismatch", setup: func(i *Input) { i.Site.Inventory.Associations[0].SerialNumber = "different-serial" }, want: "one unambiguous host association"},
			{name: "missing host mapping", setup: func(i *Input) { i.Site.Inventory.Associations[0].HostID = "missing-host" }, want: "host association does not resolve to one inventory host"},
			{name: "duplicate host mapping", setup: func(i *Input) { i.Site.Inventory.Hosts = append(i.Site.Inventory.Hosts, i.Site.Inventory.Hosts[0]) }, want: "host association does not resolve to one inventory host"},
		} {
			By(tc.name)
			in := validGenerationInput(GinkgoT().TempDir())
			tc.setup(&in)
			_, err := ResolveGeneration(in)
			Expect(err).To(MatchError(ContainSubstring(tc.want)))
		}
	})

	It("rejects absent or conflicting observed PSID mappings", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Inventory.DPUs[0].Runtime.Hardware.Cards[0].PSID = inventory.Observation{State: "unavailable"}
		resolved, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.DevicePSIDs).To(Equal(map[string]string{"dpu-1": generationPSID}))

		in = validGenerationInput(GinkgoT().TempDir())
		delete(in.Site.Provisioning.Software.PLDM, generationPSID)
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("has no selected PLDM artifact")))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Inventory.Hosts[0].Runtime.Hardware.Cards[0].PSID.Values = []string{"MT_0000000011"}
		in.Site.Provisioning.Software.PLDM["MT_0000000011"] = testArtifact("host-conflict.pldm")
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("one observed, nonconflicting PSID")))
	})

	It("requires a syntactically valid explicit DPUCluster label selector", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in.Site.Provisioning.DPUCluster.Selector = nil
		_, err := ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("selector requires labels")))

		in = validGenerationInput(GinkgoT().TempDir())
		in.Site.Provisioning.DPUCluster.Selector = map[string]string{"Invalid Key": "sentry"}
		_, err = ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("invalid DPUCluster selector")))
	})

	It("rejects duplicate selected serials even when they have different inventory IDs", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		duplicate := in.Site.Inventory.DPUs[0]
		duplicate.ID = "dpu-2"
		duplicate.BMCEndpoint = "https://192.0.2.22"
		in.Site.Inventory.DPUs = append(in.Site.Inventory.DPUs, duplicate)
		in.Site.Inventory.Associations = append(in.Site.Inventory.Associations, inventory.Association{
			DPUID: "dpu-2", HostID: "host-1", SerialNumber: duplicate.SerialNumber,
		})
		in.Site.Provisioning.Devices = append(in.Site.Provisioning.Devices, DeviceBinding{
			ID: "dpu-2", DPUDevice: "dpu-device-2", DPUNode: "dpu-node-2",
		})

		_, err := ResolveGeneration(in)
		Expect(err).To(MatchError(ContainSubstring("resolved device identities or bindings conflict")))
	})

	It("rejects ingress network and broadcast addresses", func() {
		for _, address := range []string{"10.70.0.0", "10.70.0.255"} {
			By(address)
			in := validGenerationInput(GinkgoT().TempDir())
			in.Site.Sentry.IngressIP = address
			_, err := ResolveGeneration(in)
			Expect(err).To(MatchError(ContainSubstring("usable address inside ingressCIDR")))
		}
	})

	It("isolates caller configuration, inventory and mutable resolved values", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		before, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		resolved, err := ResolveGeneration(in)
		Expect(err).NotTo(HaveOccurred())
		after, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(after)).To(Equal(string(before)))

		resolved.Provisioning.Devices[0].DPUNode = "changed-node"
		resolved.Provisioning.Software.PLDM[generationPSID] = testArtifact("changed.pldm")
		resolved.Sentry.PolicyServerNodePort = intPointer(12345)
		resolved.Devices[0].Runtime.Hardware.Cards[0].PSID.Values[0] = "changed-psid"
		after, err = yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(after)).To(Equal(string(before)))
		Expect(in.Site.Inventory.DPUs[0].Runtime.Hardware.Cards[0].PSID.Values).To(Equal([]string{generationPSID}))
	})
})

func validGenerationInput(base string) Input {
	psid := inventory.Observation{State: "observed", Values: []string{generationPSID}}
	dpu := inventory.Device{
		ID: "dpu-1", SerialNumber: "BF4-SERIAL-1", Model: "BlueField-4", ObservationState: "observed",
		BMCEndpoint:  "https://192.0.2.21",
		Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}},
		Runtime: &inventory.RuntimeObservation{Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{
			SerialNumber: "BF4-SERIAL-1", PSID: psid,
		}}}},
	}
	host := inventory.Device{
		ID: "host-1",
		Runtime: &inventory.RuntimeObservation{Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{
			SerialNumber: "BF4-SERIAL-1", PSID: psid,
		}}}},
	}
	site := Site{}
	site.DPF.Release = stringPointer("26.8.0")
	site.Management.KubeconfigRef = FileRef{File: "missing/management.kubeconfig"}
	site.Management.Context = stringPointer("management-context")
	site.Management.ClusterUID = stringPointer("management-uid-1")
	site.Management.KubernetesVersion = stringPointer("v1.35.4")
	site.Provisioning = &Provisioning{
		Devices: []DeviceBinding{{ID: "dpu-1", DPUDevice: "dpu-device-1", DPUNode: "dpu-node-1"}},
		DPUCluster: ClusterBinding{
			KubernetesVersion: "v1.35.6", Name: "tenant-one", Namespace: "dpf-operator-system", ClusterUID: "tenant-cluster-uid", ResourceUID: "resource-uid-1",
			Selector: map[string]string{"site.example.com/tenant": "sentry"},
		},
		Software: Software{
			OSISO: testArtifact("dpf-os.iso"),
			PLDM:  map[string]Artifact{generationPSID: testArtifact("bf4.pldm")},
		},
	}
	site.Sentry = &Sentry{
		ChartDirectory: "charts/sentry", IngressCIDR: "10.70.0.0/24", IngressIP: "10.70.0.20",
		ImagePullSecret: "sentry-pull", MITMSecret: "sentry-mitm", InferenceSecret: "sentry-inference",
		PolicyServerAddress: "192.0.2.40",
	}
	site.Inventory = &inventory.Inventory{
		FormatVersion: inventory.FormatVersion,
		Management:    &inventory.Management{SystemNamespaceUID: "management-uid-1", KubernetesVersion: "v1.35.4"},
		DPUs:          []inventory.Device{dpu},
		Hosts:         []inventory.Device{host},
		Associations:  []inventory.Association{{DPUID: "dpu-1", HostID: "host-1", SerialNumber: "BF4-SERIAL-1"}},
	}
	return Input{Path: filepath.Join(base, "site.yaml"), Site: site}
}

func testArtifact(name string) Artifact {
	return Artifact{URL: "https://artifacts.example.invalid/" + name, SHA256: strings.Repeat("a", 64)}
}

func decodeGenerationInput(in Input) Input {
	encoded, err := yaml.Marshal(in.Site)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	decoded, err := Decode(encoded, in.Path)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return decoded
}

func stringPointer(value string) *string { return &value }
func intPointer(value int) *int          { return &value }
