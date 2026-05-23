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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Options control the matrix run end-to-end. The l8k validate CLI
// fills these from the corresponding flags.
type Options struct {
	// ManifestDir is the deployment directory the example DaemonSet
	// manifests live under (validate uses the same dir it just
	// validated).
	ManifestDir string
	// Timeout caps the whole connectivity phase: apply + DS rollout
	// + ping execs + cleanup. 0 falls back to 5 minutes.
	Timeout time.Duration
	// PingCount is the number of ICMP echoes per src→dst pair
	// (`ping -c N`). 0 falls back to 3.
	PingCount int
	// Keep leaves the test DaemonSets running after the matrix
	// completes, for follow-up debugging. Default is to delete.
	Keep bool
	// MaxConcurrentPings caps the number of in-flight `ping` execs.
	// 0 falls back to 16 — enough to keep a 4-rail × 16-pod matrix
	// busy without overwhelming the apiserver.
	MaxConcurrentPings int
}

// MatrixResult is the aggregate output of one connectivity run. It's
// what the validate CLI prints (or marshals to JSON) at the end.
type MatrixResult struct {
	// DaemonSets is one entry per applied example DS — typically
	// one per merged group.
	DaemonSets []DaemonSetReport
	// PingResults is the flat list of every executed ping test.
	// SameRail and CrossRail are derived views (filtered by Kind)
	// for the report layer.
	PingResults []PingResult
	// Skipped is non-nil when fewer than 2 schedulable test pods
	// were available across all DaemonSets. The matrix is treated
	// as a soft skip (exit 0, reason rendered).
	Skipped *MatrixSkip
	// Summary numbers for the JSON output.
	Summary MatrixSummary
}

// MatrixSummary is the rolled-up counts validate prints under "summary".
type MatrixSummary struct {
	TotalTests int
	Passed     int
	Failed     int
	ExecErrors int
}

// DaemonSetReport captures one DS's rollout state and the test pods it
// contributed to the matrix.
type DaemonSetReport struct {
	Ref      DaemonSetRef
	Rollout  RolloutStatus
	PodCount int
}

// RunMatrix is the orchestrator. It:
//
//  1. Loads every example DaemonSet manifest from opts.ManifestDir.
//  2. Applies each via server-side apply.
//  3. Waits for the DS rollout (desired > 0 AND ready == desired).
//  4. Lists Running+Ready pods, parses each pod's multus annotation,
//     filters to secondary networks, picks the first IPv4 per rail.
//  5. Builds a test plan via Plan() — same-rail across pods + per-pod
//     cross-rail canary; soft-skip if <2 schedulable pods.
//  6. Runs every ping in parallel up to opts.MaxConcurrentPings.
//  7. Deletes the DaemonSets unless opts.Keep.
//
// All UI output flows through `uiOutput` (caller passes
// ui.FromContext(ctx)). Logs go to controller-runtime's logr.
func RunMatrix(ctx context.Context, c client.Client, restConfig *rest.Config, uiOutput ui.Output, opts Options) (*MatrixResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.PingCount <= 0 {
		opts.PingCount = 3
	}
	if opts.MaxConcurrentPings <= 0 {
		opts.MaxConcurrentPings = 16
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	uiOutput.Section("Connectivity matrix")
	uiOutput.Info("Loading example DaemonSet manifests from %s", opts.ManifestDir)

	objs, refs, err := LoadExampleDaemonSets(opts.ManifestDir)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return &MatrixResult{
			Skipped: &MatrixSkip{Reason: "no example DaemonSet manifests found under " + opts.ManifestDir},
		}, nil
	}

	result := &MatrixResult{}
	// Cleanup is registered up-front so a partial failure still
	// removes anything we applied (unless --keep).
	defer func() {
		if opts.Keep {
			uiOutput.Info("--keep set: leaving %d test DaemonSet(s) running", len(refs))
			return
		}
		// Use a fresh context for cleanup; the main ctx may have
		// already been cancelled / timed out.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, ref := range refs {
			if err := DeleteDaemonSet(cleanupCtx, c, ref); err != nil {
				uiOutput.Warning("cleanup: %v", err)
				log.Log.V(1).Info("cleanup error", "error", err.Error())
			}
		}
	}()

	for i, obj := range objs {
		ref := refs[i]
		uiOutput.Info("Applying %s/%s (from %s)", ref.Namespace, ref.Name, ref.SourceFile)
		if err := ApplyDaemonSet(ctx, c, obj); err != nil {
			return result, fmt.Errorf("apply daemonset %s/%s: %w", ref.Namespace, ref.Name, err)
		}
	}

	// Wait for each DS to roll out, then enumerate its pods.
	allPods := make([]testPodWithDS, 0)
	for _, ref := range refs {
		uiOutput.Info("Waiting for DaemonSet %s/%s to roll out", ref.Namespace, ref.Name)
		rollout, err := WaitForRollout(ctx, c, ref, opts.Timeout)
		if err != nil {
			return result, err
		}
		uiOutput.Success("DaemonSet %s/%s ready (%d/%d pods)", ref.Namespace, ref.Name, rollout.Ready, rollout.Desired)
		pods, err := ListPods(ctx, c, ref)
		if err != nil {
			return result, err
		}
		// Sanity check that every desired pod is also in the
		// Running+Ready list. ListPods filters by phase+condition,
		// so a mismatch here means we're racing — happen on rare
		// occasions, surface as warning.
		if int32(len(pods)) != rollout.Desired {
			uiOutput.Warning("DaemonSet %s/%s reports %d ready but only %d pods are Running+Ready — racing reconciliation?",
				ref.Namespace, ref.Name, rollout.Desired, len(pods))
		}
		result.DaemonSets = append(result.DaemonSets, DaemonSetReport{
			Ref:      ref,
			Rollout:  rollout,
			PodCount: len(pods),
		})
		for i := range pods {
			allPods = append(allPods, testPodWithDS{pod: &pods[i], ref: ref})
		}
	}

	// Parse multus annotations and build the TestPod list.
	testPods := make([]TestPod, 0, len(allPods))
	for _, p := range allPods {
		tp, err := buildTestPod(p.pod)
		if err != nil {
			uiOutput.Warning("Pod %s/%s: %v — excluded from matrix", p.pod.Namespace, p.pod.Name, err)
			log.Log.V(1).Info("buildTestPod failed", "pod", p.pod.Name, "error", err.Error())
			continue
		}
		if len(tp.RailOrder) == 0 {
			uiOutput.Warning("Pod %s/%s has no secondary network IPs — excluded from matrix", p.pod.Namespace, p.pod.Name)
			continue
		}
		testPods = append(testPods, tp)
	}

	plan := Plan(testPods)
	if plan.Skip != nil {
		uiOutput.Warning("Matrix skipped: %s", plan.Skip.Reason)
		result.Skipped = plan.Skip
		return result, nil
	}

	uiOutput.Info("Plan: %d same-rail tests, %d cross-rail canary tests", len(plan.SameRail), len(plan.CrossRail))

	// Build a pod-name → container name map so RunPing knows which
	// container to exec in (test DSes have a single container today,
	// but the orchestrator stays general).
	containerByPod := map[string]string{}
	namespaceByPod := map[string]string{}
	for _, p := range allPods {
		// We use the DS's resolved container name for every pod it
		// owns — pods of the same DS share the template.
		containerByPod[p.pod.Name] = p.ref.Container
		namespaceByPod[p.pod.Name] = p.pod.Namespace
	}

	tests := append([]PingTest{}, plan.SameRail...)
	tests = append(tests, plan.CrossRail...)
	result.PingResults = make([]PingResult, len(tests))

	// Bounded concurrency: a semaphore of size MaxConcurrentPings.
	sem := make(chan struct{}, opts.MaxConcurrentPings)
	var wg sync.WaitGroup
	for i := range tests {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, t PingTest) {
			defer wg.Done()
			defer func() { <-sem }()
			ns := namespaceByPod[t.SrcPod]
			cn := containerByPod[t.SrcPod]
			if ns == "" || cn == "" {
				result.PingResults[idx] = PingResult{
					Test: t,
					Err:  fmt.Errorf("no namespace/container lookup for src pod %q", t.SrcPod),
				}
				return
			}
			result.PingResults[idx] = RunPing(ctx, restConfig, ns, cn, t, opts.PingCount)
		}(i, tests[i])
	}
	wg.Wait()

	// Summary.
	for _, r := range result.PingResults {
		result.Summary.TotalTests++
		switch {
		case r.OK:
			result.Summary.Passed++
		case r.Err != nil && r.PacketLoss < 0:
			result.Summary.ExecErrors++
		default:
			result.Summary.Failed++
		}
	}
	// Render the per-rail grid before the final summary line so
	// operators see the full picture, then the rolled-up counts at
	// the bottom.
	RenderMatrixText(uiOutput, result)
	emitSummary(uiOutput, result)
	return result, nil
}

// testPodWithDS pairs a Running+Ready pod with the DaemonSetRef that
// owns it so the orchestrator can later find the right container to
// `ping` from.
type testPodWithDS struct {
	pod *corev1.Pod
	ref DaemonSetRef
}

// buildTestPod converts a corev1.Pod into a TestPod by parsing the
// multus `network-status` annotation. The first IPv4 per rail is used;
// IPv6 is currently ignored.
func buildTestPod(p *corev1.Pod) (TestPod, error) {
	ann := p.Annotations[MultusAnnotation]
	nets, err := ParseNetworkStatus(ann)
	if err != nil {
		return TestPod{}, err
	}
	tp := TestPod{
		Name:      p.Name,
		Namespace: p.Namespace,
		Node:      p.Spec.NodeName,
		IPsByRail: map[string]string{},
	}
	for _, n := range SecondaryNetworks(nets) {
		ip := pickIPv4(n.IPs)
		if ip == "" {
			continue
		}
		rail := railKey(n.Name)
		if rail == "" {
			rail = n.Name
		}
		// Avoid overwriting an earlier multus entry for the same
		// rail name — multus reports duplicates only when a pod
		// attaches twice; keep the first.
		if _, exists := tp.IPsByRail[rail]; exists {
			continue
		}
		tp.IPsByRail[rail] = ip
		tp.RailOrder = append(tp.RailOrder, rail)
	}
	sort.Strings(tp.RailOrder)
	return tp, nil
}

// pickIPv4 returns the first IPv4 address from the multus IP list, or
// "" if none. IPv6 entries are ignored (matrix uses ping, not ping6).
func pickIPv4(ips []string) string {
	for _, ip := range ips {
		if strings.Count(ip, ".") == 3 && !strings.Contains(ip, ":") {
			return ip
		}
	}
	return ""
}

// railKey strips the namespace prefix that multus prepends to the
// NetworkAttachmentDefinition reference. "ns/rail-0" → "rail-0";
// "rail-0" → "rail-0".
func railKey(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func emitSummary(uiOutput ui.Output, result *MatrixResult) {
	s := result.Summary
	uiOutput.Info("Matrix complete: %d/%d passed, %d failed, %d exec error(s)",
		s.Passed, s.TotalTests, s.Failed, s.ExecErrors)
	if s.Failed == 0 && s.ExecErrors == 0 && s.TotalTests > 0 {
		uiOutput.Success("All %d ping test(s) passed", s.TotalTests)
		return
	}
	// Render up to 5 failures so operators see what broke without
	// being buried under a massive matrix; the full list is in the
	// JSON / report.
	const maxFailures = 5
	var failures []PingResult
	for _, r := range result.PingResults {
		if !r.OK {
			failures = append(failures, r)
		}
	}
	for i, f := range failures {
		if i >= maxFailures {
			uiOutput.Warning("(%d more failure(s) omitted; see report for full list)", len(failures)-maxFailures)
			break
		}
		src := failureLabel(f.Test.SrcNode, f.Test.SrcPod)
		dst := failureLabel(f.Test.DstNode, f.Test.DstPod)
		switch {
		case f.Err != nil:
			uiOutput.Error("ping %s→%s (%s): %v", src, dst, f.Test.Rail, f.Err)
		default:
			uiOutput.Error("ping %s→%s (%s): %d%% packet loss", src, dst, f.Test.Rail, f.PacketLoss)
		}
	}
}

// failureLabel mirrors axisLabel from text_report.go — duplicated here
// so emitSummary doesn't have to expose the renderer's helper.
func failureLabel(node, pod string) string {
	if node != "" {
		return node
	}
	return pod
}
