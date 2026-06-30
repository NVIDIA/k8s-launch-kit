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
	// + RDMA test execs + cleanup. 0 falls back to 5 minutes.
	Timeout time.Duration
	// Keep leaves the test DaemonSets running after the matrix
	// completes, for follow-up debugging. Default is to delete.
	Keep bool
}

// MatrixResult is the aggregate output of one connectivity run. It's
// what the validate CLI prints (or marshals to JSON) at the end.
type MatrixResult struct {
	// DaemonSets is one entry per applied example DS — typically
	// one per merged group.
	DaemonSets []DaemonSetReport
	// PingResults is the flat list of every executed RDMA test
	// (rping + ib_write_bw, same-rail + cross-rail).
	PingResults []PingResult
	// Skipped is non-nil when fewer than 2 schedulable test pods
	// were available across all DaemonSets. The matrix is treated
	// as a soft skip (exit 0, reason rendered).
	Skipped *MatrixSkip
	// Summary numbers for the JSON output.
	Summary MatrixSummary
}

// MatrixSummary is the rolled-up counts validate prints under "summary".
// Passed counts tests that exited cleanly; Failed counts tests that
// ran but exited non-zero or produced unparseable output.
type MatrixSummary struct {
	TotalTests int
	Passed     int
	Failed     int
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
//  5. Discovers the RDMA device backing each multus interface per pod.
//  6. Builds a test plan via Plan() — same-rail rping + ib_write_bw
//     across pods plus per-pair cross-rail canaries; soft-skip if <2
//     schedulable pods.
//  7. Runs rping then ib_write_bw stages sequentially per the
//     "spawn fresh server per test" lifecycle (concurrent runs would
//     fight over the listener port).
//  8. Deletes the DaemonSets unless opts.Keep.
//
// All UI output flows through `uiOutput` (caller passes
// ui.FromContext(ctx)). Logs go to controller-runtime's logr.
func RunMatrix(ctx context.Context, c client.Client, restConfig *rest.Config, uiOutput ui.Output, opts Options) (*MatrixResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
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

	// Parse multus annotations and build the TestPod list. Each
	// pod's RDMA-device-by-rail map is filled in from a single
	// in-pod shell exec that reads
	// /sys/class/net/<iface>/device/infiniband/ per multus iface,
	// so the rping + ib_write_bw stages can pass `-d <dev>`.
	testPods := make([]TestPod, 0, len(allPods))
	for _, p := range allPods {
		tp, ifaceByRail, err := buildTestPod(p.pod)
		if err != nil {
			uiOutput.Warning("Pod %s/%s: %v — excluded from matrix", p.pod.Namespace, p.pod.Name, err)
			log.Log.V(1).Info("buildTestPod failed", "pod", p.pod.Name, "error", err.Error())
			continue
		}
		if len(tp.RailOrder) == 0 {
			uiOutput.Warning("Pod %s/%s has no secondary network IPs — excluded from matrix", p.pod.Namespace, p.pod.Name)
			continue
		}
		tp.RDMADevsByRail = DiscoverRDMADevices(ctx, restConfig, p.pod.Namespace, p.pod.Name, p.ref.Container, ifaceByRail)
		log.Log.V(1).Info("RDMA device discovery",
			"pod", p.pod.Name, "rails", tp.RailOrder,
			"rdmaDevsByRail", tp.RDMADevsByRail)
		testPods = append(testPods, tp)
	}

	plan := Plan(testPods)
	if plan.Skip != nil {
		uiOutput.Warning("Matrix skipped: %s", plan.Skip.Reason)
		result.Skipped = plan.Skip
		return result, nil
	}

	totalSameRail := len(plan.RDMASameRail)
	totalCrossRail := len(plan.RDMACrossRail)
	uiOutput.Info("Plan: %d same-rail rping + %d cross-rail rping; %d same-rail ib_write_bw + %d cross-rail ib_write_bw",
		totalSameRail, totalCrossRail, len(plan.RDMABwSameRail), len(plan.RDMABwCrossRail))
	// Defensive: Plan() can return a zero-test plan without setting
	// Skip (e.g. ≥2 schedulable pods but Plan's same-rail loop emits
	// nothing). Two possible causes, both surface here:
	//
	//   (a) Every rail had at least one endpoint with no resolvable
	//       RDMA device. Common on macvlan/ipoib-rdma-shared profiles
	//       where the in-pod RDMA device probe returns empty.
	//   (b) No rail key was shared across pods. Can't happen with
	//       any current l8k-rendered example DaemonSet (single group
	//       per render → identical multus annotations), but mentioned
	//       so the diagnostic doesn't misattribute on a future
	//       multi-group setup.
	//
	// Without surfacing this, validate prints "Matrix complete: 0/0
	// passed" and exits success, hiding a real coverage gap.
	if totalSameRail+totalCrossRail+len(plan.RDMABwSameRail)+len(plan.RDMABwCrossRail) == 0 {
		uiOutput.Warning(
			"Matrix produced 0 tests despite %d schedulable pod(s) — either every rail had at least one endpoint with no resolvable RDMA device, or no rail key was shared across pods. "+
				"Most common cause: macvlan or IPoIB secondary networks, where the in-pod iface has no PCI-direct mlx5 sysfs entry. "+
				"Verify with: `kubectl exec <pod> -- ls /sys/class/net/<iface>/device/infiniband/` (empty on every pod = the fallback path needs the host master mapping).",
			len(testPods))
	}

	// Build a pod-name → container name map so the test runners
	// know which container to exec in (test DSes have a single
	// container today, but the orchestrator stays general).
	containerByPod := map[string]string{}
	namespaceByPod := map[string]string{}
	for _, p := range allPods {
		containerByPod[p.pod.Name] = p.ref.Container
		namespaceByPod[p.pod.Name] = p.pod.Namespace
	}

	// Two stages, each running sequentially per the "spawn fresh
	// server per test" decision: every test starts its own server,
	// runs the client, kills the server. Concurrent runs would
	// fight over the listener port.
	stages := []struct {
		label string
		tests []PingTest
		run   func(t PingTest) PingResult
	}{
		{
			label: "RDMA ping (rping)",
			tests: append(append([]PingTest{}, plan.RDMASameRail...), plan.RDMACrossRail...),
			run: func(t PingTest) PingResult {
				return RunRPing(ctx, restConfig, namespaceByPod[t.DstPod],
					t.DstPod, containerByPod[t.DstPod], // server side
					t.SrcPod, containerByPod[t.SrcPod], // client side
					t, 5)
			},
		},
		{
			label: "RDMA bandwidth (ib_write_bw)",
			tests: append(append([]PingTest{}, plan.RDMABwSameRail...), plan.RDMABwCrossRail...),
			run: func(t PingTest) PingResult {
				return RunIbWriteBw(ctx, restConfig, namespaceByPod[t.DstPod],
					t.DstPod, containerByPod[t.DstPod],
					t.SrcPod, containerByPod[t.SrcPod],
					t, 0)
			},
		},
	}
	for _, stage := range stages {
		if len(stage.tests) == 0 {
			continue
		}
		uiOutput.Info("Stage: %s — %d test(s)", stage.label, len(stage.tests))
		for _, t := range stage.tests {
			if namespaceByPod[t.SrcPod] == "" || namespaceByPod[t.DstPod] == "" {
				result.PingResults = append(result.PingResults, PingResult{
					Test: t,
					Err:  fmt.Errorf("no namespace lookup for pod pair (%s, %s)", t.SrcPod, t.DstPod),
				})
				continue
			}
			result.PingResults = append(result.PingResults, stage.run(t))
		}
	}

	// Summary.
	for _, r := range result.PingResults {
		result.Summary.TotalTests++
		if r.OK {
			result.Summary.Passed++
		} else {
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
// exec into.
type testPodWithDS struct {
	pod *corev1.Pod
	ref DaemonSetRef
}

// buildTestPod converts a corev1.Pod into a TestPod by parsing the
// multus `network-status` annotation. The first IPv4 per rail is used;
// IPv6 is currently ignored. Also returns the rail→multus interface
// name mapping (e.g. "rail-0" → "net1") so the caller can hand it to
// DiscoverRDMADevices to populate RDMADevsByRail for the RDMA stages.
func buildTestPod(p *corev1.Pod) (TestPod, map[string]string, error) {
	ann := p.Annotations[MultusAnnotation]
	nets, err := ParseNetworkStatus(ann)
	if err != nil {
		return TestPod{}, nil, err
	}
	tp := TestPod{
		Name:      p.Name,
		Namespace: p.Namespace,
		Node:      p.Spec.NodeName,
		IPsByRail: map[string]string{},
	}
	ifaceByRail := map[string]string{}
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
		if n.Interface != "" {
			ifaceByRail[rail] = n.Interface
		}
	}
	sort.Strings(tp.RailOrder)
	return tp, ifaceByRail, nil
}

// pickIPv4 returns the first IPv4 address from the multus IP list, or
// "" if none. IPv6 entries are ignored.
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
	uiOutput.Info("Matrix complete: %d/%d passed, %d failed",
		s.Passed, s.TotalTests, s.Failed)
	if s.Failed == 0 && s.TotalTests > 0 {
		uiOutput.Success("All %d RDMA test(s) passed", s.TotalTests)
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
		label := stageLabelOf(f.Test.Kind)
		if f.Err != nil {
			uiOutput.Error("%s %s→%s (%s): %v", label, src, dst, f.Test.Rail, f.Err)
		} else {
			uiOutput.Error("%s %s→%s (%s): test failed (no result row parsed or non-zero exit)",
				label, src, dst, f.Test.Rail)
		}
	}
}

// stageLabelOf returns the human-friendly label for a test kind used
// in failure-line prefixes ("rping", "ib_write_bw").
func stageLabelOf(k PingTestKind) string {
	if k.IsRDMABw() {
		return "ib_write_bw"
	}
	return "rping"
}

// failureLabel mirrors axisLabel from text_report.go — duplicated here
// so emitSummary doesn't have to expose the renderer's helper.
func failureLabel(node, pod string) string {
	if node != "" {
		return node
	}
	return pod
}
