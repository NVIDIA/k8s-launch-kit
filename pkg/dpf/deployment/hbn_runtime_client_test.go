// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHBNRuntimeNativeApplicationReader(t *testing.T) {
	fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/apis/argoproj.io/v1alpha1/namespaces/operator-system/applications/cluster-service" || r.Method != "GET" {
			t.Errorf("unexpected runtime Application access %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"namespace":"operator-system","name":"cluster-service","uid":"app-uid","resourceVersion":"2"}}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := NewAPIClient(ctx, fixture.request, fixture.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	object, err := client.getHBNRuntimeApplication(ctx, "operator-system", "cluster-service")
	if err != nil || object.GetUID() != "app-uid" || fixture.writes.Load() != 0 {
		t.Fatalf("Application reader=%v err=%v writes=%d", object, err, fixture.writes.Load())
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
