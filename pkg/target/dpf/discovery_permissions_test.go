// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type permissionClientStub struct {
	check func(context.Context, deployment.ManagementPreflightRequest) deployment.PreflightResult
}

func (c permissionClientStub) CheckCallerPermissions(ctx context.Context, request deployment.ManagementPreflightRequest) deployment.PreflightResult {
	return c.check(ctx, request)
}

var _ = Describe("HBN discovery caller permission adapter", func() {
	var observer hbnPermissionObserver
	var request config.Request
	var current *inventory.Inventory
	var clientCalls int
	var fetched []string
	var result deployment.PreflightResult
	BeforeEach(func() {
		input := generationSite()
		current = input.Site.Inventory
		// Deliberately unusable saved facts prove this adapter consumes the new
		// discovery rather than authorizing a previously selected cluster.
		input.Site.Inventory = &inventory.Inventory{Management: &inventory.Management{SystemNamespaceUID: "old-cluster"}}
		request = config.Request{Input: input, DPFNamespace: "dpf-operator-system"}
		clientCalls, fetched = 0, nil
		result = deployment.PreflightResult{Ready: true, Checks: []deployment.PreflightCheck{{
			ID: "caller.rbac.selected.create", State: "passed", Message: "The caller can create the selected resource.",
		}}}
		observer = hbnPermissionObserver{
			fetchChart: func(_ context.Context, address string) ([]byte, error) {
				fetched = append(fetched, address)
				if strings.HasSuffix(address, "dpf-operator-v26.8.0.tgz") {
					return os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
				}
				return os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
			},
			newClient: func(_ context.Context, got config.Request, identity generation.ManagementIdentity, version string) (callerPermissionClient, error) {
				clientCalls++
				Expect(identity.SystemNamespaceUID).To(Equal(current.Management.SystemNamespaceUID))
				Expect(identity.TrustDigest).To(Equal(current.Management.TrustDigest))
				Expect(version).To(Equal(current.Management.KubernetesVersion))
				Expect(got.Input.Site.Inventory.Management.SystemNamespaceUID).To(Equal("old-cluster"))
				return permissionClientStub{check: func(_ context.Context, req deployment.ManagementPreflightRequest) deployment.PreflightResult {
					Expect(req.Bundle).NotTo(BeNil())
					plan, err := generation.LoadPlan(req.Bundle)
					Expect(err).NotTo(HaveOccurred())
					Expect(plan.Intent().Management.SystemNamespaceUID).To(Equal(current.Management.SystemNamespaceUID))
					Expect(plan.Stages()).To(HaveLen(7))
					return result
				}}, nil
			},
		}
	})

	It("derives permissions from a verified current bundle without replacing the saved inventory", func() {
		checks := observer.observe(context.Background(), request, current)
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].State).To(Equal("passed"))
		Expect(checks[0].ID).To(HavePrefix("dpf.hbn.management.caller-rbac."))
		Expect(checks[0].ReasonCode).To(Equal("CALLER_PERMISSION_VERIFIED"))
		Expect(checks[0].RequiredPhase).To(Equal("deploy"))
		Expect(clientCalls).To(Equal(1))
		Expect(fetched).To(HaveLen(2))
		Expect(request.Input.Site.Inventory.Management.SystemNamespaceUID).To(Equal("old-cluster"))
	})

	It("distinguishes a denied permission from an incomplete authorization review", func() {
		result = deployment.PreflightResult{Checks: []deployment.PreflightCheck{
			{ID: "caller.rbac.denied", State: "blocked", ReasonCode: "CALLER_PERMISSION_MISSING", Message: "Cannot create the selected resource."},
			{ID: "caller.rbac.unmeasured", State: "blocked", ReasonCode: "RBAC_REVIEW_INCOMPLETE", Message: "The API did not return an authorization decision."},
		}}
		checks := observer.observe(context.Background(), request, current)
		Expect(checks).To(HaveLen(2))
		Expect(checks[0].State).To(Equal("failed"))
		Expect(checks[1].State).To(Equal("unknown"))
		Expect(checks[1].ReasonCode).To(Equal("RBAC_REVIEW_INCOMPLETE"))
	})

	It("blocks missing current identities before downloading charts or opening credentials", func() {
		checks := observer.observe(context.Background(), request, nil)
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(fetched).To(BeEmpty())
		Expect(clientCalls).To(BeZero())
		current.DPUs = nil
		checks = observer.observe(context.Background(), request, current)
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_PLAN_UNAVAILABLE"))
		Expect(fetched).To(BeEmpty())
		Expect(clientCalls).To(BeZero())
	})

	It("rejects altered chart bytes before any authorization claim", func() {
		observer.fetchChart = func(context.Context, string) ([]byte, error) { return []byte("wrong archive"), nil }
		checks := observer.observe(context.Background(), request, current)
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_PLAN_INVALID"))
		Expect(clientCalls).To(BeZero())
	})

	It("does not expose transport credentials in chart or client failures", func() {
		fetch := observer.fetchChart
		observer.fetchChart = func(context.Context, string) ([]byte, error) {
			return nil, errors.New("https://user:secret@example.test")
		}
		checks := observer.observe(context.Background(), request, current)
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_OPERATOR_CHART_UNAVAILABLE"))
		Expect(checks[0].Message).NotTo(ContainSubstring("secret"))
		Expect(clientCalls).To(BeZero())
		observer.fetchChart = fetch
		observer.newClient = func(context.Context, config.Request, generation.ManagementIdentity, string) (callerPermissionClient, error) {
			return nil, errors.New("token=secret")
		}
		checks = observer.observe(context.Background(), request, current)
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_MANAGEMENT_UNAVAILABLE"))
		Expect(checks[0].Message).NotTo(ContainSubstring("secret"))
	})

	It("rejects empty and contradictory authorization results", func() {
		result = deployment.PreflightResult{Ready: true}
		checks := observer.observe(context.Background(), request, current)
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_RESULT_INCOMPLETE"))
		result = deployment.PreflightResult{Ready: true, Checks: []deployment.PreflightCheck{{
			ID: "caller.rbac.denied", State: "blocked", ReasonCode: "CALLER_PERMISSION_MISSING", Message: "Cannot create the selected resource.",
		}}}
		checks = observer.observe(context.Background(), request, current)
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_RESULT_INCOMPLETE"))
	})

	It("does not authorize after cancellation during chart inspection", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fetch := observer.fetchChart
		observer.fetchChart = func(ctx context.Context, address string) ([]byte, error) {
			data, err := fetch(ctx, address)
			cancel()
			return data, err
		}
		checks := observer.observe(ctx, request, current)
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_CANCELLED"))
		Expect(clientCalls).To(BeZero())
	})

	It("discards passed reviews when cancellation occurs before the checker returns", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		observer.newClient = func(context.Context, config.Request, generation.ManagementIdentity, string) (callerPermissionClient, error) {
			return permissionClientStub{check: func(context.Context, deployment.ManagementPreflightRequest) deployment.PreflightResult {
				cancel()
				return result
			}}, nil
		}
		checks := observer.observe(ctx, request, current)
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_CANCELLED"))
	})

	It("treats a missing management client as unmeasured", func() {
		observer.newClient = func(context.Context, config.Request, generation.ManagementIdentity, string) (callerPermissionClient, error) {
			return nil, nil
		}
		checks := observer.observe(context.Background(), request, current)
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("CALLER_RBAC_MANAGEMENT_UNAVAILABLE"))
	})
})
