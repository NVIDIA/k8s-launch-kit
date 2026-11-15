// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	BundleDigestAnnotation   = "nvidia.kubernetes-launch-kit.dpf-bundle-digest"
	IntentDigestAnnotation   = "nvidia.kubernetes-launch-kit.dpf-object-digest"
	maxOwnershipDigestLength = len("sha256:") + 64
)

// ReconciliationError carries a bounded, safe diagnostic while preserving
// causes for errors.Is/As classification.
type ReconciliationError struct {
	ReasonCode string
	Message    string
	cause      error
}

func (e *ReconciliationError) Error() string {
	if e == nil {
		return "DPF resource reconciliation failed"
	}
	return e.ReasonCode + ": " + e.Message
}

func (e *ReconciliationError) Unwrap() error { return e.cause }

func reconcileError(code, message string, cause error) error {
	return &ReconciliationError{ReasonCode: code, Message: message, cause: cause}
}

// ResourceClient is the narrow portion of APIClient used by create-or-verify.
// Create must not update or adopt an existing object.
type ResourceClient interface {
	Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Create(context.Context, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error)
}

type Disposition string

const (
	DispositionCreated           Disposition = "created"
	DispositionExisting          Disposition = "existing"
	DispositionExistingAfterRace Disposition = "existing-after-create-race"
	DispositionUncertain         Disposition = "uncertain"
)

// Reconciler verifies DPF objects against the CRD schemas retained from the
// selected operator chart and one immutable bundle identity.
type Reconciler struct {
	schemas      *generation.ObjectSchemas
	bundleDigest string
}

// NewReconciler validates the retained operator projection and bundle digest.
func NewReconciler(operatorManifest []byte, bundleDigest string) (*Reconciler, error) {
	if len(operatorManifest) == 0 {
		return nil, fmt.Errorf("retained operator manifest is required")
	}
	if !validSHA256Digest(bundleDigest) {
		return nil, fmt.Errorf("bundle digest must be a lowercase sha256 digest")
	}
	schemas, err := generation.NewObjectSchemas(append([]byte(nil), operatorManifest...))
	if err != nil {
		return nil, fmt.Errorf("compile retained operator CRD schemas: %w", err)
	}
	return &Reconciler{schemas: schemas, bundleDigest: bundleDigest}, nil
}

// BundleDigest returns the immutable bundle identity bound to this reconciler.
func (r *Reconciler) BundleDigest() string {
	if r == nil {
		return ""
	}
	return r.bundleDigest
}

// Ensure reads before creating. AlreadyExists races are resolved by a fresh
// read and the same verifier; no update, patch, adoption, or delete is issued.
// If Create succeeds but response verification fails, DispositionCreated and
// the returned object identify a possible partial write; callers must not retry
// this as an unobserved create.
func (r *Reconciler) Ensure(ctx context.Context, client ResourceClient, desired *unstructured.Unstructured) (*unstructured.Unstructured, Disposition, error) {
	if ctx == nil || client == nil || r == nil {
		return nil, "", fmt.Errorf("context, reconciler, and resource client are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	marked, err := r.markedDesired(desired)
	if err != nil {
		return nil, "", reconcileError("DESIRED_OBJECT_INVALID", "desired DPF resource failed validation", err)
	}
	current, err := client.Get(ctx, desired.DeepCopy())
	if err == nil {
		if err := r.Verify(desired, current); err != nil {
			return current, DispositionExisting, err
		}
		return current, DispositionExisting, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, "", reconcileError("RESOURCE_READ_FAILED", "cannot read the existing DPF resource", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	created, createErr := client.Create(ctx, marked, false)
	if createErr != nil {
		if !apierrors.IsAlreadyExists(createErr) {
			return nil, DispositionUncertain, reconcileError("RESOURCE_CREATE_UNCERTAIN", "create returned an error; the API may have committed the resource, re-read before retry", createErr)
		}
		current, err = client.Get(ctx, desired.DeepCopy())
		if err != nil {
			return nil, DispositionExistingAfterRace, reconcileError("RESOURCE_RACE_READ_FAILED", "cannot re-read the resource after a create race", err)
		}
		if err := r.Verify(desired, current); err != nil {
			return current, DispositionExistingAfterRace, reconcileError("RESOURCE_CONFLICT", "resource created concurrently does not match the retained bundle", err)
		}
		return current, DispositionExistingAfterRace, nil
	}
	if created == nil {
		return nil, DispositionUncertain, reconcileError("RESOURCE_CREATE_RESPONSE_MISSING", "create succeeded without an object response; re-read before retry", nil)
	}
	if err := r.Verify(desired, created); err != nil {
		return created, DispositionCreated, reconcileError("RESOURCE_POST_CREATE_CONFLICT", "resource was created but response verification failed; re-read before retry", err)
	}
	return created, DispositionCreated, nil
}

// Verify checks identity, ownership, metadata, and the complete defaulted CR
// body. It works on defensive copies and can also be used for later readback.
func (r *Reconciler) Verify(desired, current *unstructured.Unstructured) error {
	if r == nil {
		return reconcileError("RECONCILER_INVALID", "reconciler is required", nil)
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
	if !labelsMatch(marked, current) {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource labels differ from desired", nil)
	}
	if !maps.Equal(marked.GetAnnotations(), current.GetAnnotations()) {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource annotations differ from desired", nil)
	}
	if !reflect.DeepEqual(marked.GetOwnerReferences(), current.GetOwnerReferences()) {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource owner references differ from desired", nil)
	}
	if err := validateControllerMetadata(current); err != nil {
		return reconcileError("RESOURCE_METADATA_CONFLICT", "current resource has unsupported controller metadata", err)
	}
	want, err := r.schemas.DefaultedCopy(marked)
	if err != nil {
		return reconcileError("RESOURCE_SCHEMA_INVALID", "desired resource fails the retained DPF schema", err)
	}
	got, err := r.schemas.DefaultedCopy(current)
	if err != nil {
		return reconcileError("RESOURCE_SCHEMA_INVALID", "current resource fails the retained DPF schema", err)
	}
	if err := allowDPUClusterKubeconfig(want, got); err != nil {
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

func (r *Reconciler) markedDesired(desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if r == nil || desired == nil {
		return nil, reconcileError("DESIRED_OBJECT_INVALID", "reconciler and desired object are required", nil)
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
	if _, err := r.schemas.DefaultedCopy(copy); err != nil {
		return nil, reconcileError("RESOURCE_SCHEMA_INVALID", "desired resource fails the retained DPF schema", err)
	}
	return copy, nil
}

func allowDPUClusterKubeconfig(desired, current *unstructured.Unstructured) error {
	if desired.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || desired.GetKind() != "DPUCluster" {
		return nil
	}
	_, wantFound, err := unstructured.NestedString(desired.Object, "spec", "kubeconfig")
	if err != nil {
		return fmt.Errorf("read desired DPUCluster kubeconfig: %w", err)
	}
	got, gotFound, err := unstructured.NestedString(current.Object, "spec", "kubeconfig")
	if err != nil {
		return fmt.Errorf("read current DPUCluster kubeconfig: %w", err)
	}
	if !wantFound && gotFound {
		expected := desired.GetName() + "-admin-kubeconfig"
		if got != expected {
			return fmt.Errorf("DPUCluster kubeconfig differs from deterministic controller-owned Secret name")
		}
		unstructured.RemoveNestedField(current.Object, "spec", "kubeconfig")
	}
	return nil
}

func validateControllerMetadata(object *unstructured.Unstructured) error {
	allowed := map[string]map[string]bool{
		"DPUCluster":              {"provisioning.dpu.nvidia.com/cluster-manager-clean-up": true},
		"DPUDeployment":           {"dpu.nvidia.com/dpudeployment": true},
		"BlueFieldSoftware":       {"dpu.nvidia.com/dpudeployment": true},
		"DPUFlavor":               {"dpu.nvidia.com/dpudeployment": true},
		"DPUServiceTemplate":      {"dpu.nvidia.com/dpudeployment": true},
		"DPUServiceConfiguration": {"dpu.nvidia.com/dpudeployment": true},
	}
	seen := make(map[string]bool, len(object.GetFinalizers()))
	for _, finalizer := range object.GetFinalizers() {
		if !allowed[object.GetKind()][finalizer] || seen[finalizer] {
			return fmt.Errorf("current object has an unrecognized finalizer")
		}
		seen[finalizer] = true
	}
	return nil
}

func labelsMatch(desired, current *unstructured.Unstructured) bool {
	want, got := desired.GetLabels(), current.GetLabels()
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	if len(got) == len(want) {
		return true
	}
	if !controllerMayMarkDependency(current.GetKind()) || len(got) != len(want)+1 {
		return false
	}
	for key, value := range got {
		if _, expected := want[key]; expected {
			continue
		}
		const prefix = "svc.dpu.nvidia.com/consumed-by-dpudeployment-"
		suffix := strings.TrimPrefix(key, prefix)
		if suffix == key || len(suffix) != 10 || value != "" {
			return false
		}
		for _, character := range suffix {
			if !strings.ContainsRune("0123456789abcdef", character) {
				return false
			}
		}
	}
	return true
}

func controllerMayMarkDependency(kind string) bool {
	switch kind {
	case "BlueFieldSoftware", "DPUFlavor", "DPUServiceTemplate", "DPUServiceConfiguration":
		return true
	default:
		return false
	}
}

func objectDigest(object *unstructured.Unstructured) (string, error) {
	data, err := json.Marshal(object.Object)
	if err != nil {
		return "", fmt.Errorf("encode desired object for ownership digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validSHA256Digest(value string) bool {
	if len(value) != maxOwnershipDigestLength || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func jsonEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}
