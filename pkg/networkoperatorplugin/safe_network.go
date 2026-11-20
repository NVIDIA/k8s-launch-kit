// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const cleanNICGroup = "configuration.net.nvidia.com"

var cleanNADGVK = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}

func isCleanNetworkDefinition(ref cleanObjectRef, ns string) bool {
	switch ref.GVK.Group {
	case "mellanox.com":
		switch ref.GVK.Kind {
		case "MacvlanNetwork", "IPoIBNetwork", "HostDeviceNetwork":
			return ref.Namespace == ""
		}
	case "sriovnetwork.openshift.io":
		switch ref.GVK.Kind {
		case "SriovNetwork", "SriovIBNetwork", "OVSNetwork":
			return ref.Namespace == ns
		}
	}
	return false
}

// cleanNetworkDefinitions preserves the controllers and their policies while
// network CR finalizers and garbage collection remove their generated NADs. NADs
// can live outside the cleanup namespace; ownership only selects observations,
// never additional deletion requests. The all-namespace scan also covers retries
// after a root has disappeared and NADs from a previous network namespace.
func cleanNetworkDefinitions(ctx context.Context, c client.Client, refs []cleanObjectRef, ns string, interval time.Duration, out ui.Output) (int, error) {
	var networks []cleanObjectRef
	for _, ref := range refs {
		if isCleanNetworkDefinition(ref, ns) {
			networks = append(networks, ref)
		}
	}
	if _, err := listCleanNetworkNADs(ctx, c, ns); err != nil {
		return 0, err
	}
	deleted, err := deleteAllAndWait(ctx, c, networks, interval, out, metav1.DeletePropagationForeground)
	if err != nil {
		return deleted, err
	}
	pending := "generated network attachments remain"
	err = wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		nads, err := listCleanNetworkNADs(ctx, c, ns)
		if err != nil {
			return false, err
		}
		if len(nads) == 0 {
			return true, nil
		}
		pending = strings.Join(nads, ", ")
		return false, nil
	})
	if err != nil {
		return deleted, fmt.Errorf("wait for generated NetworkAttachmentDefinitions (%s): %w", pending, err)
	}
	return deleted, nil
}

func listCleanNetworkNADs(ctx context.Context, c client.Client, ns string) ([]string, error) {
	objects, err := listCleanNetworkObjects(ctx, c, cleanNADGVK, "")
	if err != nil {
		return nil, err
	}
	var pending []string
	for i := range objects {
		nad := &objects[i]
		if cleanNetworkNADOwner(nad, ns) {
			pending = append(pending, nad.GetNamespace()+"/"+nad.GetName())
		}
	}
	sort.Strings(pending)
	return pending, nil
}

func cleanNetworkNADOwner(nad *unstructured.Unstructured, ns string) bool {
	for _, owner := range nad.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(owner.APIVersion)
		if err != nil {
			continue
		}
		ref := cleanObjectRef{GVK: gv.WithKind(owner.Kind)}
		if gv.Group == "sriovnetwork.openshift.io" {
			ref.Namespace = nad.GetNamespace()
		}
		if isCleanNetworkDefinition(ref, ns) {
			return true
		}
	}
	// Cross-namespace SR-IOV NADs cannot use ownerReferences. The operator
	// records Kind.group/namespace/name and removes them through its finalizer.
	owner := strings.Split(nad.GetAnnotations()["sriovnetwork.openshift.io/owner-ref"], "/")
	if len(owner) != 3 || owner[1] != ns || owner[2] == "" {
		return false
	}
	for _, kind := range []string{"SriovNetwork", "SriovIBNetwork", "OVSNetwork"} {
		if owner[0] == kind+".sriovnetwork.openshift.io" {
			return true
		}
	}
	return false
}

func isCleanNICTemplate(ref cleanObjectRef, ns string) bool {
	if ref.GVK.Group != cleanNICGroup || ref.Namespace != ns {
		return false
	}
	switch ref.GVK.Kind {
	case "NicConfigurationTemplate", "NicFirmwareTemplate", "NicInterfaceNameTemplate":
		return true
	}
	return false
}

// cleanNICTemplates waits for controllers to remove template-derived NicDevice
// intent before those generated resources and the controllers themselves can be
// deleted. Removing intent does not restore NIC firmware/configuration, nor does
// this API observation prove that a daemon removed host udev rules: the NCO API
// exposes no acknowledgement after that host cleanup operation.
func cleanNICTemplates(ctx context.Context, c client.Client, refs []cleanObjectRef, ns string, interval time.Duration, out ui.Output) (int, error) {
	var templates []cleanObjectRef
	deviceGVK := schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}
	for _, ref := range refs {
		if isCleanNICTemplate(ref, ns) {
			templates = append(templates, ref)
		}
		if ref.GVK.Group == cleanNICGroup && ref.GVK.Kind == "NicDevice" && ref.Namespace == ns {
			deviceGVK = ref.GVK
		}
	}
	// Inspect even without templates: a previous cleanup can have removed them
	// before their asynchronous controller-generated intent was cleared.
	devices, err := listCleanNetworkObjects(ctx, c, deviceGVK, ns)
	if err != nil {
		return 0, err
	}
	expected := make(map[string]*cleanNICDeviceObservation, len(devices))
	for i := range devices {
		device := &devices[i]
		observation := &cleanNICDeviceObservation{uid: device.GetUID(), minimumGeneration: map[string]int64{}}
		if _, err := observeCleanNICDevice(device, observation); err != nil {
			return 0, err
		}
		expected[device.GetName()] = observation
	}
	deleted, err := deleteAllAndWait(ctx, c, templates, interval, out, metav1.DeletePropagationForeground)
	if err != nil {
		return deleted, err
	}
	pending := "template-derived NicDevice intent remains"
	err = wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		devices, err := listCleanNetworkObjects(ctx, c, deviceGVK, ns)
		if err != nil {
			return false, err
		}
		var reasons []string
		seen := make(map[string]bool, len(devices))
		for i := range devices {
			device := &devices[i]
			name := device.GetName()
			seen[name] = true
			observation, ok := expected[name]
			if !ok {
				observation = &cleanNICDeviceObservation{uid: device.GetUID(), minimumGeneration: map[string]int64{}}
				expected[name] = observation
			}
			if observation.uid != device.GetUID() {
				return false, fmt.Errorf("NicDevice %s/%s was replaced before template intent removal was verified", ns, name)
			}
			deviceReasons, err := observeCleanNICDevice(device, observation)
			if err != nil {
				return false, err
			}
			reasons = append(reasons, deviceReasons...)
		}
		for name := range expected {
			if !seen[name] {
				reasons = append(reasons, name+": NicDevice disappeared before template intent removal was verified")
			}
		}
		if len(reasons) == 0 {
			return true, nil
		}
		sort.Strings(reasons)
		pending = strings.Join(reasons, "; ")
		return false, nil
	})
	if err != nil {
		return deleted, fmt.Errorf("wait for NIC template intent removal (%s): %w", pending, err)
	}
	return deleted, nil
}

type cleanNICDeviceObservation struct {
	uid types.UID
	// Previously configured operations or conditions that still describe old
	// intent require a daemon acknowledgement. Unused conditions may be absent.
	minimumGeneration map[string]int64
}

var cleanNICOperationConditions = []struct{ field, conditionType, emptyReason string }{
	{"configuration", "ConfigUpdateInProgress", "DeviceConfigSpecEmpty"},
	{"firmware", "FirmwareUpdateInProgress", "DeviceFirmwareSpecEmpty"},
}

// observeCleanNICDevice records newly seen intent or old-operation conditions and checks
// prior requirements against this same device identity. A daemon empty-spec
// condition must cover at least the generation where that intent was observed;
// an older empty condition cannot stand in for processing the removed intent.
func observeCleanNICDevice(device *unstructured.Unstructured, observation *cleanNICDeviceObservation) ([]string, error) {
	name := device.GetNamespace() + "/" + device.GetName()
	var reasons []string
	present := map[string]bool{}
	for _, field := range []string{"configuration", "firmware", "interfaceNameTemplate"} {
		value, found, err := unstructured.NestedFieldNoCopy(device.Object, "spec", field)
		if err != nil {
			return nil, fmt.Errorf("inspect NicDevice %s spec.%s: %w", name, field, err)
		}
		present[field] = found && value != nil
		if present[field] {
			reasons = append(reasons, device.GetName()+": spec."+field+" remains")
		}
	}
	conditions, _, err := unstructured.NestedSlice(device.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("inspect NicDevice %s conditions: %w", name, err)
	}
	byType := map[string]map[string]interface{}{}
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("inspect NicDevice %s: malformed condition", name)
		}
		conditionType, _, err := unstructured.NestedString(condition, "type")
		if err != nil {
			return nil, fmt.Errorf("inspect NicDevice %s condition type: %w", name, err)
		}
		byType[conditionType] = condition
	}
	for _, operation := range cleanNICOperationConditions {
		condition := byType[operation.conditionType]
		// On retry, a completed old update still needs the daemon's empty-spec
		// acknowledgement. False/UpdateSuccessful describes the removed intent.
		pendingAcknowledgement := condition != nil && (condition["status"] != "False" || condition["reason"] != operation.emptyReason)
		previous, recorded := observation.minimumGeneration[operation.conditionType]
		if (present[operation.field] && device.GetGeneration() > previous) || (!recorded && (present[operation.field] || pendingAcknowledgement)) {
			observation.minimumGeneration[operation.conditionType] = device.GetGeneration()
		}
		minimum, required := observation.minimumGeneration[operation.conditionType]
		if !required {
			continue
		}
		generation, _, err := unstructured.NestedInt64(condition, "observedGeneration")
		if err != nil {
			return nil, fmt.Errorf("inspect NicDevice %s %s generation: %w", name, operation.conditionType, err)
		}
		if condition["status"] != "False" || condition["reason"] != operation.emptyReason || generation < minimum {
			reasons = append(reasons, device.GetName()+": "+operation.conditionType+" has not acknowledged an empty spec")
		}
	}
	return reasons, nil
}

// Empty namespace intentionally means all namespaces for the NAD ownership scan.
func listCleanNetworkObjects(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, ns string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	var opts []client.ListOption
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := c.List(ctx, list, opts...); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s for cleanup: %w", gvk.String(), err)
	}
	return list.Items, nil
}
