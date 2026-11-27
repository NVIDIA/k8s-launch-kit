// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OpenShiftCleanPlan binds the confirmation to the object incarnations seen in
// the retained bundle's target cluster.
type OpenShiftCleanPlan struct {
	client                               client.Client
	config                               *config.LaunchKitConfig
	plan                                 ocpCleanPlan
	shared                               []*unstructured.Unstructured
	sysctl, sysctlManifest, subscription *unstructured.Unstructured
	nfdManifest                          *unstructured.Unstructured
	approvedShared                       map[string]ocpSharedBinding
	operators                            *ocpCleanOperators
}

// PrepareOpenShiftClean inspects all deletion targets and shared configuration
// before presenting the plan or mutating the cluster.
func PrepareOpenShiftClean(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, artifacts *bundle.Bundle) (*OpenShiftCleanPlan, error) {
	if ctx == nil || c == nil || cfg == nil || cfg.Flavor != config.FlavorOCP || cfg.NetworkOperator == nil {
		return nil, fmt.Errorf("OpenShift clean requires a context, Kubernetes client and resolved OpenShift Host configuration")
	}
	cfg, err := config.CloneConfig(cfg)
	if err != nil {
		return nil, err
	}
	plan, err := prepareOCPCleanPlan(ctx, c, cfg, artifacts)
	if err != nil {
		return nil, err
	}
	if len(plan.refs) == 0 {
		return nil, fmt.Errorf("no supported OpenShift deployment resources in the retained bundle")
	}
	if err := checkOCPCleanDependencies(ctx, c, cfg, plan.refs); err != nil {
		return nil, err
	}
	shared, err := preflightOCPOperatorConfigurationRestore(ctx, c, cfg, plan.operatorConfigs, plan.owner)
	if err != nil {
		return nil, err
	}
	operators, err := prepareOCPCleanOperators(ctx, c, cfg, artifacts, plan.owner)
	if err != nil {
		return nil, err
	}
	clean := &OpenShiftCleanPlan{client: c, config: cfg, plan: plan, operators: operators, approvedShared: ocpSharedIdentities(shared)}
	for _, manifest := range plan.operatorConfigs {
		if isOCPSysctlAllowlistPatch(manifest) {
			clean.sysctlManifest = manifest
		} else if manifest.GetKind() == "NodeFeatureDiscovery" {
			clean.nfdManifest = manifest
		}
	}
	for _, obj := range slices.Backward(shared) {
		switch {
		case isOCPSysctlAllowlistPatch(obj):
			clean.sysctl = obj
		case obj.GetKind() == "Subscription":
			clean.subscription = obj
		default:
			clean.shared = append(clean.shared, obj)
		}
	}
	return clean, nil
}

// Targets describes the effects the user is being asked to approve.
func (p *OpenShiftCleanPlan) Targets() []string {
	if p == nil {
		return nil
	}
	var lines []string
	for _, ref := range slices.Backward(p.plan.refs) {
		if ref.UID != "" {
			lines = append(lines, "Delete "+cleanRefString(ref))
		}
	}
	for _, obj := range p.shared {
		lines = append(lines, p.sharedAction(obj))
	}
	if p.sysctl != nil {
		lines = append(lines, "Remove manifest allowlist entries from ConfigMap "+client.ObjectKeyFromObject(p.sysctl).String())
	}
	lines = append(lines, p.operators.targets()...)
	if p.subscription != nil && !p.operators.removesSubscription(p.subscription) {
		lines = append(lines, p.sharedAction(p.subscription))
	}
	return append(lines, p.plan.retained...)
}

func (p *OpenShiftCleanPlan) sharedAction(obj *unstructured.Unstructured) string {
	if obj.GetKind() == "NodeFeatureDiscovery" {
		return "Clean NodeFeatureDiscovery " + client.ObjectKeyFromObject(obj).String() + " (delete if worker rules exactly match the manifest, otherwise patch out its PCI settings)"
	}
	action := "Retain"
	if ocpConfigurationHasCohort(obj, p.plan.owner) {
		action = "Restore recorded changes to"
	}
	return fmt.Sprintf("%s %s %s", action, obj.GetKind(), client.ObjectKeyFromObject(obj))
}

// Execute tears down consumers while their operators can still reconcile them,
// then releases the bundle's claims on the shared operator configuration.
func (p *OpenShiftCleanPlan) Execute(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	var result CleanResult
	if p == nil || p.client == nil || ctx == nil {
		return result, fmt.Errorf("OpenShift clean requires a prepared plan and context")
	}
	namespace := ocpCleanNamespaces(p.config).operator
	if opts.Namespace != "" && opts.Namespace != namespace {
		return result, fmt.Errorf("cleanup namespace %q differs from retained bundle namespace %q", opts.Namespace, namespace)
	}
	opts.Namespace, result.Namespace = namespace, namespace
	if opts.ResourceTimeout < 0 {
		return result, fmt.Errorf("cleanup timeout must be zero or positive")
	}
	if opts.ResourceTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.ResourceTimeout)
		defer cancel()
	}
	for _, ref := range p.plan.refs {
		if _, err := inspectOCPDeletionTarget(ctx, p.client, ref); err != nil {
			return result, err
		}
	}
	current, err := preflightOCPOperatorConfigurationRestore(ctx, p.client, p.config, p.plan.operatorConfigs, p.plan.owner)
	if err != nil {
		return result, err
	}
	if !maps.Equal(ocpSharedIdentities(current), p.approvedShared) {
		return result, fmt.Errorf("shared OpenShift configuration changed after planning; inspect cleanup again")
	}
	count, err := cleanOCPResources(ctx, p.client, p.config, p.plan.refs, opts)
	result.CustomResourcesDeleted = count
	if err != nil {
		return result, err
	}
	result.Retained = slices.Clone(p.plan.retained)
	for _, target := range p.shared {
		var messages []string
		if target.GetKind() == "NodeFeatureDiscovery" {
			messages, err = cleanOCPNFDConfiguration(ctx, p.client, p.nfdManifest, target)
		} else {
			messages, err = restoreOCPConfigurationObject(ctx, p.client, target, p.plan.owner, opts.SkipWait)
		}
		result.Retained = append(result.Retained, messages...)
		if err != nil {
			return result, err
		}
	}
	if p.sysctl != nil {
		messages, err := removeOCPSysctlAllowlist(ctx, p.client, p.sysctlManifest, p.sysctl, p.plan.owner)
		result.Retained = append(result.Retained, messages...)
		if err != nil {
			return result, err
		}
	}
	messages, err := p.operators.execute(ctx, p.client, opts)
	result.Retained = append(result.Retained, messages...)
	if err == nil && p.subscription != nil {
		// A deleted Subscription needs no rollout. Restore its environment if an
		// external installation or a surviving consumer kept it in the cluster.
		messages, err = restoreOCPConfigurationObject(ctx, p.client, p.subscription, p.plan.owner, opts.SkipWait)
		result.Retained = append(result.Retained, messages...)
	}
	return result, err
}

// Resource versions can change with status alone. Bind the approved object
// incarnation and the journal that authorizes the restoration.
type ocpSharedBinding struct {
	uid     types.UID
	journal string
}

func ocpSharedIdentities(objects []*unstructured.Unstructured) map[string]ocpSharedBinding {
	bindings := make(map[string]ocpSharedBinding, len(objects))
	for _, obj := range objects {
		key := ocpCleanIdentity(obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
		binding := ocpSharedBinding{uid: obj.GetUID()}
		if obj.GetKind() != "NodeFeatureDiscovery" {
			binding.journal = obj.GetAnnotations()[ocpRollbackAnnotation]
		}
		bindings[key] = binding
	}
	return bindings
}
