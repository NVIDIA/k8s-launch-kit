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

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Check string

const (
	CheckICMP            Check = "icmp"
	CheckRPing           Check = "rping"
	CheckIBWriteBW       Check = "ib_write_bw"
	CheckGPUDirectDMABuf Check = "gpudirect_dmabuf"
)

// Options control the matrix run end-to-end. The l8k validate CLI
// fills these from the corresponding flags.
type Options struct {
	// ManifestDir is the deployment directory the example DaemonSet
	// manifests live under (validate uses the same dir it just
	// validated).
	ManifestDir string
	// Timeout caps connectivity workload setup and test execution when
	// positive. Zero selects an automatic budget derived from the generated
	// matrix plan. Cleanup uses its own short context in either mode.
	Timeout time.Duration
	// Keep leaves the test DaemonSets running after the matrix
	// completes, for follow-up debugging. Default is to delete.
	Keep bool
	// Mode controls rail-matrix coverage and cross-rail gating.
	Mode Mode
	// Routing is the profile routing mode used to decide strict
	// cross-rail expectations.
	Routing string
	// Checks selects which connectivity test families to run. Nil means
	// the default set (icmp + rping + ib_write_bw); an empty slice means skip all
	// connectivity tests without applying the example DaemonSets.
	Checks []Check
	// RPingIterations maps to `rping -C`. 0 defaults to 5, matching the
	// historical hardcoded behavior.
	RPingIterations int
	// IBWriteSize maps to `ib_write_bw -s`. 0 defaults to 65536, matching
	// the historical hardcoded behavior.
	IBWriteSize int
	// IBWriteMinBandwidthGbps is the minimum peak bandwidth that must be
	// observed for an ib_write_bw test to pass. 0 disables threshold gating.
	IBWriteMinBandwidthGbps float64
	// ClusterConfig supplies the node/rail to connected-GPU topology used by
	// the DMA-BUF test. GPUDirect is selected by including
	// CheckGPUDirectDMABuf in Checks.
	ClusterConfig []config.ClusterConfig
}

// MatrixResult is the aggregate output of one connectivity run. It's
// what the validate CLI prints (or marshals to JSON) at the end.
type MatrixResult struct {
	// DaemonSets is one entry per applied example DS — typically
	// one per merged group.
	DaemonSets []DaemonSetReport
	// PingResults is the flat list of every executed connectivity test
	// (icmp + rping + host-memory ib_write_bw + GPUDirect DMA-BUF,
	// same-rail + cross-rail).
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
//  6. Builds a test plan via PlanWithOptions().
//  7. Runs the configured connectivity stages sequentially. RDMA stages use the
//     "spawn fresh server per test" lifecycle (concurrent runs would fight over
//     the listener port).
//  8. Deletes the DaemonSets unless opts.Keep.
//
// All UI output flows through `uiOutput` (caller passes
// ui.FromContext(ctx)). Logs go to controller-runtime's logr.
func RunMatrix(ctx context.Context, c client.Client, restConfig *rest.Config, uiOutput ui.Output, opts Options) (*MatrixResult, error) {
	if opts.Timeout < 0 {
		return nil, fmt.Errorf("connectivity timeout must be greater than or equal to zero")
	}
	opts.Checks = normalizeChecks(opts.Checks)
	if opts.Mode == "" {
		opts.Mode = ModeStrict
	}
	if opts.RPingIterations <= 0 {
		opts.RPingIterations = 5
	}
	if opts.IBWriteSize <= 0 {
		opts.IBWriteSize = 65536
	}

	uiOutput.Section("Connectivity matrix")
	if len(opts.Checks) == 0 {
		return &MatrixResult{
			Skipped: &MatrixSkip{Reason: "all connectivity checks are disabled"},
		}, nil
	}

	automaticTimeout := opts.Timeout == 0
	runCtx := ctx
	runCancel := func() {}
	setupTimeout := automaticSetupTimeout
	if !automaticTimeout {
		runCtx, runCancel = context.WithTimeout(ctx, opts.Timeout)
		setupTimeout = opts.Timeout
		uiOutput.Info("Connectivity timeout set by user: %s", opts.Timeout)
	}
	defer runCancel()

	setupCtx := runCtx
	setupCancel := func() {}
	if automaticTimeout {
		setupCtx, setupCancel = context.WithTimeout(runCtx, setupTimeout)
	}
	defer setupCancel()

	uiOutput.Info("Loading example DaemonSet manifests from %s", opts.ManifestDir)

	hasICMP := checksContain(opts.Checks, CheckICMP)
	hasRDMA := checksContain(opts.Checks, CheckRPing) || checksContain(opts.Checks, CheckIBWriteBW) || checksContain(opts.Checks, CheckGPUDirectDMABuf)
	objs, refs, err := LoadExampleDaemonSets(opts.ManifestDir)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return &MatrixResult{
			Skipped: &MatrixSkip{Reason: "no example DaemonSet manifests found under " + opts.ManifestDir},
		}, nil
	}
	if hasICMP || hasRDMA {
		for _, ref := range refs {
			if ref.ICMPContainer == "" {
				return nil, fmt.Errorf("example DaemonSet %s/%s from %s does not declare the %q ICMP/route helper container",
					ref.Namespace, ref.Name, ref.SourceFile, icmpTestContainerName)
			}
		}
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
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), matrixCleanupTimeout)
		defer cleanupCancel()
		for _, ref := range refs {
			if err := DeleteDaemonSet(cleanupCtx, c, ref); err != nil {
				uiOutput.Warning("cleanup: %v", err)
				log.Log.V(1).Info("cleanup error", "error", err.Error())
			}
		}
	}()

	applyAndCollectPods := func(runCtx context.Context, objs []*unstructured.Unstructured, refs []DaemonSetRef) ([]testPodWithDS, error) {
		for i, obj := range objs {
			ref := refs[i]
			uiOutput.Info("Applying %s/%s (from %s)", ref.Namespace, ref.Name, ref.SourceFile)
			if err := ApplyDaemonSet(runCtx, c, obj); err != nil {
				return nil, fmt.Errorf("apply daemonset %s/%s: %w", ref.Namespace, ref.Name, err)
			}
		}

		// Wait for each DS to roll out, then enumerate its pods.
		out := make([]testPodWithDS, 0)
		for _, ref := range refs {
			uiOutput.Info("Waiting for DaemonSet %s/%s to roll out", ref.Namespace, ref.Name)
			rollout, err := WaitForRollout(runCtx, c, ref, setupTimeout)
			if err != nil {
				return out, err
			}
			uiOutput.Success("DaemonSet %s/%s ready (%d/%d pods)", ref.Namespace, ref.Name, rollout.Ready, rollout.Desired)
			pods, err := ListPods(runCtx, c, ref)
			if err != nil {
				return out, err
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
				out = append(out, testPodWithDS{pod: &pods[i], ref: ref})
			}
		}
		return out, nil
	}

	buildMatrixPods := func(runCtx context.Context, allPods []testPodWithDS, discoverRDMA bool) []TestPod {
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
			if discoverRDMA {
				tp.RDMADevsByRail = DiscoverRDMADevices(runCtx, restConfig, p.pod.Namespace, p.pod.Name, p.ref.RDMAContainer, ifaceByRail)
				log.Log.V(1).Info("RDMA device discovery",
					"pod", p.pod.Name, "rails", tp.RailOrder,
					"rdmaDevsByRail", tp.RDMADevsByRail)
			}
			tp.InterfacesByRail = ifaceByRail
			PopulateGPUTopology(&tp, opts.ClusterConfig)
			testPods = append(testPods, tp)
		}
		return testPods
	}

	containerMaps := func(allPods []testPodWithDS) (map[string]string, map[string]string, map[string]string) {
		rdmaContainerByPod := map[string]string{}
		icmpContainerByPod := map[string]string{}
		namespaceByPod := map[string]string{}
		for _, p := range allPods {
			rdmaContainerByPod[p.pod.Name] = p.ref.RDMAContainer
			icmpContainerByPod[p.pod.Name] = p.ref.ICMPContainer
			namespaceByPod[p.pod.Name] = p.pod.Namespace
		}
		return rdmaContainerByPod, icmpContainerByPod, namespaceByPod
	}

	allPods, err := applyAndCollectPods(setupCtx, objs, refs)
	if err != nil {
		return result, err
	}
	testPods := buildMatrixPods(setupCtx, allPods, hasRDMA)

	plan := PlanWithOptions(testPods, opts.Mode, opts.Routing)
	if plan.Skip != nil {
		uiOutput.Warning("Matrix skipped: %s", plan.Skip.Reason)
		result.Skipped = plan.Skip
		return result, nil
	}

	executionCtx := runCtx
	executionCancel := func() {}
	if automaticTimeout {
		budget := automaticTimeoutBudget(plan, opts.Checks)
		uiOutput.Info("Connectivity timeout automatically calculated from %d planned tests: %s total budget", budget.PlannedTests, budget.Total)
		setupCancel()
		executionCtx, executionCancel = context.WithTimeout(ctx, budget.executionTimeout())
	}
	defer executionCancel()

	totalSameRail := len(plan.RDMASameRail)
	totalCrossRail := len(plan.RDMACrossRail)
	plannedTests := 0
	if checksContain(opts.Checks, CheckICMP) {
		uiOutput.Info("Plan: %d same-rail ICMP + %d cross-rail ICMP",
			len(plan.ICMPSameRail), len(plan.ICMPCrossRail))
		plannedTests += len(plan.ICMPSameRail) + len(plan.ICMPCrossRail)
	}
	if checksContain(opts.Checks, CheckRPing) {
		uiOutput.Info("Plan: %d same-rail rping + %d cross-rail rping",
			totalSameRail, totalCrossRail)
		plannedTests += totalSameRail + totalCrossRail
	}
	if checksContain(opts.Checks, CheckIBWriteBW) {
		uiOutput.Info("Plan: %d same-rail ib_write_bw + %d cross-rail ib_write_bw",
			len(plan.RDMABwSameRail), len(plan.RDMABwCrossRail))
		plannedTests += len(plan.RDMABwSameRail) + len(plan.RDMABwCrossRail)
	}
	if checksContain(opts.Checks, CheckGPUDirectDMABuf) {
		uiOutput.Info("Plan: %d same-rail GPUDirect DMA-BUF + %d cross-rail GPUDirect DMA-BUF",
			len(plan.GPUDirectDMABufSameRail), len(plan.GPUDirectDMABufCrossRail))
		plannedTests += len(plan.GPUDirectDMABufSameRail) + len(plan.GPUDirectDMABufCrossRail)
	}
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
	if plannedTests == 0 {
		uiOutput.Warning(
			"Matrix produced 0 tests despite %d schedulable pod(s) — either every rail had at least one endpoint with no resolvable RDMA device, or no rail key was shared across pods. "+
				"Most common cause: macvlan or IPoIB secondary networks, where the in-pod iface has no PCI-direct mlx5 sysfs entry. "+
				"Verify with: `kubectl exec <pod> -- ls /sys/class/net/<iface>/device/infiniband/` (empty on every pod = the fallback path needs the host master mapping).",
			len(testPods))
	}

	// Build pod-name → container maps so ICMP can run in netshoot while RDMA
	// tooling stays in the DOCA container that owns the RDMA resources.
	rdmaContainerByPod, icmpContainerByPod, namespaceByPod := containerMaps(allPods)

	stages := []struct {
		check Check
		label string
		tests []PingTest
		run   func(t PingTest) PingResult
	}{
		{
			check: CheckRPing,
			label: "RDMA ping (rping)",
			tests: testsForCheck(plan, CheckRPing),
		},
		{
			check: CheckIBWriteBW,
			label: "RDMA bandwidth (ib_write_bw)",
			tests: testsForCheck(plan, CheckIBWriteBW),
		},
		{
			check: CheckGPUDirectDMABuf,
			label: "GPUDirect RDMA bandwidth (DMA-BUF)",
			tests: testsForCheck(plan, CheckGPUDirectDMABuf),
		},
		{
			check: CheckICMP,
			label: "Layer 3 ping (ICMP)",
			tests: testsForCheck(plan, CheckICMP),
			run: func(t PingTest) PingResult {
				return RunICMP(executionCtx, restConfig, namespaceByPod[t.SrcPod],
					t.SrcPod, icmpContainerByPod[t.SrcPod], t)
			},
		},
	}
	for _, stage := range stages {
		if !checksContain(opts.Checks, stage.check) {
			continue
		}
		tests := stage.tests
		if len(tests) == 0 {
			continue
		}
		uiOutput.Info("Stage: %s — %d test(s)", stage.label, len(tests))
		if stage.check != CheckICMP {
			tests = checkSourceRoutes(executionCtx, restConfig, namespaceByPod, icmpContainerByPod, tests)
		}
		if stage.check == CheckRPing {
			result.PingResults = append(result.PingResults,
				RunRPingBatches(executionCtx, restConfig, namespaceByPod, rdmaContainerByPod, tests, opts.RPingIterations)...)
			continue
		}
		if stage.check == CheckIBWriteBW {
			result.PingResults = append(result.PingResults,
				RunIbWriteBwBatches(executionCtx, restConfig, namespaceByPod, rdmaContainerByPod, tests, opts.IBWriteSize, opts.IBWriteMinBandwidthGbps)...)
			continue
		}
		if stage.check == CheckGPUDirectDMABuf {
			result.PingResults = append(result.PingResults,
				RunGPUDirectDMABufBatches(executionCtx, restConfig, namespaceByPod, rdmaContainerByPod, tests, opts.IBWriteSize, opts.IBWriteMinBandwidthGbps)...)
			continue
		}
		for _, t := range tests {
			if namespaceByPod[t.SrcPod] == "" || namespaceByPod[t.DstPod] == "" || icmpContainerByPod[t.SrcPod] == "" {
				result.PingResults = append(result.PingResults, PingResult{
					Test: t,
					Err:  fmt.Errorf("no namespace/container lookup for pod pair (%s, %s)", t.SrcPod, t.DstPod),
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

func normalizeChecks(checks []Check) []Check {
	if checks == nil {
		return []Check{CheckICMP, CheckRPing, CheckIBWriteBW}
	}
	out := make([]Check, 0, len(checks))
	seen := map[Check]bool{}
	for _, check := range checks {
		if check == "" || seen[check] {
			continue
		}
		seen[check] = true
		out = append(out, check)
	}
	return out
}

func checkSourceRoutes(ctx context.Context, restConfig *rest.Config, namespaceByPod, containerByPod map[string]string, tests []PingTest) []PingTest {
	out := append([]PingTest(nil), tests...)
	for i := range out {
		if out[i].Expectation != ExpectRequired {
			continue
		}
		namespace := namespaceByPod[out[i].SrcPod]
		container := containerByPod[out[i].SrcPod]
		if namespace == "" || container == "" {
			out[i].sourceRouteErr = fmt.Errorf("no netshoot namespace/container lookup for source pod %s", out[i].SrcPod)
			continue
		}
		out[i].sourceRoute = checkRoute(ctx, restConfig, namespace, out[i].SrcPod, container, out[i])
		if routeMismatch(out[i].sourceRoute, out[i]) {
			out[i].sourceRouteErr = fmt.Errorf("source route selected dev %q, expected %q (route: %s)",
				out[i].sourceRoute.Dev, out[i].SrcIface, out[i].sourceRoute.Output)
		}
	}
	return out
}

func checksContain(checks []Check, want Check) bool {
	for _, check := range checks {
		if check == want {
			return true
		}
	}
	return false
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
		Name:             p.Name,
		Namespace:        p.Namespace,
		Node:             p.Spec.NodeName,
		IPsByRail:        map[string]string{},
		InterfacesByRail: map[string]string{},
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
			tp.InterfacesByRail[rail] = n.Interface
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
		uiOutput.Success("All %d connectivity test(s) passed", s.TotalTests)
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

// stageLabelOf returns the human-friendly family label for a test kind used
// in failure-line prefixes.
func stageLabelOf(k PingTestKind) string {
	if k.IsICMP() {
		return "icmp"
	}
	if k.IsRDMABw() {
		return "ib_write_bw"
	}
	if k.IsGPUDirectDMABuf() {
		return "gpudirect_dmabuf"
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
