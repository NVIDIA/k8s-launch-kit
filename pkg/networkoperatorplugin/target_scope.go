// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	netutil "k8s.io/apimachinery/pkg/util/net"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SourceGroupsAnnotation associates a hardware resource with source inventory in
// the effective configuration. Live metadata must never redefine this intent.
const SourceGroupsAnnotation = "nvidia.kubernetes-launch-kit.source-groups"

const selectorOnlyWarning = "target scope unverified: selector-only configuration has no worker inventory"

type targetScope struct {
	workers []string // nil means the explicit selector-only compatibility contract
}

// Mark observation reads so transport classification cannot turn a semantic
// scope error containing similar text into a retryable failure.
type targetScopeReadError struct{ error }

func (e targetScopeReadError) Unwrap() error { return e.error }

func hasTargetScope(obj *unstructured.Unstructured) bool {
	gvk := obj.GroupVersionKind()
	return gvk.Group == "sriovnetwork.openshift.io" && gvk.Kind == "SriovNetworkNodePolicy" ||
		gvk.Group == "nv-ipam.nvidia.com" && gvk.Kind == "IPPool"
}

// Only known temporary observation failures may retry. Invalid intent, selector
// mismatches and denied reads remain terminal, and no write guard is relaxed.
func isRetryableTargetScopeReadError(err error) bool {
	var readErr targetScopeReadError
	if !errors.As(err, &readErr) {
		return false
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || errors.Is(err, context.Canceled) {
		return false
	}
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err) || netutil.IsTimeout(err) ||
		netutil.IsConnectionReset(err) || netutil.IsConnectionRefused(err) ||
		netutil.IsHTTP2ConnectionLost(err) || netutil.IsProbableEOF(err) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// resolveTargetScope is pure. Annotation-bearing artifacts require configuration;
// legacy association is inferable only with a single configured source.
func resolveTargetScope(obj *unstructured.Unstructured, cfg *config.LaunchKitConfig) (targetScope, error) {
	var groups []config.ClusterConfig
	if cfg != nil {
		groups = cfg.ClusterConfig
	}
	raw, annotated := obj.GetAnnotations()[SourceGroupsAnnotation]
	var ids []string
	if annotated {
		if err := json.Unmarshal([]byte(raw), &ids); err != nil || len(ids) == 0 {
			return targetScope{}, fmt.Errorf("invalid %s annotation: expected nonempty JSON source ID array", SourceGroupsAnnotation)
		}
	} else if len(groups) == 1 {
		ids = []string{groups[0].Identifier}
	} else {
		for _, group := range groups {
			if len(group.WorkerNodes) > 0 {
				return targetScope{}, fmt.Errorf("legacy resource has ambiguous source inventory; regenerate manifests with source-group metadata")
			}
		}
		return targetScope{}, nil
	}
	if err := config.ValidateClusterConfigIdentifiers(groups); err != nil {
		return targetScope{}, err
	}
	workerOwners := map[string]string{}
	for _, group := range groups {
		for _, node := range group.WorkerNodes {
			if owner, exists := workerOwners[node]; exists {
				return targetScope{}, fmt.Errorf("duplicate worker %q in source groups %q and %q", node, owner, group.Identifier)
			}
			workerOwners[node] = group.Identifier
		}
	}
	byID := make(map[string]config.ClusterConfig, len(groups))
	for _, group := range groups {
		byID[group.Identifier] = group
	}
	workers := make(map[string]bool)
	seen := make(map[string]bool)
	emptySources := 0
	for _, id := range ids {
		// Empty is the established identity of an unnamed sole source. Its
		// explicit annotation still requires exactly that configuration.
		if id == "" && (len(ids) != 1 || len(groups) != 1 || groups[0].Identifier != "") {
			return targetScope{}, fmt.Errorf("unnamed source reference requires exactly one unnamed configured source")
		}
		if strings.TrimSpace(id) != id || seen[id] {
			return targetScope{}, fmt.Errorf("empty or duplicate source group ID %q", id)
		}
		seen[id] = true
		group, exists := byID[id]
		if !exists {
			return targetScope{}, fmt.Errorf("source group %q is missing from configuration; supply its resolved configuration", id)
		}
		if len(group.WorkerNodes) == 0 {
			emptySources++
			continue
		}
		for _, node := range group.WorkerNodes {
			if strings.TrimSpace(node) == "" || strings.TrimSpace(node) != node || workers[node] {
				return targetScope{}, fmt.Errorf("empty, malformed or duplicate worker %q in source inventory", node)
			}
			workers[node] = true
		}
	}
	if emptySources == len(ids) {
		return targetScope{}, nil
	}
	if emptySources > 0 {
		return targetScope{}, fmt.Errorf("source groups mix populated and empty worker inventories; supply complete inventory or consistently selector-only configuration")
	}
	result := targetScope{workers: make([]string, 0, len(workers))}
	for node := range workers {
		result.workers = append(result.workers, node)
	}
	slices.Sort(result.workers)
	return result, nil
}

// checkTargetScope evaluates the actual selector against the intended source
// workers. The caller owns API reads and decides whether desired or live state is
// being checked. A selector-only result is always visibly unverified.
func checkTargetScope(obj *unstructured.Unstructured, scope targetScope, nodes []corev1.Node) (string, error) {
	if scope.workers == nil {
		return selectorOnlyWarning, nil
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "spec", "nodeSelector")
	if err != nil || !found || len(raw) == 0 {
		return "", fmt.Errorf("inventory-backed resource requires a nonempty nodeSelector")
	}
	selected := make(map[string]bool)
	for i := range nodes {
		matches, err := resourceSelectsNode(obj, &nodes[i])
		if err != nil {
			return "", err
		}
		if matches {
			selected[nodes[i].Name] = true
		}
	}
	var missing, extra []string
	for _, worker := range scope.workers {
		if !selected[worker] {
			missing = append(missing, worker)
		}
		delete(selected, worker)
	}
	for node := range selected {
		extra = append(extra, node)
	}
	slices.Sort(extra)
	if len(missing)+len(extra) > 0 {
		return "", fmt.Errorf("selector scope differs from intended workers: missing=%v extra=%v", missing, extra)
	}
	return "inventory-backed target scope verified", nil
}

func resourceSelectsNode(obj *unstructured.Unstructured, node *corev1.Node) (bool, error) {
	if obj.GetKind() == "SriovNetworkNodePolicy" {
		selector, _, err := unstructured.NestedStringMap(obj.Object, "spec", "nodeSelector")
		if err != nil {
			return false, fmt.Errorf("invalid nodeSelector: %w", err)
		}
		return labels.SelectorFromSet(selector).Matches(labels.Set(node.Labels)), nil
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "spec", "nodeSelector")
	if err != nil {
		return false, fmt.Errorf("invalid IPPool nodeSelector: %w", err)
	}
	if !found {
		return true, nil
	}
	var selector corev1.NodeSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &selector); err != nil {
		return false, fmt.Errorf("invalid IPPool nodeSelector: %w", err)
	}
	anyMatch := false
	for _, term := range selector.NodeSelectorTerms {
		// Kubernetes empty terms match no nodes; terms are OR, requirements AND.
		matches := len(term.MatchExpressions)+len(term.MatchFields) > 0
		for _, requirement := range term.MatchExpressions {
			matched, err := matchesNodeRequirement(requirement, node.Labels)
			if err != nil {
				return false, err
			}
			matches = matches && matched
		}
		for _, requirement := range term.MatchFields {
			if requirement.Key != "metadata.name" {
				return false, fmt.Errorf("unsupported node selector field %q", requirement.Key)
			}
			matched, err := matchesNodeRequirement(requirement, map[string]string{"metadata.name": node.Name})
			if err != nil {
				return false, err
			}
			matches = matches && matched
		}
		anyMatch = anyMatch || matches
	}
	return anyMatch, nil
}

func matchesNodeRequirement(req corev1.NodeSelectorRequirement, values map[string]string) (bool, error) {
	operators := map[corev1.NodeSelectorOperator]selection.Operator{
		corev1.NodeSelectorOpIn: selection.In, corev1.NodeSelectorOpNotIn: selection.NotIn,
		corev1.NodeSelectorOpExists: selection.Exists, corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
		corev1.NodeSelectorOpGt: selection.GreaterThan, corev1.NodeSelectorOpLt: selection.LessThan,
	}
	op, exists := operators[req.Operator]
	if !exists {
		return false, fmt.Errorf("invalid node selector operator %q", req.Operator)
	}
	requirement, err := labels.NewRequirement(req.Key, op, req.Values)
	if err != nil {
		return false, fmt.Errorf("invalid node selector requirement: %w", err)
	}
	return requirement.Matches(labels.Set(values)), nil
}

func checkBundleTargetScopes(ctx context.Context, c client.Client, artifacts *bundle.Bundle, cfg *config.LaunchKitConfig) error {
	for _, doc := range artifacts.Documents() {
		obj := doc.ObjectCopy()
		if doc.Role() != bundle.Deployment || !hasTargetScope(obj) {
			continue
		}
		message, err := inspectDesiredTargetScope(ctx, c, obj, cfg)
		if err != nil {
			return fmt.Errorf("%s/%s target scope: %w", obj.GetKind(), obj.GetName(), err)
		}
		if message == selectorOnlyWarning {
			ui.FromContext(ctx).Warning("%s/%s: %s", obj.GetKind(), obj.GetName(), message)
		}
	}
	return nil
}

// inspectDesiredTargetScope also runs immediately before an affected resource
// apply/reapply, so a long operator bootstrap cannot hide intervening label drift.
func inspectDesiredTargetScope(ctx context.Context, c client.Client, obj *unstructured.Unstructured, cfg *config.LaunchKitConfig) (string, error) {
	if !hasTargetScope(obj) {
		return "", nil
	}
	scope, err := resolveTargetScope(obj, cfg)
	if err != nil {
		return "", err
	}
	nodes := &corev1.NodeList{}
	if scope.workers != nil {
		if c == nil {
			return "", fmt.Errorf("node client is required to inspect target scope")
		}
		if err := c.List(ctx, nodes); err != nil {
			return "", fmt.Errorf("inspect target nodes: %w", err)
		}
	}
	return checkTargetScope(obj, scope, nodes.Items)
}

// inspectLiveTargetScope always resolves intent from the retained desired object,
// then evaluates the live selector. NotFound remains the readiness registry's job.
func inspectLiveTargetScope(ctx context.Context, c client.Client, desired *unstructured.Unstructured, cfg *config.LaunchKitConfig) (string, error) {
	if !hasTargetScope(desired) {
		return "", nil
	}
	scope, err := resolveTargetScope(desired, cfg)
	if err != nil {
		return "", err
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(desired), live); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect live selector: %w", targetScopeReadError{err})
	}
	nodes := &corev1.NodeList{}
	if scope.workers != nil {
		if err := c.List(ctx, nodes); err != nil {
			return "", fmt.Errorf("inspect target nodes: %w", targetScopeReadError{err})
		}
	}
	return checkTargetScope(live, scope, nodes.Items)
}

// annotateTargetSources preserves source ownership at the existing render scope
// boundary, including aggregate IPPools and strict-subset per-source policies.
func annotateTargetSources(rendered map[string]string, sources []config.ClusterConfig, kind string, cfg *config.LaunchKitConfig) error {
	if kind != "SriovNetworkNodePolicy" && kind != "IPPool" || len(rendered) == 0 {
		return nil
	}
	if err := config.ValidateClusterConfigIdentifiers(sources); err != nil {
		return err
	}
	ids := make([]string, 0, len(sources))
	for _, src := range sources {
		if src.Identifier == "" && (len(sources) != 1 || cfg == nil || len(cfg.ClusterConfig) != 1 || cfg.ClusterConfig[0].Identifier != "") {
			return fmt.Errorf("unnamed source reference requires exactly one unnamed configured source")
		}
		if strings.TrimSpace(src.Identifier) != src.Identifier {
			return fmt.Errorf("source group identifier %q contains surrounding whitespace", src.Identifier)
		}
		ids = append(ids, src.Identifier)
	}
	if len(ids) == 0 {
		return fmt.Errorf("hardware resource has no source groups")
	}
	slices.Sort(ids)
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	for name, content := range rendered {
		annotated, err := annotateResourcesWithAnnotation([]byte(content), SourceGroupsAnnotation, string(encoded))
		if err != nil {
			return fmt.Errorf("annotate rendered %s: %w", name, err)
		}
		rendered[name] = string(annotated)
	}
	return nil
}
