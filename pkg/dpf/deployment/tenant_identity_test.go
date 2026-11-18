// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestTenantIdentityDiagnosticUsesMeasuredDriftOnly(t *testing.T) {
	const canary = "private-token-kubeconfig-api-canary"
	for _, tc := range []struct {
		name, code string
		cause      error
	}{
		{name: "confirmed drift", code: "TENANT_API_IDENTITY_CHANGED", cause: fmt.Errorf("%s: %w", canary, errTenantIdentityChanged)},
		{name: "unavailable", code: "TENANT_API_IDENTITY_UNVERIFIED", cause: apierrors.NewServiceUnavailable(canary)},
		{name: "denied", code: "TENANT_API_IDENTITY_UNVERIFIED", cause: apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "private-name", errors.New(canary))},
		{name: "unknown text is not evidence", code: "TENANT_API_IDENTITY_UNVERIFIED", cause: errors.New("tenant API identity changed: " + canary)},
		{name: "canceled", code: "TENANT_API_IDENTITY_CANCELED", cause: fmt.Errorf("%s: %w", canary, context.Canceled)},
		{name: "deadline", code: "TENANT_API_IDENTITY_CANCELED", cause: context.DeadlineExceeded},
		{name: "canceled drift", code: "TENANT_API_IDENTITY_CANCELED", cause: errors.Join(errTenantIdentityChanged, context.Canceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tenantIdentityDiagnostic(context.Background(), tc.cause)
			encoded, err := json.Marshal(got)
			if err != nil || got.ReasonCode != tc.code || strings.Contains(got.Error()+string(encoded), canary) || strings.Contains(string(encoded), "private-name") {
				t.Fatalf("unsafe/wrong diagnostic: %v %s", got, encoded)
			}
			if !errors.Is(got, tc.cause) {
				t.Fatal("private cause lost")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := tenantIdentityDiagnostic(ctx, errors.New(canary))
	if got.ReasonCode != "TENANT_API_IDENTITY_CANCELED" || !errors.Is(got, context.Canceled) || strings.Contains(got.Error(), canary) {
		t.Fatal("canceled context was lost behind a read failure")
	}
}

func TestTenantIdentityVerifierMarksOnlyBoundDifferences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed bool
		edit    func(*tenantClient, *tenantSourceStub)
	}{
		{name: "tenant UID", changed: true, edit: func(c *tenantClient, _ *tenantSourceStub) { c.systemUID = "previous-tenant" }},
		{name: "DPUCluster UID", changed: true, edit: func(c *tenantClient, _ *tenantSourceStub) { c.dpuClusterUID = "previous-cluster" }},
		{name: "source fingerprint", changed: true, edit: func(_ *tenantClient, s *tenantSourceStub) { s.sources.secret.UID = "new-secret" }},
		{name: "missing tenant baseline", edit: func(c *tenantClient, _ *tenantSourceStub) { c.systemUID = "" }},
		{name: "missing cluster baseline", edit: func(c *tenantClient, _ *tenantSourceStub) { c.dpuClusterUID = "" }},
		{name: "missing source baseline", edit: func(c *tenantClient, _ *tenantSourceStub) { c.sourceDigest = [32]byte{} }},
		{name: "unready source", edit: func(_ *tenantClient, s *tenantSourceStub) {
			s.sources.observed.DPUCluster.Object["status"] = map[string]any{}
		}},
		{name: "failed read", edit: func(c *tenantClient, _ *tenantSourceStub) {
			c.source = tenantWaitSourceFunc(func(context.Context, controllercontract.TenantReadinessExpected) (tenantSources, error) {
				return tenantSources{}, apierrors.NewServiceUnavailable("private-api-canary")
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected, sources := tenantSourceFixture(t)
			source := &tenantSourceStub{sources: sources}
			client, _ := tenantIdentityClientFixture(t, expected, source)
			tc.edit(client, source)
			err := client.VerifyIdentity(context.Background())
			if err == nil || errors.Is(err, errTenantIdentityChanged) != tc.changed {
				t.Fatalf("changed=%v err=%v", tc.changed, err)
			}
		})
	}
}
