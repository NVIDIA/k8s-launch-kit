// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Readiness is a closed kind registry. Structural existence is sufficient only
// for declarative kinds without a reconciliation/readiness API.
func Readiness(o *unstructured.Unstructured) (ready, terminal bool, message string) {
	if o == nil {
		return false, false, "resource is absent"
	}
	if !o.GetDeletionTimestamp().IsZero() {
		return false, true, "resource is deleting"
	}
	if requiresObservedGeneration(o.GetKind()) && (o.GetGeneration() < 1 || number(o, "status", "observedGeneration") != o.GetGeneration()) {
		return false, false, "current generation is not observed"
	}
	switch o.GetKind() {
	case "ConfigMap", "Secret", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "NetworkAttachmentDefinition", "DPUFlavor":
		return true, false, "resource exists; this kind has no readiness condition"
	case "Namespace":
		return text(o, "status", "phase") == "Active", false, "namespace must be Active"
	case "Node":
		return condition(o, "Ready"), false, "Kubernetes Node Ready condition"
	case "Pod":
		phase := text(o, "status", "phase")
		return phase == "Running" && condition(o, "Ready"), phase == "Failed", "Pod must be Running and Ready"
	case "Deployment":
		want, supplied, err := unstructured.NestedInt64(o.Object, "spec", "replicas")
		if !supplied {
			want = 1
		}
		// During a surge, updated and available replicas can be disjoint:
		// the previous ReplicaSet still serves while the new pod is unready.
		return err == nil && want > 0 && o.GetGeneration() > 0 &&
			number(o, "status", "observedGeneration") == o.GetGeneration() &&
			number(o, "status", "replicas") == want &&
			number(o, "status", "updatedReplicas") == want &&
			number(o, "status", "readyReplicas") == want &&
			number(o, "status", "availableReplicas") == want, false, "Deployment must have a current fully available completed rollout"
	case "DaemonSet":
		want := number(o, "status", "desiredNumberScheduled")
		return want > 0 && number(o, "status", "observedGeneration") >= o.GetGeneration() && number(o, "status", "numberReady") == want && number(o, "status", "updatedNumberScheduled") == want, false, "DaemonSet must have a positive fully Ready rollout"
	case "StatefulSet":
		want := number(o, "spec", "replicas")
		if want == 0 {
			want = 1
		}
		return number(o, "status", "observedGeneration") >= o.GetGeneration() && number(o, "status", "readyReplicas") == want, false, "StatefulSet replicas must be Ready"
	case "Job":
		return condition(o, "Complete"), condition(o, "Failed"), "Job completion condition"
	case "Service":
		return text(o, "spec", "clusterIP") != "", false, "Service clusterIP allocation"
	case "CustomResourceDefinition":
		return condition(o, "Established") && condition(o, "NamesAccepted"), false, "CRD must be established with accepted names"
	case "BlueFieldSoftware":
		return text(o, "status", "phase") == "Ready" && condition(o, "Ready"), text(o, "status", "phase") == "Error", "BlueFieldSoftware must be Ready"
	case "DPU":
		return text(o, "status", "phase") == "Ready" && condition(o, "Ready") && number(o, "status", "observedGeneration") == o.GetGeneration(), text(o, "status", "phase") == "Error", "DPU phase, condition and observed generation must be Ready"
	case "DPUCluster":
		// rc.1 and GA use cutil.NewCondition without observedGeneration.
		phase := text(o, "status", "phase")
		return phase == "Ready" && condition(o, "Ready"), phase == "Failed", "DPUCluster phase=" + phase + "; " + conditionMessage(o)
	case "DPUServiceNAD", "DPUServiceCredentialRequest", "Certificate", "Issuer", "ClusterIssuer":
		return freshCondition(o, "Ready"), condition(o, "Error"), conditionMessage(o)
	case "DPUSet", "DPUService", "DPUServiceInterface", "DPUServiceChain":
		return condition(o, "Ready"), condition(o, "Error"), conditionMessage(o)
	default:
		return false, true, fmt.Sprintf("no readiness contract for %s", o.GetKind())
	}
}

func conditionMessage(o *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		if c, ok := raw.(map[string]any); ok && c["type"] == "Ready" {
			reason, _ := c["reason"].(string)
			if len(reason) > 256 {
				reason = reason[:256]
			}
			return fmt.Sprintf("%s Ready=%v reason=%s", o.GetKind(), c["status"], reason)
		}
	}
	return o.GetKind() + " has no observed Ready condition"
}

func condition(o *unstructured.Unstructured, kind string) bool {
	if o == nil {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		if c, ok := raw.(map[string]any); ok && c["type"] == kind && c["status"] == "True" {
			return true
		}
	}
	return false
}

func hasCondition(o *unstructured.Unstructured, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		if c, ok := raw.(map[string]any); ok && c["type"] == kind {
			return true
		}
	}
	return false
}

func nodeForDPU(dpu *unstructured.Unstructured) string {
	// Exact rc.1 GenerateNodeName and GetNodeFromDPUCluster use the DPU name.
	return dpu.GetName()
}

func resourcesInBundle(b *bundle.Bundle, kind string) []bundle.Document {
	var documents []bundle.Document
	for _, stage := range b.Index().Stages {
		view, _ := b.Stage(stage.ID)
		for _, doc := range view.Documents() {
			if doc.Ref().Kind == kind {
				documents = append(documents, doc)
			}
		}
	}
	return documents
}

// These exact rc.1 APIs publish status.observedGeneration. NAD and
// CredentialRequest instead use condition.observedGeneration, stamped by
// pkg/conditions.SetSummary. DPUCluster exposes neither observation field.
func requiresObservedGeneration(kind string) bool {
	switch kind {
	case "BlueFieldSoftware", "DPU", "DPUSet", "DPUService", "DPUServiceInterface", "DPUServiceChain":
		return true
	default:
		return false
	}
}

func freshCondition(o *unstructured.Unstructured, kind string) bool {
	if o == nil || o.GetGeneration() < 1 {
		return false
	}
	entries, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, entry := range entries {
		c, ok := entry.(map[string]any)
		if !ok || c["type"] != kind || c["status"] != "True" {
			continue
		}
		observation, exists, err := unstructured.NestedInt64(c, "observedGeneration")
		return err == nil && exists && observation == o.GetGeneration()
	}
	return false
}
