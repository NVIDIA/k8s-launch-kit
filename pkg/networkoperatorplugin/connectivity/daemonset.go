// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

package connectivity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yaml "sigs.k8s.io/yaml"
)

// DaemonSetRef identifies an applied test DaemonSet so the orchestrator
// can later list its pods and (optionally) delete it.
type DaemonSetRef struct {
	Namespace  string
	Name       string
	Container  string // primary container name to ping from
	SourceFile string // file the DS was loaded from (for log breadcrumbs)
}

// LoadExampleDaemonSets reads every `*example*.yaml` manifest under
// manifestDir, decodes it as a DaemonSet, and returns one DaemonSetRef
// per file. Multi-doc YAMLs are walked; non-DaemonSet docs are skipped
// (the file pattern is descriptive, not enforced, so we tolerate a
// ConfigMap or two beside the DS).
func LoadExampleDaemonSets(manifestDir string) ([]*unstructured.Unstructured, []DaemonSetRef, error) {
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest dir %s: %w", manifestDir, err)
	}
	var objs []*unstructured.Unstructured
	var refs []DaemonSetRef
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name()), "example") {
			continue
		}
		full := filepath.Join(manifestDir, e.Name())
		content, rErr := os.ReadFile(full)
		if rErr != nil {
			return nil, nil, fmt.Errorf("read %s: %w", full, rErr)
		}
		for _, doc := range splitYAMLDocs(string(content)) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
				continue
			}
			if obj.GetKind() != "DaemonSet" || obj.GetName() == "" {
				continue
			}
			if apiv := obj.GetAPIVersion(); apiv != "" {
				if gv, err := schema.ParseGroupVersion(apiv); err == nil {
					obj.SetGroupVersionKind(gv.WithKind(obj.GetKind()))
				}
			}
			container := firstContainerName(obj)
			objs = append(objs, obj)
			refs = append(refs, DaemonSetRef{
				Namespace:  obj.GetNamespace(),
				Name:       obj.GetName(),
				Container:  container,
				SourceFile: e.Name(),
			})
		}
	}
	return objs, refs, nil
}

// ApplyDaemonSet performs a kubectl-style server-side apply of obj.
func ApplyDaemonSet(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("l8k-connectivity"), client.ForceOwnership)
}

// DeleteDaemonSet removes the DaemonSet by Namespace/Name, ignoring
// NotFound. Called from the orchestrator's cleanup unless --keep was
// requested.
func DeleteDaemonSet(ctx context.Context, c client.Client, ref DaemonSetRef) error {
	ds := &appsv1.DaemonSet{}
	ds.SetName(ref.Name)
	ds.SetNamespace(ref.Namespace)
	if err := c.Delete(ctx, ds); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete daemonset %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return nil
}

// RolloutStatus captures the DaemonSet's reconciled vs desired counts.
// Ready is the canonical signal: numberReady == desiredNumberScheduled
// && desiredNumberScheduled > 0. The remaining fields are exposed so
// the orchestrator can render a helpful "<R>/<D> pods ready" message
// while it waits.
type RolloutStatus struct {
	Desired   int32
	Available int32
	Ready     int32
	NotReady  int32 // desired - ready, never negative
}

// IsRolledOut reports whether the DaemonSet has finished its rollout
// AND scheduled a non-zero number of pods. The second clause matters:
// `desiredNumberScheduled == 0` happens when the affinity matched no
// nodes — silently passing that as "ready" would let an empty matrix
// claim success.
func (s RolloutStatus) IsRolledOut() bool {
	return s.Desired > 0 && s.Ready == s.Desired
}

// WaitForRollout polls the DaemonSet's status until it satisfies
// IsRolledOut or ctx is cancelled / pollTimeout elapses. The poll
// cadence is 3s — same as the deploy state machine.
//
// Returns the last observed status and a non-nil error on timeout,
// context cancel, or get-error.
func WaitForRollout(ctx context.Context, c client.Client, ref DaemonSetRef, pollTimeout time.Duration) (RolloutStatus, error) {
	deadline := time.Now().Add(pollTimeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var last RolloutStatus
	for {
		ds := &appsv1.DaemonSet{}
		err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, ds)
		if err == nil {
			last = RolloutStatus{
				Desired:   ds.Status.DesiredNumberScheduled,
				Available: ds.Status.NumberAvailable,
				Ready:     ds.Status.NumberReady,
			}
			last.NotReady = last.Desired - last.Ready
			if last.NotReady < 0 {
				last.NotReady = 0
			}
			if last.IsRolledOut() {
				return last, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return last, fmt.Errorf("get daemonset %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		if time.Now().After(deadline) {
			if last.Desired == 0 {
				return last, fmt.Errorf("daemonset %s/%s has desiredNumberScheduled=0 — its node affinity matched no nodes",
					ref.Namespace, ref.Name)
			}
			return last, fmt.Errorf("daemonset %s/%s did not roll out within %s: %d/%d pods ready",
				ref.Namespace, ref.Name, pollTimeout, last.Ready, last.Desired)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-ticker.C:
		}
	}
}

// ListPods returns the Running+Ready pods belonging to the given DS.
// Uses spec.selector.matchLabels for filtering, then walks
// OwnerReferences for membership in the right DS (label selectors can
// be borrowed by orphaned pods after a deletion-then-recreate cycle —
// the owner reference is the unambiguous link).
func ListPods(ctx context.Context, c client.Client, ref DaemonSetRef) ([]corev1.Pod, error) {
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, ds); err != nil {
		return nil, fmt.Errorf("get daemonset %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	var podList corev1.PodList
	listOpts := []client.ListOption{client.InNamespace(ref.Namespace)}
	if ds.Spec.Selector != nil && len(ds.Spec.Selector.MatchLabels) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(ds.Spec.Selector.MatchLabels))
	}
	if err := c.List(ctx, &podList, listOpts...); err != nil {
		return nil, fmt.Errorf("list pods for %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	out := make([]corev1.Pod, 0, len(podList.Items))
	for _, p := range podList.Items {
		if !ownedBy(p, ds.UID) {
			continue
		}
		if !podRunningReady(p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func ownedBy(p corev1.Pod, dsUID types.UID) bool {
	for _, ref := range p.OwnerReferences {
		if ref.UID == dsUID {
			return true
		}
	}
	return false
}

func podRunningReady(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// firstContainerName returns the first container name from a
// DaemonSet's pod template — that's the container the orchestrator
// execs `ping` in. Returns "" if the spec is malformed (caller
// surfaces a useful error).
func firstContainerName(ds *unstructured.Unstructured) string {
	containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return ""
	}
	c, ok := containers[0].(map[string]interface{})
	if !ok {
		return ""
	}
	name, _, _ := unstructured.NestedString(c, "name")
	return name
}

// splitYAMLDocs is a minimal local copy of the YAML doc splitter in
// pkg/networkoperatorplugin/deploy.go. Duplicated to keep the
// connectivity package self-contained (no upward dependency on the
// parent package).
func splitYAMLDocs(s string) []string {
	var docs []string
	var cur []string
	for _, ln := range strings.Split(s, "\n") {
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
