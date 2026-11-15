// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTenantRuntimePermissionsExactScopeAndFailClosed(t *testing.T) {
	for _, scenario := range []string{"allowed", "denied", "contradictory", "evaluation-error", "unavailable", "nil-review", "cancel-during-last-review"} {
		t.Run(scenario, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			var requests []authorizationv1.ResourceAttributes
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				request, ok := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				if !ok || request.Spec.ResourceAttributes == nil || request.Spec.NonResourceAttributes != nil {
					t.Fatal("expected resource-only tenant SSAR")
				}
				attributes := *request.Spec.ResourceAttributes
				requests = append(requests, attributes)
				if scenario == "cancel-during-last-review" && len(requests) == 4 {
					cancel()
				}
				status := authorizationv1.SubjectAccessReviewStatus{Allowed: true}
				switch scenario {
				case "denied":
					status.Allowed = false
				case "contradictory":
					status.Denied = true
				case "evaluation-error":
					status.EvaluationError = "private authorization detail"
				case "unavailable":
					return true, nil, errors.New("private transport detail")
				case "nil-review":
					return true, nil, nil
				}
				return true, &authorizationv1.SelfSubjectAccessReview{Status: status}, nil
			})
			// The management-side TenantControlPlane namespace is not necessarily
			// the workload namespace that contains HBN runtime resources.
			tenant := &tenantClient{api: client, expected: TenantReadinessExpected{Namespace: "control-plane"}}

			checks := append(checkTenantRuntimeReadPermissions(ctx, tenant, "dpf-system"), checkTenantPodExecPermissions(ctx, tenant, "dpf-system", "hbn-0")...)
			wantRequests := []authorizationv1.ResourceAttributes{
				{Verb: "list", Group: "apps", Version: "v1", Resource: "daemonsets", Namespace: "dpf-system"},
				{Verb: "list", Version: "v1", Resource: "pods", Namespace: "dpf-system"},
				{Verb: "get", Version: "v1", Resource: "pods", Namespace: "dpf-system", Name: "hbn-0"},
				{Verb: "create", Version: "v1", Resource: "pods", Subresource: "exec", Namespace: "dpf-system", Name: "hbn-0"},
			}
			if !reflect.DeepEqual(requests, wantRequests) {
				t.Fatalf("SSAR attributes = %#v, want %#v", requests, wantRequests)
			}
			if len(client.Actions()) != len(wantRequests) {
				t.Fatalf("unexpected non-SSAR API actions: %#v", client.Actions())
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "create" || action.GetResource().Resource != "selfsubjectaccessreviews" {
					t.Fatalf("performed prohibited tenant API action: %#v", action)
				}
			}
			for _, check := range checks {
				if strings.Contains(check.Message, "private") {
					t.Fatalf("authorization detail leaked: %#v", check)
				}
				wantPassed := scenario == "allowed" || (scenario == "cancel-during-last-review" && len(requests) == 4 && check.ID != "tenant.rbac.runtime.pod.exec")
				if wantPassed != (check.State == "passed") {
					t.Fatalf("scenario %s unexpectedly classified: %#v", scenario, check)
				}
			}
		})
	}
}

func TestTenantRuntimePermissionsInvalidInputsHaveNoAPIEffects(t *testing.T) {
	client := fake.NewSimpleClientset()
	tenant := &tenantClient{api: client, expected: TenantReadinessExpected{Namespace: "tenant-a"}}
	valid, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	canceled, cancelCanceled := context.WithCancel(valid)
	cancelCanceled()
	var missingContext context.Context
	for _, checks := range [][]PreflightCheck{
		checkTenantRuntimeReadPermissions(missingContext, tenant, "tenant-a"),
		checkTenantRuntimeReadPermissions(context.Background(), tenant, "tenant-a"),
		checkTenantRuntimeReadPermissions(canceled, tenant, "tenant-a"),
		checkTenantRuntimeReadPermissions(valid, nil, "tenant-a"),
		checkTenantRuntimeReadPermissions(valid, tenant, "Invalid_Namespace"),
		checkTenantPodExecPermissions(valid, tenant, "tenant-a", "bad/pod"),
	} {
		if len(checks) != 1 || checks[0].State == "passed" {
			t.Fatalf("invalid input accepted: %#v", checks)
		}
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("invalid input caused API calls: %#v", client.Actions())
	}
}
