// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	dpfdiscovery "github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSoftwarePermissionsScopeAndFailClosed(t *testing.T) {
	for _, scenario := range []string{"allowed", "denied", "contradictory", "unavailable", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			var requests []authorizationv1.ResourceAttributes
			client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				request := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				if request.Spec.ResourceAttributes == nil {
					t.Fatal("missing resource authorization scope")
				}
				attributes := *request.Spec.ResourceAttributes
				requests = append(requests, attributes)
				status := authorizationv1.SubjectAccessReviewStatus{Allowed: true}
				if attributes.Subresource == "portforward" {
					switch scenario {
					case "denied":
						status.Allowed = false
					case "contradictory":
						status.Denied = true
					case "unavailable":
						return true, nil, errors.New("private transport detail")
					case "incomplete":
						status.EvaluationError = "private authorization detail"
					}
				}
				return true, &authorizationv1.SelfSubjectAccessReview{Status: status}, nil
			})
			api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
			desired := &unstructured.Unstructured{Object: map[string]any{"apiVersion": blueFieldSoftwareAPIVersion, "kind": "BlueFieldSoftware", "metadata": map[string]any{"namespace": "workload", "name": "retained-software"}}}
			checks := api.checkSoftwarePermissions(context.Background(), desired, "operator")
			wanted := []authorizationv1.ResourceAttributes{
				{Verb: "get", Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Resource: "bluefieldsoftwares", Namespace: "workload", Name: "retained-software"},
				{Verb: "create", Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Resource: "bluefieldsoftwares", Namespace: "workload"},
				{Verb: "create", Version: "v1", Resource: "pods", Subresource: "portforward", Namespace: "operator", Name: "bfb-registry"},
			}
			if !reflect.DeepEqual(requests, wanted) {
				t.Fatalf("authorization scope = %#v", requests)
			}
			if len(checks) != 3 || checks[0].State != "passed" || checks[1].State != "passed" || (checks[2].State == "passed") != (scenario == "allowed") {
				t.Fatalf("checks=%#v", checks)
			}
			for _, check := range checks {
				if strings.Contains(check.Message, "private") {
					t.Fatalf("server detail leaked: %#v", check)
				}
			}
			if !strings.HasSuffix(checks[2].ID, ".portforward") {
				t.Fatalf("subresource absent from check identity: %#v", checks[2])
			}
		})
	}
}

func TestSoftwarePermissionsInvalidInputsHaveNoAPIEffects(t *testing.T) {
	client := fake.NewSimpleClientset()
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	desired := &unstructured.Unstructured{Object: map[string]any{"apiVersion": blueFieldSoftwareAPIVersion, "kind": "BlueFieldSoftware", "metadata": map[string]any{"namespace": "workload", "name": "software"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var missingContext context.Context
	for _, checks := range [][]PreflightCheck{
		api.checkSoftwarePermissions(missingContext, desired, "operator"),
		api.checkSoftwarePermissions(context.Background(), nil, "operator"),
		api.checkSoftwarePermissions(context.Background(), desired, ""),
		api.checkSoftwarePermissions(ctx, desired, "operator"),
	} {
		if len(checks) != 1 || checks[0].State == "passed" {
			t.Fatalf("invalid input accepted: %#v", checks)
		}
	}
	if len(client.Actions()) != 0 {
		t.Fatal("invalid input caused API calls")
	}
}
