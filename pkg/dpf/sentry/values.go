// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sentry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/util/json"
)

const networksAnnotation = "k8s.v1.cni.cncf.io/networks"
const serviceIDLabel = "svc.dpu.nvidia.com/service"
const trustedSFAnnotation = "dpuservicenad.svc.dpu.nvidia.com/use-trusted-sfs"

// projectionInputs contains only declared interface/NAD objects. Projection may
// not discover or silently substitute an external network dependency.
type projectionInputs struct {
	interfaces map[string]*unstructured.Unstructured
	nads       map[string]*unstructured.Unstructured
}

func (p *projectionInputs) record(obj *unstructured.Unstructured) error {
	var destination map[string]*unstructured.Unstructured
	switch obj.GetKind() {
	case "DPUServiceInterface":
		destination = p.interfaces
	case "DPUServiceNAD":
		destination = p.nads
	default:
		return nil
	}
	key := obj.GetNamespace() + "/" + obj.GetName()
	if _, exists := destination[key]; exists {
		return fmt.Errorf("duplicate projection dependency %s/%s", obj.GetKind(), key)
	}
	destination[key] = obj
	return nil
}

// networkSelection preserves Multus v4.0.2's field order and explicit null
// cni-args. The Sentry profile uses only this bounded annotation shape; reject
// other fields rather than discarding values the DPF controller would retain.
type networkSelection struct {
	Name      string                  `json:"name"`
	Namespace string                  `json:"namespace,omitempty"`
	Interface string                  `json:"interface,omitempty"`
	CNIArgs   *map[string]interface{} `json:"cni-args"`
}

// This is the five-field standard-service value contract shared by both pinned
// releases. Core Kubernetes value types preserve upstream JSON omission and
// resource.Quantity canonicalization without importing the DPF root module.
type daemonSetInput struct {
	NodeSelector   *corev1.NodeSelector            `json:"nodeSelector,omitempty"`
	UpdateStrategy *appsv1.DaemonSetUpdateStrategy `json:"updateStrategy,omitempty"`
	Labels         map[string]string               `json:"labels,omitempty"`
	Annotations    map[string]string               `json:"annotations,omitempty"`
	Resources      corev1.ResourceList             `json:"resources,omitempty"`
}

// effectiveHelmValues models the identical standard-service transformations in
// DPF 26.8.0-rc.1 and 26.8.0: interfaces -> Multus sentinel/sort -> NAD resources
// -> service-ID label -> authoritative generated values merged over raw values.
// Native DPUService inputs are never mutated. System components/config ports
// belong to a different controller branch and are outside this profile.
func effectiveHelmValues(release string, service *unstructured.Unstructured, inputs projectionInputs) (map[string]interface{}, error) {
	if release != "26.8.0-rc.1" && release != "26.8.0" {
		return nil, fmt.Errorf("no effective Helm-input contract for DPF %q", release)
	}
	if service == nil || service.GetKind() != "DPUService" {
		return nil, fmt.Errorf("effective Helm inputs require a DPUService")
	}
	if _, system := service.GetLabels()["dpu.nvidia.com/component"]; system {
		return nil, fmt.Errorf("system-component Helm inputs are outside the Sentry profile")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(service.Object, "spec", "configPorts"); found {
		return nil, fmt.Errorf("config-port Helm inputs are outside the Sentry profile")
	}
	serviceID, _, err := unstructured.NestedString(service.Object, "spec", "serviceID")
	if err != nil || serviceID == "" {
		return nil, fmt.Errorf("sentry DPUService requires an explicit serviceID")
	}
	rawDaemonSet, _, err := unstructured.NestedMap(service.Object, "spec", "serviceDaemonSet")
	if err != nil {
		return nil, err
	}
	var daemonSet daemonSetInput
	if rawDaemonSet != nil {
		data, err := json.Marshal(rawDaemonSet)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&daemonSet); err != nil {
			return nil, fmt.Errorf("unsupported serviceDaemonSet input: %w", err)
		}
	}
	interfaces, hasInterfaces, err := unstructured.NestedStringSlice(service.Object, "spec", "interfaces")
	if err != nil {
		return nil, err
	}
	if hasInterfaces {
		generated := map[string]networkSelection{"invalid-interface": {Name: "invalid-network", Namespace: "invalid-namespace", Interface: "invalid-interface"}}
		resources := map[string]int{}
		for _, name := range interfaces {
			iface := inputs.interfaces[service.GetNamespace()+"/"+name]
			if iface == nil {
				return nil, fmt.Errorf("DPUService %s has undeclared interface %s", service.GetName(), name)
			}
			definition, found, err := unstructured.NestedMap(iface.Object, "spec", "template", "spec", "template", "spec", "service")
			if err != nil || !found {
				return nil, fmt.Errorf("interface %s has no service definition", name)
			}
			kind, _, _ := unstructured.NestedString(iface.Object, "spec", "template", "spec", "template", "spec", "interfaceType")
			if kind != "service" || definition["serviceID"] != serviceID {
				return nil, fmt.Errorf("interface %s has a conflicting service identity", name)
			}
			network, _ := definition["network"].(string)
			networkName, networkNamespace, err := splitNetwork(network)
			if err != nil {
				return nil, err
			}
			interfaceName, _ := definition["interfaceName"].(string)
			if interfaceName == "" {
				return nil, fmt.Errorf("interface %s has no explicit interfaceName", name)
			}
			generated[interfaceName] = networkSelection{Name: networkName, Namespace: networkNamespace, Interface: interfaceName}
			if networkNamespace == "" {
				networkNamespace = iface.GetNamespace()
			}
			nad := inputs.nads[networkNamespace+"/"+networkName]
			if nad == nil {
				return nil, fmt.Errorf("interface %s has undeclared NAD %s/%s", name, networkNamespace, networkName)
			}
			resource, err := nadResource(nad)
			if err != nil {
				return nil, err
			}
			if resource != "" {
				resources[resource]++
			}
		}
		if daemonSet.Annotations == nil {
			daemonSet.Annotations = map[string]string{}
		}
		annotation, err := mergeNetworks(generated, daemonSet.Annotations[networksAnnotation])
		if err != nil {
			return nil, err
		}
		daemonSet.Annotations[networksAnnotation] = annotation
		if len(resources) > 0 {
			if daemonSet.Resources == nil {
				daemonSet.Resources = corev1.ResourceList{}
			}
			for name, count := range resources {
				daemonSet.Resources[corev1.ResourceName(name)] = *kresource.NewQuantity(int64(count), kresource.DecimalSI)
			}
		}
	}
	if daemonSet.Labels == nil {
		daemonSet.Labels = map[string]string{}
	}
	daemonSet.Labels[serviceIDLabel] = serviceID
	data, err := json.Marshal(daemonSet)
	if err != nil {
		return nil, err
	}
	var daemonSetValues map[string]interface{}
	if err = kjson.Unmarshal(data, &daemonSetValues); err != nil {
		return nil, err
	}
	values, _, err := unstructured.NestedMap(service.Object, "spec", "helmChart", "values")
	if err != nil {
		return nil, err
	}
	generatedValues := map[string]interface{}{"serviceDaemonSet": daemonSetValues}
	mergeMissingValues(generatedValues, values)
	return generatedValues, nil
}

func splitNetwork(network string) (name, namespace string, err error) {
	parts := strings.Split(network, "/")
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		namespace, name = parts[0], parts[1]
	default:
		err = fmt.Errorf("invalid declared network %q", network)
	}
	if name == "" {
		err = fmt.Errorf("declared network name is empty")
	}
	return name, namespace, err
}

func nadResource(nad *unstructured.Unstructured) (string, error) {
	kind, _, _ := unstructured.NestedString(nad.Object, "spec", "resourceType")
	switch kind {
	case "sf":
		if _, trusted := nad.GetAnnotations()[trustedSFAnnotation]; trusted {
			return "nvidia.com/bf_sf_trusted", nil
		}
		return "nvidia.com/bf_sf", nil
	case "vf":
		return "nvidia.com/bf_vf", nil
	case "veth":
		return "", nil
	default:
		return "", fmt.Errorf("NAD %s has unsupported resourceType %q", nad.GetName(), kind)
	}
}

func mergeNetworks(generated map[string]networkSelection, raw string) (string, error) {
	var existing []networkSelection
	if raw != "" {
		decoder := json.NewDecoder(bytes.NewBufferString(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&existing); err != nil {
			return "", fmt.Errorf("invalid Sentry network annotation: %w", err)
		}
		if err := decoder.Decode(new(interface{})); err != io.EOF {
			return "", fmt.Errorf("invalid Sentry network annotation: trailing JSON data")
		}
	}
	merged := make([]networkSelection, 0, len(existing)+len(generated))
	for _, e := range existing {
		if n, ok := generated[e.Interface]; ok {
			// Exact upstream mergo.WithOverride behavior for the bounded fields:
			// nonempty existing fields override the generated interface selection.
			if e.Name != "" {
				n.Name = e.Name
			}
			if e.Namespace != "" {
				n.Namespace = e.Namespace
			}
			if e.Interface != "" {
				n.Interface = e.Interface
			}
			if e.CNIArgs != nil {
				n.CNIArgs = e.CNIArgs
			}
			merged = append(merged, n)
		} else {
			merged = append(merged, e)
		}
		delete(generated, e.Interface)
	}
	for _, n := range generated {
		merged = append(merged, n)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Name == merged[j].Name {
			return merged[i].Interface < merged[j].Interface
		}
		return merged[i].Name < merged[j].Name
	})
	data, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DPF MergeMaps gives generated destination values precedence recursively. Copy
// fallback leaves as well, so no retained projection aliases native chart input.
func mergeMissingValues(destination, fallback map[string]interface{}) {
	for key, value := range fallback {
		current, exists := destination[key]
		if !exists {
			destination[key] = runtime.DeepCopyJSONValue(value)
			continue
		}
		dst, dstMap := current.(map[string]interface{})
		src, srcMap := value.(map[string]interface{})
		if dstMap && srcMap {
			mergeMissingValues(dst, src)
		}
	}
}
