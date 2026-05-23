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

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	yaml "sigs.k8s.io/yaml"
)

// deployPollInterval is the cadence of the state-machine deploy loop's
// polling between Validate calls. Matches the historical 3-second wait
// helper cadence so logs feel familiar.
const deployPollInterval = 3 * time.Second

// DeployProfile is a thin wrapper that preserves the existing plugin call
// shape (profile arg unused). Delegates to ApplyManifestsFromDir.
func (p *NetworkOperatorPlugin) DeployProfile(ctx context.Context, profile *profiles.Profile, kubeClient client.Client, manifestsDir string) error {
	_ = profile
	return ApplyManifestsFromDir(ctx, kubeClient, manifestsDir, false)
}

// ApplyManifestsFromDir reads Kubernetes manifests from manifestsDir and
// applies them to the cluster in four phases:
//
//  1. NicClusterPolicy — apply, then wait until the registry reports
//     success or error. NCP is upstream of every per-node component and
//     gates the rest of the deploy.
//  2. NicNodePolicy — apply each NNP and wait until it reports success
//     or error before moving on (preserves historical sequential
//     behavior since NNP-per-group manifests carry orthogonal node
//     selectors but downstream device plugins depend on each landing).
//  3. Remaining manifests — apply ALL in one pass without waiting.
//     Networks, IP pools, OVS configs, example DaemonSets and the
//     SR-IOV / Spectrum-X CRs all reconcile concurrently in the cluster,
//     so launching them back-to-back lets the wall-clock budget overlap.
//  4. Verify — poll the registry for every manifest applied in phase 3
//     until each reaches a terminal state (success/error) or the deploy
//     context is cancelled. Skipped in dry-run mode.
//
// Per-manifest deadlines have been removed: the only timeout that applies
// is whatever the caller threads into ctx (typically wrapped via
// context.WithTimeout for a maintenance-window-sized budget). When ctx
// has no deadline, the deploy waits indefinitely for reconciliation —
// which is the right default for SR-IOV configuration on large clusters,
// where a single policy can easily exceed any small per-manifest budget.
//
// When dryRun is true the apply path uses server-side dry-run
// (client.DryRunAll) so the cluster validates manifests without
// persisting them; phase 4 is skipped entirely.
func ApplyManifestsFromDir(ctx context.Context, kubeClient client.Client, manifestsDir string, dryRun bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	uiOutput := ui.FromContext(ctx)

	// Read & triage manifest docs from the deployment directory.
	nicDoc, nnpDocs, otherDocs, err := readManifestDir(manifestsDir)
	if err != nil {
		return err
	}

	// Pre-decode NCP / NNP so any YAML error surfaces before we start
	// touching the cluster. The "other" docs are decoded lazily inside
	// phase 3 so a Pod retry can re-use the same Unstructured object.
	var ncpObj *unstructured.Unstructured
	if len(nicDoc) != 0 {
		ncpObj, err = decodeUnstructured(nicDoc)
		if err != nil {
			return fmt.Errorf("decode NicClusterPolicy: %w", err)
		}
	}
	nnpObjs := make([]*unstructured.Unstructured, 0, len(nnpDocs))
	for i, b := range nnpDocs {
		obj, err := decodeUnstructured(b)
		if err != nil {
			return fmt.Errorf("decode NicNodePolicy manifest %d: %w", i+1, err)
		}
		nnpObjs = append(nnpObjs, obj)
	}

	registry := crstate.NewDefault()

	// Compute total phase count for the headers — skip empty phases so
	// users don't see "Phase 2/4" when there are no NNPs.
	phases := computePhases(ncpObj, nnpObjs, otherDocs, dryRun)
	uiOutput.Info("Deploying %d manifest(s): %d NicClusterPolicy, %d NicNodePolicy, %d other%s",
		phases.totalManifests, btoi(ncpObj != nil), len(nnpObjs), len(otherDocs), phases.dryRunSuffix)
	if deadline, ok := ctx.Deadline(); ok {
		uiOutput.Info("Deploy budget: %s (until %s)", time.Until(deadline).Round(time.Second), deadline.Format(time.RFC3339))
	} else {
		uiOutput.Info("Deploy budget: unbounded (no --deploy-timeout set)")
	}

	// Phase 1 — NicClusterPolicy.
	if ncpObj != nil {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — NicClusterPolicy", phases.next(), phases.total))
		if err := applyAndWait(ctx, kubeClient, registry, ncpObj, dryRun, manifestLabel(ncpObj, 0, 0)); err != nil {
			return err
		}
	}

	// Phase 2 — NicNodePolicies (sequential apply + wait per policy).
	if len(nnpObjs) > 0 {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — NicNodePolicies (%d)", phases.next(), phases.total, len(nnpObjs)))
		for i, obj := range nnpObjs {
			if err := applyAndWait(ctx, kubeClient, registry, obj, dryRun, manifestLabel(obj, i+1, len(nnpObjs))); err != nil {
				return err
			}
		}
	}

	// Phase 3 — apply remaining manifests, no per-manifest wait.
	appliedOthers := make([]*unstructured.Unstructured, 0, len(otherDocs))
	if len(otherDocs) > 0 {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — Applying %d additional manifest(s)", phases.next(), phases.total, len(otherDocs)))
		for i, b := range otherDocs {
			obj, err := decodeUnstructured(b)
			if err != nil {
				return fmt.Errorf("decode manifest: %w", err)
			}
			label := manifestLabel(obj, i+1, len(otherDocs))
			uiOutput.Info("Applying %s", label)
			log.Log.Info("Applying manifest",
				"kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(),
				"index", i+1, "total", len(otherDocs))
			if err := applyUnstructuredWithRetry(ctx, kubeClient, obj, dryRun); err != nil {
				uiOutput.Error("Failed to apply %s: %v", label, err)
				return err
			}
			appliedOthers = append(appliedOthers, obj)
		}
		uiOutput.Success("Applied %d additional manifest(s)", len(appliedOthers))
	}

	// Phase 4 — verify remaining manifests reach a terminal state.
	// Dry-run skips this; nothing was actually persisted.
	if dryRun {
		uiOutput.Info("Dry-run mode: skipping reconciliation verification")
		return nil
	}
	if len(appliedOthers) > 0 {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — Verifying %d manifest(s) reconcile", phases.next(), phases.total, len(appliedOthers)))
		for i, obj := range appliedOthers {
			if err := pollUntilTerminal(ctx, kubeClient, registry, obj, manifestLabel(obj, i+1, len(appliedOthers))); err != nil {
				return err
			}
		}
		uiOutput.Success("All %d additional manifest(s) reconciled", len(appliedOthers))
	}

	return nil
}

// readManifestDir reads every YAML doc under manifestsDir (non-recursive)
// and triages them into the three deploy buckets. Files matching the
// example-manifest naming pattern (see isExampleManifest) are skipped —
// they're test fixtures consumed by `l8k validate --connectivity` to
// stand up a temporary ping-matrix DaemonSet, not part of the actual
// network-operator surface that `l8k deploy` should apply.
func readManifestDir(manifestsDir string) (nicDoc []byte, nnpDocs [][]byte, otherDocs [][]byte, err error) {
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return nil, nil, nil, err
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
		if isExampleManifest(e.Name()) {
			log.Log.V(1).Info("Skipping example manifest at deploy time", "file", e.Name())
			continue
		}
		filePaths = append(filePaths, filepath.Join(manifestsDir, e.Name()))
	}
	sort.Strings(filePaths)

	for _, p := range filePaths {
		content, rErr := os.ReadFile(p)
		if rErr != nil {
			return nil, nil, nil, rErr
		}
		for _, doc := range splitYAMLDocuments(string(content)) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			b := []byte(doc)
			switch {
			case containsNicClusterPolicyKind(b):
				if len(nicDoc) != 0 {
					return nil, nil, nil, fmt.Errorf("multiple NicClusterPolicy manifests found; only one is allowed")
				}
				nicDoc = b
			case containsNicNodePolicyKind(b):
				nnpDocs = append(nnpDocs, b)
			default:
				otherDocs = append(otherDocs, b)
			}
		}
	}
	return nicDoc, nnpDocs, otherDocs, nil
}

// decodeUnstructured parses a YAML document into an Unstructured object
// and ensures its GroupVersionKind is set so server-side apply works.
func decodeUnstructured(doc []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(doc, obj); err != nil {
		return nil, err
	}
	if apiv, kind := obj.GetAPIVersion(), obj.GetKind(); apiv != "" && kind != "" {
		gv, err := schema.ParseGroupVersion(apiv)
		if err == nil {
			obj.SetGroupVersionKind(gv.WithKind(kind))
		}
	}
	return obj, nil
}

// applyAndWait applies obj and then polls until the registry reports a
// terminal state, re-applying once if the object goes missing mid-flight.
// Used for NCP and per-NNP in phases 1 and 2.
func applyAndWait(ctx context.Context, c client.Client, registry *crstate.Registry, obj *unstructured.Unstructured, dryRun bool, label string) error {
	uiOutput := ui.FromContext(ctx)

	progress := uiOutput.StartProgress(fmt.Sprintf("Applying %s", label))
	log.Log.Info("Applying manifest", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
	if err := applyUnstructured(ctx, c, obj, dryRun); err != nil {
		progress.Fail(fmt.Sprintf("Failed to apply %s: %v", label, err))
		return err
	}
	progress.Success(fmt.Sprintf("Applied %s", label))

	if dryRun {
		return nil
	}
	return pollUntilTerminal(ctx, c, registry, obj, label)
}

// pollUntilTerminal polls the registry's Validator for obj until it
// reports StateSuccess or StateError. not-deployed transitions trigger a
// single re-apply (object vanished between apply and poll); the only
// exit condition besides terminal state is ctx.Done().
//
// A spinner shows "Waiting for <label> to reconcile" so operators have
// a visible heartbeat. Reason transitions are emitted as discrete
// uiOutput.Info() lines — the StandardOutput now shares a mutex with
// the spinner goroutine and clears the spinner row before printing, so
// log lines land cleanly *above* the spinner instead of being glued
// onto the same row. The reason is deduped against the previous tick
// so a noisy 3-second polling loop only emits a fresh line when
// something actually changed.
func pollUntilTerminal(ctx context.Context, c client.Client, registry *crstate.Registry, obj *unstructured.Unstructured, label string) error {
	uiOutput := ui.FromContext(ctx)
	progress := uiOutput.StartProgress(fmt.Sprintf("Waiting for %s to reconcile", label))
	log.Log.Info("Waiting for manifest to reconcile", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())

	ticker := time.NewTicker(deployPollInterval)
	defer ticker.Stop()

	var lastReason string
	reportProgress := func(reason string) {
		if reason == lastReason || reason == "" {
			return
		}
		lastReason = reason
		// History line — scrollback record of every distinct
		// state transition. Always emitted.
		uiOutput.Info("  %s: %s", label, reason)
		log.Log.V(1).Info("manifest in-progress", "kind", obj.GetKind(), "name", obj.GetName(), "reason", reason)
		// Spinner-label update — TTY only. The spinner's paint
		// truncates the line to terminal width so long reasons
		// (e.g. "ready: 11/12; pending: state-OFED, …") render
		// on a single, in-place-redrawn line above which the
		// history scrolls. In non-TTY mode, progress.Update would
		// print a *duplicate* of the Info line above, so we skip
		// it here and rely on the history line alone.
		if uiOutput.IsTTY() {
			progress.Update(fmt.Sprintf("%s: %s", label, reason))
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			progress.Fail(fmt.Sprintf("Cancelled or timed out while waiting for %s", label))
			return err
		}

		res, err := registry.Validate(ctx, c, obj)
		if err != nil {
			log.Log.V(1).Info("Validate transient failure",
				"kind", obj.GetKind(), "name", obj.GetName(), "error", err.Error())
			reportProgress(fmt.Sprintf("transient validate error: %v", err))
		} else {
			switch res.State {
			case crstate.StateSuccess:
				progress.Success(fmt.Sprintf("%s reconciled (%s)", label, res.Reason))
				log.Log.Info("Manifest reconciled",
					"kind", obj.GetKind(), "name", obj.GetName(), "reason", res.Reason)
				return nil
			case crstate.StateError:
				progress.Fail(fmt.Sprintf("%s error: %s", label, res.Reason))
				log.Log.Error(nil, "Manifest reported error",
					"kind", obj.GetKind(), "name", obj.GetName(), "reason", res.Reason)
				return fmt.Errorf("%s/%s: %s", obj.GetKind(), obj.GetName(), res.Reason)
			case crstate.StateNotDeployed:
				// Object vanished between apply and poll (admission
				// webhook race, manual kubectl delete). Re-apply once
				// and continue polling.
				uiOutput.Warning("%s went missing mid-flight; re-applying", label)
				log.Log.Info("Manifest reported not-deployed; re-applying",
					"kind", obj.GetKind(), "name", obj.GetName())
				if err := applyUnstructured(ctx, c, obj, false); err != nil {
					progress.Fail(fmt.Sprintf("Re-apply of %s failed: %v", label, err))
					return err
				}
				reportProgress("re-applied after disappearance")
			case crstate.StateInProgress:
				reportProgress(res.Reason)
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail(fmt.Sprintf("Cancelled or timed out while waiting for %s", label))
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// applyUnstructuredWithRetry wraps applyUnstructured with the legacy
// Pod-specific retry path (up to 3 attempts, 30s apart). Non-Pod kinds
// surface the first apply error unmodified.
func applyUnstructuredWithRetry(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dryRun bool) error {
	uiOutput := ui.FromContext(ctx)
	err := applyUnstructured(ctx, c, obj, dryRun)
	if err == nil || !strings.EqualFold(obj.GetKind(), "Pod") {
		return err
	}
	const maxAttempts = 3
	for attempt := 2; attempt <= maxAttempts && err != nil; attempt++ {
		uiOutput.Warning("    Retrying %s/%s (%d/%d)...", obj.GetKind(), obj.GetName(), attempt, maxAttempts)
		log.Log.Info("Pod apply failed, retrying",
			"name", obj.GetName(), "attempt", attempt, "delay", "30s", "error", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
		err = applyUnstructured(ctx, c, obj, dryRun)
	}
	return err
}

// phaseCounter tracks the active phase number for human-readable
// "Phase X/Y" headers, skipping phases with no manifests.
type phaseCounter struct {
	total          int
	current        int
	totalManifests int
	dryRunSuffix   string
}

func (p *phaseCounter) next() int {
	p.current++
	return p.current
}

func computePhases(ncp *unstructured.Unstructured, nnps []*unstructured.Unstructured, others [][]byte, dryRun bool) *phaseCounter {
	pc := &phaseCounter{}
	if ncp != nil {
		pc.total++
		pc.totalManifests++
	}
	if len(nnps) > 0 {
		pc.total++
		pc.totalManifests += len(nnps)
	}
	if len(others) > 0 {
		pc.total++ // apply phase
		pc.totalManifests += len(others)
		if !dryRun {
			pc.total++ // verify phase
		}
	}
	if dryRun {
		pc.dryRunSuffix = " (dry-run)"
	}
	return pc
}

// manifestLabel formats a manifest identifier for log lines. When n > 0,
// "[i/n]" is appended so users can see batch progress at a glance.
func manifestLabel(obj *unstructured.Unstructured, i, n int) string {
	kindName := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	if ns := obj.GetNamespace(); ns != "" {
		kindName = fmt.Sprintf("%s/%s in %s", obj.GetKind(), obj.GetName(), ns)
	}
	if n > 0 {
		return fmt.Sprintf("%s [%d/%d]", kindName, i, n)
	}
	return kindName
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
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
