// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const hbnRuntimePageSize = 64
const hbnRuntimeMaxPages = 16

type hbnRuntimeStageClient interface {
	activationBoundaryReader
	checkHBNRuntimeAccess(context.Context, string, string, string) []PreflightCheck
	listHBNRuntimeObjects(context.Context, string, string, string) ([]unstructured.Unstructured, error)
	getHBNRuntimeApplication(context.Context, string, string) (*unstructured.Unstructured, error)
}

// The service/interface list is namespaced; the App permission names one exact
// generated Application. This adds no write permission other than SSAR create.
func (c *APIClient) checkHBNRuntimeAccess(ctx context.Context, namespace, argoNamespace, application string) []PreflightCheck {
	requirements := map[accessRequirement]struct{}{}
	for _, resource := range []string{"dpuservices", "dpuserviceinterfaces"} {
		requirements[accessRequirement{verb: "list", group: "svc.dpu.nvidia.com", version: "v1alpha1", resource: resource, namespace: namespace}] = struct{}{}
	}
	if application != "" {
		requirements[accessRequirement{verb: "get", group: "argoproj.io", version: "v1alpha1", resource: "applications", namespace: argoNamespace, resourceName: application}] = struct{}{}
	}
	return c.checkAccessRequirements(ctx, requirements)
}

func (c *APIClient) listHBNRuntimeObjects(ctx context.Context, kind, namespace, selector string) ([]unstructured.Unstructured, error) {
	if ctx == nil || namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 || kind != "DPUService" && kind != "DPUServiceInterface" {
		return nil, fmt.Errorf("HBN_RUNTIME_LIST_INPUT_INVALID")
	}
	template := runtimeObject(kind, "", namespace, nil)
	resource, err := c.resource(ctx, template, false)
	if err != nil {
		return nil, err
	}
	cursor, rv := "", ""
	seenCursor, seenName, seenUID := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var result []unstructured.Unstructured
	for range hbnRuntimeMaxPages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := resource.List(ctx, metav1.ListOptions{Limit: hbnRuntimePageSize, Continue: cursor, LabelSelector: selector})
		if err != nil {
			return nil, safeAPIError("list HBN runtime source", err)
		}
		if page == nil || page.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || page.GetKind() != kind+"List" || page.GetResourceVersion() == "" || len(page.Items) > hbnRuntimePageSize || rv != "" && rv != page.GetResourceVersion() {
			return nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
		}
		rv = page.GetResourceVersion()
		for i := range page.Items {
			object := &page.Items[i]
			uid := string(object.GetUID())
			if object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetKind() != kind || object.GetNamespace() != namespace || object.GetName() == "" || uid == "" || object.GetResourceVersion() == "" || seenName[object.GetName()] || seenUID[uid] {
				return nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
			}
			seenName[object.GetName()], seenUID[uid] = true, true
			result = append(result, *object.DeepCopy())
		}
		cursor = page.GetContinue()
		if cursor == "" {
			return result, nil
		}
		if seenCursor[cursor] {
			return nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
		}
		seenCursor[cursor] = true
	}
	return nil, fmt.Errorf("HBN_RUNTIME_LIST_LIMIT")
}

type hbnRuntimeBindingSnapshot struct {
	anchors       map[string]hbnPodBindingAnchor
	sourceDigests map[string]string
}

// observeHBNRuntimeBindings verifies source/tenant identity and complete lists.
// The caller brackets this with the shared activation/allocation observer and
// compares semantic anchors for the entire selected cohort around every read.
func observeHBNRuntimeBindings(ctx context.Context, client hbnRuntimeStageClient, execution *managementVerificationInputs, tenant hbnRuntimeTenantReader,
	activation activationStageReceipt, fence hostNetworkLeaseFence, image hbnPodImageIdentity, timeout time.Duration,
) (hbnRuntimeBindingSnapshot, error) {
	var empty hbnRuntimeBindingSnapshot
	observeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if tenant == nil {
		return empty, fmt.Errorf("HBN_RUNTIME_TENANT_UNAVAILABLE")
	}
	if err := client.VerifyIdentity(observeCtx); err != nil {
		return empty, err
	}
	if err := tenant.VerifyIdentity(observeCtx); err != nil {
		return empty, err
	}
	namespace := execution.plan.Intent().Namespace
	if !runtimePermissionsPassed(tenant.checkTenantRuntimeReadPermissions(observeCtx, namespace)) || !runtimePermissionsPassed(client.checkHBNRuntimeAccess(observeCtx, namespace, execution.operatorNamespace, "")) {
		return empty, fmt.Errorf("HBN_RUNTIME_PERMISSION_DENIED")
	}
	selector := labels.Set{activationParentLabel: namespace + "_" + fence.activation.deployment.GetName(), hbnServiceReferenceLabel: hbnContainerName}.AsSelector().String()
	services, err := client.listHBNRuntimeObjects(observeCtx, "DPUService", namespace, selector)
	if err != nil {
		return empty, err
	}
	interfaces, err := client.listHBNRuntimeObjects(observeCtx, "DPUServiceInterface", namespace, selector)
	if err != nil {
		return empty, err
	}
	var selected *unstructured.Unstructured
	var expected hbnPodBindingIntent
	for i := range services {
		object := &services[i]
		candidate, err := compileHBNRuntimeBinding(execution, activation, fence, object, interfaces, image)
		if err != nil {
			continue
		}
		if selected != nil {
			return empty, fmt.Errorf("HBN_RUNTIME_SERVICE_AMBIGUOUS")
		}
		selected = object
		expected = candidate
	}
	if selected == nil {
		return empty, fmt.Errorf("HBN_RUNTIME_CURRENT_SERVICE_UNAVAILABLE")
	}
	appName := dpfApplicationName(expected.DPUClusterName, expected.DPUServiceName)
	if !runtimePermissionsPassed(client.checkHBNRuntimeAccess(observeCtx, namespace, execution.operatorNamespace, appName)) {
		return empty, fmt.Errorf("HBN_RUNTIME_PERMISSION_DENIED")
	}
	app, err := client.getHBNRuntimeApplication(observeCtx, execution.operatorNamespace, appName)
	if err != nil {
		return empty, err
	}
	daemonSets, pods, err := tenant.listRuntimeObjects(observeCtx, namespace, labels.Set{hbnServiceIDLabel: expected.ServiceID}.AsSelector().String())
	if err != nil {
		return empty, err
	}
	result := hbnRuntimeBindingSnapshot{anchors: map[string]hbnPodBindingAnchor{}, sourceDigests: map[string]string{}}
	interfaceNames, found, err := unstructured.NestedStringSlice(selected.Object, "spec", "interfaces")
	if err != nil || !found {
		return empty, fmt.Errorf("HBN_RUNTIME_INTERFACES_MISMATCH")
	}
	for _, name := range interfaceNames {
		object := uniqueObjectByName(interfaces, name)
		if object == nil {
			return empty, fmt.Errorf("HBN_RUNTIME_INTERFACES_MISMATCH")
		}
		result.sourceDigests[name] = hbnBindingDigest(struct {
			UID                 string
			Generation          int64
			Spec                any
			Owners              any
			Version, ConsumedBy string
		}{string(object.GetUID()), object.GetGeneration(), object.Object["spec"], object.GetOwnerReferences(), object.GetAnnotations()[hbnServiceVersionAnnotation], object.GetAnnotations()["dpu.nvidia.com/consumed-by"]})
	}
	observedAt := time.Now().UTC()
	for _, lease := range fence.leases {
		intent := expected
		intent.TenantNodeName = lease.TenantNodeName
		anchor, err := validateHBNPodBinding(intent, selected, app, daemonSets, pods, observedAt)
		if err != nil {
			return empty, err
		}
		if !runtimePermissionsPassed(tenant.checkTenantPodExecPermissions(observeCtx, namespace, anchor.PodName)) {
			return empty, fmt.Errorf("HBN_RUNTIME_PERMISSION_DENIED")
		}
		// A named fresh read fences the actual exec target, not only a list cache.
		pod, err := tenant.readPod(observeCtx, namespace, anchor.PodName)
		if err != nil {
			return empty, safeAPIError("read selected HBN Pod", err)
		}
		if pod == nil {
			return empty, fmt.Errorf("HBN_RUNTIME_POD_CHANGED")
		}
		named, err := validateHBNPodBinding(intent, selected, app, daemonSets, []corev1.Pod{*pod}, time.Now().UTC())
		if err != nil || !sameHBNPodBinding(anchor, named) {
			return empty, fmt.Errorf("HBN_RUNTIME_POD_CHANGED")
		}
		result.anchors[lease.Serial] = anchor
	}
	if err := client.VerifyIdentity(observeCtx); err != nil {
		return empty, err
	}
	if err := tenant.VerifyIdentity(observeCtx); err != nil {
		return empty, err
	}
	if err := observeCtx.Err(); err != nil {
		return empty, err
	}
	return result, nil
}

func runtimePermissionsPassed(checks []PreflightCheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.State != "passed" {
			return false
		}
	}
	return true
}

func (tenant *tenantClient) listRuntimeObjects(ctx context.Context, namespace, selector string) ([]appsv1.DaemonSet, []corev1.Pod, error) {
	var daemonSets []appsv1.DaemonSet
	var pods []corev1.Pod
	for _, kind := range []string{"daemonsets", "pods"} {
		cursor, rv := "", ""
		seenCursor, seenName, seenUID := map[string]bool{}, map[string]bool{}, map[string]bool{}
		complete := false
		for range hbnRuntimeMaxPages {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			options := metav1.ListOptions{Limit: hbnRuntimePageSize, Continue: cursor, LabelSelector: selector}
			var meta metav1.ListMeta
			var identities []metav1.Object
			if kind == "daemonsets" {
				page, err := tenant.api.AppsV1().DaemonSets(namespace).List(ctx, options)
				if err != nil {
					return nil, nil, safeAPIError("list HBN DaemonSets", err)
				}
				if page == nil {
					return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
				}
				if page.APIVersion != "" && page.APIVersion != "apps/v1" || page.Kind != "" && page.Kind != "DaemonSetList" {
					return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
				}
				meta = page.ListMeta
				for i := range page.Items {
					identities = append(identities, &page.Items[i])
				}
				daemonSets = append(daemonSets, page.Items...)
			} else {
				page, err := tenant.api.CoreV1().Pods(namespace).List(ctx, options)
				if err != nil {
					return nil, nil, safeAPIError("list HBN Pods", err)
				}
				if page == nil {
					return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
				}
				if page.APIVersion != "" && page.APIVersion != "v1" || page.Kind != "" && page.Kind != "PodList" {
					return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
				}
				meta = page.ListMeta
				for i := range page.Items {
					identities = append(identities, &page.Items[i])
				}
				pods = append(pods, page.Items...)
			}
			if meta.ResourceVersion == "" || rv != "" && rv != meta.ResourceVersion || len(identities) > hbnRuntimePageSize {
				return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
			}
			rv = meta.ResourceVersion
			for _, object := range identities {
				uid := string(object.GetUID())
				name := object.GetName()
				if name == "" || uid == "" || object.GetResourceVersion() == "" || object.GetNamespace() != namespace || seenName[name] || seenUID[uid] {
					return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
				}
				seenName[name], seenUID[uid] = true, true
			}
			cursor = meta.Continue
			if cursor == "" {
				complete = true
				break
			}
			if seenCursor[cursor] {
				return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_INCOMPLETE")
			}
			seenCursor[cursor] = true
		}
		if !complete {
			return nil, nil, fmt.Errorf("HBN_RUNTIME_LIST_LIMIT")
		}
	}
	return daemonSets, pods, nil
}

// Applications are controller runtime sources, not renderable bundle objects.
// Keep this fixed read separate from the generic render-kind scope registry.
func (c *APIClient) getHBNRuntimeApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	if c == nil || c.dynamic == nil || ctx == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, fmt.Errorf("HBN_RUNTIME_APPLICATION_INPUT_INVALID")
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	object, err := c.getCurrentHBNRuntimeApplication(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return object, nil
}

func (c *APIClient) getCurrentHBNRuntimeApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	if c == nil || c.dynamic == nil || ctx == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, fmt.Errorf("HBN_RUNTIME_APPLICATION_INPUT_INVALID")
	}
	object, err := c.dynamic.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, safeAPIError("read selected HBN Application", err)
	}
	if object == nil || object.GetNamespace() != namespace || object.GetName() != name || object.GetAPIVersion() != "argoproj.io/v1alpha1" || object.GetKind() != "Application" {
		return nil, fmt.Errorf("HBN_RUNTIME_APPLICATION_INVALID")
	}
	return object, nil
}
