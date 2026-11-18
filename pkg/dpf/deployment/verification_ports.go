// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// These ports belong to observations, rather than inheriting previous stages.
// Permission reviews and physical observations may use bounded transport/session
// work; none of these ports authorizes deployment mutation.
type identityResourceReader interface {
	resourceReader
	VerifyIdentity(context.Context) error
}
type resourceLister interface {
	List(context.Context, *unstructured.Unstructured, string) ([]unstructured.Unstructured, error)
}
type resourceDiscoveryReader interface {
	discoverResourceList(context.Context, string) (*metav1.APIResourceList, error)
}
type operatorConfigReader interface {
	resourceLister
	resourceDiscoveryReader
}
type operatorCRDReader interface {
	resourceReader
	resourceDiscoveryReader
}
type enrollmentScopeReader interface {
	resourceLister
	resourceDiscoveryReader
	VerifyIdentity(context.Context) error
	getOperatorCRD(context.Context, string) (*unstructured.Unstructured, error)
}
type enrollmentResourceReader interface {
	identityResourceReader
	enrollmentScopeReader
}
type enrollmentPhysicalVerifier interface {
	verifyEnrollmentBMCs(context.Context, []enrollmentBMC, time.Duration, *executionJournal) error
}
type enrollmentBoundaryReader interface {
	enrollmentResourceReader
	enrollmentPhysicalVerifier
}
type enrollmentBindingReader interface {
	tenantSourceReader
	matchesBinding(generation.ManagementIdentity, string) bool
}
type controlResourceReader interface {
	identityResourceReader
	resourceLister
}
type activationSoftwareReader interface {
	identityResourceReader
	observeRegistrySnapshot(context.Context, string) (registrySnapshot, error)
}
type activationControlReader interface {
	enrollmentResourceReader
	activationSoftwareReader
}
type activationBoundaryReader interface {
	activationControlReader
	enrollmentPhysicalVerifier
}
type activationObservationReader interface {
	identityResourceReader
	resourceLister
}
type managementLeaseReader interface {
	identityResourceReader
	enrollmentBindingReader
}
type workloadPermissionChecker interface {
	checkCallerPermissions(context.Context, preflightPlan) PreflightResult
}

type controlPlanePreparationReader interface {
	enrollmentBoundaryReader
	enrollmentBindingReader
}
type tenantNetworkChecker interface {
	checkTenantNetwork(context.Context, bootstrap.Tenant, tenantNetworkRunner, string, time.Duration) ([]inventory.Check, error)
}

type operationReader interface {
	getOperation(context.Context, string, string) (*corev1.ConfigMap, error)
}
type resumePrerequisiteReader interface {
	activationControlReader
	operationReader
}
type tenantConnectionReader interface {
	resourceReader
	enrollmentBindingReader
}
type retainedTenantConnector interface {
	connectTenant(context.Context, *generation.Plan, types.UID, *Reconciler, *executionJournal, time.Duration, time.Duration) (tenantStageReader, error)
}
type receiptRestorationReader interface {
	activationSoftwareReader
	retainedTenantConnector
}
