// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"path"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/apimachinery/pkg/runtime"
)

// These additions are the selected 26.8 operator's explicit input contract,
// not defaults guessed from observations. Source: stable release
// internal/operator/inventory/{variables,from_dpu_service_manifests}.go at
// f1b50e85122a3df6047708e41a8203f8cdce03a1. Retained profile selection rejects
// other releases before these helpers can run.
func systemControllerValues(intent generation.ResolvedIntent, role systemArtifactRole, values map[string]any) {
	component := values[role.Component].(map[string]any)
	switch role.Component {
	case "cni-installer":
		component["cniBinDir"] = "/opt/cni/bin"
	case "multus", "nvidia-k8s-ipam":
		component["cniBinDir"] = "/opt/cni/bin"
		component["cniConfDir"] = "/etc/cni/net.d/"
	case "flannel":
		nested := component["flannel"].(map[string]any)
		nested["cniBinDir"] = "/opt/cni/bin"
		nested["cniConfDir"] = "/etc/cni/net.d/"
	case "sfc-controller":
		component["openvSwitchBinDir"] = "/usr/bin/"
		component["openvSwitchRunDir"] = "/var/run/openvswitch/"
		component["openvSwitchSharedLibraryDir"] = "/lib"
	}
	if !validationPerClusterRole(role) {
		return
	}
	tenant := intent.Bootstrap.Tenant
	suffix := systemClusterSuffix(tenant.Name, tenant.Namespace)
	account := role.Component + "-" + suffix
	if !role.Management {
		component["rbac"] = map[string]any{"serviceAccounts": []any{map[string]any{"name": account, "namespace": intent.Bootstrap.OperatorNamespace}}}
		return
	}
	secret := role.Component + "-credentials-" + suffix
	request := systemCredentialRequestStem(role) + "-" + suffix
	component["serviceAccount"] = map[string]any{"name": account, "labels": map[string]any{"svc.dpu.nvidia.com/credential-request-name": request, "svc.dpu.nvidia.com/credential-request-namespace": intent.Bootstrap.OperatorNamespace, "svc.dpu.nvidia.com/managed-by-controller": "dpuservicecredentialrequestcontroller"}}
	component["env"] = []any{
		map[string]any{"name": "KUBERNETES_SERVICE_HOST", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": "KUBERNETES_SERVICE_HOST"}}},
		map[string]any{"name": "KUBERNETES_SERVICE_PORT", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": "KUBERNETES_SERVICE_PORT"}}},
	}
	sources := []any{}
	for _, item := range []struct{ key, file string }{{"TOKEN_FILE", "token"}, {"KUBERNETES_CA_DATA", "ca.crt"}} {
		sources = append(sources, map[string]any{"secret": map[string]any{"name": secret, "items": []any{map[string]any{"key": item.key, "path": item.file}}}})
	}
	component["volumes"] = []any{map[string]any{"name": "tokenfile", "projected": map[string]any{"sources": sources}}}
	component["volumeMounts"] = []any{map[string]any{"name": "tokenfile", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount", "readOnly": true}}
}
func systemClusterSuffix(name, namespace string) string {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	_ = encoder.Encode(name)
	_ = encoder.Encode(namespace)
	return fmt.Sprintf("%x", hash.Sum(nil))[:10]
}

// Compare effective component values against independently compiled intent.
// Helm defaults permit omitted/default-equivalent fields, but unknown keys and
// functional overrides remain visible and fail equality.
func systemValuesMatch(current, expected, defaults map[string]any) bool {
	if len(current) != len(expected) {
		return false
	}
	want := chartutil.CoalesceTables(runtime.DeepCopyJSON(expected), runtime.DeepCopyJSON(defaults))
	got := chartutil.CoalesceTables(runtime.DeepCopyJSON(current), runtime.DeepCopyJSON(defaults))
	normalizeSystemPaths(want)
	normalizeSystemPaths(got)
	return jsonEqual(want, got)
}
func normalizeSystemPaths(value map[string]any) {
	for key, item := range value {
		switch nested := item.(type) {
		case map[string]any:
			normalizeSystemPaths(nested)
		case string:
			switch key {
			case "cniBinDir", "cniConfDir", "openvSwitchBinDir", "openvSwitchRunDir", "openvSwitchSharedLibraryDir", "openvSwitchSharedLibrary64Dir":
				if strings.HasPrefix(nested, "/") {
					value[key] = path.Clean(nested)
				}
			}
		case []any:
			if key == "serviceAccounts" {
				sort.Slice(nested, func(i, j int) bool {
					a, _ := json.Marshal(nested[i])
					b, _ := json.Marshal(nested[j])
					return string(a) < string(b)
				})
			}
		}
	}
}

// Global RBAC roles legitimately include other registered clusters. Additional
// subjects are allowed only when independently reconstructed from a complete
// current DPUCluster list under the pinned producer's naming/filter rules.
func (o *validationObserver) systemRoleValuesForAccounts(ctx context.Context, management validationManagementReader, services []unstructured.Unstructured, role systemArtifactRole, expected map[string]any) (map[string]any, error) {
	if role.Management || !validationPerClusterRole(role) {
		return expected, nil
	}
	var current map[string]any
	for i := range services {
		service := &services[i]
		if service.GetLabels()[dpfComponentLabel] != role.Component {
			continue
		}
		inCluster, _, _ := unstructured.NestedBool(service.Object, "spec", "deployInCluster")
		if inCluster || service.GetLabels()[dpfDPUClusterNameLabel] != "" {
			continue
		}
		current, _, _ = unstructured.NestedMap(service.Object, "spec", "helmChart", "values")
		break
	}
	if current == nil {
		return expected, nil
	}
	actual, _, _ := unstructured.NestedSlice(current, role.Component, "rbac", "serviceAccounts")
	wanted, _, _ := unstructured.NestedSlice(expected, role.Component, "rbac", "serviceAccounts")
	if jsonEqual(actual, wanted) {
		return expected, nil
	}
	clusters, err := management.List(ctx, validationObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUCluster", "", ""), "")
	if err != nil {
		return nil, err
	}
	if len(clusters) > 256 {
		return nil, validationFailure(validation.Unverified, validation.ReasonIncomplete, "System RBAC controller source set exceeds the supported finite bound.")
	}
	accounts := []any{}
	seen := map[string]bool{}
	for i := range clusters {
		cluster := &clusters[i]
		if cluster.GetUID() == "" || cluster.GetName() == "" || cluster.GetNamespace() == "" || cluster.GetDeletionTimestamp() != nil {
			return nil, validationFailure(validation.Unverified, validation.ReasonIncomplete, "System RBAC cluster source is incomplete or deleting.")
		}
		if role.Component == "coredns" {
			kind, _, _ := unstructured.NestedString(cluster.Object, "spec", "type")
			vip, _, _ := unstructured.NestedString(cluster.Object, "spec", "clusterEndpoint", "keepalived", "vip")
			if kind != "kamaji" || vip == "" {
				continue
			}
		}
		name := role.Component + "-" + systemClusterSuffix(cluster.GetName(), cluster.GetNamespace())
		if seen[name] {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "System RBAC subject derivation is ambiguous.")
		}
		seen[name] = true
		accounts = append(accounts, map[string]any{"name": name, "namespace": o.desired.operatorNamespace})
	}
	result := runtime.DeepCopyJSON(expected)
	if err := unstructured.SetNestedSlice(result, accounts, role.Component, "rbac", "serviceAccounts"); err != nil {
		return nil, err
	}
	return result, nil
}

// The SCS template deliberately uses a legacy request name; the ServiceAccount
// and Secret retain the component name. Source: servicefunctionchainset-controller.yaml.
func systemCredentialRequestStem(role systemArtifactRole) string {
	if role.Component == "servicechainset-controller" {
		return "servicesetcontroller"
	}
	return role.Component
}
