// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// compileHBNRuntimeBinding is the one owner of expected Service and App values.
// It implements the HBN subset of DPF fc4974ed's generateDPUService,
// generateDPUServiceInterface, addNetworkAnnotationToServiceDaemonSet and
// argoCDValuesFromDPUService. Observed random names are accepted only after
// owner, cohort selector and complete retained-spec checks; configuration is
// never taken from the observed Service or App.
func compileHBNRuntimeBinding(execution *managementExecution, activation activationStageReceipt, fence hostNetworkLeaseFence,
	service *unstructured.Unstructured, interfaces []unstructured.Unstructured, image hbnPodImageIdentity,
) (hbnPodBindingIntent, error) {
	bad := func() (hbnPodBindingIntent, error) {
		return hbnPodBindingIntent{}, fmt.Errorf("HBN_RUNTIME_EXPECTATION_MISMATCH")
	}
	if execution == nil || execution.plan == nil || execution.reconciler == nil || service == nil || fence.activation.deployment == nil || !image.valid() {
		return bad()
	}
	objects, err := retainedWorkloadObjects(execution)
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
	template, err = execution.reconciler.schemas.DefaultedCopy(template)
	if err != nil {
		return bad()
	}
	configuration, err = execution.reconciler.schemas.DefaultedCopy(configuration)
	if err != nil {
		return bad()
	}
	desired := execution.plan.Intent()
	deployment := fence.activation.deployment
	reference := desired.Generation.ChartName
	if reference != hbnContainerName || template.GetName() != reference || configuration.GetName() != reference || deployment.GetUID() != activation.DeploymentUID {
		return bad()
	}
	parent := desired.Namespace + "_" + deployment.GetName()
	version := service.GetAnnotations()[hbnServiceVersionAnnotation]
	id, _, err := unstructured.NestedString(service.Object, "spec", "serviceID")
	_, systemComponent := service.GetLabels()[dpfComponentLabel]
	if len(service.GetOwnerReferences()) != 1 || err != nil || version == "" || id != deployment.GetName()+"_"+reference+"_"+version && id != "dpudeployment_"+deployment.GetName()+"_"+reference ||
		!activationOwnerMatches(service, "svc.dpu.nvidia.com/v1alpha1", "DPUDeployment", deployment.GetName(), activation.DeploymentUID) || service.GetLabels()[activationParentLabel] != parent ||
		service.GetLabels()[hbnServiceReferenceLabel] != reference || systemComponent {
		return bad()
	}
	// The selector's version name can be preserved by an in-place update. Bind
	// it to every allocated selected tenant Node, never copy a Service selector.
	versionKey := "svc.dpu.nvidia.com/dpuservice-" + reference + "-version"
	versionName := ""
	for _, lease := range fence.leases {
		node := uniqueObjectByName(fence.snapshot.TenantNodes, lease.TenantNodeName)
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
	source, _, err := unstructured.NestedMap(template.Object, "spec", "helmChart", "source")
	if err != nil || source == nil {
		return bad()
	}
	if _, ok := source["path"]; !ok {
		source["path"] = ""
	} // typed ApplicationSource.Path has no omitempty
	templateValues, _, err := unstructured.NestedMap(template.Object, "spec", "helmChart", "values")
	if err != nil {
		return bad()
	}
	configValues, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "helmChart", "values")
	if err != nil {
		return bad()
	}
	values := mergeHBNValues(configValues, templateValues)
	sds, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "serviceDaemonSet")
	if err != nil {
		return bad()
	}
	if sds == nil {
		sds = map[string]any{}
	}
	labels, _, err := unstructured.NestedMap(sds, "labels")
	if err != nil {
		return bad()
	}
	if labels == nil {
		labels = map[string]any{}
	}
	labels[hbnServiceReferenceLabel] = reference
	sds["labels"] = labels
	sds["nodeSelector"] = nodeSelector
	// HBN uses only predefined mybrhbn networks, so DPF's dynamic resource
	// injection and ConfigPorts branches cannot add unretained values here.
	config, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration")
	if err != nil {
		return bad()
	}
	for key := range config {
		if key != "helmChart" && key != "serviceDaemonSet" && key != "deployInCluster" {
			return bad()
		}
	}
	if value, ok := config["deployInCluster"]; ok && value != false {
		return bad()
	}
	names, networks, err := compileHBNServiceInterfaces(execution, service, interfaces, configuration, selector, clusterSelector, id, version)
	if err != nil {
		return bad()
	}
	spec := map[string]any{"helmChart": map[string]any{"source": source, "values": values}, "serviceID": id, "serviceDaemonSet": sds, "interfaces": names, "dpuClusterSelector": clusterSelector, "security": map[string]any{"privileged": true}}
	if value, ok := config["deployInCluster"]; ok {
		spec["deployInCluster"] = value
	}
	if security, found, e := unstructured.NestedMap(template.Object, "spec", "security"); e != nil {
		return bad()
	} else if found {
		spec["security"] = security
	}
	expected := runtimeObject("DPUService", service.GetName(), desired.Namespace, spec)
	expected, err = execution.reconciler.schemas.DefaultedCopy(expected)
	if err != nil {
		return bad()
	}
	expectedSpec, _, err := unstructured.NestedMap(expected.Object, "spec")
	if err != nil {
		return bad()
	}
	if !jsonEqual(service.Object["spec"], expectedSpec) {
		return bad()
	}
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
	annotation, err := compileHBNNetworkAnnotation(existing, networks)
	if err != nil {
		return bad()
	}
	annotations["k8s.v1.cni.cncf.io/networks"] = annotation
	effectiveSDS["annotations"] = annotations
	effective := mergeHBNValues(map[string]any{"serviceDaemonSet": effectiveSDS}, values)
	return hbnPodBindingIntent{DPUDeploymentName: deployment.GetName(), DPUDeploymentUID: string(activation.DeploymentUID), DPUServiceUID: string(service.GetUID()),
		ExpectedServiceSpec: expectedSpec, ApplicationValues: effective, ArgoNamespace: execution.operatorNamespace, DPUServiceName: service.GetName(), DPUServiceNamespace: desired.Namespace,
		DPUServiceCreatedAt: service.GetCreationTimestamp().Time, DPUClusterName: desired.Bootstrap.Tenant.Name, DPUClusterNamespace: desired.Bootstrap.Tenant.Namespace, TargetNamespace: desired.Namespace,
		ChartRepoURL: strings.TrimPrefix(stringValue(source, "repoURL"), "oci://"), ChartName: reference, ChartPath: stringValue(source, "path"), ChartVersion: stringValue(source, "version"), ChartReleaseName: stringValue(source, "releaseName"), ServiceID: id, Image: image}, nil
}

func runtimeObject(kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec}}
}

// mergeHBNValues mirrors DPF MergeMaps: destination wins recursively, including
// explicit false/zero/null. It never aliases or mutates either input.
func mergeHBNValues(dst, src map[string]any) map[string]any {
	result := runtime.DeepCopyJSON(src)
	if result == nil {
		result = map[string]any{}
	}
	for key, value := range dst {
		if left, ok := value.(map[string]any); ok {
			if right, ok := result[key].(map[string]any); ok {
				result[key] = mergeHBNValues(left, right)
				continue
			}
		}
		result[key] = runtime.DeepCopyJSONValue(value)
	}
	return result
}

func compileHBNServiceInterfaces(execution *managementExecution, service *unstructured.Unstructured, observed []unstructured.Unstructured, configuration *unstructured.Unstructured, selector, clusterSelector map[string]any, id, version string) ([]any, []hbnNetworkSelection, error) {
	bad := func() ([]any, []hbnNetworkSelection, error) {
		return nil, nil, fmt.Errorf("HBN_RUNTIME_INTERFACES_MISMATCH")
	}
	rows, found, err := unstructured.NestedSlice(configuration.Object, "spec", "interfaces")
	if err != nil || !found || len(rows) != 3 {
		return bad()
	}
	names := []string{}
	networks := []hbnNetworkSelection{}
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
		// The pinned typed ServiceInterfaceSetSpecTemplate serializes its empty
		// ObjectMeta as metadata:{}; structural defaulting does not create it.
		spec := map[string]any{"dpuClusterSelector": clusterSelector, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"nodeSelector": selector, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{hbnServiceIDLabel: id, "svc.dpu.nvidia.com/interface": name}}, "spec": map[string]any{"interfaceType": "service", "service": map[string]any{"serviceID": id, "network": "mybrhbn", "interfaceName": name}}}}}}
		expected, err := execution.reconciler.schemas.DefaultedCopy(runtimeObject("DPUServiceInterface", selected.GetName(), selected.GetNamespace(), spec))
		if err != nil || !jsonEqual(expected.Object["spec"], selected.Object["spec"]) {
			return bad()
		}
		names = append(names, selected.GetName())
		networks = append(networks, hbnNetworkSelection{Name: "mybrhbn", Interface: name})
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

// The retained HBN profile's Multus fields, in pinned Multus JSON field order.
// Additional unqualified fields fail closed rather than silently disappearing.
type hbnNetworkSelection struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace,omitempty"`
	Interface string          `json:"interface,omitempty"`
	CNIArgs   *map[string]any `json:"cni-args"`
}

func compileHBNNetworkAnnotation(existing string, networks []hbnNetworkSelection) (string, error) {
	generated := map[string]hbnNetworkSelection{"invalid-interface": {Name: "invalid-network", Namespace: "invalid-namespace", Interface: "invalid-interface"}}
	for _, network := range networks {
		generated[network.Interface] = network
	}
	var result []hbnNetworkSelection
	if existing != "" {
		decoder := json.NewDecoder(strings.NewReader(existing))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result); err != nil {
			return "", err
		}
	}
	seen := map[string]bool{}
	for i, network := range result {
		if seen[network.Interface] {
			return "", fmt.Errorf("duplicate network interface")
		}
		seen[network.Interface] = true
		if value, found := generated[network.Interface]; found {
			if network.Name != "" {
				value.Name = network.Name
			}
			if network.Namespace != "" {
				value.Namespace = network.Namespace
			}
			if network.CNIArgs != nil {
				value.CNIArgs = network.CNIArgs
			}
			result[i] = value
			delete(generated, network.Interface)
		}
	}
	for _, value := range generated {
		result = append(result, value)
	}
	slices.SortFunc(result, func(a, b hbnNetworkSelection) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Interface, b.Interface)
	})
	encoded, err := json.Marshal(result)
	return string(encoded), err
}
