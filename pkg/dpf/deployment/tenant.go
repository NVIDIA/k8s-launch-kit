// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// connectRetainedTenant is the handoff after the tenant stage has created or
// exactly verified its DPUCluster. It waits for controller resources, then opens
// and verifies the independent tenant API. It cannot allocate a VIP, create a
// DPUCluster, enroll devices, or attest complete HBN deployment readiness.
func connectRetainedTenant(ctx context.Context, management *APIClient, plan *generation.Plan, establishedUID types.UID,
	reconciler nativeObjectReconciler, journal *executionJournal, interval, requestTimeout time.Duration,
) (*tenantClient, error) {
	if ctx == nil || management == nil || plan == nil || journal == nil || reconciler == nil || establishedUID == "" || interval <= 0 || requestTimeout <= 0 {
		return nil, fmt.Errorf("tenant access requires the retained plan, bound management client, established cluster UID and execution controls")
	}
	if plan.Digest() == "" || journal.digest != plan.Digest() || reconciler.BundleDigest() != plan.Digest() {
		return nil, fmt.Errorf("tenant access and execution controls must identify the same retained bundle")
	}
	intent := plan.Intent()
	if !management.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return nil, fmt.Errorf("tenant source management binding differs from the retained plan")
	}
	expected, desired, err := tenantPlanInputs(plan)
	if err != nil {
		return nil, err
	}
	if _, err := waitTenantReady(ctx, management, management, expected, desired, establishedUID, reconciler, journal, interval); err != nil {
		return nil, err
	}
	event := executionEvent{Stage: generation.StageTenant, Operation: "tenant-api", Subject: expected.Namespace + "/" + expected.Name, State: "attempted", ReasonCode: "TENANT_API_INSPECTION_STARTED", Message: "Authenticate through controller-created tenant credentials and verify the independent cluster identity."}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	client, err := newTenantClient(ctx, management, expected, string(establishedUID), requestTimeout)
	if err != nil {
		return nil, journalFailure(journal, event, false, "TENANT_API_NOT_VERIFIED", "Tenant API access or its bound source identity could not be verified; dependent stages remain stopped.", err)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return nil, journalFailure(journal, event, false, "TENANT_API_IDENTITY_CHANGED", "Tenant API or source identity did not remain valid through the access check.", err)
	}
	event.State, event.ReasonCode = "confirmed", "TENANT_API_VERIFIED"
	event.Message = "Tenant API authentication, source binding, Kubernetes version and cluster identity are verified; enrollment and workload readiness require later stages."
	if err := journal.record(event); err != nil {
		return nil, err
	}
	return client, nil
}

func tenantPlanInputs(plan *generation.Plan) (TenantReadinessExpected, *unstructured.Unstructured, error) {
	if plan == nil {
		return TenantReadinessExpected{}, nil, fmt.Errorf("validated tenant plan is required")
	}
	intent := plan.Intent()
	tenant := intent.Bootstrap.Tenant
	expected := TenantReadinessExpected{
		Name: tenant.Name, Namespace: tenant.Namespace, Labels: tenant.Labels, MaxNodes: int64(tenant.MaxNodes),
		KubernetesVersion: intent.Bootstrap.TenantKubernetesVersion,
		VIP:               tenant.VIP, Interface: tenant.Interface, VirtualRouterID: int64(tenant.RouterID), NodeSelector: tenant.NodeSelector,
		PodCIDR: intent.Bootstrap.PodCIDR, ServiceCIDR: tenant.ServiceCIDR,
	}
	if err := validateTenantExpectation(expected); err != nil {
		return TenantReadinessExpected{}, nil, err
	}
	var desired *unstructured.Unstructured
	for _, stage := range plan.Stages() {
		if stage.Stage().ID != generation.StageTenant {
			continue
		}
		for _, document := range stage.Documents() {
			object := document.ObjectCopy()
			if desired != nil || object.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || object.GetKind() != "DPUCluster" || object.GetName() != tenant.Name || object.GetNamespace() != tenant.Namespace {
				return TenantReadinessExpected{}, nil, fmt.Errorf("retained tenant stage must contain the exact selected DPUCluster")
			}
			desired = object
		}
	}
	if desired == nil {
		return TenantReadinessExpected{}, nil, fmt.Errorf("retained tenant stage has no selected DPUCluster")
	}
	return expected, desired, nil
}
