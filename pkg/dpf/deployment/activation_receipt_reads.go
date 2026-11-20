// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// acceptanceReceiptReadSource owns the source boundary of a synchronous receipt
// collection. Effect-facing clients retain their per-operation identity guards.
type acceptanceReceiptReadSource interface {
	activationBoundaryReader
	withAcceptanceReceiptReads(context.Context, acceptanceTenantReadSource, func(activationControlReader, acceptanceTenantReceiptReader) error) error
}

func (c *APIClient) withAcceptanceReceiptReads(ctx context.Context, tenant acceptanceTenantReadSource, collect func(activationControlReader, acceptanceTenantReceiptReader) error) error {
	if c == nil || ctx == nil || tenant == nil || collect == nil {
		return fmt.Errorf("acceptance receipt collection requires bound sources and a collector")
	}
	verify := func() error {
		return errors.Join(c.VerifyIdentity(ctx), tenant.VerifyIdentity(ctx), ctx.Err())
	}
	if err := verify(); err != nil {
		return err
	}
	readErr := collect(activationReceiptReads{client: c}, tenant.currentAcceptanceReceiptReads())
	// Closing both sources is required even when collection failed, so drift
	// cannot be hidden by an earlier ordinary read failure.
	return errors.Join(readErr, verify())
}

// This view contains no mutation, operation-store or physical capabilities.
// Retained receipt validators own explicit inner identity checks; individual
// object reads do not recursively open another source verification boundary.
// API discovery remains current on every read; no mapping or object is cached.
type activationReceiptReads struct{ client *APIClient }

func (r activationReceiptReads) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return r.client.getCurrentResource(ctx, desired)
}
func (r activationReceiptReads) List(ctx context.Context, template *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	return r.client.listCurrentResources(ctx, template, selector)
}
func (r activationReceiptReads) VerifyIdentity(ctx context.Context) error {
	return r.client.VerifyIdentity(ctx)
}
func (r activationReceiptReads) discoverResourceList(ctx context.Context, gv string) (*metav1.APIResourceList, error) {
	return r.client.discoverResourceList(ctx, gv)
}
func (r activationReceiptReads) getOperatorCRD(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return r.client.getCurrentOperatorCRD(ctx, name)
}
func (r activationReceiptReads) observeRegistrySnapshot(ctx context.Context, namespace string) (registrySnapshot, error) {
	return r.client.observeRegistrySnapshot(ctx, namespace)
}

// acceptanceTenantReadSource supplies current object reads without acquiring a
// second tenant source boundary for each child in an already bounded collection.
type acceptanceTenantReadSource interface {
	tenantVerificationReader
	currentAcceptanceReceiptReads() acceptanceTenantReceiptReader
}

func (c *tenantClient) currentAcceptanceReceiptReads() acceptanceTenantReceiptReader {
	return activationTenantReceiptReads{client: c}
}

type activationTenantReceiptReads struct{ client *tenantClient }

func (r activationTenantReceiptReads) VerifyIdentity(ctx context.Context) error {
	return r.client.VerifyIdentity(ctx)
}
func (r activationTenantReceiptReads) matchesTenantBinding(source tenantSourceReader, expected controllercontract.TenantReadinessExpected, uid types.UID) bool {
	return r.client.matchesTenantBinding(source, expected, uid)
}
func (r activationTenantReceiptReads) matchesTenantTarget(expected controllercontract.TenantReadinessExpected, uid types.UID) bool {
	return r.client.matchesTenantTarget(expected, uid)
}
func (r activationTenantReceiptReads) clusterIdentity() types.UID { return r.client.clusterIdentity() }
func (r activationTenantReceiptReads) expectation() controllercontract.TenantReadinessExpected {
	return r.client.expectation()
}
func (r activationTenantReceiptReads) getWorkloadChild(ctx context.Context, parent *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return r.client.getCurrentWorkloadChild(ctx, parent)
}

// The receipt callback can collect both child objects and complete Node lists;
// source/target identity remains available only through its explicit verifier.
type acceptanceTenantReceiptReader interface {
	tenantVerificationReader
	currentHostLeaseTenantReader
}

func (r activationTenantReceiptReads) listCompleteNodes(ctx context.Context) ([]unstructured.Unstructured, error) {
	return r.client.listCompleteNodes(ctx)
}

type acceptanceLeaseSnapshotReader func(context.Context, resourceReader, currentHostLeaseTenantReader) (hostWorkloadLeaseSnapshot, error)
