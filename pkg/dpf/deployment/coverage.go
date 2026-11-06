// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// Re-evaluate the complete tenant Node domain, including unselected nodes. A
// scope label copied onto another node would broaden every service in the scope.
func (s *run) tenantScope(ctx context.Context, selected map[string]map[string]string) ([]*unstructured.Unstructured, error) {
	nodes, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "", "Node", "", ""), "")
	if err != nil {
		return nil, fail("TENANT_NODE_SCOPE_UNAVAILABLE", "cannot enumerate tenant Nodes")
	}
	scopes := map[string]bool{}
	for _, expected := range selected {
		scopes[expected[sentry.ScopeLabel]] = true
	}
	found := map[string]bool{}
	for _, node := range nodes {
		expected, isSelected := selected[node.GetName()]
		if !isSelected {
			if scopes[node.GetLabels()[sentry.ScopeLabel]] {
				return nil, fail("UNSELECTED_TENANT_NODE_MATCH", "Sentry scope label also selects unrequested tenant Node "+node.GetName())
			}
			continue
		}
		found[node.GetName()] = true
		for key, value := range expected {
			if node.GetLabels()[key] != value {
				return nil, fail("TENANT_NODE_LABEL_DRIFT", node.GetName()+" no longer has the exact DPUSet node labels")
			}
		}
	}
	if len(found) != len(selected) {
		return nil, fail("TENANT_NODE_SCOPE_CHANGED", "a selected tenant Node disappeared")
	}
	return nodes, nil
}

// Positive DaemonSet counters alone do not prove coverage of selected devices.
// Require exact scheduling membership and one Ready pod owned by this DaemonSet
// on every selected node. A replacement pod may be pending; extra ready or
// unselected owned pods remain a conflict, including during a rollout.
func (s *run) daemonSetCoverage(ctx context.Context, object *unstructured.Unstructured) (bool, string, error) {
	if len(s.selectedNodes) != len(s.g.Provisioning.Devices) || len(s.selectedNodes) == 0 {
		return false, "selected tenant Node scope is not established", nil
	}
	nodes, err := s.tenantScope(ctx, s.selectedNodes)
	if err != nil {
		return false, "", err
	}
	var ds appsv1.DaemonSet
	data, err := json.Marshal(object.Object)
	if err != nil || json.Unmarshal(data, &ds) != nil || ds.UID == "" {
		return false, "", fail("DAEMONSET_IDENTITY_UNAVAILABLE", "cannot decode service DaemonSet identity")
	}
	matched := 0
	for _, node := range nodes {
		matches, err := podMatchesNode(&ds.Spec.Template.Spec, node)
		if err != nil {
			return false, "", fail("DAEMONSET_SELECTOR_UNSUPPORTED", err.Error())
		}
		_, selected := s.selectedNodes[node.GetName()]
		if matches && !selected {
			return false, "", fail("DAEMONSET_SCOPE_BROADENED", "service DaemonSet schedules on unselected Node "+node.GetName())
		}
		if matches {
			matched++
		}
	}
	if matched != len(s.selectedNodes) {
		return false, "service DaemonSet scheduling does not cover every selected tenant Node", nil
	}
	selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil || ds.Spec.Selector == nil {
		return false, "", fail("DAEMONSET_SELECTOR_INVALID", "service DaemonSet selector is invalid")
	}
	pods, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "", "Pod", object.GetNamespace(), ""), selector.String())
	if err != nil {
		return false, "", fail("DAEMONSET_PODS_UNAVAILABLE", "cannot inspect service DaemonSet pods")
	}
	coverage := map[string]int{}
	for _, pod := range pods {
		owned := false
		for _, owner := range pod.GetOwnerReferences() {
			if owner.Kind == "DaemonSet" && owner.UID == ds.UID && owner.Controller != nil && *owner.Controller {
				owned = true
			}
		}
		if !owned {
			continue
		}
		node := text(pod, "spec", "nodeName")
		if node == "" {
			continue
		}
		if _, selected := s.selectedNodes[node]; !selected {
			return false, "", fail("DAEMONSET_UNSELECTED_POD", "service DaemonSet owns a pod on an unselected tenant Node")
		}
		ready, _, _ := Readiness(pod)
		if ready {
			coverage[node]++
		}
	}
	for node := range s.selectedNodes {
		if coverage[node] != 1 {
			return false, "service DaemonSet requires exactly one Ready owned pod on selected Node " + node, nil
		}
	}
	if number(object, "status", "desiredNumberScheduled") != int64(len(s.selectedNodes)) {
		return false, "service DaemonSet counters do not match exact selected node count", nil
	}
	return true, "service DaemonSet has exact selected-node scheduling and Ready owned pod coverage", nil
}

// Scheduling predicates are explicit inputs. Required node-affinity terms use
// the Kubernetes OR-of-ANDs contract; unsupported MatchFields fail closed.
func podMatchesNode(p *corev1.PodSpec, node *unstructured.Unstructured) (bool, error) {
	if p.NodeName != "" && p.NodeName != node.GetName() {
		return false, nil
	}
	if !labels.SelectorFromSet(p.NodeSelector).Matches(labels.Set(node.GetLabels())) {
		return false, nil
	}
	if p.Affinity == nil || p.Affinity.NodeAffinity == nil || p.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true, nil
	}
	required := p.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	for _, term := range required.NodeSelectorTerms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		matches, err := matchesRequirements(term.MatchExpressions, labels.Set(node.GetLabels()))
		if err != nil {
			return false, err
		}
		fields := labels.Set{"metadata.name": node.GetName()}
		for _, field := range term.MatchFields {
			if field.Key != "metadata.name" {
				return false, fmt.Errorf("unsupported node affinity field %s", field.Key)
			}
		}
		fieldMatches, err := matchesRequirements(term.MatchFields, fields)
		if err != nil {
			return false, err
		}
		if matches && fieldMatches {
			return true, nil
		}
	}
	return false, nil
}
func matchesRequirements(requirements []corev1.NodeSelectorRequirement, values labels.Set) (bool, error) {
	selector := labels.NewSelector()
	for _, requirement := range requirements {
		var op selection.Operator
		switch requirement.Operator {
		case corev1.NodeSelectorOpIn:
			op = selection.In
		case corev1.NodeSelectorOpNotIn:
			op = selection.NotIn
		case corev1.NodeSelectorOpExists:
			op = selection.Exists
		case corev1.NodeSelectorOpDoesNotExist:
			op = selection.DoesNotExist
		case corev1.NodeSelectorOpGt:
			op = selection.GreaterThan
		case corev1.NodeSelectorOpLt:
			op = selection.LessThan
		default:
			return false, fmt.Errorf("unsupported node affinity operator %s", requirement.Operator)
		}
		r, err := labels.NewRequirement(requirement.Key, op, requirement.Values)
		if err != nil {
			return false, err
		}
		selector = selector.Add(*r)
	}
	return selector.Matches(values), nil
}
