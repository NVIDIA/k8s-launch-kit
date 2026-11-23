// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ocpCleanOwnerAnnotation  = "nvidia.kubernetes-launch-kit.ocp-bundle-owner"
	ocpCleanUIDAnnotation    = "nvidia.kubernetes-launch-kit.ocp-object-uid"
	ocpCleanRetainAnnotation = "nvidia.kubernetes-launch-kit.ocp-clean-retain"
)

// An OCP plan contains exact bundle identities, never a namespace inventory.
// Absent refs retain desired intent but have no UID and cannot authorize deletion.
type ocpCleanPlan struct {
	owner           string
	refs            []cleanObjectRef
	operatorConfigs []*unstructured.Unstructured
	retained        []string
}

func prepareOCPCleanPlan(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, artifacts *bundle.Bundle) (ocpCleanPlan, error) {
	plan, err := ocpCleanDesiredPlan(cfg, artifacts)
	if err != nil {
		return ocpCleanPlan{}, err
	}
	if c == nil {
		return ocpCleanPlan{}, fmt.Errorf("OpenShift cleanup requires a Kubernetes client")
	}
	for i := range plan.refs {
		ref := &plan.refs[i]
		live := objectForCleanRef(*ref)
		if err := c.Get(ctx, client.ObjectKeyFromObject(live), live); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return ocpCleanPlan{}, fmt.Errorf("inspect cleanup target %s: %w", cleanRefString(*ref), err)
		}
		if err := validateOCPCleanLiveOwnership(ctx, c, ref.Desired, live); err != nil {
			return ocpCleanPlan{}, fmt.Errorf("refuse cleanup of %s: %w", cleanRefString(*ref), err)
		}
		ref.UID = live.GetUID()
	}
	return plan, nil
}

func ocpCleanDesiredPlan(cfg *config.LaunchKitConfig, artifacts *bundle.Bundle) (ocpCleanPlan, error) {
	if cfg == nil {
		return ocpCleanPlan{}, fmt.Errorf("OpenShift cleanup requires a cluster config")
	}
	selected, err := prepareDeploymentObjects(artifacts, true)
	if err != nil {
		return ocpCleanPlan{}, err
	}
	if _, present := artifacts.ValuesContent(); present {
		return ocpCleanPlan{}, fmt.Errorf("OpenShift cleanup does not accept Helm values.yaml")
	}
	operatorNS := ocpCleanNamespaces(cfg).operator
	owner, err := ocpBundleOwner(artifacts, operatorNS)
	if err != nil {
		return ocpCleanPlan{}, err
	}
	plan := ocpCleanPlan{owner: owner, operatorConfigs: selected.operatorConfigs}
	seen := map[bundle.ObjectRef]bool{}
	for _, doc := range artifacts.Documents() {
		obj := doc.ObjectCopy()
		if doc.Role() != bundle.Deployment || isOLMInstallTemplate(filepath.Base(doc.Source().File)) || isOCPOperatorConfig(obj) {
			continue
		}
		if !isOCPCleanKind(obj.GroupVersionKind()) {
			plan.retained = append(plan.retained, fmt.Sprintf("Retain %s %s from %s: automatic rollback is unavailable for this resource kind", obj.GetKind(), client.ObjectKeyFromObject(obj), doc.Source().File))
			continue
		}
		if err := validateOCPCleanIdentity(cfg, obj); err != nil {
			return ocpCleanPlan{}, &bundle.Error{Source: doc.Source(), Reason: "unsafe OpenShift cleanup identity", Err: err}
		}
		ref := doc.Ref()
		if seen[ref] {
			return ocpCleanPlan{}, &bundle.Error{Source: doc.Source(), Reason: "duplicate OpenShift cleanup identity"}
		}
		seen[ref] = true
		setOCPCleanOwner(obj, owner)
		plan.refs = append(plan.refs, cleanObjectRef{GVK: obj.GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), Desired: obj})
	}
	for _, obj := range plan.operatorConfigs {
		if err := validateOCPSharedConfigIdentity(cfg, obj); err != nil {
			return ocpCleanPlan{}, err
		}
	}
	sortCleanRefs(plan.refs)
	return plan, nil
}

// Only CRs emitted by supported OCP profiles can enter the deletion plan. In
// particular operator singleton configuration, OLM resources and examples cannot.
func isOCPCleanKind(gvk schema.GroupVersionKind) bool {
	switch gvk.GroupVersion().String() + "/" + gvk.Kind {
	case "mellanox.com/v1alpha1/NicClusterPolicy", "mellanox.com/v1alpha1/NicNodePolicy",
		"mellanox.com/v1alpha1/HostDeviceNetwork", "mellanox.com/v1alpha1/IPoIBNetwork", "mellanox.com/v1alpha1/MacvlanNetwork",
		"sriovnetwork.openshift.io/v1/SriovNetworkNodePolicy", "sriovnetwork.openshift.io/v1/SriovNetworkPoolConfig",
		"sriovnetwork.openshift.io/v1/SriovNetwork", "sriovnetwork.openshift.io/v1/SriovIBNetwork",
		"nv-ipam.nvidia.com/v1alpha1/IPPool",
		"configuration.net.nvidia.com/v1alpha1/NicConfigurationTemplate", "configuration.net.nvidia.com/v1alpha1/NicInterfaceNameTemplate":
		return true
	}
	return false
}

type ocpNamespaces struct {
	operator, sriov, maintenance, nfd, nfdName string
}

func ocpCleanNamespaces(cfg *config.LaunchKitConfig) ocpNamespaces {
	names := ocpNamespaces{
		operator: "nvidia-network-operator", sriov: config.DefaultSriovOperatorNamespace,
		maintenance: config.DefaultMaintenanceOperatorNamespace, nfd: config.DefaultNFDOperatorNamespace,
		nfdName: config.DefaultNFDConfigurationName,
	}
	if cfg.NetworkOperator != nil && cfg.NetworkOperator.Namespace != "" {
		names.operator = cfg.NetworkOperator.Namespace
	}
	if cfg.Sriov != nil && cfg.Sriov.OperatorNamespace != "" {
		names.sriov = cfg.Sriov.OperatorNamespace
	}
	if cfg.Maintenance != nil && cfg.Maintenance.OperatorNamespace != "" {
		names.maintenance = cfg.Maintenance.OperatorNamespace
	}
	if cfg.NFD != nil {
		if cfg.NFD.OperatorNamespace != "" {
			names.nfd = cfg.NFD.OperatorNamespace
		}
		if cfg.NFD.ConfigurationName != "" {
			names.nfdName = cfg.NFD.ConfigurationName
		}
	}
	return names
}

func validateOCPCleanIdentity(cfg *config.LaunchKitConfig, obj *unstructured.Unstructured) error {
	names := ocpCleanNamespaces(cfg)
	expected := ""
	switch obj.GroupVersionKind().Group {
	case "sriovnetwork.openshift.io":
		expected = names.sriov
		if (obj.GetKind() == "SriovNetworkNodePolicy" || obj.GetKind() == "SriovNetworkPoolConfig") && obj.GetName() == "default" {
			return fmt.Errorf("platform default %s is not a generated cleanup target", obj.GetKind())
		}
	case "nv-ipam.nvidia.com", "configuration.net.nvidia.com":
		expected = names.operator
	}
	if obj.GetNamespace() != expected {
		return fmt.Errorf("%s %s has namespace %q, expected %q", obj.GetKind(), obj.GetName(), obj.GetNamespace(), expected)
	}
	return nil
}

func validateOCPSharedConfigIdentity(cfg *config.LaunchKitConfig, obj *unstructured.Unstructured) error {
	for _, target := range ocpConfigurationSingletons(cfg) {
		if target.GroupVersionKind() == obj.GroupVersionKind() && client.ObjectKeyFromObject(target) == client.ObjectKeyFromObject(obj) {
			return nil
		}
	}
	return fmt.Errorf("unreviewed OpenShift shared configuration identity %s %s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
}

// The stable root identity permits regeneration to add/remove grouped CRs without
// changing ownership of every existing CR. Bundles targeting the same NCP address
// the same logical deployment. Per-object UID bindings protect incarnations;
// these markers do not establish the historical UID of legacy resources.
func ocpBundleOwner(artifacts *bundle.Bundle, operatorNamespace string) (string, error) {
	if artifacts == nil {
		return "", fmt.Errorf("OpenShift artifact bundle is nil")
	}
	var root *bundle.ObjectRef
	for _, doc := range artifacts.Documents() {
		obj := doc.ObjectCopy()
		if doc.Role() == bundle.Deployment && !isOLMInstallTemplate(filepath.Base(doc.Source().File)) && isNicClusterPolicy(obj) {
			if root != nil {
				return "", fmt.Errorf("OpenShift ownership requires exactly one NicClusterPolicy root")
			}
			ref := doc.Ref()
			root = &ref
		}
	}
	if root == nil {
		return "", fmt.Errorf("OpenShift ownership requires a retained NicClusterPolicy root")
	}
	encoded, err := json.Marshal(struct {
		Root              bundle.ObjectRef
		OperatorNamespace string
	}{Root: *root, OperatorNamespace: operatorNamespace})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func setOCPCleanOwner(obj *unstructured.Unstructured, owner string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[ocpCleanOwnerAnnotation] = owner
	obj.SetAnnotations(annotations)
}

// prepareOCPCleanDeployment checks the entire ownership boundary before adding
// markers to deployment copies. It must run before any apply or remediation.
func prepareOCPCleanDeployment(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, artifacts *bundle.Bundle, selected *deploymentObjects) error {
	if c == nil || cfg == nil || artifacts == nil || selected == nil {
		return fmt.Errorf("OpenShift ownership preparation requires a client, config, bundle and selected objects")
	}
	// Arbitrary supported deploy inputs do not acquire cleanup authority. A
	// reviewed OCP root is required to opt into tracking, but its absence must
	// not change the existing general-purpose deploy contract.
	if selected.ncp == nil || !isNicClusterPolicy(selected.ncp) {
		return nil
	}
	operatorNS := ocpCleanNamespaces(cfg).operator
	owner, err := ocpBundleOwner(artifacts, operatorNS)
	if err != nil {
		return err
	}
	ordinary := append([]*unstructured.Unstructured{}, selected.nnps...)
	ordinary = append(ordinary, selected.others...)
	ordinary = append(ordinary, selected.ncp)
	for _, obj := range ordinary {
		if !isOCPCleanKind(obj.GroupVersionKind()) {
			continue
		}
		desired := obj.DeepCopy()
		setOCPCleanOwner(desired, owner)
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				return fmt.Errorf("inspect OpenShift deployment ownership: %w", err)
			}
		} else if err := validateOCPBoundOwner(desired, live); err != nil {
			return err
		}
	}
	// Only mutate selected copies after every ownership inspection succeeds.
	for _, obj := range ordinary {
		if !isOCPCleanKind(obj.GroupVersionKind()) {
			continue
		}
		if err := validateOCPCleanIdentity(cfg, obj); err != nil {
			annotations := obj.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[ocpCleanRetainAnnotation] = "true"
			obj.SetAnnotations(annotations)
			continue
		}
		setOCPCleanOwner(obj, owner)
	}
	selected.ocpOwner = owner
	return nil
}

func validateOCPCleanOwnership(desired, live *unstructured.Unstructured) error {
	if err := validateOCPProvenance(desired, live, true); err != nil {
		return err
	}
	want, found, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil || !found || len(want) == 0 {
		return fmt.Errorf("retained desired spec is missing or invalid")
	}
	have, found, err := unstructured.NestedMap(live.Object, "spec")
	// A matching desired subset cannot authorize deleting a wider live
	// deployment: an older bundle can omit a newly enabled driver, selector or
	// network component owned by this same l8k manager. Until schema defaults
	// are explicitly normalized, every unexplained spec difference is unsafe.
	if err != nil || !found || !reflect.DeepEqual(have, want) {
		return fmt.Errorf("live spec differs from retained generated intent")
	}
	return nil
}

func validateOCPProvenance(desired, live *unstructured.Unstructured, requireVersion bool) error {
	if desired == nil || live == nil || desired.GroupVersionKind() != live.GroupVersionKind() || client.ObjectKeyFromObject(desired) != client.ObjectKeyFromObject(live) {
		return fmt.Errorf("resource identity differs from retained generated intent")
	}
	if live.GetUID() == "" || len(live.GetOwnerReferences()) > 0 {
		return fmt.Errorf("live resource has no UID or belongs to another controller")
	}
	wantAnnotations, haveAnnotations := desired.GetAnnotations(), live.GetAnnotations()
	if haveAnnotations[ocpCleanRetainAnnotation] != "" {
		return fmt.Errorf("live resource was retained because deployment could not establish exclusive ownership")
	}
	if err := validateOCPBoundOwner(desired, live); err != nil {
		return err
	}
	if requireVersion && (wantAnnotations[launchKitVersionAnnotation] == "" || wantAnnotations[launchKitVersionAnnotation] != haveAnnotations[launchKitVersionAnnotation]) {
		return fmt.Errorf("live resource lacks matching Launch Kit generation provenance")
	}
	owned := false
	desiredSpec, found, err := unstructured.NestedFieldNoCopy(desired.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("retained desired spec is missing or invalid")
	}
	for _, entry := range live.GetManagedFields() {
		if entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields); err != nil {
			return fmt.Errorf("invalid live field ownership: %w", err)
		}
		specFields, spec := fields["f:spec"]
		if !spec {
			continue
		}
		// Atomic create records an Update entry before the first SSA. Its
		// defaults can remain owned there. Accept it only for UID-bound
		// objects, while still requiring Apply ownership of desired fields.
		boundCreate := haveAnnotations[ocpCleanOwnerAnnotation] != "" && entry.Manager == "l8k" && entry.Operation == metav1.ManagedFieldsOperationUpdate
		if boundCreate {
			continue
		}
		if entry.Manager != "l8k" || entry.Operation != metav1.ManagedFieldsOperationApply {
			return fmt.Errorf("live spec has ownership outside Launch Kit")
		}
		owned = owned || ocpOwnsDesiredFields(desiredSpec, specFields)
	}
	if !owned {
		return fmt.Errorf("live spec lacks Launch Kit server-side apply ownership")
	}
	return nil
}

// FieldsV1 uses f: for object fields, k: for associative-list keys and v: for
// set values. Empty field trees denote atomic ownership; a dot alone denotes
// presence, not ownership of every nested value.
func ocpOwnsDesiredFields(want, fields any) bool {
	tree, ok := fields.(map[string]any)
	if !ok {
		return false
	}
	if len(tree) == 0 {
		return true
	}
	switch value := want.(type) {
	case map[string]any:
		for key, child := range value {
			field, exists := tree["f:"+key]
			if !exists || !ocpOwnsDesiredFields(child, field) {
				return false
			}
		}
		return true
	case []any:
		for _, element := range value {
			matched := false
			for key, child := range tree {
				if strings.HasPrefix(key, "k:") {
					var selector map[string]any
					if json.Unmarshal([]byte(strings.TrimPrefix(key, "k:")), &selector) == nil && ocpSpecContains(element, selector) && ocpOwnsDesiredFields(element, child) {
						matched = true
					}
				} else if strings.HasPrefix(key, "v:") {
					var member any
					if json.Unmarshal([]byte(strings.TrimPrefix(key, "v:")), &member) == nil && reflect.DeepEqual(member, element) {
						matched = true
					}
				}
			}
			if !matched {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func validateOCPBoundOwner(desired, live *unstructured.Unstructured) error {
	annotations := live.GetAnnotations()
	if marker := annotations[ocpCleanOwnerAnnotation]; marker != "" {
		if marker != desired.GetAnnotations()[ocpCleanOwnerAnnotation] {
			return fmt.Errorf("live resource belongs to a different OpenShift bundle")
		}
		if annotations[ocpCleanUIDAnnotation] == "" {
			return fmt.Errorf("live resource has an incomplete OpenShift ownership binding; inspect the partial deployment before retrying")
		}
		if annotations[ocpCleanUIDAnnotation] != string(live.GetUID()) {
			return fmt.Errorf("live resource UID differs from its OpenShift ownership binding")
		}
	} else if annotations[ocpCleanUIDAnnotation] != "" {
		return fmt.Errorf("live resource has a UID binding without a bundle owner")
	}
	return nil
}

// ocpSpecContains matches associative-list keys while inspecting FieldsV1. It
// must not authorize cleanup from a desired subset; that comparison is exact.
func ocpSpecContains(have, want any) bool {
	wantMap, ok := want.(map[string]any)
	if !ok {
		return reflect.DeepEqual(have, want)
	}
	haveMap, ok := have.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range wantMap {
		actual, exists := haveMap[key]
		if !exists || !ocpSpecContains(actual, value) {
			return false
		}
	}
	return true
}
