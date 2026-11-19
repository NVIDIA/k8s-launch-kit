// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// This real HTTP seam applies API-side label filtering: a query regression
// cannot accidentally receive the hidden owner-bound Pod from a broad fake.
func TestValidationKubeProxyReadCannotHideOwnedPod(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(map[bool]string{false: "current membership", true: "hidden owned contradiction"}[hidden], func(t *testing.T) {
			set, pods, nodes := validationDaemonMembershipFixture()
			set.ResourceVersion = "10"
			if hidden {
				extra := pods[0].DeepCopy()
				extra.Name = "hidden"
				extra.UID = "hidden"
				extra.Labels["k8s-app"] = "different"
				pods = append(pods, *extra)
			}
			expected, sources := tenantSourceFixture(t)
			requests := 0
			tenant, _ := tenantIdentityClientFixtureWithHandler(t, expected, &tenantSourceStub{sources: sources}, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation %s", r.Method)
					http.Error(w, "read only", http.StatusMethodNotAllowed)
					return true
				}
				if r.URL.Path != "/apis/apps/v1/namespaces/kube-system/daemonsets" && r.URL.Path != "/api/v1/namespaces/kube-system/pods" {
					return false
				}
				requests++
				if r.URL.Query().Get("labelSelector") != "" || r.URL.Query().Get("limit") != "64" {
					t.Errorf("membership read hid candidate Pods or was unbounded: %s", r.URL)
				}
				selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid selector", http.StatusBadRequest)
					return true
				}
				if r.URL.Path == "/apis/apps/v1/namespaces/kube-system/daemonsets" {
					_ = json.NewEncoder(w).Encode(&appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "20"}, Items: []appsv1.DaemonSet{*set}})
					return true
				}
				items := []corev1.Pod{}
				for _, pod := range pods {
					if selector.Matches(labels.Set(pod.Labels)) {
						items = append(items, pod)
					}
				}
				_ = json.NewEncoder(w).Encode(&corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "21"}, Items: items})
				return true
			})
			got, err := validationKubeProxyOwnedPods(context.Background(), tenant, set, nodes)
			if hidden {
				failure, ok := err.(*validationCheckError)
				if !ok || failure.state != validation.Failed {
					t.Fatalf("API filtering hid exact-owned contradictory Pod: %#v", err)
				}
			} else if err != nil || len(got) != 1 {
				t.Fatalf("current membership rejected: got=%v err=%#v", got, err)
			}
			if requests != 2 {
				t.Fatalf("unexpected finite runtime read count: %d", requests)
			}
		})
	}
}
