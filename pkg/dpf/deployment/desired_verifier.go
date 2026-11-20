// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// desiredVerifier owns immutable comparison rules only. It has no API client,
// create method, operation state or deployment-history prerequisite.
type desiredVerifier struct {
	rules              *controllercontract.Rules
	bundleDigest       string
	controllerMetadata controllercontract.MetadataPolicy
}

func newDesiredVerifier(rules *controllercontract.Rules, bundleDigest string, policy controllercontract.MetadataPolicy) (*desiredVerifier, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	if !validSHA256Digest(bundleDigest) {
		return nil, fmt.Errorf("bundle digest must be a lowercase sha256 digest")
	}
	return &desiredVerifier{rules: rules, bundleDigest: bundleDigest, controllerMetadata: policy}, nil
}

// BundleDigest returns the immutable retained identity used for ownership.
func (r *desiredVerifier) BundleDigest() string {
	if r == nil {
		return ""
	}
	return r.bundleDigest
}

// Verify checks identity, ownership, metadata, and the complete defaulted CR
// body. It works on defensive copies and can also be used for later readback.
func (r *desiredVerifier) Verify(desired, current *unstructured.Unstructured) error {
	if r == nil {
		return reconcileError("DESIRED_VERIFIER_INVALID", "desired verifier is required", nil)
	}
	marked, err := r.markedDesired(desired)
	if err != nil {
		return reconcileError("DESIRED_OBJECT_INVALID", "desired DPF resource failed validation", err)
	}
	if current == nil {
		return reconcileError("RESOURCE_IDENTITY_MISMATCH", "current resource is missing", nil)
	}
	if marked.GroupVersionKind() != current.GroupVersionKind() || marked.GetName() != current.GetName() || marked.GetNamespace() != current.GetNamespace() {
		return reconcileError("RESOURCE_IDENTITY_MISMATCH", "current resource identity differs from desired", nil)
	}
	if current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
		return reconcileError("RESOURCE_UNAVAILABLE", "current resource lacks server identity or is deleting", nil)
	}
	if current.GetAnnotations()[BundleDigestAnnotation] != r.bundleDigest || current.GetAnnotations()[IntentDigestAnnotation] != marked.GetAnnotations()[IntentDigestAnnotation] {
		return reconcileError("RESOURCE_OWNERSHIP_CONFLICT", "current resource is not marked for this bundle and desired object", nil)
	}
	if !r.rules.LabelsMatch(marked, current) {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource labels differ from desired", nil)
	}
	if !maps.Equal(marked.GetAnnotations(), current.GetAnnotations()) {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource annotations differ from desired", nil)
	}
	if !reflect.DeepEqual(marked.GetOwnerReferences(), current.GetOwnerReferences()) {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource owner references differ from desired", nil)
	}
	if err := r.rules.ValidateMetadata(current, r.controllerMetadata); err != nil {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource has unsupported controller metadata", err)
	}
	want, err := r.rules.DefaultedCopy(marked)
	if err != nil {
		return reconcileError("RESOURCE_SCHEMA_INVALID", "desired resource fails the retained DPF schema", err)
	}
	got, err := r.rules.DefaultedCopy(current)
	if err != nil {
		return reconcileError("RESOURCE_SCHEMA_INVALID", "current resource fails the retained DPF schema", err)
	}
	want, got, err = r.rules.ComparisonCopies(want, got)
	if err != nil {
		return reconcileError("RESOURCE_CONTENT_CONFLICT", "current DPUCluster controller field differs from the expected value", err)
	}
	for _, object := range []*unstructured.Unstructured{want, got} {
		delete(object.Object, "metadata")
		delete(object.Object, "status")
	}
	if !jsonEqual(want.Object, got.Object) {
		return reconcileError("RESOURCE_CONTENT_CONFLICT", "current resource content differs from desired", nil)
	}
	return nil
}

func (r *desiredVerifier) markedDesired(desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if r == nil || desired == nil {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired verifier and desired object are required", nil)
	}
	if desired.GetAPIVersion() == "" || desired.GetKind() == "" || desired.GetName() == "" || desired.GetNamespace() == "" {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired custom resource requires apiVersion, kind, name, and namespace", nil)
	}
	if desired.GetUID() != "" || desired.GetResourceVersion() != "" || desired.GetDeletionTimestamp() != nil || desired.GetGenerateName() != "" {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired resource contains server-owned identity metadata", nil)
	}
	if _, found := desired.Object["status"]; found {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired resource must not contain status", nil)
	}
	if !strings.Contains(desired.GetAPIVersion(), "/") {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired object must be a namespaced DPF custom resource", nil)
	}
	if desired.GetAnnotations()[BundleDigestAnnotation] != "" || desired.GetAnnotations()[IntentDigestAnnotation] != "" {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired object must not supply deployment ownership annotations", nil)
	}
	copy := desired.DeepCopy()
	annotations := copy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	annotations[BundleDigestAnnotation] = r.bundleDigest
	intentDigest, err := objectDigest(desired)
	if err != nil {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "desired object cannot be encoded for ownership", err)
	}
	annotations[IntentDigestAnnotation] = intentDigest
	copy.SetAnnotations(annotations)
	if _, err := r.rules.DefaultedCopy(copy); err != nil {
		return nil, reconcileError("RESOURCE_SCHEMA_INVALID", "desired resource fails the retained DPF schema", err)
	}
	return copy, nil
}
