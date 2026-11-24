// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func joinOCPCleanReasons(reasons []string) string {
	sort.Strings(reasons)
	return strings.Join(reasons, "; ")
}

func ocpNetworkAttachments(ctx context.Context, c client.Client, refs []cleanObjectRef) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	nads, err := listCleanNetworkObjects(ctx, c, cleanNADGVK, "")
	if err != nil {
		return nil, err
	}
	var pending []string
	for i := range nads {
		nad := &nads[i]
		for _, ref := range refs {
			owned := false
			for _, owner := range nad.GetOwnerReferences() {
				gv, err := schema.ParseGroupVersion(owner.APIVersion)
				if err == nil && gv.Group == ref.GVK.Group && owner.Kind == ref.GVK.Kind && owner.Name == ref.Name && (ref.UID == "" || owner.UID == ref.UID) && (ref.Namespace == "" || ref.Namespace == nad.GetNamespace()) {
					owned = true
				}
			}
			// Cross-namespace SR-IOV NAD ownership has no UID representation. The
			// controller's exact kind/group/namespace/name annotation is its contract.
			annotation := ref.GVK.Kind + "." + ref.GVK.Group + "/" + ref.Namespace + "/" + ref.Name
			if ref.GVK.Group == cleanSriovGroup && nad.GetAnnotations()["sriovnetwork.openshift.io/owner-ref"] == annotation {
				owned = true
			}
			if owned {
				pending = append(pending, nad.GetNamespace()+"/"+nad.GetName())
				break
			}
		}
	}
	return pending, nil
}

type ocpNICDeviceClean struct {
	ref         cleanObjectRef
	fields      map[string]bool
	observation cleanNICDeviceObservation
}
type ocpNICClean struct{ devices map[string]*ocpNICDeviceClean }

func prepareOCPNICClean(ctx context.Context, c client.Client, refs []cleanObjectRef) (*ocpNICClean, error) {
	result := &ocpNICClean{devices: map[string]*ocpNICDeviceClean{}}
	var templates []cleanObjectRef
	for _, ref := range refs {
		if isCleanNICTemplate(ref, ref.Namespace) {
			templates = append(templates, ref)
		}
	}
	if len(templates) == 0 {
		return result, nil
	}
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("inspect NIC template nodes: %w", err)
	}
	nodeLabels := map[string]map[string]string{}
	for i := range nodes.Items {
		nodeLabels[nodes.Items[i].Name] = nodes.Items[i].Labels
	}
	for _, ref := range templates {
		desired, err := ocpDesired(ref)
		if err != nil {
			return nil, err
		}
		devices, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: cleanNICGroup, Version: ref.GVK.Version, Kind: "NicDevice"}, ref.Namespace)
		if err != nil {
			return nil, err
		}
		matched := 0
		for i := range devices {
			device := &devices[i]
			node, _, err := unstructured.NestedString(device.Object, "status", "node")
			if err != nil {
				return nil, err
			}
			labels, known := nodeLabels[node]
			if !known {
				return nil, fmt.Errorf("NicDevice %s/%s has no observable node %q; template scope cannot be verified", ref.Namespace, device.GetName(), node)
			}
			match, err := ocpNICMatches(desired, device, labels)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
			matched++
			key := device.GetNamespace() + "/" + device.GetName()
			entry := result.devices[key]
			if entry == nil {
				entry = &ocpNICDeviceClean{ref: cleanObjectRef{GVK: device.GroupVersionKind(), Namespace: device.GetNamespace(), Name: device.GetName(), UID: device.GetUID()}, fields: map[string]bool{}, observation: cleanNICDeviceObservation{uid: device.GetUID(), minimumGeneration: map[string]int64{}}}
				result.devices[key] = entry
			}
			entry.fields[ocpNICTemplateField(ref.GVK.Kind)] = true
			if _, err := observeOCPNICDevice(device, entry); err != nil {
				return nil, err
			}
		}
		if matched == 0 {
			return nil, fmt.Errorf("no NicDevices observable for selected %s; intent removal cannot be verified", cleanRefString(ref))
		}
	}
	return result, nil
}

func ocpNICTemplateField(kind string) string {
	switch kind {
	case "NicConfigurationTemplate":
		return "configuration"
	case "NicFirmwareTemplate":
		return "firmware"
	default:
		return "interfaceNameTemplate"
	}
}

func ocpNICMatches(template, device *unstructured.Unstructured, nodeLabels map[string]string) (bool, error) {
	selected, _, err := unstructured.NestedStringMap(template.Object, "spec", "nodeSelector")
	if err != nil {
		return false, err
	}
	for key, value := range selected {
		if actual, present := nodeLabels[key]; !present || actual != value {
			return false, nil
		}
	}
	if template.GetKind() == "NicInterfaceNameTemplate" {
		return true, nil
	}
	nicType, _, err := unstructured.NestedString(template.Object, "spec", "nicSelector", "nicType")
	if err != nil {
		return false, err
	}
	actualType, _, err := unstructured.NestedString(device.Object, "status", "type")
	if err != nil {
		return false, err
	}
	if nicType != actualType {
		return false, nil
	}
	for _, pair := range [][2]string{{"serialNumbers", "serialNumber"}, {"partNumbers", "partNumber"}} {
		values, _, err := unstructured.NestedStringSlice(template.Object, "spec", "nicSelector", pair[0])
		if err != nil {
			return false, err
		}
		actual, _, err := unstructured.NestedString(device.Object, "status", pair[1])
		if err != nil {
			return false, err
		}
		if len(values) > 0 && !slices.Contains(values, actual) {
			return false, nil
		}
	}
	addresses, _, err := unstructured.NestedStringSlice(template.Object, "spec", "nicSelector", "pciAddresses")
	if err != nil {
		return false, err
	}
	if len(addresses) == 0 {
		return true, nil
	}
	ports, _, err := unstructured.NestedSlice(device.Object, "status", "ports")
	if err != nil {
		return false, err
	}
	for _, value := range ports {
		port, ok := value.(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("malformed NicDevice port")
		}
		address, _, err := unstructured.NestedString(port, "pci")
		if err != nil {
			return false, err
		}
		if slices.Contains(addresses, address) {
			return true, nil
		}
	}
	return false, nil
}

func observeOCPNICDevice(device *unstructured.Unstructured, entry *ocpNICDeviceClean) ([]string, error) {
	// Reuse acknowledgement rules only for the operations selected for removal.
	// Other templates may intentionally keep firmware or configuration present.
	scoped := device.DeepCopy()
	for _, field := range []string{"configuration", "firmware", "interfaceNameTemplate"} {
		if !entry.fields[field] {
			unstructured.RemoveNestedField(scoped.Object, "spec", field)
		}
	}
	conditions, _, err := unstructured.NestedSlice(scoped.Object, "status", "conditions")
	if err != nil {
		return nil, err
	}
	var retained []interface{}
	for _, value := range conditions {
		condition, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("malformed NicDevice condition")
		}
		kind, _, err := unstructured.NestedString(condition, "type")
		if err != nil {
			return nil, err
		}
		if (kind == "ConfigUpdateInProgress" && !entry.fields["configuration"]) || (kind == "FirmwareUpdateInProgress" && !entry.fields["firmware"]) {
			continue
		}
		retained = append(retained, value)
	}
	if err := unstructured.SetNestedSlice(scoped.Object, retained, "status", "conditions"); err != nil {
		return nil, err
	}
	return observeCleanNICDevice(scoped, &entry.observation)
}

func (reset *ocpNICClean) wait(ctx context.Context, c client.Client, interval time.Duration) error {
	if len(reset.devices) == 0 {
		return nil
	}
	return pollOCPClean(ctx, interval, "selected NIC template intent removal", func(ctx context.Context) (string, error) {
		var reasons []string
		for _, entry := range reset.devices {
			device := objectForCleanRef(entry.ref)
			if err := c.Get(ctx, types.NamespacedName{Namespace: entry.ref.Namespace, Name: entry.ref.Name}, device); err != nil {
				return "", fmt.Errorf("observe selected NicDevice %s: %w", cleanRefString(entry.ref), err)
			}
			if device.GetUID() != entry.ref.UID {
				return "", fmt.Errorf("selected NicDevice %s changed identity", cleanRefString(entry.ref))
			}
			pending, err := observeOCPNICDevice(device, entry)
			if err != nil {
				return "", err
			}
			reasons = append(reasons, pending...)
		}
		return joinOCPCleanReasons(reasons), nil
	})
}
