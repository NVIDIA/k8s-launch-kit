// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func validationSpec(id string, area validation.Area, subject string, coverage bool) validation.CheckSpec {
	return validation.CheckSpec{ID: validation.CheckID(id), Area: area, Subject: subject, Required: true, Coverage: coverage}
}
func validationRootID(object *unstructured.Unstructured) validation.CheckID {
	return validation.CheckID("desired/" + url.PathEscape(object.GetKind()) + "/" + url.PathEscape(object.GetNamespace()) + "/" + url.PathEscape(object.GetName()))
}
func (o *validationObserver) validationDesiredRoots() []*unstructured.Unstructured {
	var result []*unstructured.Unstructured
	for _, stage := range o.desired.plan.Stages() {
		for _, document := range stage.Documents() {
			object := document.ObjectCopy()
			if object.GetKind() != "Namespace" && object.GetKind() != "DPFOperatorConfig" {
				result = append(result, object)
			}
		}
	}
	return result
}
func (o *validationObserver) validationResourceChecks() []validation.CheckSpec {
	checks := []validation.CheckSpec{
		validationSpec("management/identity", validation.AreaManagement, "Management cluster identity", true),
		validationSpec("operator/release", validation.AreaOperator, "Retained operator Helm release", true),
		validationSpec("operator/config", validation.AreaOperator, "Operator configuration", false),
		validationSpec("operator/manager", validation.AreaOperator, "Operator manager rollout", false),
		validationSpec("definitions/coverage", validation.AreaDefinitions, "Retained management API definitions", true),
		validationSpec("desired/coverage", validation.AreaManagement, "Retained desired roots", true),
		validationSpec("tenant/sources", validation.AreaTenant, "Selected tenant control plane and source chain", true),
		validationSpec("tenant/api", validation.AreaTenant, "Selected tenant API identity", false),
		validationSpec("tenant/storage", validation.AreaTenant, "Selected tenant backing storage", false),
		validationSpec("tenant/nodes", validation.AreaTenant, "Selected tenant Node health and coverage", false),
		validationSpec("software/cache", validation.AreaSoftware, "Current reported software and cache binding", true),
	}
	for _, namespace := range o.desired.namespaces {
		checks = append(checks, validationSpec("management/namespace/"+namespace.GetName(), validation.AreaManagement, "Namespace "+namespace.GetName(), false))
	}
	for _, crd := range o.desired.crds {
		checks = append(checks, validationSpec("definitions/management/"+crd.name, validation.AreaDefinitions, crd.name, false))
	}
	for _, object := range o.validationDesiredRoots() {
		checks = append(checks, validationSpec(string(validationRootID(object)), validationRootArea(object), object.GetKind()+" "+object.GetNamespace()+"/"+object.GetName(), false))
	}
	checks = append(checks, o.validationDependencyChecks()...)
	return append(checks, o.systemValidationChecks()...)
}
func validationRootArea(object *unstructured.Unstructured) validation.Area {
	switch object.GetKind() {
	case "BlueFieldSoftware":
		return validation.AreaSoftware
	case "DPUCluster":
		return validation.AreaTenant
	default:
		return validation.AreaManagement
	}
}
func (o *validationObserver) observeManagementResources(ctx context.Context, management validationManagementReader, r *validationRecorder) error {
	for _, namespace := range o.desired.namespaces {
		err := r.run(validation.CheckID("management/namespace/"+namespace.GetName()), func() error { return verifyValidationNamespace(ctx, management, namespace) })
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	}
	if err := o.observeValidationDependencies(ctx, management, r); err != nil {
		return err
	}
	run := func(id validation.CheckID, check func() error) error {
		err := r.run(id, check)
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
		return nil
	}
	if err := run("operator/release", func() error { return management.inspectValidationOperator(ctx, o.desired.operator) }); err != nil {
		return err
	}
	if err := run("operator/manager", func() error {
		current, err := management.Get(ctx, o.desired.manager.DeepCopy())
		if err != nil {
			return err
		}
		if !managerDeploymentIntentMatches(o.desired.manager, current) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Operator manager specification differs from the retained chart.")
		}
		if err := validationReady(o.desired.rules.ResourceReadiness(current)); err != nil {
			return err
		}
		return verifyValidationDeploymentPods(ctx, management, current)
	}); err != nil {
		return err
	}
	if err := run("operator/config", func() error {
		configs, err := listOperatorConfigs(ctx, management)
		if err != nil {
			return err
		}
		if len(configs) != 1 {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "The selected installation requires exactly one DPFOperatorConfig.")
		}
		current := &configs[0]
		if err := o.desired.verifier.Verify(o.desired.configObject.DeepCopy(), current); err != nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Operator configuration differs from retained intent or ownership.")
		}
		if !operatorConfigVersionMatches(current, o.desired.operatorVersion) {
			return validationFailure(validation.Pending, validation.ReasonProgressing, "Operator configuration has not observed the retained operator version.")
		}
		return validationReady(o.desired.rules.ResourceReadiness(current))
	}); err != nil {
		return err
	}
	for _, expected := range o.desired.crds {
		err := r.run(validation.CheckID("definitions/management/"+expected.name), func() error { return verifyValidationCRD(ctx, management, expected) })
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	}
	r.aggregate("definitions/coverage", "definitions/management/", nil)
	for _, desired := range o.validationDesiredRoots() {
		err := r.run(validationRootID(desired), func() error {
			current, err := management.Get(ctx, desired.DeepCopy())
			if err != nil {
				return err
			}
			if err := o.desired.verifier.Verify(desired.DeepCopy(), current); err != nil {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Selected root differs from retained intent, schema or ownership.")
			}
			switch desired.GetKind() {
			case "BlueFieldSoftware", "DPUCluster", "DPUDiscovery", "DPUDeployment":
				return validationReady(o.desired.rules.ResourceReadiness(current))
			case "DPUServiceInterface":
				return validationReady(o.desired.rules.ValidationConditions(current, "Ready", "ServiceInterfacePreReqsReady", "ServiceInterfaceSetReady", "ServiceInterfaceSetReconciled"))
			case "DPUFlavor", "DPUServiceConfiguration", "DPUServiceTemplate", "DPUServiceChain", "DPUServiceIPAM":
				return nil // Exact declaration; descendants have independent coverage.
			default:
				return validationFailure(validation.Unverified, validation.ReasonUnsupported, "No selected declaration contract exists for this retained kind.")
			}
		})
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	}
	r.aggregate("desired/coverage", "desired/", nil)
	if err := run("software/cache", func() error {
		var desired *unstructured.Unstructured
		for _, object := range o.validationDesiredRoots() {
			if object.GetKind() == "BlueFieldSoftware" {
				if desired != nil {
					return validationFailure(validation.Failed, validation.ReasonIncomplete, "Retained software selection is ambiguous.")
				}
				desired = object
			}
		}
		if desired == nil {
			return validationFailure(validation.Failed, validation.ReasonMissing, "Retained software selection is missing.")
		}
		current, err := management.Get(ctx, desired.DeepCopy())
		if err != nil {
			return err
		}
		if err := o.desired.verifier.Verify(desired, current); err != nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Software object no longer matches the retained lock selection.")
		}
		if err := validationReady(o.desired.rules.ResourceReadiness(current)); err != nil {
			return err
		}
		psids := []string{}
		for _, device := range o.desired.plan.Intent().Devices {
			psids = append(psids, device.PSID)
		}
		sort.Strings(psids)
		if _, err := softwareCacheTargets(o.desired.rules, desired, current, o.desired.plan.Intent().Software, psids); err != nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Controller-reported software readiness or selected cache paths differ from retained locks.")
		}
		snapshot, err := management.observeRegistrySnapshot(ctx, o.desired.operatorNamespace)
		if err != nil {
			return err
		}
		release, err := releases.Resolve(o.desired.plan.Intent().Release)
		if err != nil {
			return err
		}
		if snapshot.manager.container.image != release.Operator.Image {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Current cache manager image differs from the selected release.")
		}
		return nil
	}); err != nil {
		return err
	}
	// A cache row proves reported paths/current serving binding, not file hashes.
	if out := r.outcomes["software/cache"]; out.State == validation.Passed {
		r.record("software/cache", validation.Passed, validation.ReasonNone, "Software reports selected locked paths and readiness; current cache/controller/storage binding is healthy. Cached bytes were not hashed.", "")
	}
	return nil
}
func validationReady(ready controllercontract.Readiness) error {
	if ready.Ready {
		return nil
	}
	state, reason := validation.Pending, validation.ReasonProgressing
	switch ready.ReasonCode {
	case "RESOURCE_KIND_UNSUPPORTED", "RESOURCE_API_UNSUPPORTED", "CONTROLLER_RULES_UNAVAILABLE":
		state, reason = validation.Unverified, validation.ReasonUnsupported
	case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_STATUS_INVALID", "RESOURCE_STATUS_AMBIGUOUS", "RESOURCE_GENERATION_MISSING", "RESOURCE_DELETING", "DEPLOYMENT_ROLLOUT_FAILED":
		state, reason = validation.Failed, validation.ReasonHealth
	}
	return validationFailure(state, reason, "Selected controller evidence does not establish current readiness.")
}
func verifyValidationCRD(ctx context.Context, reader operatorCRDReader, expected operatorCRDExpectation) error {
	current, err := reader.Get(ctx, validationObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", expected.name))
	if err != nil {
		return err
	}
	if crdMismatchReason(expected, current) != "" {
		return validationFailure(validation.Failed, validation.ReasonHealth, "API definition identity, schema, defaults or conversion differs from the selected chart.")
	}
	stored, found, err := unstructured.NestedStringSlice(current.Object, "status", "storedVersions")
	if err != nil || !found || len(stored) != 1 {
		return validationFailure(validation.Failed, validation.ReasonHealth, "API definition has missing or ambiguous stored-version evidence.")
	}
	storageVersion := ""
	versions, _, _ := unstructured.NestedSlice(current.Object, "spec", "versions")
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		storage, _ := version["storage"].(bool)
		if storage {
			storageVersion, _ = version["name"].(string)
		}
	}
	if storageVersion == "" || stored[0] != storageVersion {
		return validationFailure(validation.Failed, validation.ReasonHealth, "API definition stored version differs from the selected chart.")
	}
	for _, version := range expected.versions {
		requirement := preflightAPIRequirement{crd: expected.name, group: expected.group, version: version, resource: expected.plural, kind: expected.kind, namespaced: expected.namespaced}
		if !externalCRDReady(current, requirement) {
			return validationFailure(validation.Pending, validation.ReasonProgressing, "API definition is not established with the selected stored version.")
		}
		resources, err := reader.discoverResourceList(ctx, expected.group+"/"+version)
		if err != nil {
			return err
		}
		if !discoveredResourceMatches(resources, requirement) {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "Discovery does not advertise the selected API resource and scope.")
		}
	}
	return nil
}
func validationObject(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}}}
}

func (o *validationObserver) openValidationTenant(ctx context.Context, management validationManagementReader) (*tenantClient, error) {
	expected, desired, err := tenantPlanInputs(o.desired.plan)
	if err != nil {
		return nil, err
	}
	current, err := management.Get(ctx, desired.DeepCopy())
	if err != nil {
		return nil, err
	}
	if err := o.desired.verifier.Verify(desired, current); err != nil {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Selected DPUCluster differs from retained intent or ownership.")
	}
	sources, err := management.observeTenantSources(ctx, expected)
	if err != nil {
		return nil, err
	}
	if sources.observed.DPUCluster == nil || sources.observed.DPUCluster.GetUID() != current.GetUID() {
		return nil, fmt.Errorf("tenant source identity changed: %w", errTenantIdentityChanged)
	}
	if err := o.desired.verifier.Verify(desired, sources.observed.DPUCluster); err != nil {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Tenant source DPUCluster differs from retained intent.")
	}
	if err := validationTenantReady(o.desired.rules.TenantReadiness(expected, sources.observed)); err != nil {
		return nil, err
	}
	tenant, err := newTenantClient(ctx, o.desired.rules, management, expected, string(current.GetUID()), o.request.RequestTimeout, o.runtime.tenantDial, o.runtime.requestLimiter)
	if err != nil {
		return nil, validationTenantConnectionError(ctx, err)
	}
	return tenant, nil
}

// Only the constructor's direct source result can establish ordinary progress.
// A joined/wrapped failure may contain identity, access or cancellation evidence
// and must keep its original classification. Bound-session fencing never calls
// this projection.
func validationTenantConnectionError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}
	if source, ok := err.(*tenantSourceReadinessError); ok {
		return errors.Join(validationTenantReady(source.readiness), err)
	}
	return err
}

// Tenant source convergence has its own classifier. Conflicts and malformed
// observations must not acquire the runner's retry policy through a dependent.
func validationTenantReady(ready controllercontract.TenantReadiness) error {
	if ready.Ready {
		return nil
	}
	switch ready.State {
	case controllercontract.TenantReadinessPending:
		return validationFailure(validation.Pending, validation.ReasonProgressing, "Selected tenant source chain has not reached current owned readiness.")
	case controllercontract.TenantReadinessConflict:
		return validationFailure(validation.Failed, validation.ReasonHealth, "Selected tenant source chain conflicts with retained intent or reports terminal failure.")
	default:
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Selected tenant source evidence is malformed or unsupported.")
	}
}

func (o *validationObserver) observeTenantResources(ctx context.Context, management validationManagementReader, r *validationRecorder) (*tenantClient, error) {
	return o.observeTenantResourcesWithOpener(ctx, management, r, o.openValidationTenant)
}

// Keep source classification and node coverage in the native observation path
// when substituting only the connection to an already source-bound tenant.
func (o *validationObserver) observeTenantResourcesWithOpener(ctx context.Context, management validationManagementReader, r *validationRecorder, open func(context.Context, validationManagementReader) (*tenantClient, error)) (*tenantClient, error) {
	r.run("tenant/storage", func() error {
		return validationTenantStorageResult(management.observeValidationTenantStorage(ctx, o.desired.operatorNamespace))
	})
	expected, desired, err := tenantPlanInputs(o.desired.plan)
	if err != nil {
		return nil, err
	}
	err = r.run("tenant/sources", func() error {
		sources, err := management.observeTenantSources(ctx, expected)
		if err != nil {
			return err
		}
		if err := o.desired.verifier.Verify(desired, sources.observed.DPUCluster); err != nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Selected tenant source root differs from retained intent.")
		}
		return validationTenantReady(o.desired.rules.TenantReadiness(expected, sources.observed))
	})
	if err != nil {
		r.pendingDependents("tenant/api", "tenant/sources")
		r.pendingDependents("tenant/nodes", "tenant/sources")
		return nil, err
	}
	var tenant *tenantClient
	err = r.run("tenant/api", func() error {
		var openErr error
		tenant, openErr = open(ctx, management)
		return openErr
	})
	if err != nil {
		r.pendingDependents("tenant/nodes", "tenant/api")
		return nil, err
	}
	err = r.run("tenant/nodes", func() error {
		if err := tenant.VerifyIdentity(ctx); err != nil {
			return err
		}
		nodes, err := o.validationSelectedTenantNodes(ctx, management, tenant)
		if err != nil {
			return err
		}
		if len(nodes) != len(o.desired.plan.Intent().Devices) {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "Tenant Node binding does not cover the retained selected devices.")
		}
		for _, node := range nodes {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&node)
			if err != nil {
				return err
			}
			if err := validationReady(o.desired.rules.ResourceReadiness(&unstructured.Unstructured{Object: object})); err != nil {
				return err
			}
		}

		return tenant.VerifyIdentity(ctx)
	})
	return tenant, err
}

func verifyValidationNamespace(ctx context.Context, reader resourceReader, expected *unstructured.Unstructured) error {
	current, err := reader.Get(ctx, expected.DeepCopy())
	if err != nil {
		return err
	}
	phase, _, _ := unstructured.NestedString(current.Object, "status", "phase")
	if current.GetAPIVersion() != "v1" || current.GetKind() != "Namespace" || current.GetName() != expected.GetName() || current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil || phase != "Active" {
		return validationFailure(validation.Failed, validation.ReasonHealth, "A selected Namespace is not currently Active with a stable nondeleting identity.")
	}
	return nil
}

// Preserve the read collector's failed/unknown distinction. Deployment preflight
// intentionally collapses it to blocked and is not an observation result model.
func (c *APIClient) observeValidationTenantStorage(ctx context.Context, namespace string) managementprobe.TenantStorageResult {
	if c == nil {
		return managementprobe.TenantStorageResult{State: "unknown", ReasonCode: "TENANT_STORAGE_CLIENT_UNAVAILABLE"}
	}
	return (managementprobe.Collector{Kubernetes: c.access.Client, Dynamic: c.dynamic}).CollectTenantStorage(ctx, namespace)
}
func validationTenantStorageResult(result managementprobe.TenantStorageResult) error {
	if result.State == "passed" {
		return nil
	}
	if result.State == "failed" {
		reason := validation.ReasonHealth
		if result.ReasonCode == "TENANT_STORAGE_RESOURCE_MISSING" {
			reason = validation.ReasonMissing
		}
		return validationFailure(validation.Failed, reason, "Selected tenant backing storage has a missing or unhealthy required resource.")
	}
	reason := validation.ReasonUnavailable
	switch result.ReasonCode {
	case "TENANT_STORAGE_ACCESS_DENIED":
		reason = validation.ReasonDenied
	case "TENANT_STORAGE_CANCELED":
		reason = validation.ReasonCanceled
	case "TENANT_STORAGE_CHANGED_DURING_CHECK":
		reason = validation.ReasonIncomplete
	}
	return validationFailure(validation.Unverified, reason, "Selected tenant backing storage could not be completely inspected.")
}
