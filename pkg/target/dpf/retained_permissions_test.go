// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"os"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("retained deployment caller authorization", func() {
	It("uses the exact retained bundle and blocks current intent changes before authorization", func() {
		original := deploymentSite()
		loaded, retained := generatedUseCaseSite(original)
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(loaded.Path, publicFixtureBytes(loaded), 0600)).To(Succeed())
		input, err := config.LoadInputs(loaded.Path, "")
		Expect(err).NotTo(HaveOccurred())
		clientCalls, downloads := 0, 0
		observer := hbnPermissionObserver{
			fetchChart: func(context.Context, string) ([]byte, error) { downloads++; return nil, nil },
			newClient: func(_ context.Context, _ config.Request, identity generation.ManagementIdentity, version string) (callerPermissionClient, error) {
				clientCalls++
				Expect(identity).To(Equal(plan.Intent().Management))
				Expect(version).To(Equal(plan.Intent().KubernetesVersion))
				return permissionClientStub{check: func(_ context.Context, request deployment.ManagementPreflightRequest) deployment.PreflightResult {
					Expect(request.Bundle).To(BeIdenticalTo(retained))
					return deployment.PreflightResult{Ready: true, Checks: []deployment.PreflightCheck{{ID: "caller.selected.create", State: "passed", Message: "Current selected authorization was verified."}}}
				}}, nil
			},
		}
		request := config.Request{Input: input, DPFNamespace: "dpf-operator-system"}
		observe := observer.observeRetained(retained, plan)
		checks := observe(context.Background(), request, input.Site.Inventory)
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].State).To(Equal("passed"))
		Expect(clientCalls).To(Equal(1))
		Expect(downloads).To(BeZero())
		settings := config.Configuration(input)
		memory := "8Gi"
		settings.HBN = &config.HBNOverrides{Memory: &memory}
		request.Input, err = config.WithConfiguration(input, settings)
		Expect(err).NotTo(HaveOccurred())
		checks = observe(context.Background(), request, input.Site.Inventory)
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_PLAN_UNAVAILABLE"))
		Expect(clientCalls).To(Equal(1), "changed intent must not authorize a different plan")
		Expect(downloads).To(BeZero())
	})
})
