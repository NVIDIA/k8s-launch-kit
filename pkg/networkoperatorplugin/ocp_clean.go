// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OpenShiftCleanPlan binds the confirmation to the object incarnations seen in
// the retained bundle's target cluster. OLM installation is external to deploy.
type OpenShiftCleanPlan struct {
	client client.Client
	config *config.LaunchKitConfig
	plan   ocpCleanPlan
	shared []*unstructured.Unstructured
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
	return &OpenShiftCleanPlan{client: c, config: cfg, plan: plan, shared: shared}, nil
}

// Targets describes the effects the user is being asked to approve.
func (p *OpenShiftCleanPlan) Targets() []string {
	if p == nil {
		return nil
	}
	lines := append([]string(nil), p.plan.retained...)
	for _, ref := range p.plan.refs {
		if ref.UID != "" {
			lines = append(lines, "Delete "+cleanRefString(ref))
		}
	}
	for _, obj := range p.shared {
		action := "Restore recorded changes to"
		if !ocpConfigurationHasCohort(obj, p.plan.owner) {
			action = "Retain"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", action, obj.GetKind(), client.ObjectKeyFromObject(obj)))
	}
	sort.Strings(lines)
	return lines
}

// Execute tears down consumers while their operators can still reconcile them,
// then releases the bundle's claims on the shared operator configuration.
func (p *OpenShiftCleanPlan) Execute(ctx context.Context, opts CleanOptions) (result CleanResult, operationErr error) {
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
	release, err := acquireOCPOperation(ctx, p.client, namespace, p.plan.owner, "clean")
	if err != nil {
		return result, err
	}
	defer func() { operationErr = errors.Join(operationErr, release()) }()
	for _, ref := range p.plan.refs {
		if _, err := inspectOCPDeletionTarget(ctx, p.client, ref); err != nil {
			return result, err
		}
	}
	current, err := preflightOCPOperatorConfigurationRestore(ctx, p.client, p.config, p.plan.operatorConfigs, p.plan.owner)
	if err != nil {
		return result, err
	}
	if !maps.Equal(ocpSharedIdentities(current), ocpSharedIdentities(p.shared)) {
		return result, fmt.Errorf("shared OpenShift configuration changed after planning; inspect cleanup again")
	}
	count, err := cleanOCPResources(ctx, p.client, p.config, p.plan.refs, opts)
	result.CustomResourcesDeleted = count
	if err != nil {
		return result, err
	}
	retained, err := restoreOCPOperatorConfiguration(ctx, p.client, p.shared, p.plan.owner, opts.SkipWait)
	result.Retained = append(append([]string(nil), p.plan.retained...), retained...)
	if err != nil {
		return result, err
	}
	return result, completeOCPCleanReceipt(ctx, p.client, namespace, p.plan.owner)
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
		bindings[key] = ocpSharedBinding{uid: obj.GetUID(), journal: obj.GetAnnotations()[ocpRollbackAnnotation]}
	}
	return bindings
}
