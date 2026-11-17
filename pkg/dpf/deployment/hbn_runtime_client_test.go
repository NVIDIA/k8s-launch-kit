// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
)

func TestHBNRuntimeNativeApplicationReaderAndPermissions(t *testing.T) {
	var reviews []authorizationv1.ResourceAttributes
	fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prefix/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			if r.Method != "POST" {
				t.Error("SSAR requires create")
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			if err != nil {
				t.Error(err)
				return
			}
			object, _, err := kubescheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
			request, ok := object.(*authorizationv1.SelfSubjectAccessReview)
			if err != nil || !ok || request.Spec.ResourceAttributes == nil || request.Spec.NonResourceAttributes != nil {
				t.Errorf("unexpected permission shape: %v", err)
				http.Error(w, "invalid", 500)
				return
			}
			reviews = append(reviews, *request.Spec.ResourceAttributes)
			_ = json.NewEncoder(w).Encode(&authorizationv1.SelfSubjectAccessReview{TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"}, Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}})
		case "/prefix/apis/argoproj.io/v1alpha1/namespaces/operator-system/applications/cluster-service":
			if r.Method != "GET" {
				t.Error("Application mutation prohibited")
			}
			fmt.Fprint(w, `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"namespace":"operator-system","name":"cluster-service","uid":"app-uid","resourceVersion":"2"}}`)
		default:
			t.Errorf("unexpected runtime management access %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := NewAPIClient(ctx, fixture.request, fixture.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	if !runtimePermissionsPassed(client.checkHBNRuntimeAccess(ctx, "selected-workload", "operator-system", "cluster-service")) {
		t.Fatal("scoped authorization failed")
	}
	if len(reviews) != 3 {
		t.Fatalf("reviews=%+v", reviews)
	}
	for _, attributes := range reviews {
		if attributes.Resource == "applications" {
			if attributes.Verb != "get" || attributes.Namespace != "operator-system" || attributes.Name != "cluster-service" || attributes.Group != "argoproj.io" {
				t.Fatal("Application permission broadened")
			}
		} else if attributes.Group != "svc.dpu.nvidia.com" || attributes.Verb != "list" || attributes.Namespace != "selected-workload" || attributes.Name != "" || attributes.Resource != "dpuservices" && attributes.Resource != "dpuserviceinterfaces" {
			t.Fatalf("unexpected runtime permission %+v", attributes)
		}
	}
	object, err := client.getHBNRuntimeApplication(ctx, "operator-system", "cluster-service")
	if err != nil || object.GetUID() != "app-uid" {
		t.Fatalf("runtime Application reader=%v err=%v", object, err)
	}
	if fixture.writes.Load() != 3 {
		t.Fatal("runtime management read issued writes beyond the three scoped SSARs")
	}
}

func TestHBNRuntimeManagementListsRequireCompleteBoundCollections(t *testing.T) {
	for _, scenario := range []string{"complete", "changed-rv", "repeated-cursor", "duplicate-uid", "foreign-namespace", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			pages := 0
			fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prefix/apis/svc.dpu.nvidia.com/v1alpha1" {
					fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"svc.dpu.nvidia.com/v1alpha1","resources":[{"name":"dpuservices","kind":"DPUService","namespaced":true,"verbs":["list"]}]}`)
					return
				}
				if r.URL.Path != "/prefix/apis/svc.dpu.nvidia.com/v1alpha1/namespaces/workloads/dpuservices" || r.Method != "GET" || r.URL.Query().Get("limit") != "64" || r.URL.Query().Get("labelSelector") != "selected=true" {
					t.Errorf("list broadened: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
					return
				}
				pages++
				rv, cursor := "10", ""
				name, uid, namespace := "service-a", "uid-a", "workloads"
				if pages == 1 && scenario != "complete" && scenario != "foreign-namespace" && scenario != "oversized" {
					cursor = "next"
				}
				if pages > 1 {
					name, uid = "service-b", "uid-b"
					if scenario == "changed-rv" {
						rv = "11"
					}
					if scenario == "repeated-cursor" {
						cursor = "next"
					}
					if scenario == "duplicate-uid" {
						uid = "uid-a"
					}
				}
				if scenario == "foreign-namespace" {
					namespace = "foreign"
				}
				item := map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUService", "metadata": map[string]any{"name": name, "namespace": namespace, "uid": uid, "resourceVersion": "2"}}
				items := []any{item}
				if scenario == "oversized" {
					items = make([]any, hbnRuntimePageSize+1)
					for i := range items {
						items[i] = item
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUServiceList", "metadata": map[string]any{"resourceVersion": rv, "continue": cursor}, "items": items})
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			client, err := NewAPIClient(ctx, fixture.request, fixture.expected, "v1.35.6")
			if err != nil {
				t.Fatal(err)
			}
			objects, err := client.listHBNRuntimeObjects(ctx, "DPUService", "workloads", "selected=true")
			if scenario == "complete" {
				if err != nil || len(objects) != 1 {
					t.Fatalf("complete list=%+v err=%v", objects, err)
				}
			} else if err == nil || objects != nil {
				t.Fatalf("partial list accepted: %+v %v", objects, err)
			}
			if fixture.writes.Load() != 0 {
				t.Fatal("listing issued an effect")
			}
			if _, err := client.listHBNRuntimeObjects(ctx, "DPUService", "", "selected=true"); err == nil {
				t.Fatal("all-namespace list accepted")
			}
			if _, err := client.listHBNRuntimeObjects(ctx, "Secret", "workloads", ""); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("arbitrary runtime kind accepted")
			}
		})
	}
}
