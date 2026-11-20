// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// compileHBNRuntimeBinding binds pure expected Service and App projections to
// fresh selected-child, owner, cohort and image observations.
// It implements the HBN subset of pinned DPF 26.8's generateDPUService,
// generateDPUServiceInterface, addNetworkAnnotationToServiceDaemonSet and
// argoCDValuesFromDPUService. Observed random names are accepted only after
// owner, cohort selector and complete retained-spec checks; configuration is
// never taken from the observed Service or App.
func compileHBNRuntimeBinding(execution *managementVerificationInputs, activation activationStageReceipt, fence hostNetworkLeaseFence,
	service *unstructured.Unstructured, interfaces []unstructured.Unstructured, image hbnPodImageIdentity,
) (hbnPodBindingIntent, error) {
	if execution == nil || execution.plan == nil || execution.reconciler == nil || !image.valid() {
		return hbnPodBindingIntent{}, fmt.Errorf("HBN_RUNTIME_EXPECTATION_MISMATCH")
	}
	return compileCurrentHBNBinding(execution.plan, execution.rules, execution.operatorNamespace, activation.DeploymentUID, fence.activation.deployment, fence.leases, fence.snapshot.TenantNodes, service, interfaces, image)
}

// compileCurrentHBNBinding has only retained desired and fresh current inputs.
func compileCurrentHBNBinding(plan *generation.Plan, rules *controllercontract.Rules, operatorNamespace string, deploymentUID types.UID, deployment *unstructured.Unstructured, leases []hostWorkloadLease, tenantNodes []unstructured.Unstructured, service *unstructured.Unstructured, interfaces []unstructured.Unstructured, image hbnPodImageIdentity) (hbnPodBindingIntent, error) {
	bad := func() (hbnPodBindingIntent, error) {
		return hbnPodBindingIntent{}, fmt.Errorf("HBN_RUNTIME_EXPECTATION_MISMATCH")
	}
	if plan == nil || rules == nil || service == nil || deployment == nil {
		return bad()
	}
	objects, err := retainedPlanWorkloadObjects(plan)
	if err != nil {
		return bad()
	}
	var template, configuration *unstructured.Unstructured
	for _, object := range objects {
		switch object.GetKind() {
		case "DPUServiceTemplate":
			template = object
		case "DPUServiceConfiguration":
			configuration = object
		}
	}
	if template == nil || configuration == nil {
		return bad()
	}
	template, err = rules.DefaultedCopy(template)
	if err != nil {
		return bad()
	}
	configuration, err = rules.DefaultedCopy(configuration)
	if err != nil {
		return bad()
	}
	desired := plan.Intent()
	reference := desired.Generation.ChartName
	if reference != hbnContainerName || template.GetName() != reference || configuration.GetName() != reference || deployment.GetUID() != deploymentUID {
		return bad()
	}
	parent := desired.Namespace + "_" + deployment.GetName()
	version := service.GetAnnotations()[hbnServiceVersionAnnotation]
	id, _, err := unstructured.NestedString(service.Object, "spec", "serviceID")
	_, systemComponent := service.GetLabels()[dpfComponentLabel]
	if len(service.GetOwnerReferences()) != 1 || err != nil || version == "" || id != deployment.GetName()+"_"+reference+"_"+version && id != "dpudeployment_"+deployment.GetName()+"_"+reference ||
		!activationOwnerMatches(service, "svc.dpu.nvidia.com/v1alpha1", "DPUDeployment", deployment.GetName(), deploymentUID) || service.GetLabels()[activationParentLabel] != parent ||
		service.GetLabels()[hbnServiceReferenceLabel] != reference || systemComponent {
		return bad()
	}
	// The selector's version name can be preserved by an in-place update. Bind
	// it to every allocated selected tenant Node, never copy a Service selector.
	versionKey := "svc.dpu.nvidia.com/dpuservice-" + reference + "-version"
	versionName := ""
	for _, lease := range leases {
		node := uniqueObjectByName(tenantNodes, lease.TenantNodeName)
		if node == nil || node.GetUID() != lease.NodeUID || node.GetLabels()[activationParentLabel] != parent {
			return bad()
		}
		value := node.GetLabels()[versionKey]
		if len(validation.IsDNS1123Subdomain(value)) != 0 || value == "" || versionName != "" && versionName != value {
			return bad()
		}
		versionName = value
	}
	if versionName == "" {
		return bad()
	}
	expressions := []any{map[string]any{"key": versionKey, "operator": "In", "values": []any{versionName}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	nodeSelector := map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": expressions}}}
	selector := map[string]any{"matchExpressions": expressions}
	clusterExpressions := []any{}
	for _, key := range slices.Sorted(maps.Keys(desired.Bootstrap.Tenant.Labels)) {
		clusterExpressions = append(clusterExpressions, map[string]any{"key": key, "operator": "In", "values": []any{desired.Bootstrap.Tenant.Labels[key]}})
	}
	clusterSelector := map[string]any{"matchExpressions": clusterExpressions}
	names, networks, err := compileCurrentHBNServiceInterfaces(rules, service, interfaces, configuration, selector, clusterSelector, id, version)
	if err != nil {
		return bad()
	}
	projection, err := rules.HBNServiceSpec(template, configuration, service.GetName(), desired.Namespace, reference, id, nodeSelector, clusterSelector, names)
	if err != nil || !jsonEqual(service.Object["spec"], projection.Spec) {
		return bad()
	}
	expectedSpec, sds, values, source := projection.Spec, projection.DaemonSet, projection.Values, projection.Source
	effectiveSDS := runtime.DeepCopyJSON(sds)
	effectiveLabels := effectiveSDS["labels"].(map[string]any)
	effectiveLabels[hbnServiceIDLabel] = id
	annotations, _, err := unstructured.NestedMap(effectiveSDS, "annotations")
	if err != nil {
		return bad()
	}
	if annotations == nil {
		annotations = map[string]any{}
	}
	existing, _ := annotations["k8s.v1.cni.cncf.io/networks"].(string)
	annotation, err := rules.HBNNetworkAnnotation(existing, networks)
	if err != nil {
		return bad()
	}
	annotations["k8s.v1.cni.cncf.io/networks"] = annotation
	effectiveSDS["annotations"] = annotations
	effective, err := rules.HBNApplicationValues(effectiveSDS, values)
	if err != nil {
		return bad()
	}
	return hbnPodBindingIntent{DPUDeploymentName: deployment.GetName(), DPUDeploymentUID: string(deploymentUID), DPUServiceUID: string(service.GetUID()),
		ExpectedServiceSpec: expectedSpec, ApplicationValues: effective, ArgoNamespace: operatorNamespace, DPUServiceName: service.GetName(), DPUServiceNamespace: desired.Namespace,
		DPUServiceCreatedAt: service.GetCreationTimestamp().Time, DPUClusterName: desired.Bootstrap.Tenant.Name, DPUClusterNamespace: desired.Bootstrap.Tenant.Namespace, TargetNamespace: desired.Namespace,
		ChartRepoURL: strings.TrimPrefix(stringValue(source, "repoURL"), "oci://"), ChartName: desired.Generation.Chart, ChartPath: stringValue(source, "path"), ChartVersion: stringValue(source, "version"), ChartReleaseName: stringValue(source, "releaseName"), ServiceID: id, Image: image}, nil
}

func compileCurrentHBNServiceInterfaces(rules *controllercontract.Rules, service *unstructured.Unstructured, observed []unstructured.Unstructured, configuration *unstructured.Unstructured, selector, clusterSelector map[string]any, id, version string) ([]any, []controllercontract.HBNNetworkSelection, error) {
	bad := func() ([]any, []controllercontract.HBNNetworkSelection, error) {
		return nil, nil, fmt.Errorf("HBN_RUNTIME_INTERFACES_MISMATCH")
	}
	rows, found, err := unstructured.NestedSlice(configuration.Object, "spec", "interfaces")
	if err != nil || !found || len(rows) != 3 {
		return bad()
	}
	names := []string{}
	networks := []controllercontract.HBNNetworkSelection{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return bad()
		}
		name := stringValue(row, "name")
		if name != "p0_if" && name != "p1_if" && name != "pf0hpf_if" || stringValue(row, "network") != "mybrhbn" || len(row) != 2 {
			return bad()
		}
		var selected *unstructured.Unstructured
		for i := range observed {
			object := &observed[i]
			if object.GetAnnotations()[hbnServiceVersionAnnotation] != version {
				continue
			}
			port, _, _ := unstructured.NestedString(object.Object, "spec", "template", "spec", "template", "spec", "service", "interfaceName")
			if port != name {
				continue
			}
			if selected != nil {
				return bad()
			}
			selected = object
		}
		if selected == nil || selected.GetNamespace() != service.GetNamespace() || selected.GetUID() == "" || selected.GetResourceVersion() == "" || selected.GetGeneration() < 1 || selected.GetDeletionTimestamp() != nil ||
			selected.GetLabels()[activationParentLabel] != service.GetLabels()[activationParentLabel] || selected.GetLabels()[hbnServiceReferenceLabel] != hbnContainerName || selected.GetAnnotations()["dpu.nvidia.com/consumed-by"] != service.GetName() {
			return bad()
		}
		owners := selected.GetOwnerReferences()
		deploymentOwner, serviceOwner := false, false
		for _, owner := range owners {
			if owner.APIVersion != "svc.dpu.nvidia.com/v1alpha1" {
				return bad()
			}
			switch owner.Kind {
			case "DPUDeployment":
				deploymentOwner = owner.Name == service.GetOwnerReferences()[0].Name && owner.UID == service.GetOwnerReferences()[0].UID && owner.Controller != nil && *owner.Controller
			case "DPUService":
				serviceOwner = owner.Name == service.GetName() && owner.UID == service.GetUID() && owner.Controller != nil && !*owner.Controller
			default:
				return bad()
			}
		}
		if len(owners) != 2 || !deploymentOwner || !serviceOwner {
			return bad()
		}
		expected, err := rules.HBNServiceInterface(selected.GetName(), selected.GetNamespace(), name, id, selector, clusterSelector)
		if err != nil || !jsonEqual(expected.Object["spec"], selected.Object["spec"]) {
			return bad()
		}
		names = append(names, selected.GetName())
		networks = append(networks, controllercontract.HBNNetworkSelection{Name: "mybrhbn", Interface: name})
	}
	slices.Sort(names)
	if len(slices.Compact(names)) != 3 {
		return bad()
	}
	result := make([]any, len(names))
	for i, name := range names {
		result[i] = name
	}
	return result, networks, nil
}

func runtimeObject(kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec}}
}
