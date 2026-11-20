// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func newSafeNetworkTestClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	gvks := []schema.GroupVersionKind{cleanNADGVK}
	for _, kind := range []string{"NicDevice", "NicConfigurationTemplate", "NicFirmwareTemplate", "NicInterfaceNameTemplate"} {
		gvks = append(gvks, schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: kind})
	}
	for _, gvk := range gvks {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func safeNetworkRef(obj client.Object) cleanObjectRef {
	return cleanObjectRef{GVK: obj.GetObjectKind().GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

func TestCleanNetworkNADOwner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		group      string
		kind       string
		namespace  string
		annotation string
		want       bool
	}{
		{name: "cluster root outside target namespace", group: "mellanox.com", kind: "MacvlanNetwork", namespace: "application", want: true},
		{name: "other cluster network root", group: "mellanox.com", kind: "HostDeviceNetwork", namespace: "application", want: true},
		{name: "same namespace sriov owner", group: "sriovnetwork.openshift.io", kind: "SriovIBNetwork", namespace: "operator", want: true},
		{name: "other operator owner", group: "sriovnetwork.openshift.io", kind: "SriovNetwork", namespace: "other-operator", want: false},
		{name: "cross namespace annotation", namespace: "application", annotation: "SriovNetwork.sriovnetwork.openshift.io/operator/net", want: true},
		{name: "cross namespace ovs annotation", namespace: "application", annotation: "OVSNetwork.sriovnetwork.openshift.io/operator/net", want: true},
		{name: "other namespace annotation", namespace: "application", annotation: "SriovNetwork.sriovnetwork.openshift.io/other-operator/net", want: false},
		{name: "unknown kind", group: "mellanox.com", kind: "UnrelatedNetwork", namespace: "application", want: false},
		{name: "unowned same name", namespace: "application", want: false},
		{name: "invalid annotation", namespace: "application", annotation: "SriovNetwork.sriovnetwork.openshift.io/operator/", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nad := testCleanObject(cleanNADGVK, tc.namespace, "net")
			if tc.group != "" {
				nad.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: tc.group + "/v1", Kind: tc.kind, Name: "net", UID: "owner"}})
			}
			if tc.annotation != "" {
				nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": tc.annotation})
			}
			assert.Equal(t, tc.want, cleanNetworkNADOwner(nad, "operator"))
		})
	}
}

func TestCleanNetworkDefinitionsWaitsForCrossNamespaceGarbageCollection(t *testing.T) {
	network := testCleanObject(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "IPoIBNetwork"}, "", "net")
	nad := testCleanObject(cleanNADGVK, "application", "net")
	nad.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "mellanox.com/v1alpha1", Kind: "IPoIBNetwork", Name: "net", UID: "root"}})
	manual := testCleanObject(cleanNADGVK, "application", "manual")
	base := newSafeNetworkTestClient(network, nad, manual)
	var deleted []string
	polls := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetObjectKind().GroupVersionKind().Kind)
			return c.Delete(ctx, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "NetworkAttachmentDefinitionList" {
				polls++
				if polls == 3 {
					// Simulate delayed garbage collection, not a direct CLI NAD delete.
					assert.Error(t, c.Get(ctx, client.ObjectKeyFromObject(network), network))
					require.NoError(t, c.Delete(ctx, nad))
				}
			}
			return c.List(ctx, list, opts...)
		},
	})
	count, err := cleanNetworkDefinitions(context.Background(), c, []cleanObjectRef{safeNetworkRef(network)}, "operator", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, []string{"IPoIBNetwork"}, deleted)
	assert.GreaterOrEqual(t, polls, 3)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(manual), manual))
}

func TestCleanNetworkDefinitionsRetryStillWaitsForOrphanNAD(t *testing.T) {
	nad := testCleanObject(cleanNADGVK, "application", "stuck")
	nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovIBNetwork.sriovnetwork.openshift.io/operator/removed"})
	c := newSafeNetworkTestClient(nad)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	count, err := cleanNetworkDefinitions(ctx, c, nil, "operator", time.Millisecond, ui.NewSilent())
	assert.Equal(t, 0, count)
	require.Error(t, err)
	assert.ErrorContains(t, err, "application/stuck")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(nad), nad))
}

func TestCleanNetworkDefinitionsInspectionFailurePrecedesDeletion(t *testing.T) {
	network := testCleanObject(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "MacvlanNetwork"}, "", "net")
	base := newSafeNetworkTestClient(network)
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: cleanNADGVK.Group, Resource: "network-attachment-definitions"}, "", nil)
		},
	})
	_, err := cleanNetworkDefinitions(context.Background(), c, []cleanObjectRef{safeNetworkRef(network)}, "operator", time.Millisecond, ui.NewSilent())
	require.Error(t, err)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(network), network))
}

func TestCleanNICTemplatesWaitsForControllerGeneratedFields(t *testing.T) {
	var templates []*unstructured.Unstructured
	var refs []cleanObjectRef
	var objects []client.Object
	for _, kind := range []string{"NicConfigurationTemplate", "NicFirmwareTemplate", "NicInterfaceNameTemplate"} {
		obj := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: kind}, "operator", "template")
		templates = append(templates, obj)
		objects = append(objects, obj)
		refs = append(refs, safeNetworkRef(obj))
	}
	device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
	device.Object["spec"] = map[string]interface{}{"configuration": map[string]interface{}{}, "firmware": map[string]interface{}{}, "interfaceNameTemplate": map[string]interface{}{}}
	other := device.DeepCopy()
	other.SetNamespace("other-operator")
	objects = append(objects, device, other)
	base := newSafeNetworkTestClient(objects...)
	deleted := 0
	polls := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			assert.NotEqual(t, "NicDevice", obj.GetObjectKind().GroupVersionKind().Kind)
			deleted++
			return c.Delete(ctx, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "NicDeviceList" {
				polls++
				if polls == 3 {
					assert.Equal(t, 3, deleted)
					for _, obj := range templates {
						assert.Error(t, c.Get(ctx, client.ObjectKeyFromObject(obj), obj))
					}
					latest := device.DeepCopy()
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(device), latest))
					latest.Object["spec"] = map[string]interface{}{}
					latest.Object["status"] = emptySafeNICStatus()
					require.NoError(t, c.Update(ctx, latest))
				}
			}
			return c.List(ctx, list, opts...)
		},
	})
	count, err := cleanNICTemplates(context.Background(), c, refs, "operator", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.GreaterOrEqual(t, polls, 3)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(other), other))
	_, present, err := unstructured.NestedFieldNoCopy(other.Object, "spec", "configuration")
	require.NoError(t, err)
	assert.True(t, present)
}

func TestCleanNICTemplatesRetryBlocksOnRetainedIntent(t *testing.T) {
	for _, field := range []string{"configuration", "firmware", "interfaceNameTemplate"} {
		t.Run(field, func(t *testing.T) {
			device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "busy")
			device.Object["spec"] = map[string]interface{}{field: map[string]interface{}{}}
			c := newSafeNetworkTestClient(device)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()
			count, err := cleanNICTemplates(ctx, c, nil, "operator", time.Millisecond, ui.NewSilent())
			assert.Equal(t, 0, count)
			require.Error(t, err)
			assert.ErrorContains(t, err, "busy: spec."+field+" remains")
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(device), device))
		})
	}
}

func TestCleanNICTemplatesInspectionFailurePrecedesDeletion(t *testing.T) {
	template := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicConfigurationTemplate"}, "operator", "template")
	base := newSafeNetworkTestClient(template)
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: cleanNICGroup, Resource: "nicdevices"}, "", nil)
		},
	})
	_, err := cleanNICTemplates(context.Background(), c, []cleanObjectRef{safeNetworkRef(template)}, "operator", time.Millisecond, ui.NewSilent())
	require.Error(t, err)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(template), template))
}

func emptySafeNICStatus() map[string]interface{} {
	return map[string]interface{}{"conditions": []interface{}{
		map[string]interface{}{"type": "ConfigUpdateInProgress", "status": "False", "reason": "DeviceConfigSpecEmpty"},
		map[string]interface{}{"type": "FirmwareUpdateInProgress", "status": "False", "reason": "DeviceFirmwareSpecEmpty"},
	}}
}

func TestCleanNICTemplatesWaitsForDaemonAfterIntentCleared(t *testing.T) {
	device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
	device.Object["status"] = map[string]interface{}{"conditions": []interface{}{
		map[string]interface{}{"type": "ConfigUpdateInProgress", "status": "True", "reason": "Updating"},
		map[string]interface{}{"type": "FirmwareUpdateInProgress", "status": "False", "reason": "DeviceFirmwareSpecEmpty"},
	}}
	base := newSafeNetworkTestClient(device)
	reads := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads++
			if reads == 3 {
				latest := device.DeepCopy()
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(device), latest))
				latest.Object["status"] = emptySafeNICStatus()
				require.NoError(t, c.Update(ctx, latest))
			}
			return c.List(ctx, list, opts...)
		},
	})
	_, err := cleanNICTemplates(context.Background(), c, nil, "operator", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, reads, 3)
}

func TestCleanNICTemplatesMissingDaemonAcknowledgementBlocks(t *testing.T) {
	device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
	device.Object["spec"] = map[string]interface{}{"configuration": map[string]interface{}{}}
	c := newSafeNetworkTestClient(device)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := cleanNICTemplates(ctx, c, nil, "operator", time.Millisecond, ui.NewSilent())
	require.Error(t, err)
	assert.ErrorContains(t, err, "ConfigUpdateInProgress has not acknowledged an empty spec")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestCleanNICTemplatesPreservesDiscoveryOnlyDevicesWithoutOptionalConditions(t *testing.T) {
	device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
	c := newSafeNetworkTestClient(device)
	_, err := cleanNICTemplates(context.Background(), c, nil, "operator", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(device), device))
}

func TestCleanNICTemplatesDisappearanceCannotAcknowledgeRemoval(t *testing.T) {
	device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
	device.SetUID("original")
	device.Object["spec"] = map[string]interface{}{"configuration": map[string]interface{}{}}
	base := newSafeNetworkTestClient(device)
	reads := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads++
			if reads == 2 {
				require.NoError(t, c.Delete(ctx, device))
			}
			return c.List(ctx, list, opts...)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := cleanNICTemplates(ctx, c, nil, "operator", time.Millisecond, ui.NewSilent())
	require.Error(t, err)
	assert.ErrorContains(t, err, "NicDevice disappeared")
}

func TestCleanNICTemplatesStaleEmptyAcknowledgementDoesNotQualify(t *testing.T) {
	device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
	device.SetGeneration(5)
	device.Object["spec"] = map[string]interface{}{"configuration": map[string]interface{}{}}
	device.Object["status"] = emptySafeNICStatus()
	observation := &cleanNICDeviceObservation{uid: "", minimumGeneration: map[string]int64{}}
	_, err := observeCleanNICDevice(device, observation)
	require.NoError(t, err)
	device.SetGeneration(6)
	device.Object["spec"] = map[string]interface{}{}
	reasons, err := observeCleanNICDevice(device, observation)
	require.NoError(t, err)
	assert.Contains(t, reasons, "device: ConfigUpdateInProgress has not acknowledged an empty spec")
	conditions := device.Object["status"].(map[string]interface{})["conditions"].([]interface{})
	conditions[0].(map[string]interface{})["observedGeneration"] = int64(6)
	reasons, err = observeCleanNICDevice(device, observation)
	require.NoError(t, err)
	assert.Empty(t, reasons)
}

func TestCleanNICTemplatesRetryWaitsForEmptySpecAfterSuccessfulOldUpdate(t *testing.T) {
	for _, operation := range cleanNICOperationConditions {
		t.Run(operation.field, func(t *testing.T) {
			device := testCleanObject(schema.GroupVersionKind{Group: cleanNICGroup, Version: "v1alpha1", Kind: "NicDevice"}, "operator", "device")
			device.SetGeneration(10)
			// The previous attempt removed the spec, but this condition still
			// describes a successful reconciliation of the old nonempty intent.
			device.Object["status"] = map[string]interface{}{"conditions": []interface{}{
				map[string]interface{}{"type": operation.conditionType, "status": "False", "reason": "UpdateSuccessful", "observedGeneration": int64(9)},
			}}
			base := newSafeNetworkTestClient(device)
			reads := 0
			c := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					reads++
					if reads == 3 {
						latest := device.DeepCopy()
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(device), latest))
						latest.Object["status"] = map[string]interface{}{"conditions": []interface{}{
							map[string]interface{}{"type": operation.conditionType, "status": "False", "reason": operation.emptyReason, "observedGeneration": int64(10)},
						}}
						require.NoError(t, c.Update(ctx, latest))
					}
					return c.List(ctx, list, opts...)
				},
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := cleanNICTemplates(ctx, c, nil, "operator", time.Millisecond, ui.NewSilent())
			require.NoError(t, err)
			assert.GreaterOrEqual(t, reads, 3, "old successful conditions must not qualify an empty spec on retry")
		})
	}
}
