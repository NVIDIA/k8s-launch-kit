// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"embed"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestBootstrap(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF bootstrap rendering") }

// Unmodified CRDs from DPF 26.8.0 chart SHA256
// 0252d473beaf473e93a09b23a39b7b78fff931c9a801f4e60ef93bface5e28c8.
//
//go:embed testdata/*.yaml
var schemas embed.FS

func inputs() Inputs {
	return Inputs{
		OperatorNamespace: "dpf-system", OperatorConfigName: "dpf-config", BMCFactoryResetPolicy: BMCFactoryResetPolicyNever, ManagementAPIIP: "192.0.2.10", ManagementAPIPort: 6443,
		ArtifactPVC: "", PodCIDR: "10.244.0.0/14",
		Tenant:  Tenant{ServiceCIDR: "10.96.0.0/16", Name: "hbn-tenant", Namespace: "hbn-tenant", Labels: map[string]string{"l8k.example/tenant": "hbn"}, VIP: "192.0.2.20", Interface: "eth0", RouterID: 126, NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}, MaxNodes: 2},
		Devices: []Device{{Serial: "DPU-B", BMCIP: "192.0.2.102", BMCPort: 443}, {Serial: "DPU-A", BMCIP: "192.0.2.100", BMCPort: 8443}},
	}
}

var _ = Describe("Management bootstrap resources", func() {
	It("renders only selected singleton scans in stable serial order without secrets or workload roots", func() {
		in := inputs()
		files, err := Render(in)
		Expect(err).NotTo(HaveOccurred())
		var scans []map[string]any
		for _, file := range files {
			var object map[string]any
			Expect(yaml.Unmarshal(file.Data, &object)).To(Succeed())
			Expect(object["kind"]).NotTo(BeElementOf("Secret", "DPUDevice", "DPUNode", "DPUDeployment", "DPUSet"))
			if object["kind"] == "DPUDiscovery" {
				scans = append(scans, object)
			}
		}
		Expect(scans).To(HaveLen(2))
		for index, expected := range []Device{in.Devices[1], in.Devices[0]} {
			scan := scans[index]["spec"].(map[string]any)["ipRangeSpec"].(map[string]any)["ipRange"].(map[string]any)
			Expect(scan["startIP"]).To(Equal(expected.BMCIP))
			Expect(scan["endIP"]).To(Equal(expected.BMCIP))
			Expect(scan["port"]).To(Equal(float64(expected.BMCPort)))
		}
		Expect(in.Devices[0].Serial).To(Equal("DPU-B"))
		in.Devices[0], in.Devices[1] = in.Devices[1], in.Devices[0]
		reordered, err := Render(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(reordered).To(Equal(files))
		in.Tenant.Labels["l8k.example/tenant"] = "changed"
		Expect(string(files[3].Data)).NotTo(ContainSubstring("changed"))
	})

	It("emits the resolved factory reset policy and preserves an explicit upstream-default override", func() {
		for _, policy := range []BMCFactoryResetPolicy{BMCFactoryResetPolicyNever, BMCFactoryResetPolicyOnInitialization} {
			in := inputs()
			in.BMCFactoryResetPolicy = policy
			files, err := Render(in)
			Expect(err).NotTo(HaveOccurred())
			var rendered map[string]any
			for _, file := range files {
				var candidate map[string]any
				Expect(yaml.Unmarshal(file.Data, &candidate)).To(Succeed())
				if candidate["kind"] == "DPFOperatorConfig" {
					rendered = candidate
					break
				}
			}
			Expect(rendered).NotTo(BeNil())
			got := rendered["spec"].(map[string]any)["provisioningController"].(map[string]any)["installInterface"].(map[string]any)["installViaRedfish"].(map[string]any)["discoveredDPUDeviceBMCFactoryResetPolicy"]
			Expect(got).To(Equal(string(policy)))
		}
	})

	It("emits one namespace when the tenant shares the operator namespace and preserves optional PVC intent", func() {
		in := inputs()
		in.Tenant.Namespace = in.OperatorNamespace
		in.ArtifactPVC = "artifact-cache"
		files, err := Render(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(HaveLen(5))
		Expect(string(files[1].Data)).To(ContainSubstring("bfbPVCName: artifact-cache"))
		in.ArtifactPVC = ""
		files, err = Render(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(files[1].Data)).NotTo(ContainSubstring("bfbPVCName"))
	})

	DescribeTable("rejects invalid or ambiguous targets before producing any files", func(mutate func(*Inputs)) {
		in := inputs()
		mutate(&in)
		files, err := Render(in)
		Expect(err).To(HaveOccurred())
		Expect(files).To(BeNil())
	},
		Entry("no selected targets", func(in *Inputs) { in.Devices = nil }),
		Entry("missing factory reset policy", func(in *Inputs) { in.BMCFactoryResetPolicy = "" }),
		Entry("invalid factory reset policy", func(in *Inputs) { in.BMCFactoryResetPolicy = "Sometimes" }),
		Entry("duplicate serial", func(in *Inputs) { in.Devices[1].Serial = in.Devices[0].Serial }),
		Entry("duplicate endpoint", func(in *Inputs) {
			in.Devices[1].BMCIP = in.Devices[0].BMCIP
			in.Devices[1].BMCPort = in.Devices[0].BMCPort
		}),
		Entry("IPv6 is not accepted by DPUDiscovery", func(in *Inputs) { in.Devices[0].BMCIP = "2001:db8::1" }),
		Entry("no inferred DNS target", func(in *Inputs) { in.Devices[0].BMCIP = "bmc.example" }),
		Entry("invalid BMC port", func(in *Inputs) { in.Devices[0].BMCPort = 0 }),
		Entry("tenant VIP conflicts with BMC", func(in *Inputs) { in.Tenant.VIP = in.Devices[0].BMCIP }),
		Entry("tenant VIP conflicts with management API", func(in *Inputs) { in.Tenant.VIP = in.ManagementAPIIP }),
		Entry("pod CIDR overlaps BMC", func(in *Inputs) { in.PodCIDR = "192.0.2.0/24" }),
		Entry("noncanonical pod CIDR", func(in *Inputs) { in.PodCIDR = "10.244.0.1/14" }),
		Entry("tenant pod and service CIDRs overlap", func(in *Inputs) { in.PodCIDR = "10.96.1.0/24" }),
		Entry("unsupported service CIDR override", func(in *Inputs) { in.Tenant.ServiceCIDR = "10.97.0.0/16" }),
		Entry("missing selector", func(in *Inputs) { in.Tenant.NodeSelector = nil }),
		Entry("invalid selector", func(in *Inputs) { in.Tenant.NodeSelector = map[string]string{"invalid//label": ""} }),
		Entry("no cluster identity labels", func(in *Inputs) { in.Tenant.Labels = nil }),
		Entry("insufficient capacity", func(in *Inputs) { in.Tenant.MaxNodes = 1 }),
		Entry("invalid VRID", func(in *Inputs) { in.Tenant.RouterID = 256 }),
		Entry("unsafe interface", func(in *Inputs) { in.Tenant.Interface = "eth0;cmd" }),
		Entry("tenant name starts with digit", func(in *Inputs) { in.Tenant.Name = "1tenant" }),
	)

	It("validates emitted custom resources against exact GA chart OpenAPI schemas", func() {
		files, err := Render(inputs())
		Expect(err).NotTo(HaveOccurred())
		validators := map[string]validation.SchemaCreateValidator{}
		entries, err := schemas.ReadDir("testdata")
		Expect(err).NotTo(HaveOccurred())
		for _, entry := range entries {
			data, err := schemas.ReadFile("testdata/" + entry.Name())
			Expect(err).NotTo(HaveOccurred())
			var crd apiextensionsv1.CustomResourceDefinition
			Expect(yaml.Unmarshal(data, &crd)).To(Succeed())
			for _, version := range crd.Spec.Versions {
				if version.Name != "v1alpha1" || !version.Served {
					continue
				}
				schema := &apiextensions.JSONSchemaProps{}
				Expect(apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, schema, nil)).To(Succeed())
				validator, _, err := validation.NewSchemaValidator(schema)
				Expect(err).NotTo(HaveOccurred())
				validators[crd.Spec.Names.Kind] = validator
			}
		}
		Expect(validators).To(HaveLen(3))
		for _, file := range files {
			var object map[string]any
			Expect(yaml.Unmarshal(file.Data, &object)).To(Succeed())
			kind := object["kind"].(string)
			if kind == "Namespace" {
				continue
			}
			Expect(validators).To(HaveKey(kind))
			Expect(validation.ValidateCustomResource(field.NewPath("resource"), object, validators[kind])).To(BeEmpty(), file.Path)
		}
	})
})
