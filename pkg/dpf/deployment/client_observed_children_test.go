// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNativeClientListsObservedReplicaSetsWithoutAuthorizingDesiredResources(t *testing.T) {
	if _, known := bundle.ResourceNamespaced("apps", "ReplicaSet"); known {
		t.Fatal("observed child was added to the desired-resource kind registry")
	}
	const selector = "app=selected,component=manager"
	var lists, discovery atomic.Int32
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prefix/apis/apps/v1":
			discovery.Add(1)
			_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
				{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true, Verbs: []string{"list"}},
			}})
		case "/prefix/apis/apps/v1/namespaces/selected/replicasets":
			lists.Add(1)
			if r.Method != http.MethodGet || r.URL.Query().Get("labelSelector") != selector || r.URL.Query().Get("limit") != "256" {
				t.Errorf("unexpected child list request: method=%s query=%s", r.Method, r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"apiVersion":"apps/v1","kind":"ReplicaSetList","items":[{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"selected-child","namespace":"selected","uid":"child-one"}}]}`))
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	template := validationObject("apps/v1", "ReplicaSet", "selected", "")
	objects, err := c.List(context.Background(), template, selector)
	if err != nil || len(objects) != 1 || objects[0].GetUID() != "child-one" || objects[0].GetNamespace() != "selected" {
		t.Fatalf("current ReplicaSet list: objects=%v error=%v", objects, err)
	}
	if template.GetName() != "" || template.GetNamespace() != "selected" {
		t.Fatal("list mutated the requested child scope")
	}
	for _, test := range []struct{ name, version, group, namespace string }{
		{name: "all namespaces", version: "v1", group: "apps"},
		{name: "unknown version", version: "v2", group: "apps", namespace: "selected"},
		{name: "legacy version", version: "v1beta1", group: "apps", namespace: "selected"},
		{name: "unknown group", version: "v1", group: "other", namespace: "selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := validationObject(test.group+"/"+test.version, "ReplicaSet", test.namespace, "")
			if _, err := c.List(context.Background(), object, selector); err == nil {
				t.Fatal("unreviewed child list scope was accepted")
			}
		})
	}
	template.SetName("selected-child")
	if _, err := c.Get(context.Background(), template); err == nil {
		t.Fatal("observed ReplicaSet authorization escaped into Get")
	}
	for _, dryRun := range []bool{false, true} {
		if _, err := c.Create(context.Background(), template, dryRun); err == nil {
			t.Fatal("observed ReplicaSet authorization escaped into Create")
		}
	}
	if lists.Load() != 1 || discovery.Load() != 1 || f.writes.Load() != 0 {
		t.Fatalf("unexpected transport effects: list=%d discovery=%d writes=%d", lists.Load(), discovery.Load(), f.writes.Load())
	}
}

func TestNativeClientRejectsInvalidObservedReplicaSetDiscoveryAndDeniedLists(t *testing.T) {
	for _, test := range []struct {
		name, groupVersion string
		namespaced         bool
		status             int
		reason             metav1.StatusReason
	}{
		{name: "cluster scoped discovery", groupVersion: "apps/v1", namespaced: false},
		{name: "wrong discovery version", groupVersion: "apps/v2", namespaced: true},
		{name: "forbidden", groupVersion: "apps/v1", namespaced: true, status: http.StatusForbidden, reason: metav1.StatusReasonForbidden},
		{name: "unauthorized", groupVersion: "apps/v1", namespaced: true, status: http.StatusUnauthorized, reason: metav1.StatusReasonUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			var lists atomic.Int32
			f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/prefix/apis/apps/v1":
					_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: test.groupVersion, APIResources: []metav1.APIResource{
						{Name: "replicasets", Kind: "ReplicaSet", Namespaced: test.namespaced, Verbs: []string{"list"}},
					}})
				case "/prefix/apis/apps/v1/namespaces/selected/replicasets":
					lists.Add(1)
					w.WriteHeader(test.status)
					_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: int32(test.status), Reason: test.reason})
				default:
					t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			c, err := NewAPIClient(context.Background(), f.request, f.expected, "v1.35.6")
			if err != nil {
				t.Fatal(err)
			}
			objects, err := c.List(context.Background(), validationObject("apps/v1", "ReplicaSet", "selected", ""), "app=selected")
			if err == nil || objects != nil {
				t.Fatalf("invalid/denied list accepted: objects=%v error=%v", objects, err)
			}
			if test.reason == metav1.StatusReasonForbidden && !apierrors.IsForbidden(err) || test.reason == metav1.StatusReasonUnauthorized && !apierrors.IsUnauthorized(err) {
				t.Fatalf("access classification lost: %v", err)
			}
			wantLists := int32(0)
			if test.status != 0 {
				wantLists = 1
			}
			if lists.Load() != wantLists || f.writes.Load() != 0 {
				t.Fatalf("unexpected transport effects: list=%d writes=%d", lists.Load(), f.writes.Load())
			}
		})
	}
}
