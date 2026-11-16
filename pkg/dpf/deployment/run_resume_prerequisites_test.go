// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunResumeBindsOperationBeforeCallingPrerequisiteProvider(t *testing.T) {
	var object *corev1.ConfigMap
	var mu sync.Mutex
	namespaceUID := "operator-namespace-uid"
	mutations := 0
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews") {
			fmt.Fprint(w, `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true}}`)
			return
		}
		if r.Method != http.MethodGet {
			mutations++
			t.Errorf("unexpected effect %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Path == "/prefix/api/v1/namespaces/dpf-operator-system" {
			_ = json.NewEncoder(w).Encode(corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "dpf-operator-system", UID: types.UID(namespaceUID)}})
			return
		}
		if object != nil && strings.HasSuffix(r.URL.Path, "/configmaps/"+object.Name) {
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
	})
	inputs := preflightGenerationInputs(t)
	inputs.Management = api.expected
	inputs.Operator.KubernetesVersion = "v1.35.6"
	retained, err := generateFixtureBundle(inputs)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(retained, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	deployment, err := retainedDeploymentObject(execution)
	if err != nil {
		t.Fatal(err)
	}
	state := operationState{
		Binding:   operationBinding{Version: 1, ID: "fixture-operation", BundleDigest: execution.plan.Digest(), Management: api.expected, NamespaceUID: "operator-namespace-uid", DeploymentName: deployment.GetName(), CohortDigest: inventory.Digest("dpf-operation-cohort-v1", execution.plan.Intent().Devices)},
		TenantUID: "tenant-one", TenantSystemUID: "tenant-system", TenantSources: map[string]types.UID{"Secret/tenant/source": "source-one"},
		Workload: []workloadResourceReceipt{{UID: "workload-one"}}, Children: map[string]activationObjectReceipt{}, HoldIntents: map[string]operationProgress{}, Progress: map[string]operationProgress{}, Resets: map[string]operationReset{}, Hosts: map[string]operationHost{},
	}
	state.Management.BundleDigest = state.Binding.BundleDigest
	state.Enrollment.BundleDigest = state.Binding.BundleDigest
	state.Enrollment.TenantUID = state.TenantUID
	state.Enrollment.Secret.UID = "secret-one"
	state.Enrollment.Devices = []enrolledDeviceReceipt{{Serial: "SERIAL-A", DeviceUID: "device-one", DPUNodeUID: "node-one"}}
	state.Software.SoftwareUID = "software-one"
	encoded, err := encodeOperation(state)
	if err != nil {
		t.Fatal(err)
	}
	op := &deploymentOperation{state: state, object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: operationName(deployment), Namespace: deployment.GetNamespace(), UID: types.UID("123e4567-e89b-12d3-a456-426614174006"), ResourceVersion: "1", Labels: map[string]string{operationLabel: "v1"}}, Data: map[string]string{"checkpoint.json": encoded}}}

	for _, test := range []struct {
		name string
		edit func(*RunRequest)
		want string
	}{
		{"bound operation still requires its recorded access", func(*RunRequest) {}, ""},
		{"missing operation", func(*RunRequest) { object = nil }, "operation missing/replaced"},
		{"replaced operation", func(*RunRequest) { object.UID = "123e4567-e89b-12d3-a456-426614174099" }, "operation missing/replaced"},
		{"foreign bundle", func(*RunRequest) {
			state := op.state
			state.Binding.BundleDigest = "sha256:" + strings.Repeat("a", 64)
			state.Management.BundleDigest = state.Binding.BundleDigest
			state.Enrollment.BundleDigest = state.Binding.BundleDigest
			data, e := encodeOperation(state)
			if e != nil {
				t.Fatal(e)
			}
			object.Data["checkpoint.json"] = data
		}, "cluster, bundle or cohort changed"},
		{"replaced namespace", func(*RunRequest) { namespaceUID = "replacement" }, "namespace replaced"},
		{"wrong reference", func(r *RunRequest) { r.Resume.Name = "dpf-op-foreign" }, "resume reference differs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			object = op.object.DeepCopy()
			namespaceUID = "operator-namespace-uid"
			mu.Unlock()
			called := false
			request := imagePrerequisiteRunRequest(t, retained, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
				called = true
				return PrerequisiteVerification{}, fmt.Errorf("provider must not run before exact operation binding")
			})
			request.Access = api.request
			request.Resume = op.ref()
			mu.Lock()
			test.edit(&request)
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			result, err := runWithImageResolver(ctx, request, retainedTestImageResolver)
			mu.Lock()
			defer mu.Unlock()
			if test.want == "" && (result.Operation == nil || result.Operation.UID != op.object.UID) {
				t.Fatal("valid UID-bound operation was not loaded before access verification")
			}
			if err == nil || test.want != "" && !strings.Contains(err.Error(), test.want) || called || mutations != 0 {
				t.Fatalf("result=%+v err=%v providerCalled=%v effects=%d", result, err, called, mutations)
			}
		})
	}
}
