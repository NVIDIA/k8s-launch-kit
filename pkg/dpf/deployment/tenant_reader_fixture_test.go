// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Stage fixtures supply controller-created children as tenant-owned reads.
// Native transport fixtures continue to exercise tenantClient directly.
type fixtureTenantReader struct {
	*tenantClient
	child func(context.Context, *tenantClient, *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

func (c *fixtureTenantReader) getWorkloadChild(ctx context.Context, parent *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return c.child(ctx, c.tenantClient, parent)
}
func testTenant(reader tenantStageReader) *tenantClient {
	switch c := reader.(type) {
	case *tenantClient:
		return c
	case *fixtureTenantReader:
		return c.tenantClient
	default:
		panic("unexpected test tenant reader")
	}
}
