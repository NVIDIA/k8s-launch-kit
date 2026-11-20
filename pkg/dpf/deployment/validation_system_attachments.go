// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func verifyValidationSystemAttachment(ctx context.Context, tenant *tenantClient, app, expected *unstructured.Unstructured, namespace string) error {
	if err := validationAttachmentInventory(app, expected.GetName(), namespace); err != nil {
		return err
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return err
	}
	var current unstructured.Unstructured
	path := "/apis/k8s.cni.cncf.io/v1/namespaces/" + namespace + "/network-attachment-definitions/" + expected.GetName()
	if err := tenant.api.Discovery().RESTClient().Get().AbsPath(path).Do(ctx).Into(&current); err != nil {
		return err
	}
	if err := validationAttachmentMatches(app, expected, &current, namespace); err != nil {
		return err
	}
	return tenant.VerifyIdentity(ctx)
}
func validationAttachmentInventory(app *unstructured.Unstructured, name, namespace string) error {
	resources, found, err := unstructured.NestedSlice(app.Object, "status", "resources")
	if err != nil || !found {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "System App has no complete attachment inventory.")
	}
	matches := 0
	for _, raw := range resources {
		object, ok := raw.(map[string]any)
		if !ok {
			return validationFailure(validation.Unverified, validation.ReasonIncomplete, "System App attachment inventory is malformed.")
		}
		kind, _ := object["kind"].(string)
		resourceName, _ := object["name"].(string)
		if kind != "NetworkAttachmentDefinition" || resourceName != name {
			continue
		}
		group, _ := object["group"].(string)
		version, _ := object["version"].(string)
		ns, _ := object["namespace"].(string)
		status, _ := object["status"].(string)
		hook, _ := object["hook"].(bool)
		pruning, _ := object["requiresPruning"].(bool)
		if group != "k8s.cni.cncf.io" || version != "v1" || ns != namespace || status != "Synced" || hook || pruning {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Required attachment inventory has a conflicting API, namespace or sync state.")
		}
		matches++
	}
	if matches != 1 {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "System App inventory must contain exactly one required network attachment.")
	}
	return nil
}
func validationAttachmentMatches(app, expected, current *unstructured.Unstructured, namespace string) error {
	if current == nil || current.GetAPIVersion() != "k8s.cni.cncf.io/v1" || current.GetKind() != "NetworkAttachmentDefinition" || current.GetNamespace() != namespace || current.GetName() != expected.GetName() || current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Required attachment identity is unavailable or differs from its selected chart.")
	}
	tracking := app.GetName() + ":k8s.cni.cncf.io/NetworkAttachmentDefinition:" + namespace + "/" + expected.GetName()
	if current.GetAnnotations()["argocd.argoproj.io/tracking-id"] != tracking || !jsonEqual(expected.Object["spec"], current.Object["spec"]) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Required attachment config or Argo source tracking differs from the locked chart role.")
	}
	return nil
}
