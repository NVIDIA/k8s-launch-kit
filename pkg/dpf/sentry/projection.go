// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sentry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func loadCharts(archives map[string][]byte) (map[string]*chart.Chart, error) {
	if len(archives) != len(Catalog()) {
		return nil, fmt.Errorf("sentry requires exactly four pinned local chart archives")
	}
	out := make(map[string]*chart.Chart, len(archives))
	for _, lock := range Catalog() {
		data, ok := archives[lock.Name]
		if !ok {
			return nil, fmt.Errorf("missing local chart archive %s", lock.Filename)
		}
		if len(data) > 20<<20 {
			return nil, fmt.Errorf("chart %s exceeds archive size limit", lock.Name)
		}
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) != lock.SHA256 {
			return nil, fmt.Errorf("chart archive %s SHA-256 mismatch", lock.Filename)
		}
		c, err := loader.LoadArchive(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("load chart archive %s: %w", lock.Filename, err)
		}
		if c.Metadata.Name != lock.Name || c.Metadata.Version != lock.Version {
			return nil, fmt.Errorf("chart archive %s has unexpected name or version", lock.Filename)
		}
		if len(c.Metadata.Dependencies) != 0 {
			return nil, fmt.Errorf("chart %s requires unsupported unresolved dependencies", lock.Name)
		}
		out[lock.Name] = c
	}
	return out, nil
}

// projectChart is deliberately offline. engine.Render has no Kubernetes client;
// the pinned policy controller's random certificate path is disabled by explicit
// webhook.enabled=false. CRDs and cluster-scoped RBAC remain validation-only.
func projectChart(c *chart.Chart, values map[string]interface{}, release, namespace, kubernetesVersion string) ([]*unstructured.Unstructured, error) {
	version, err := chartutil.ParseKubeVersion(kubernetesVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid captured Kubernetes API version: %w", err)
	}
	caps := &chartutil.Capabilities{KubeVersion: *version, APIVersions: projectionAPIVersions(), HelmVersion: chartutil.DefaultCapabilities.HelmVersion}
	renderValues, err := chartutil.ToRenderValues(c, values, chartutil.ReleaseOptions{Name: release, Namespace: namespace, Revision: 1, IsInstall: true}, caps)
	if err != nil {
		return nil, err
	}
	rendered, err := engine.Render(c, renderValues)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rendered))
	for key := range rendered {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []*unstructured.Unstructured
	for _, key := range keys {
		body := strings.TrimSpace(rendered[key])
		if body == "" || strings.HasSuffix(key, "NOTES.txt") || strings.HasPrefix(path.Base(key), "_") {
			continue
		}
		objects, err := decodeDocuments([]byte(body))
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", key, err)
		}
		for _, obj := range objects {
			if obj.GetAnnotations()["helm.sh/hook"] != "" {
				return nil, fmt.Errorf("chart %s has unsupported hook %s", c.Name(), obj.GetName())
			}
			switch obj.GetKind() {
			case "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition", "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
				if obj.GetNamespace() != "" {
					return nil, fmt.Errorf("cluster-scoped projection %s has namespace", obj.GetName())
				}
			default:
				if obj.GetNamespace() == "" {
					obj.SetNamespace(namespace)
				}
				if obj.GetNamespace() != namespace {
					return nil, fmt.Errorf("projection %s escapes configured namespace", obj.GetName())
				}
			}
			out = append(out, obj)
		}
	}
	for _, crd := range c.CRDObjects() {
		objects, err := decodeDocuments(crd.File.Data)
		if err != nil {
			return nil, fmt.Errorf("CRD %s: %w", crd.Name, err)
		}
		for _, obj := range objects {
			if obj.GetKind() != "CustomResourceDefinition" || obj.GetNamespace() != "" {
				return nil, fmt.Errorf("invalid chart CRD %s", crd.Name)
			}
			out = append(out, obj)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chart %s produced no validation projection", c.Name())
	}
	return out, nil
}

// The pinned four charts do not branch on Capabilities.APIVersions. Freeze the
// advertised set rather than inferring or probing an ambient cluster. A future
// chart version using .Has must review this explicit profile contract.
func projectionAPIVersions() chartutil.VersionSet {
	return chartutil.VersionSet{"v1", "apps/v1", "rbac.authorization.k8s.io/v1", "apiextensions.k8s.io/v1"}
}
