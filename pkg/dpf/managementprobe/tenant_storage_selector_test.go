// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCollectTenantStorageUsesProvisionerSelectorSemantics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*appsv1.Deployment)
		passed bool
	}{
		{name: "additional template labels", passed: true, mutate: func(d *appsv1.Deployment) {
			d.Spec.Template.Labels["app.kubernetes.io/instance"] = "storage-release"
			d.Spec.Template.Labels["app.kubernetes.io/version"] = "custom-label"
		}},
		{name: "matching expression", passed: true, mutate: func(d *appsv1.Deployment) {
			d.Spec.Template.Labels["tier"] = "storage"
			d.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"storage", "shared"}}}
		}},
		{name: "nonmatching label", passed: false, mutate: func(d *appsv1.Deployment) {
			d.Spec.Template.Labels["app.kubernetes.io/name"] = "foreign-provisioner"
		}},
		{name: "nonmatching expression", passed: false, mutate: func(d *appsv1.Deployment) {
			d.Spec.Template.Labels["tier"] = "other"
			d.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"storage"}}}
		}},
		{name: "missing expression label", passed: false, mutate: func(d *appsv1.Deployment) {
			d.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpExists}}
		}},
		{name: "invalid expression", passed: false, mutate: func(d *appsv1.Deployment) {
			d.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "Invalid"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTenantStorageFixture()
			tt.mutate(fixture.provisioner)
			before := fixture.provisioner.DeepCopy()
			collector, kube, dynamic := tenantStorageCollectorFromFixture(fixture)
			result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
			if tt.passed {
				if result.State != "passed" || result.ReasonCode != "TENANT_STORAGE_READY" {
					t.Fatalf("matching selector was rejected: %+v", result)
				}
			} else if result.State != "failed" || result.ReasonCode != "TENANT_PROVISIONER_SELECTOR_INVALID" {
				t.Fatalf("invalid or nonmatching selector was accepted: %+v", result)
			}
			if !reflect.DeepEqual(before, fixture.provisioner) {
				t.Fatal("storage observation changed the provisioner input")
			}
			assertTenantStorageReadOnly(t, kube.Actions())
			assertTenantStorageReadOnly(t, dynamic.Actions())
		})
	}
}
