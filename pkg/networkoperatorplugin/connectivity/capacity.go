// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// checkOpenShiftDeviceCapacity catches the deterministic case where a test
// DaemonSet requests a device that no selected worker advertises. Scheduler
// events remain authoritative for devices already consumed by other pods.
func checkOpenShiftDeviceCapacity(ctx context.Context, c client.Client, manifests []*unstructured.Unstructured) error {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return fmt.Errorf("list OpenShift workers for validation capacity: %w", err)
	}
	for _, manifest := range manifests {
		var ds appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(manifest.Object, &ds); err != nil {
			return fmt.Errorf("decode validation DaemonSet %s: %w", manifest.GetName(), err)
		}
		requests := map[corev1.ResourceName]resource.Quantity{}
		for _, container := range ds.Spec.Template.Spec.Containers {
			for name, quantity := range container.Resources.Requests {
				if !strings.Contains(string(name), "/") {
					continue
				}
				combined := requests[name]
				combined.Add(quantity)
				requests[name] = combined
			}
		}
		matching := 0
		for _, node := range nodes.Items {
			matches, err := podMatchesNode(ds.Spec.Template.Spec, node)
			if err != nil {
				return fmt.Errorf("validation DaemonSet %s node affinity: %w", ds.Name, err)
			}
			if !matches {
				continue
			}
			matching++
			for resourceName, required := range requests {
				capacity := node.Status.Allocatable[resourceName]
				if capacity.Cmp(required) < 0 {
					return fmt.Errorf("validation DaemonSet %s requests %s on node %s but allocatable capacity is insufficient",
						ds.Name, resourceName, node.Name)
				}
			}
		}
		if matching == 0 {
			return fmt.Errorf("validation DaemonSet %s matches no OpenShift worker nodes", ds.Name)
		}
	}
	return nil
}

func podMatchesNode(spec corev1.PodSpec, node corev1.Node) (bool, error) {
	for key, value := range spec.NodeSelector {
		if node.Labels[key] != value {
			return false, nil
		}
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true, nil
	}
	for _, term := range spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		if len(term.MatchFields) > 0 {
			return false, fmt.Errorf("matchFields is unsupported by the OpenShift capacity precheck")
		}
		matches := true
		for _, expression := range term.MatchExpressions {
			selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: expression.Key, Operator: metav1.LabelSelectorOperator(expression.Operator), Values: expression.Values,
			}}})
			if err != nil {
				return false, err
			}
			if !selector.Matches(labels.Set(node.Labels)) {
				matches = false
				break
			}
		}
		if matches {
			return true, nil
		}
	}
	return false, nil
}
