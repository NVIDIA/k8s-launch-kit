// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Collection ports deliberately omit source verification, exec and mutations.
// Verification belongs to withValidationCurrentReads; these capabilities are
// local to its synchronous callback and are never retained by a runtime session.
type validationCurrentGraphReader interface {
	resourceReader
	resourceLister
}

type validationCurrentManagementReader interface {
	validationCurrentGraphReader
	getHBNRuntimeApplication(context.Context, string, string) (*unstructured.Unstructured, error)
}

type validationCurrentTenantReader interface {
	getWorkloadChild(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	listCompleteNodes(context.Context) ([]unstructured.Unstructured, error)
	listRuntimeObjects(context.Context, string, string) ([]appsv1.DaemonSet, []corev1.Pod, error)
	readPod(context.Context, string, string) (*corev1.Pod, error)
	checkTenantPodExecPermissions(context.Context, string, string) []PreflightCheck
	listValidationSFCObjects(context.Context, string, string, string, string) ([]unstructured.Unstructured, error)
}

type validationManagementReadSource interface {
	VerifyIdentity(context.Context) error
	currentValidationReads() validationCurrentManagementReader
}

type validationTenantReadSource interface {
	VerifyIdentity(context.Context) error
	currentValidationReads() validationCurrentTenantReader
}

// withValidationCurrentReads accepts a finite collection only after both source
// boundaries succeed. This samples boundary consistency, not atomic isolation
// or every transient A->B->A source change. A nil tenant explicitly selects a
// management-only region for independent device evidence.
func withValidationCurrentReads(ctx context.Context, management validationManagementReadSource, tenant validationTenantReadSource, collect func(validationCurrentManagementReader, validationCurrentTenantReader) error) error {
	if ctx == nil || management == nil || collect == nil {
		return fmt.Errorf("bounded validation collection inputs required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	verify := func() error {
		managementErr := management.VerifyIdentity(ctx)
		var tenantErr error
		if tenant != nil {
			tenantErr = tenant.VerifyIdentity(ctx)
		}
		return errors.Join(managementErr, tenantErr, ctx.Err())
	}
	if err := verify(); err != nil {
		return err
	}
	var tenantReads validationCurrentTenantReader
	if tenant != nil {
		tenantReads = tenant.currentValidationReads()
	}
	readErr := collect(management.currentValidationReads(), tenantReads)
	// Do not short circuit closing sources on a collection or first close error.
	// Both verifiers retain the same bounded context, including cancellation.
	return errors.Join(readErr, verify(), ctx.Err())
}

type validationManagementReads struct{ client *APIClient }

func (c *APIClient) currentValidationReads() validationCurrentManagementReader {
	return validationManagementReads{client: c}
}
func (r validationManagementReads) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return r.client.getCurrentResource(ctx, desired)
}
func (r validationManagementReads) List(ctx context.Context, template *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	return r.client.listCurrentResources(ctx, template, selector)
}
func (r validationManagementReads) getHBNRuntimeApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return r.client.getCurrentHBNRuntimeApplication(ctx, namespace, name)
}

type validationTenantReads struct{ client *tenantClient }

func (c *tenantClient) currentValidationReads() validationCurrentTenantReader {
	return validationTenantReads{client: c}
}
func (r validationTenantReads) getWorkloadChild(ctx context.Context, parent *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return r.client.getCurrentWorkloadChild(ctx, parent)
}
func (r validationTenantReads) listCompleteNodes(ctx context.Context) ([]unstructured.Unstructured, error) {
	return r.client.listCompleteNodes(ctx)
}
func (r validationTenantReads) listRuntimeObjects(ctx context.Context, namespace, selector string) ([]appsv1.DaemonSet, []corev1.Pod, error) {
	return r.client.listRuntimeObjects(ctx, namespace, selector)
}
func (r validationTenantReads) readPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return r.client.readPod(ctx, namespace, name)
}
func (r validationTenantReads) checkTenantPodExecPermissions(ctx context.Context, namespace, pod string) []PreflightCheck {
	return r.client.checkTenantPodExecPermissions(ctx, namespace, pod)
}
func (r validationTenantReads) listValidationSFCObjects(ctx context.Context, kind, namespace, name, selector string) ([]unstructured.Unstructured, error) {
	return r.client.listCurrentValidationSFCObjects(ctx, kind, namespace, name, selector)
}
