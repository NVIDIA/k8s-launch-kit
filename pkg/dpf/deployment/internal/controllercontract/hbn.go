// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

type HBNServiceProjection struct {
	Spec map[string]any
	// App values use the pinned producer's pre-defaulting maps, not values
	// introduced only by structural defaults on the Service API object.
	Source    map[string]any
	Values    map[string]any
	DaemonSet map[string]any
}

// HBNServiceSpec projects retained template/configuration values with
// explicit controller identity references. It performs no reads and accepts no
// observed configuration values; late runtime validation also qualifies the
// referenced interface objects before calling it.
func (r *Rules) HBNServiceSpec(template, configuration *unstructured.Unstructured,
	name, namespace, reference, id string, nodeSelector, clusterSelector map[string]any, names []any,
) (HBNServiceProjection, error) {
	if !r.valid() || template == nil || configuration == nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	source, _, err := unstructured.NestedMap(template.Object, "spec", "helmChart", "source")
	if err != nil || source == nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	if _, ok := source["path"]; !ok {
		source["path"] = ""
	} // typed ApplicationSource.Path has no omitempty
	templateValues, _, err := unstructured.NestedMap(template.Object, "spec", "helmChart", "values")
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	configValues, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "helmChart", "values")
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	values := mergeHBNValues(configValues, templateValues)
	sds, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "serviceDaemonSet")
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	if sds == nil {
		sds = map[string]any{}
	}
	labels, _, err := unstructured.NestedMap(sds, "labels")
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	if labels == nil {
		labels = map[string]any{}
	}
	labels["svc.dpu.nvidia.com/dpudeployment-service"] = reference
	sds["labels"] = labels
	sds["nodeSelector"] = runtime.DeepCopyJSON(nodeSelector)
	// HBN uses only predefined mybrhbn networks, so DPF's dynamic resource
	// injection and ConfigPorts branches cannot add unretained values here.
	config, _, err := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration")
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	for key := range config {
		if key != "helmChart" && key != "serviceDaemonSet" && key != "deployInCluster" {
			return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
		}
	}
	if value, ok := config["deployInCluster"]; ok && value != false {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	spec := map[string]any{"helmChart": map[string]any{"source": source, "values": values}, "serviceID": id, "serviceDaemonSet": sds, "interfaces": runtime.DeepCopyJSONValue(names), "dpuClusterSelector": runtime.DeepCopyJSON(clusterSelector), "security": map[string]any{"privileged": true}}
	if value, ok := config["deployInCluster"]; ok {
		spec["deployInCluster"] = value
	}
	if security, found, e := unstructured.NestedMap(template.Object, "spec", "security"); e != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	} else if found {
		spec["security"] = security
	}
	expected := runtimeObject("DPUService", name, namespace, spec)
	expected, err = r.DefaultedCopy(expected)
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	expectedSpec, _, err := unstructured.NestedMap(expected.Object, "spec")
	if err != nil {
		return HBNServiceProjection{}, fmt.Errorf("HBN_SERVICE_EXPECTATION_MISMATCH")
	}
	return HBNServiceProjection{Spec: expectedSpec, Source: source, Values: values, DaemonSet: sds}, nil
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

// The retained HBN profile's Multus fields, in pinned Multus JSON field order.
// Additional unqualified fields fail closed rather than silently disappearing.
type HBNNetworkSelection struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace,omitempty"`
	Interface string          `json:"interface,omitempty"`
	CNIArgs   *map[string]any `json:"cni-args"`
}

func (r *Rules) HBNNetworkAnnotation(existing string, networks []HBNNetworkSelection) (string, error) {
	if !r.valid() {
		return "", fmt.Errorf("selected controller rules are required")
	}
	generated := map[string]HBNNetworkSelection{"invalid-interface": {Name: "invalid-network", Namespace: "invalid-namespace", Interface: "invalid-interface"}}
	for _, network := range networks {
		generated[network.Interface] = network
	}
	var result []HBNNetworkSelection
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
	slices.SortFunc(result, func(a, b HBNNetworkSelection) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Interface, b.Interface)
	})
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

// HBNServiceInterface projects only the expected retained interface body.
func (r *Rules) HBNServiceInterface(objectName, namespace, name, id string, selector, clusterSelector map[string]any) (*unstructured.Unstructured, error) {
	if !r.valid() {
		return nil, fmt.Errorf("selected controller rules are required")
	}
	spec := map[string]any{"dpuClusterSelector": clusterSelector, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"nodeSelector": selector, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"svc.dpu.nvidia.com/service": id, "svc.dpu.nvidia.com/interface": name}}, "spec": map[string]any{"interfaceType": "service", "service": map[string]any{"serviceID": id, "network": "mybrhbn", "interfaceName": name}}}}}}
	expected, err := r.DefaultedCopy(runtimeObject("DPUServiceInterface", objectName, namespace, spec))
	return expected, err
}

// HBNApplicationValues merges the explicit retained App inputs with the controller-computed daemonset values.
func (r *Rules) HBNApplicationValues(daemonSet, values map[string]any) (map[string]any, error) {
	if !r.valid() {
		return nil, fmt.Errorf("selected controller rules are required")
	}
	return mergeHBNValues(map[string]any{"serviceDaemonSet": daemonSet}, values), nil
}
