// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const hbnRuntimePageSize = 64
const hbnRuntimeMaxPages = 16

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
