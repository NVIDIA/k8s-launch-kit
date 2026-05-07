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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	yaml "sigs.k8s.io/yaml"
)

// Apply reads Kubernetes manifests from dirPath and applies them to the cluster.
// If a NicClusterPolicy is present, it is applied first and the function waits
// for it to become ready before applying the remaining manifests.
//
// Thin wrapper that preserves the existing call-site shape (profile arg
// unused). Delegates to ApplyManifestsFromDir.
func (p *NetworkOperatorPlugin) DeployProfile(ctx context.Context, profile *profiles.Profile, kubeClient client.Client, manifestsDir string) error {
	_ = profile
	return ApplyManifestsFromDir(ctx, kubeClient, manifestsDir, false)
}

// ApplyManifestsFromDir reads Kubernetes manifests from manifestsDir and
// applies them to the cluster in dependency order: NicClusterPolicy first
// (waits for ready), then per-group NicNodePolicy (waits for each), then
// remaining manifests. When dryRun is true the apply uses server-side
// dry-run (client.DryRunAll) so the cluster validates manifests without
// persisting them.
func ApplyManifestsFromDir(ctx context.Context, kubeClient client.Client, manifestsDir string, dryRun bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	uiOutput := ui.FromContext(ctx)

	// List files in directory (non-recursive) and sort
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return err
	}
	filePaths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		filePaths = append(filePaths, filepath.Join(manifestsDir, e.Name()))
	}
	sort.Strings(filePaths)

	// Collect manifests from all files (support multi-doc YAML using '---')
	var nicDoc []byte
	var nnpDocs [][]byte
	var otherDocs [][]byte
	for _, p := range filePaths {
		content, rErr := os.ReadFile(p)
		if rErr != nil {
			return rErr
		}
		docs := splitYAMLDocuments(string(content))
		for _, doc := range docs {
			if len(strings.TrimSpace(doc)) == 0 {
				continue
			}
			b := []byte(doc)
			if containsNicClusterPolicyKind(b) {
				if len(nicDoc) != 0 {
					return fmt.Errorf("multiple NicClusterPolicy manifests found; only one is allowed")
				}
				nicDoc = b
			} else if containsNicNodePolicyKind(b) {
				nnpDocs = append(nnpDocs, b)
			} else {
				otherDocs = append(otherDocs, b)
			}
		}
	}

	// Apply NicClusterPolicy first if present
	if len(nicDoc) != 0 {
		progress := uiOutput.StartProgress("Applying NIC Cluster Policy")
		log.Log.Info("Applying NicClusterPolicy for selected profile")
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(nicDoc, obj); err != nil {
			progress.Fail("Failed to decode manifest")
			return fmt.Errorf("failed to decode NicClusterPolicy: %w", err)
		}
		// Ensure GVK set for server-side apply
		apiv, kind := obj.GetAPIVersion(), obj.GetKind()
		if apiv != "" && kind != "" {
			gv, err := schema.ParseGroupVersion(apiv)
			if err == nil {
				obj.SetGroupVersionKind(gv.WithKind(kind))
			}
		}
		if err := applyUnstructured(ctx, kubeClient, obj, dryRun); err != nil {
			progress.Fail("Failed to apply policy")
			return err
		}

		progress.Success("NIC Cluster Policy applied")
		log.Log.Info("Waiting for NicClusterPolicy to be ready")
		if err := WaitNicClusterPolicyReady(ctx, kubeClient, obj.GetName()); err != nil {
			return err
		}
	}

	// Apply NicNodePolicies after NicClusterPolicy, wait for each to become ready
	if len(nnpDocs) > 0 {
		uiOutput.Info("Applying %d NIC Node Policy manifest(s)", len(nnpDocs))
		log.Log.Info("Applying NicNodePolicy manifests", "count", len(nnpDocs))
	}
	for i, b := range nnpDocs {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(b, obj); err != nil {
			uiOutput.Error("Failed to decode NicNodePolicy manifest: %v", err)
			return fmt.Errorf("failed to decode NicNodePolicy manifest: %w", err)
		}
		apiv, kind := obj.GetAPIVersion(), obj.GetKind()
		if apiv != "" && kind != "" {
			gv, err := schema.ParseGroupVersion(apiv)
			if err == nil {
				obj.SetGroupVersionKind(gv.WithKind(kind))
			}
		}

		progress := uiOutput.StartProgress(fmt.Sprintf("Applying NIC Node Policy %q (%d/%d)", obj.GetName(), i+1, len(nnpDocs)))
		log.Log.Info("Applying NicNodePolicy", "name", obj.GetName())
		if err := applyUnstructured(ctx, kubeClient, obj, dryRun); err != nil {
			progress.Fail(fmt.Sprintf("Failed to apply NicNodePolicy %q", obj.GetName()))
			return err
		}
		progress.Success(fmt.Sprintf("NIC Node Policy %q applied", obj.GetName()))

		log.Log.Info("Waiting for NicNodePolicy to be ready", "name", obj.GetName())
		if err := WaitNicNodePolicyReady(ctx, kubeClient, obj.GetName()); err != nil {
			return err
		}
	}

	// Apply remaining manifests
	if len(otherDocs) > 0 {
		uiOutput.Info("Applying %d additional manifest(s)", len(otherDocs))
	}
	log.Log.Info("Applying remaining profile manifests", "count", len(otherDocs))
	for i, b := range otherDocs {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(b, obj); err != nil {
			uiOutput.Error("Failed to decode manifest: %v", err)
			return fmt.Errorf("failed to decode manifest: %w", err)
		}
		// Ensure GVK set for server-side apply
		apiv, kind := obj.GetAPIVersion(), obj.GetKind()
		if apiv != "" && kind != "" {
			gv, err := schema.ParseGroupVersion(apiv)
			if err == nil {
				obj.SetGroupVersionKind(gv.WithKind(kind))
			}
		}
		uiOutput.Info("  [%d/%d] Applying %s/%s", i+1, len(otherDocs), obj.GetKind(), obj.GetName())
		log.Log.Info("Applying object", "kind", obj.GetKind(), "name", obj.GetName(), "version", obj.GetAPIVersion())

		// Apply with retry for Pod kind
		applyErr := applyUnstructured(ctx, kubeClient, obj, dryRun)
		if applyErr != nil && strings.EqualFold(obj.GetKind(), "Pod") {
			const maxAttempts = 3
			for attempt := 2; attempt <= maxAttempts && applyErr != nil; attempt++ {
				uiOutput.Warning("    Retrying (%d/%d)...", attempt, maxAttempts)
				log.Log.Info("Pod apply failed, retrying", "name", obj.GetName(), "attempt", attempt, "delay", "30s", "error", applyErr.Error())
				time.Sleep(30 * time.Second)
				applyErr = applyUnstructured(ctx, kubeClient, obj, dryRun)
			}
		}
		if applyErr != nil {
			uiOutput.Error("    Failed: %v", applyErr)
			return applyErr
		}
	}

	return nil
}

func containsNicClusterPolicyKind(b []byte) bool {
	return sniffKind(b) == "NicClusterPolicy"
}

func containsNicNodePolicyKind(b []byte) bool {
	return sniffKind(b) == "NicNodePolicy"
}

// sniffKind extracts the Kind field from a YAML document without full parsing.
func sniffKind(b []byte) string {
	type metaOnly struct {
		Kind string `yaml:"kind"`
	}
	var mo metaOnly
	if err := yaml.Unmarshal(b, &mo); err != nil {
		return ""
	}
	return mo.Kind
}

func applyUnstructured(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dryRun bool) error {
	// kubectl-style server-side apply. dryRun appends client.DryRunAll so the
	// cluster validates the object without persisting it.
	opts := []client.PatchOption{client.FieldOwner("l8k"), client.ForceOwnership}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	return c.Patch(ctx, obj, client.Apply, opts...)
}

// splitYAMLDocuments splits a YAML stream by lines that start with '---' (doc separators)
func splitYAMLDocuments(s string) []string {
	var docs []string
	var cur []string
	lines := strings.Split(s, "\n")
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "---") {
			if len(cur) > 0 {
				docs = append(docs, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		docs = append(docs, strings.Join(cur, "\n"))
	}
	return docs
}
