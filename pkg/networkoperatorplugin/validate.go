// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	yaml "sigs.k8s.io/yaml"
)

// ValidationResult captures the cluster state of one manifest discovered
// under the deployment-files directory.
type ValidationResult struct {
	Kind       string
	APIVersion string
	Name       string
	Namespace  string
	SourceFile string
	Found      bool
	Missing    bool
	Detail     string
}

// HelmReleaseInfo carries the subset of fields we care about from a
// Helm release Secret.
type HelmReleaseInfo struct {
	Name         string
	Namespace    string
	ChartName    string
	ChartVersion string
	AppVersion   string
	Revision     int
	Status       string
}

// VersionCheck captures the comparison between the Network Operator
// Helm release deployed in the cluster and the release line declared
// in the user's cluster-config.yaml.
type VersionCheck struct {
	// Skipped is true when the check could not be performed (no
	// selectedRelease in user-config, or no Helm release Secret found).
	Skipped       bool
	Reason        string
	SelectedRelease string // from user-config (e.g. "26.4")
	ExpectedVersion string // from embedded release catalog (e.g. "v26.4.0-beta.6")
	DeployedRelease *HelmReleaseInfo
	Match         bool
}

const (
	helmReleaseSecretType   = "helm.sh/release.v1"
	helmReleaseSecretPrefix = "sh.helm.release.v1."
)

// ValidateManifests reads YAML manifests from manifestDir (skipping example
// workloads) and looks up each object in the cluster. Returns a slice of
// per-object results.
func ValidateManifests(ctx context.Context, c client.Client, manifestDir string) ([]ValidationResult, error) {
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return nil, fmt.Errorf("read manifest dir %s: %w", manifestDir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if isExampleManifest(e.Name()) {
			log.Log.V(1).Info("Skipping example manifest", "file", e.Name())
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	var results []ValidationResult
	for _, name := range files {
		full := filepath.Join(manifestDir, name)
		content, err := os.ReadFile(full)
		if err != nil {
			return results, fmt.Errorf("read %s: %w", full, err)
		}
		for _, doc := range splitYAMLDocuments(string(content)) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
				log.Log.V(1).Info("Skipping unparseable manifest doc", "file", name, "error", err.Error())
				continue
			}
			if obj.GetKind() == "" || obj.GetName() == "" {
				continue
			}
			r := ValidationResult{
				Kind:       obj.GetKind(),
				APIVersion: obj.GetAPIVersion(),
				Name:       obj.GetName(),
				Namespace:  obj.GetNamespace(),
				SourceFile: name,
			}

			lookup := &unstructured.Unstructured{}
			gv, gvErr := schema.ParseGroupVersion(obj.GetAPIVersion())
			if gvErr != nil {
				r.Detail = fmt.Sprintf("invalid apiVersion %q: %v", obj.GetAPIVersion(), gvErr)
				results = append(results, r)
				continue
			}
			lookup.SetGroupVersionKind(gv.WithKind(obj.GetKind()))

			key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
			err := c.Get(ctx, key, lookup)
			switch {
			case err == nil:
				r.Found = true
			case apierrors.IsNotFound(err):
				r.Missing = true
				r.Detail = "not found in cluster"
			default:
				r.Detail = fmt.Sprintf("get error: %v", err)
			}
			results = append(results, r)
		}
	}
	return results, nil
}

// isExampleManifest treats files matching *example* (e.g.
// 50-example-daemonset.yaml) as test/demo workloads outside the
// network-operator surface to validate.
func isExampleManifest(name string) bool {
	return strings.Contains(strings.ToLower(name), "example")
}

// CheckHelmReleaseVersion compares the Network Operator Helm release
// installed in `namespace` against the version expected by `selectedRelease`
// looked up in the embedded catalog.
//
// When `selectedRelease` is empty, or no matching Helm release Secret is
// found, the returned VersionCheck has Skipped=true with a Reason.
func CheckHelmReleaseVersion(ctx context.Context, c client.Client, namespace, selectedRelease string) (*VersionCheck, error) {
	if selectedRelease == "" {
		return &VersionCheck{Skipped: true, Reason: "no networkOperator.selectedRelease in user-config"}, nil
	}

	rel, ok := LookupRelease(selectedRelease)
	if !ok {
		return &VersionCheck{
			Skipped:         true,
			Reason:          fmt.Sprintf("selectedRelease %q is not in the embedded catalog", selectedRelease),
			SelectedRelease: selectedRelease,
		}, nil
	}

	deployed, err := findNetworkOperatorHelmRelease(ctx, c, namespace)
	if err != nil {
		return &VersionCheck{
			Skipped:         true,
			Reason:          fmt.Sprintf("Helm release lookup failed: %v", err),
			SelectedRelease: selectedRelease,
			ExpectedVersion: rel.NetworkOperator.Version,
		}, nil
	}

	return &VersionCheck{
		SelectedRelease: selectedRelease,
		ExpectedVersion: rel.NetworkOperator.Version,
		DeployedRelease: deployed,
		Match:           deployed.AppVersion == rel.NetworkOperator.Version,
	}, nil
}

func findNetworkOperatorHelmRelease(ctx context.Context, c client.Client, namespace string) (*HelmReleaseInfo, error) {
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list secrets in %s: %w", namespace, err)
	}
	var matched []corev1.Secret
	for _, s := range secrets.Items {
		if string(s.Type) != helmReleaseSecretType {
			continue
		}
		if !strings.HasPrefix(s.Name, helmReleaseSecretPrefix) {
			continue
		}
		releaseName := helmReleaseNameFromSecret(s.Name)
		if !strings.Contains(strings.ToLower(releaseName), "network-operator") {
			continue
		}
		matched = append(matched, s)
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no Helm release Secret containing 'network-operator' in namespace %s", namespace)
	}
	sort.Slice(matched, func(i, j int) bool {
		return helmRevisionFromName(matched[i].Name) > helmRevisionFromName(matched[j].Name)
	})
	latest := matched[0]
	info, err := decodeHelmRelease(latest.Data["release"])
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", latest.Name, err)
	}
	return info, nil
}

// helmReleaseNameFromSecret extracts the release name from a Helm release
// secret name like "sh.helm.release.v1.<release>.v<N>".
func helmReleaseNameFromSecret(secretName string) string {
	rest := strings.TrimPrefix(secretName, helmReleaseSecretPrefix)
	dot := strings.LastIndex(rest, ".v")
	if dot < 0 {
		return rest
	}
	return rest[:dot]
}

func helmRevisionFromName(secretName string) int {
	rest := strings.TrimPrefix(secretName, helmReleaseSecretPrefix)
	dot := strings.LastIndex(rest, ".v")
	if dot < 0 {
		return 0
	}
	rev, err := strconv.Atoi(rest[dot+2:])
	if err != nil {
		return 0
	}
	return rev
}

// decodeHelmRelease parses the "release" key from a Helm release Secret.
// The value is base64-encoded gzip(JSON).
func decodeHelmRelease(b []byte) (*HelmReleaseInfo, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty release data")
	}
	decoded, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	plain, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	var rel struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status string `json:"status"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name       string `json:"name"`
				Version    string `json:"version"`
				AppVersion string `json:"appVersion"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(plain, &rel); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &HelmReleaseInfo{
		Name:         rel.Name,
		Namespace:    rel.Namespace,
		ChartName:    rel.Chart.Metadata.Name,
		ChartVersion: rel.Chart.Metadata.Version,
		AppVersion:   rel.Chart.Metadata.AppVersion,
		Revision:     rel.Version,
		Status:       rel.Info.Status,
	}, nil
}
