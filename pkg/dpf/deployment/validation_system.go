// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
)

var validationTenantDefinitionNames = []string{"cidrpools.nv-ipam.nvidia.com", "ippools.nv-ipam.nvidia.com", "network-attachment-definitions.k8s.cni.cncf.io", "nodeserviceinterfaces.svc.dpu.nvidia.com", "servicechains.svc.dpu.nvidia.com", "servicechainsets.svc.dpu.nvidia.com", "serviceinterfaces.svc.dpu.nvidia.com", "serviceinterfacesets.svc.dpu.nvidia.com"}
var validationDirectManagers = []struct{ component, name, role string }{{"dpf-provisioning-controller-manager", "dpf-provisioning-controller-manager", "provisioning-controller"}, {"dpuservice-controller-manager", "dpuservice-controller-manager", "dpuservice-controller"}, {"kamaji-cluster-manager", "kamaji-cm-controller-manager", "kamaji-cluster-manager"}}

func systemRoleID(role systemArtifactRole) string {
	target := "tenant"
	if role.Management {
		target = "management"
	}
	return "system/role/" + role.Component + "/" + target
}
func systemRoleKind(role systemArtifactRole) string {
	for _, component := range systemArtifactComponents {
		if component.name == role.Component {
			if role.Management {
				return "Deployment"
			}
			return component.tenantKind
		}
	}
	return ""
}
func (o *validationObserver) systemValidationChecks() []validation.CheckSpec {
	result := []validation.CheckSpec{validationSpec("system/artifact", validation.AreaSystem, "Locked system networking chart", false), validationSpec("system/coverage", validation.AreaSystem, "Enabled system component graph", true), validationSpec("definitions/tenant/coverage", validation.AreaDefinitions, "Projected tenant API definitions", false)}
	for _, manager := range validationDirectManagers {
		result = append(result, validationSpec("system/manager/"+manager.role, validation.AreaSystem, manager.role, false))
	}
	for _, role := range systemArtifactRoles() {
		for _, part := range []string{"service", "application", "workload"} {
			result = append(result, validationSpec(systemRoleID(role)+"/"+part, validation.AreaSystem, role.Component+" "+strings.TrimPrefix(systemRoleID(role), "system/role/"), false))
		}
		for _, name := range systemArtifactAttachmentNames(role) {
			spec := validationSpec(systemRoleID(role)+"/attachment/"+name, validation.AreaSystem, role.Component+" network attachment "+name, false)
			spec.CohortBound = true
			result = append(result, spec)
		}
		if systemRoleKind(role) == "DaemonSet" {
			for _, device := range o.desired.plan.Intent().Devices {
				spec := validationSpec(systemRoleID(role)+"/device/"+url.PathEscape(device.Serial), validation.AreaSystem, role.Component+" device "+device.Serial, false)
				spec.CohortBound = true
				result = append(result, spec)
			}
		}
	}
	for _, name := range validationTenantDefinitionNames {
		result = append(result, validationSpec("definitions/tenant/"+name, validation.AreaDefinitions, name, false))
	}
	return result
}
func (o *validationObserver) loadValidationSystemArtifacts(ctx context.Context) (*systemArtifacts, error) {
	o.systemMu.Lock()
	defer o.systemMu.Unlock()
	if o.systemArtifacts != nil {
		return o.systemArtifacts, nil
	}
	release, err := releases.Resolve(o.desired.plan.Intent().Release)
	if err != nil || release.SystemNetworking == nil {
		return nil, fmt.Errorf("selected system networking artifact is unavailable")
	}
	requestCtx, cancel := context.WithTimeout(ctx, o.request.RequestTimeout)
	defer cancel()
	archive, err := o.fetchSystemChart(requestCtx, release.SystemNetworking.ArchiveURL())
	if err != nil {
		return nil, err
	}
	projected, err := projectSystemArtifacts(archive, o.desired.plan.Intent())
	if err != nil {
		return nil, err
	}
	o.systemArtifacts = &projected
	return &projected, nil
}
func (o *validationObserver) observeSystemResources(ctx context.Context, management validationManagementReader, tenant *tenantClient, r *validationRecorder) error {
	var artifacts *systemArtifacts
	r.run("system/artifact", func() error { var err error; artifacts, err = o.loadValidationSystemArtifacts(ctx); return err })
	release, err := releases.Resolve(o.desired.plan.Intent().Release)
	if err != nil {
		return err
	}
	for _, manager := range validationDirectManagers {
		err := r.run(validation.CheckID("system/manager/"+manager.role), func() error {
			current, err := management.Get(ctx, validationObject("apps/v1", "Deployment", o.desired.operatorNamespace, manager.name))
			if err != nil {
				return err
			}
			expectedMembership := validationApplySetID(manager.role, o.desired.operatorNamespace)
			if current.GetLabels()[dpfComponentLabel] != manager.component || current.GetLabels()["operator.dpu.nvidia.com/dpf-version"] != o.desired.operatorVersion || current.GetLabels()["applyset.kubernetes.io/part-of"] != expectedMembership {
				return validationFailure(validation.Failed, validation.ReasonHealth, "System manager identity, component version or ApplySet membership differs.")
			}
			images, err := containerImages(current)
			if err != nil || images["manager"] != release.Operator.Image {
				return validationFailure(validation.Failed, validation.ReasonHealth, "System manager image differs from the selected operator release.")
			}
			if err := validationReady(o.desired.rules.ResourceReadiness(current)); err != nil {
				return err
			}
			return verifyValidationDeploymentPods(ctx, management, current)
		})
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	}
	services, listErr := management.List(ctx, validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", o.desired.operatorNamespace, ""), "")
	var nodes map[string]corev1.Node
	if tenant != nil {
		nodes, _ = o.validationSelectedTenantNodes(ctx, management, tenant)
	}
	for _, role := range systemArtifactRoles() {
		prefix := systemRoleID(role)
		var service, application *unstructured.Unstructured
		var expected systemArtifactExpectation
		if artifacts != nil {
			expected = artifacts.Roles[role]
		} else {
			expected = systemArtifactExpectation{WorkloadKind: systemRoleKind(role), Values: systemArtifactValues(o.desired.plan.Intent(), release, role, role.Management || validationPerClusterRole(role))}
		}
		serviceErr := r.run(validation.CheckID(prefix+"/service"), func() error {
			if listErr != nil {
				return listErr
			}
			var err error
			service, err = o.selectValidationSystemService(services, role, expected.Values, release)
			return err
		})
		if validationIdentityChanged(serviceErr) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: serviceErr}
		}
		if service == nil {
			r.pendingDependents(prefix+"/", validation.CheckID(prefix+"/service"))
			continue
		}
		appErr := r.run(validation.CheckID(prefix+"/application"), func() error {
			target := o.desired.plan.Intent().Bootstrap.Tenant.Name
			if role.Management {
				target = "in-cluster"
			}
			var err error
			application, err = management.getHBNRuntimeApplication(ctx, o.desired.operatorNamespace, dpfApplicationName(target, service.GetName()))
			if err != nil {
				return err
			}
			return o.verifyValidationSystemApplication(role, service, application, release)
		})
		if validationIdentityChanged(appErr) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: appErr}
		}
		if appErr != nil {
			r.pendingDependents(prefix+"/", validation.CheckID(prefix+"/application"))
			continue
		}
		for _, name := range systemArtifactAttachmentNames(role) {
			err := r.run(validation.CheckID(prefix+"/attachment/"+name), func() error {
				if artifacts == nil {
					return validationFailure(validation.Unverified, validation.ReasonDependency, "The locked chart could not be projected for attachment provenance.")
				}
				if tenant == nil {
					return r.tenantDependencyError()
				}
				expectedAttachment := expected.Attachments[name]
				if expectedAttachment == nil {
					return validationFailure(validation.Failed, validation.ReasonIncomplete, "Locked system role does not contain its required network attachment.")
				}
				return verifyValidationSystemAttachment(ctx, tenant, application, expectedAttachment, service.GetNamespace())
			})
			if validationIdentityChanged(err) {
				return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
			}
		}
		err := r.run(validation.CheckID(prefix+"/workload"), func() error {
			if artifacts == nil {
				return validationFailure(validation.Unverified, validation.ReasonDependency, "The locked system chart could not be projected for workload provenance.")
			}
			return o.observeValidationSystemWorkload(ctx, management, tenant, role, service, application, expected, nodes, r)
		})
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
		r.pendingDependents(prefix+"/device/", validation.CheckID(prefix+"/workload"))
		// Current semantic anchors fence correlated App/workload observations.
		current, err := management.Get(ctx, validationObject(service.GetAPIVersion(), service.GetKind(), service.GetNamespace(), service.GetName()))
		if err == nil && !sameValidationSemantic(service, current) {
			err = fmt.Errorf("system service changed: %w", errValidationResourceChanged)
		}
		if err == nil {
			var currentApp *unstructured.Unstructured
			currentApp, err = management.getHBNRuntimeApplication(ctx, application.GetNamespace(), application.GetName())
			if err == nil && !sameValidationSemantic(application, currentApp) {
				err = fmt.Errorf("system app changed: %w", errValidationResourceChanged)
			}
		}
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
		if err != nil {
			r.recordError(validation.CheckID(prefix+"/application"), err)
			r.record(validation.CheckID(prefix+"/workload"), validation.Unverified, validation.ReasonDependency, "System source could not be fenced after workload reads.", "Repeat validation after source inspection.")
		}
	}
	if tenant != nil && artifacts != nil {
		reader := validationTenantDefinitionReader{tenant: tenant}
		for _, name := range validationTenantDefinitionNames {
			err := r.run(validation.CheckID("definitions/tenant/"+name), func() error {
				object := artifacts.TenantCRDs[name]
				if object == nil {
					return validationFailure(validation.Failed, validation.ReasonIncomplete, "Locked system chart lacks a required tenant definition.")
				}
				expected, err := operatorCRDFromObject(object)
				if err != nil {
					return err
				}
				return verifyValidationCRD(ctx, reader, expected)
			})
			if validationIdentityChanged(err) {
				return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
			}
		}
	}
	if tenant == nil && artifacts != nil {
		r.pendingDependents("definitions/tenant/", "tenant/api")
	}
	r.aggregate("definitions/tenant/coverage", "definitions/tenant/", nil)
	r.aggregate("system/coverage", "system/", []validation.CheckID{"system/coverage"})
	return nil
}
func validationPerClusterRole(role systemArtifactRole) bool {
	for _, component := range systemArtifactComponents {
		if component.name == role.Component {
			return component.perCluster
		}
	}
	return false
}
func validationApplySetID(component, namespace string) string {
	digest := sha256.Sum256([]byte(component + "-applyset." + namespace + ".Secret."))
	return "applyset-" + base64.RawURLEncoding.EncodeToString(digest[:]) + "-v1"
}
func (o *validationObserver) selectValidationSystemService(services []unstructured.Unstructured, role systemArtifactRole, values map[string]any, release releases.Release) (*unstructured.Unstructured, error) {
	var selected *unstructured.Unstructured
	tenant := o.desired.plan.Intent().Bootstrap.Tenant
	for i := range services {
		service := &services[i]
		if service.GetLabels()[dpfComponentLabel] != role.Component {
			continue
		}
		management, _, err := unstructured.NestedBool(service.Object, "spec", "deployInCluster")
		if err != nil {
			return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Selected system service role is malformed.")
		}
		if management != role.Management {
			continue
		}
		labels := service.GetLabels()
		if role.Management && (labels[dpfDPUClusterNameLabel] != tenant.Name || labels[dpfDPUClusterNamespaceLabel] != tenant.Namespace) {
			continue
		}
		if !role.Management && (labels[dpfDPUClusterNameLabel] != "" || labels[dpfDPUClusterNamespaceLabel] != "") {
			continue
		}
		if selected != nil {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "More than one current system service matches the selected role.")
		}
		selected = service
	}
	if selected == nil {
		return nil, validationFailure(validation.Failed, validation.ReasonMissing, "A required enabled system service role is absent.")
	}
	if selected.GetNamespace() != o.desired.operatorNamespace || selected.GetUID() == "" || selected.GetResourceVersion() == "" || selected.GetDeletionTimestamp() != nil || selected.GetLabels()["operator.dpu.nvidia.com/dpf-version"] != o.desired.operatorVersion || selected.GetLabels()["applyset.kubernetes.io/part-of"] != validationApplySetID(role.Component, o.desired.operatorNamespace) {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Selected system service identity, version or ApplySet binding differs.")
	}
	source, _, _ := unstructured.NestedMap(selected.Object, "spec", "helmChart", "source")
	repo, _, _ := unstructured.NestedString(source, "repoURL")
	chart, _, _ := unstructured.NestedString(source, "chart")
	version, _, _ := unstructured.NestedString(source, "version")
	path, _, _ := unstructured.NestedString(source, "path")
	if release.SystemNetworking == nil || repo != release.SystemNetworking.ChartRepository || chart != release.SystemNetworking.Chart || version != release.SystemNetworking.ChartVersion || path != "" {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "System service chart source differs from the selected release.")
	}
	currentValues, _, _ := unstructured.NestedMap(selected.Object, "spec", "helmChart", "values")
	if !validationMapContains(currentValues, values) {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "System service effective values differ from retained component intent.")
	}
	if err := validationReady(o.desired.rules.ValidationConditions(selected, "Ready", "ApplicationsReconciled", "ApplicationsReady")); err != nil {
		return nil, err
	}
	return selected, nil
}
func validationMapContains(current, expected map[string]any) bool {
	for key, want := range expected {
		got, ok := current[key]
		if !ok {
			return false
		}
		if sub, ok := want.(map[string]any); ok {
			live, ok := got.(map[string]any)
			if !ok || !validationMapContains(live, sub) {
				return false
			}
		} else if !reflect.DeepEqual(want, got) {
			return false
		}
	}
	return true
}
func sameValidationSemantic(before, after *unstructured.Unstructured) bool {
	return before != nil && after != nil && before.GetUID() == after.GetUID() && before.GetGeneration() == after.GetGeneration() && after.GetDeletionTimestamp() == nil && reflect.DeepEqual(before.Object["spec"], after.Object["spec"]) && maps.Equal(before.GetLabels(), after.GetLabels()) && maps.Equal(before.GetAnnotations(), after.GetAnnotations())
}

func (o *validationObserver) verifyValidationSystemApplication(role systemArtifactRole, service, app *unstructured.Unstructured, release releases.Release) error {
	tenant := o.desired.plan.Intent().Bootstrap.Tenant
	target := tenant.Name
	targetNamespace := service.GetNamespace()
	if role.Management {
		target = "in-cluster"
	}
	if app == nil || app.GetKind() != "Application" || app.GetAPIVersion() != "argoproj.io/v1alpha1" || app.GetName() != dpfApplicationName(target, service.GetName()) || app.GetNamespace() != o.desired.operatorNamespace || app.GetUID() == "" || app.GetResourceVersion() == "" || app.GetDeletionTimestamp() != nil || !slices.Contains(app.GetFinalizers(), argoApplicationFinalizer) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System Application identity or lifecycle binding differs.")
	}
	appLabels := app.GetLabels()
	if appLabels[dpfComponentLabel] != dpfServiceManagerComponent || appLabels[dpfDPUServiceNameLabel] != service.GetName() || appLabels[dpfDPUServiceNamespaceLabel] != service.GetNamespace() || appLabels[dpfDPUClusterNameLabel] != target || (!role.Management && appLabels[dpfDPUClusterNamespaceLabel] != tenant.Namespace) || (role.Management && appLabels[dpfDPUClusterNamespaceLabel] != "") {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System Application service or target labels differ.")
	}
	source, found, _ := unstructured.NestedMap(app.Object, "spec", "source")
	comparedSource, compared, _ := unstructured.NestedMap(app.Object, "status", "sync", "comparedTo", "source")
	destination, destFound, _ := unstructured.NestedMap(app.Object, "spec", "destination")
	comparedDestination, comparedDest, _ := unstructured.NestedMap(app.Object, "status", "sync", "comparedTo", "destination")
	if !found || !compared || !destFound || !comparedDest || !jsonEqual(source, comparedSource) || !jsonEqual(destination, comparedDestination) {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "System Application has not compared its current source and destination.")
	}
	values, _, _ := unstructured.NestedMap(service.Object, "spec", "helmChart", "values")
	daemonSet, _, _ := unstructured.NestedMap(service.Object, "spec", "serviceDaemonSet")
	if daemonSet == nil {
		daemonSet = map[string]any{}
	}
	daemonLabels, _, _ := unstructured.NestedMap(daemonSet, "labels")
	if daemonLabels == nil {
		daemonLabels = map[string]any{}
	}
	// DPF assigns the system service ID in status, not as retained input. The
	// App must copy its current service label rather than another role's label.
	labelsID := appSystemServiceID(service)
	if labelsID == "" {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "System service has no current service identity for Application values.")
	}
	daemonLabels[hbnServiceIDLabel] = labelsID
	daemonSet["labels"] = daemonLabels
	daemonAnnotations, _, _ := unstructured.NestedMap(daemonSet, "annotations")
	if daemonAnnotations == nil {
		daemonAnnotations = map[string]any{}
	}
	daemonAnnotations[dpfDPUClusterNameLabel] = target
	daemonAnnotations[dpfDPUClusterNamespaceLabel] = tenant.Namespace
	daemonSet["annotations"] = daemonAnnotations
	if values == nil {
		values = map[string]any{}
	}
	values["global"] = map[string]any{"serviceDaemonSet": daemonSet}
	expectedSource := map[string]any{"repoURL": release.SystemNetworking.ChartRepository, "chart": release.SystemNetworking.Chart, "targetRevision": release.SystemNetworking.ChartVersion, "helm": map[string]any{"valuesObject": values}}
	if !jsonEqual(source, expectedSource) || !jsonEqual(destination, map[string]any{"name": target, "namespace": targetNamespace}) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System Application effective chart values or destination differ from its selected service.")
	}
	if sources, exists, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); exists && len(sources) != 0 {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System Application contains unsupported alternate sources.")
	}
	syncState, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
	stamp, _, _ := unstructured.NestedString(app.Object, "status", "reconciledAt")
	reconciledAt, timeErr := time.Parse(time.RFC3339Nano, stamp)
	if syncState != "Synced" || health != "Healthy" || timeErr != nil || reconciledAt.After(time.Now()) || reconciledAt.Before(service.GetCreationTimestamp().Time) {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "System Application has not reported a current healthy synchronized reconciliation.")
	}
	return nil
}
func appSystemServiceID(service *unstructured.Unstructured) string {
	id, _, _ := unstructured.NestedString(service.Object, "status", "serviceID")
	labels, _, _ := unstructured.NestedStringMap(service.Object, "spec", "serviceDaemonSet", "labels")
	if id == "" || labels[hbnServiceIDLabel] != id {
		return ""
	}
	return id
}

type validationTenantDefinitionReader struct{ tenant *tenantClient }

func (r validationTenantDefinitionReader) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	var object unstructured.Unstructured
	if err := r.tenant.api.Discovery().RESTClient().Get().AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + wanted.GetName()).Do(ctx).Into(&object); err != nil {
		return nil, err
	}
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return &object, nil
}
func (r validationTenantDefinitionReader) discoverResourceList(ctx context.Context, version string) (*metav1.APIResourceList, error) {
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	resources, err := discovery.ToServerResourcesInterfaceWithContext(r.tenant.api.Discovery()).ServerResourcesForGroupVersionWithContext(ctx, version)
	if err != nil {
		return nil, err
	}
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return resources, nil
}

// aggregate preserves denied/unavailable dependencies instead of turning their
// absence of evidence into healthy coverage or an asserted health failure.
func (r *validationRecorder) aggregate(id validation.CheckID, prefix string, exclude []validation.CheckID) {
	r.start(id)
	state, reason := validation.Passed, validation.ReasonNone
	for _, key := range r.order {
		if key == id || slices.Contains(exclude, key) || !strings.HasPrefix(string(key), prefix) {
			continue
		}
		out, ok := r.outcomes[key]
		if !ok {
			if state != validation.Failed {
				state, reason = validation.Unverified, validation.ReasonDependency
			}
			continue
		}
		switch out.State {
		case validation.Failed:
			state, reason = validation.Failed, validation.ReasonIncomplete
		case validation.Unverified, validation.Skipped:
			if state != validation.Failed {
				state, reason = validation.Unverified, validation.ReasonIncomplete
			}
		case validation.Pending:
			if state == validation.Passed {
				state, reason = validation.Pending, validation.ReasonProgressing
			}
		}
	}
	summary := "Every selected component and descendant obligation is verified."
	action := ""
	if state != validation.Passed {
		summary = "Selected component coverage is incomplete; inspect the individual obligation rows."
		action = "Resolve failed or unverified prerequisites and repeat validation."
	}
	r.record(id, state, reason, summary, action)
}

// Compile-time guards keep the tenant observation adapter read-only.
var _ operatorCRDReader = validationTenantDefinitionReader{}

// pendingDependents keeps ordinary controller convergence eligible for the
// runner's wait policy. Permission failures and absent evidence remain unverified.
func (r *validationRecorder) pendingDependents(prefix string, parent validation.CheckID) {
	if r.outcomes[parent].State != validation.Pending {
		return
	}
	for _, id := range r.order {
		if id == parent || !strings.HasPrefix(string(id), prefix) {
			continue
		}
		if _, ok := r.outcomes[id]; !ok {
			r.record(id, validation.Pending, validation.ReasonProgressing, "A selected controller dependency is still progressing.", "Wait for the dependency to reconcile or inspect its controller.")
		}
	}
}
func (r *validationRecorder) tenantDependencyError() error {
	if r.outcomes["tenant/sources"].State == validation.Pending || r.outcomes["tenant/api"].State == validation.Pending {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "Selected tenant control plane dependency is still progressing.")
	}
	return validationFailure(validation.Unverified, validation.ReasonDependency, "Selected tenant API could not be verified.")
}
