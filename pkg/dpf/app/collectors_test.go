// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifactcheck"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/switchprobe"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func expectConcreteCollectors(deps discovery.Dependencies, runDir string) {
	GinkgoHelper()
	Expect(deps.Switches).To(BeAssignableToTypeOf(switchprobe.Collector{}))
	Expect(deps.SSH).To(BeAssignableToTypeOf(sshprobe.Collector{}))
	Expect(deps.HostLLDP).To(BeAssignableToTypeOf(sshprobe.Collector{}))
	Expect(deps.Artifacts).To(BeAssignableToTypeOf(artifactcheck.Verifier{}))
	Expect(deps.HostNetworking).NotTo(BeNil())
	Expect(deps.HBNEnrollment).NotTo(BeNil())
	Expect(deps.HBNPermissions).NotTo(BeNil())
	runner, ok := deps.Probes.(probe.Runner)
	Expect(ok).To(BeTrue())
	Expect(runner.Assets).To(Equal(catalog.ProbeAssets()))
	receipts := []probe.Receipt{{Kind: "Pod", Namespace: "diagnostics", Name: "probe-a", UID: "probe-one"}}
	Expect(runner.Journal(receipts)).To(Succeed())
	data, err := os.ReadFile(filepath.Join(runDir, "resources.json"))
	Expect(err).NotTo(HaveOccurred())
	var persisted []probe.Receipt
	Expect(json.Unmarshal(data, &persisted)).To(Succeed())
	Expect(persisted).To(Equal(receipts))
}

var _ = Describe("application collector composition", func() {
	It("assembles standalone collectors without opening access or mutating prior inventory", func() {
		input := generationSite()
		intent, err := config.ResolveIntent(input.Site)
		Expect(err).NotTo(HaveOccurred())
		for _, hbnSelected := range []bool{false, true} {
			for _, softwareSelected := range []bool{false, true} {
				request := config.Request{Input: input, Kubeconfig: "/must-not-open-kubeconfig"}
				if hbnSelected {
					request.HBN = intent.Networking
				}
				if softwareSelected {
					request.Software = &intent.Release
				}
				before, err := json.Marshal(request.Input.Site.Inventory)
				Expect(err).NotTo(HaveOccurred())
				runDir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
				Expect(err).NotTo(HaveOccurred())
				deps := StandaloneCollectors(request, runDir)
				files, err := os.ReadDir(runDir)
				Expect(err).NotTo(HaveOccurred())
				Expect(files).To(BeEmpty(), "construction must not write a journal")
				expectConcreteCollectors(deps, runDir)
				bmc := deps.BMC.(redfish.Collector)
				Expect(bmc.IncludeNetworkFunctionMACs).To(Equal(hbnSelected))
				release := ""
				if hbnSelected && softwareSelected {
					release = intent.Release.Version
				}
				Expect(bmc.ProvisioningRelease).To(Equal(release))
				Expect(deps.RetainedProvisioning).To(BeNil())
				Expect(deps.DeferHBNReadiness).To(BeFalse())
				Expect(deps.ArtifactSourceMode).To(Equal(discovery.VerifyArtifactOrigins))
				checks := deps.HBNPermissions(context.Background(), request, nil)
				Expect(checks).To(ContainElement(HaveField("ReasonCode", "CALLER_RBAC_INPUT_UNAVAILABLE")))
				Expect(checks).To(ContainElement(HaveField("Message", ContainSubstring("current management discovery and the deployment-plan checker"))))
				Expect(deps.HBNEnrollment(request, nil).State).To(Equal("unknown"))
				after, err := json.Marshal(request.Input.Site.Inventory)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
			}
		}
	})

	It("binds fresh, resume and post-activation policies to the retained plan", func() {
		_, retained := generatedUseCaseSite(generationSite())
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		verify := DeploymentPrerequisites(retained, filepath.Join(GinkgoT().TempDir(), "must-not-create"), false)
		_, err = verify(context.Background(), config.Request{}, nil, deployment.BeforeActivation)
		Expect(err).To(MatchError(ContainSubstring("validated retained plan")))
		_, err = verify(context.Background(), config.Request{}, plan, deployment.PrerequisitePhase("invalid"))
		Expect(err).To(MatchError("unsupported prerequisite phase"))
		for _, resume := range []bool{false, true} {
			for _, phase := range []deployment.PrerequisitePhase{deployment.BeforeActivation, deployment.AfterActivation} {
				runDir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
				Expect(err).NotTo(HaveOccurred())
				deps := retainedCollectors(runDir, retained, plan, resume, phase)
				expectConcreteCollectors(deps, runDir)
				bmc := deps.BMC.(redfish.Collector)
				Expect(bmc.IncludeNetworkFunctionMACs).To(BeTrue())
				Expect(bmc.ProvisioningRelease).To(Equal(plan.Intent().Release))
				Expect(deps.RetainedProvisioning).To(BeIdenticalTo(plan))
				Expect(deps.DeferHBNReadiness).To(Equal(resume || phase == deployment.AfterActivation))
				mode := discovery.VerifyArtifactOrigins
				if phase == deployment.AfterActivation {
					mode = discovery.ArtifactInputsOnly
				}
				Expect(deps.ArtifactSourceMode).To(Equal(mode))
				checks := deps.HBNPermissions(context.Background(), config.Request{}, &inventory.Inventory{})
				Expect(checks).To(ContainElement(HaveField("Message", ContainSubstring("validated retained plan"))))
			}
		}
	})
})
