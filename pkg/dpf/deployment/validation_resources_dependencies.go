// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func (o *validationObserver) validationExternalWorkloads() []preflightWorkload {
	namespace := o.desired.operatorNamespace
	result := pinnedHBNWorkloads(ExternalNamespaceOverrides{CertManager: "cert-manager", ArgoCD: namespace, NodeFeature: namespace, Maintenance: namespace, Kamaji: namespace}, false)
	return append(result, preflightWorkload{id: "argo.controller", namespace: namespace, kind: "StatefulSet", selector: "app.kubernetes.io/name=argocd-application-controller"}, preflightWorkload{id: "argo.repository", namespace: namespace, kind: "Deployment", selector: "app.kubernetes.io/name=argocd-repo-server"})
}
func (o *validationObserver) validationDependencyChecks() []validation.CheckSpec {
	result := []validation.CheckSpec{validationSpec("management/dependency/kamaji.compatibility", validation.AreaManagement, "Kamaji tenant networking compatibility", false)}
	for _, workload := range o.validationExternalWorkloads() {
		result = append(result, validationSpec("management/dependency/"+workload.id, validation.AreaManagement, workload.id, false))
	}
	for _, api := range pinnedHBNAPIs() {
		result = append(result, validationSpec("definitions/external/"+api.id, validation.AreaDefinitions, api.id, false))
	}
	return result
}
func (o *validationObserver) observeValidationDependencies(ctx context.Context, management validationManagementReader, r *validationRecorder) error {
	err := r.run("management/dependency/kamaji.compatibility", func() error {
		result := management.observeKamajiCompatibility(ctx, o.desired.operatorNamespace, o.desired.plan.Intent().Release)
		if result.Namespace != o.desired.operatorNamespace || result.Release != o.desired.plan.Intent().Release || result.Message == "" || result.ReasonCode == "" {
			return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Kamaji compatibility was not observed for the selected namespace and release.")
		}
		if result.ReasonCode == "KAMAJI_COMPATIBILITY_CHANGED" {
			return fmt.Errorf("kamaji changed during compatibility observation: %w", errValidationResourceChanged)
		}
		if result.State == "passed" && result.ReasonCode == "KAMAJI_COMPATIBILITY_VERIFIED" {
			return nil
		}
		if result.ReasonCode == "KAMAJI_COMPATIBILITY_ROLLOUT_PENDING" {
			return validationFailure(validation.Pending, validation.ReasonProgressing, result.Message)
		}
		if result.State == "failed" {
			return validationFailure(validation.Failed, validation.ReasonHealth, result.Message)
		}
		return validationFailure(validation.Unverified, validation.ReasonUnavailable, result.Message)
	})
	if validationIdentityChanged(err) {
		return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
	}
	for _, workload := range o.validationExternalWorkloads() {
		err := r.run(validation.CheckID("management/dependency/"+workload.id), func() error {
			objects, err := management.List(ctx, validationObject("apps/v1", workload.kind, workload.namespace, ""), workload.selector)
			if err != nil {
				return err
			}
			if len(objects) != 1 {
				return validationFailure(validation.Failed, validation.ReasonIncomplete, "An external controller is missing or ambiguous in its selected namespace.")
			}
			object := &objects[0]
			if object.GetUID() == "" || object.GetDeletionTimestamp() != nil {
				return validationFailure(validation.Failed, validation.ReasonHealth, "An external controller is deleting or lacks current server identity.")
			}
			switch workload.kind {
			case "Deployment":
				return validationReady(o.desired.rules.ResourceReadiness(object))
			case "StatefulSet":
				var set appsv1.StatefulSet
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &set); err != nil {
					return err
				}
				if !statefulSetAvailable(set) || !argoControllerUsesInClusterNamespace(set, workload.namespace) {
					return validationFailure(validation.Pending, validation.ReasonProgressing, "The Argo application controller is not current, ready and scoped to the selected namespace.")
				}
				return nil
			default:
				return validationFailure(validation.Unverified, validation.ReasonUnsupported, "External workload kind has no supported readiness contract.")
			}
		})
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	}
	for _, api := range pinnedHBNAPIs() {
		err := r.run(validation.CheckID("definitions/external/"+api.id), func() error {
			current, err := management.Get(ctx, validationObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", api.crd))
			if err != nil {
				return err
			}
			group, _, _ := unstructured.NestedString(current.Object, "spec", "group")
			plural, _, _ := unstructured.NestedString(current.Object, "spec", "names", "plural")
			kind, _, _ := unstructured.NestedString(current.Object, "spec", "names", "kind")
			scope, _, _ := unstructured.NestedString(current.Object, "spec", "scope")
			wantScope := "Cluster"
			if api.namespaced {
				wantScope = "Namespaced"
			}
			if group != api.group || plural != api.resource || kind != api.kind || scope != wantScope || !externalCRDReady(current, api) {
				return validationFailure(validation.Failed, validation.ReasonHealth, "An external API definition has a conflicting identity, scope or establishment state.")
			}
			resources, err := management.discoverResourceList(ctx, api.group+"/"+api.version)
			if err != nil {
				return err
			}
			if !discoveredResourceMatches(resources, api) {
				return validationFailure(validation.Failed, validation.ReasonIncomplete, "An external API is not currently discoverable with the selected resource and scope.")
			}
			return nil
		})
		if validationIdentityChanged(err) {
			return &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	}
	return nil
}
