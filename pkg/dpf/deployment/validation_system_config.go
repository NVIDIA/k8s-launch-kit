// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"sigs.k8s.io/yaml"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type validationTenantResources struct{ tenant *tenantClient }

func validationTenantResourcePath(object *unstructured.Unstructured) (string, error) {
	prefix, plural := "", ""
	switch object.GetAPIVersion() + "/" + object.GetKind() {
	case "v1/ConfigMap":
		prefix, plural = "/api/v1", "configmaps"
	case "v1/Service":
		prefix, plural = "/api/v1", "services"
	case "v1/ServiceAccount":
		prefix, plural = "/api/v1", "serviceaccounts"
	case "v1/Pod":
		prefix, plural = "/api/v1", "pods"
	case "apps/v1/DaemonSet":
		prefix, plural = "/apis/apps/v1", "daemonsets"
	case "discovery.k8s.io/v1/EndpointSlice":
		prefix, plural = "/apis/discovery.k8s.io/v1", "endpointslices"
	case "rbac.authorization.k8s.io/v1/ClusterRoleBinding":
		return "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", nil
	default:
		return "", fmt.Errorf("unsupported selected tenant dependency read")
	}
	if object.GetNamespace() == "" {
		return "", fmt.Errorf("selected tenant dependency namespace is required")
	}
	return prefix + "/namespaces/" + object.GetNamespace() + "/" + plural, nil
}
func (r validationTenantResources) Get(ctx context.Context, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	path, err := validationTenantResourcePath(object)
	if err != nil {
		return nil, err
	}
	var result unstructured.Unstructured
	if err := r.tenant.api.Discovery().RESTClient().Get().AbsPath(path + "/" + object.GetName()).Do(ctx).Into(&result); err != nil {
		return nil, err
	}
	if result.GroupVersionKind() != object.GroupVersionKind() || result.GetName() != object.GetName() || result.GetNamespace() != object.GetNamespace() || result.GetUID() == "" || result.GetResourceVersion() == "" {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Selected tenant dependency returned a different or incomplete server identity.")
	}
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return &result, nil
}
func (r validationTenantResources) List(ctx context.Context, object *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if err := r.tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	path, err := validationTenantResourcePath(object)
	if err != nil {
		return nil, err
	}
	var result []unstructured.Unstructured
	cursor, version := "", ""
	seen := map[string]bool{}
	for range 16 {
		var page unstructured.UnstructuredList
		request := r.tenant.api.Discovery().RESTClient().Get().AbsPath(path).Param("limit", "64").Param("labelSelector", selector)
		if cursor != "" {
			request.Param("continue", cursor)
		}
		if err := request.Do(ctx).Into(&page); err != nil {
			return nil, err
		}
		if len(page.Items) > 64 {
			return nil, fmt.Errorf("tenant dependency list page exceeded its finite bound")
		}
		if page.GetResourceVersion() == "" || (version != "" && version != page.GetResourceVersion()) {
			return nil, fmt.Errorf("tenant dependency list snapshot is incomplete")
		}
		version = page.GetResourceVersion()
		for _, item := range page.Items {
			key := string(item.GetUID())
			if key == "" || item.GetName() == "" || item.GetResourceVersion() == "" || item.GetNamespace() != object.GetNamespace() || seen[key] {
				return nil, fmt.Errorf("tenant dependency list identities are incomplete")
			}
			seen[key] = true
			result = append(result, item)
		}
		next := page.GetContinue()
		if next == "" {
			if err := r.tenant.VerifyIdentity(ctx); err != nil {
				return nil, err
			}
			return result, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("tenant dependency list cursor repeated")
		}
		cursor = next
	}
	return nil, fmt.Errorf("tenant dependency list exceeded its finite bound")
}

func validationAppInventoryContains(app, expected *unstructured.Unstructured) error {
	entries, found, err := unstructured.NestedSlice(app.Object, "status", "resources")
	if err != nil || !found {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "System App has no complete functional resource inventory.")
	}
	count := 0
	gv := expected.GroupVersionKind()
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return validationFailure(validation.Unverified, validation.ReasonIncomplete, "System App resource inventory is malformed.")
		}
		kind, _ := entry["kind"].(string)
		name, _ := entry["name"].(string)
		if kind != expected.GetKind() || name != expected.GetName() {
			continue
		}
		group, _ := entry["group"].(string)
		version, _ := entry["version"].(string)
		namespace, _ := entry["namespace"].(string)
		status, _ := entry["status"].(string)
		hook, _ := entry["hook"].(bool)
		pruning, _ := entry["requiresPruning"].(bool)
		if group != gv.Group || version != gv.Version || namespace != expected.GetNamespace() || status != "Synced" || hook || pruning {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Functional resource inventory has the wrong API, scope or sync state.")
		}
		count++
	}
	if count != 1 {
		return validationFailure(validation.Failed, validation.ReasonMissing, "A required functional chart resource is missing or ambiguous in its App inventory.")
	}
	return nil
}
func validationChartObjectIdentity(app, expected, current *unstructured.Unstructured) error {
	if current == nil || current.GroupVersionKind() != expected.GroupVersionKind() || current.GetNamespace() != expected.GetNamespace() || current.GetName() != expected.GetName() || current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Functional chart resource identity is unavailable or differs.")
	}
	gv := expected.GroupVersionKind()
	tracking := app.GetName() + ":" + gv.Group + "/" + expected.GetKind() + ":" + expected.GetNamespace() + "/" + expected.GetName()
	if current.GetAnnotations()["argocd.argoproj.io/tracking-id"] != tracking {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Functional chart resource is not tracked by the selected App.")
	}
	return nil
}
func verifyValidationSystemConfiguration(ctx context.Context, reader resourceReader, app *unstructured.Unstructured, expected systemArtifactExpectation, role systemArtifactRole) error {
	for _, wanted := range expected.ConfigMaps {
		if err := validationAppInventoryContains(app, wanted); err != nil {
			return err
		}
		current, err := reader.Get(ctx, wanted.DeepCopy())
		if err != nil {
			return err
		}
		if err := validationChartObjectIdentity(app, wanted, current); err != nil {
			return err
		}
		if !validationSystemConfigMapDataMatches(role, wanted, current) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Consumed ConfigMap content differs from the independently projected chart.")
		}
	}
	return nil
}

var _ resourceReader = validationTenantResources{}
var _ resourceLister = validationTenantResources{}

// KSM parses its pinned config.yaml as YAML; Helm/Argo can preserve equivalent
// trailing whitespace differently. Compare that one consumed format strictly,
// while retaining exact coverage and exact bytes for all other payloads.
func validationSystemConfigMapDataMatches(role systemArtifactRole, wanted, current *unstructured.Unstructured) bool {
	want, _, we := unstructured.NestedStringMap(wanted.Object, "data")
	got, _, ge := unstructured.NestedStringMap(current.Object, "data")
	if we != nil || ge != nil || !jsonEqual(wanted.Object["binaryData"], current.Object["binaryData"]) {
		return false
	}
	if role.Component == "kube-state-metrics" && role.Management {
		wantYAML, wf := want["config.yaml"]
		gotYAML, gf := got["config.yaml"]
		if !wf || !gf {
			return false
		}
		var wantConfig, gotConfig any
		wantJSON, we := yaml.YAMLToJSONStrict([]byte(wantYAML))
		gotJSON, ge := yaml.YAMLToJSONStrict([]byte(gotYAML))
		if we != nil || ge != nil || json.Unmarshal(wantJSON, &wantConfig) != nil || json.Unmarshal(gotJSON, &gotConfig) != nil || !jsonEqual(wantConfig, gotConfig) {
			return false
		}
		delete(want, "config.yaml")
		delete(got, "config.yaml")
	}
	return jsonEqual(want, got)
}
