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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"k8s.io/client-go/rest"
)

// rdmaServerSettleDelay is the pause between starting the server
// process and connecting from the client. ib_write_bw and rping both
// open a TCP listener on the server side before accepting QP connect
// requests; without a small settle window the client races the
// listener and reports "Couldn't connect to server".
const rdmaServerSettleDelay = 2 * time.Second

// rdmaTestTimeout caps any single RDMA test (server start + client
// run + cleanup). Generous enough to survive a sluggish first
// connection; the orchestrator caps the whole matrix separately via
// Options.Timeout.
const rdmaTestTimeout = 90 * time.Second

const rpingBatchResultMarker = "__L8K_RPING_RESULT__"

const (
	ibWriteBwBatchResultMarker = "__L8K_IBWRITEBW_RESULT__"
	ibWriteBwBatchEndMarker    = "__L8K_IBWRITEBW_END__"
)

// railNameIndex extracts the numeric rail index from a rail key like
// "macvlan-network-rail-3-some-suffix" → 3. Matches the rendered
// NetworkAttachmentDefinition / MacvlanNetwork names every l8k profile
// emits (always shaped `<base>-rail-<N>-<identifier>`). Returns -1 when
// the pattern doesn't match (e.g. shared-network profiles where rail
// indexing isn't part of the network name) so the caller skips the
// rail-index fallback for that key.
var railNameIndex = regexp.MustCompile(`(?:^|-)rail-(\d+)(?:-|$)`)

func railIndex(rail string) int {
	m := railNameIndex.FindStringSubmatch(rail)
	if len(m) < 2 {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// DiscoverRDMADevices runs once per test pod to map each multus
// secondary-network interface to its RDMA device. Two probes are
// tried in order, per interface:
//
//  1. /sys/class/net/<iface>/device/infiniband/ — this exists for any
//     PCI-direct mlx5 device, including SR-IOV VFs moved into the pod
//     netns (host-device-rdma, sriov-*-rdma profiles). Contains
//     exactly one entry: the RDMA device name (e.g. "mlx5_4").
//
//  2. Fallback for non-PCI-direct attachments — macvlan slaves,
//     IPoIB child interfaces, and anything else that's a kernel
//     netdev layered on top of a master mlx5 device. Those netdevs
//     don't have a `device/` symlink to a PCI function inside the
//     pod, so probe (1) returns empty. The rdmaSharedDevicePlugin
//     mounts the host's `/sys/class/infiniband/<mlx5_X>` entries
//     into the pod for each rail-resource the pod requested, so we
//     enumerate that directory. When exactly one entry is present
//     (single-NIC nodes — the common case), use it. When multiple
//     entries are present (multi-rail rdmaShared on a multi-NIC
//     node) we can't disambiguate purely pod-side and fall back to
//     "no device for this rail"; an orchestrator-side host probe
//     reading the rendered MacvlanNetwork master fields would be the
//     fix and is tracked as a follow-up.
//
// ifaceByRail tells which net interface (e.g. "net1") backs each
// rail. Returns map[rail]→<rdmaDev>; rails whose interface couldn't
// be resolved are absent from the map.
func DiscoverRDMADevices(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, ifaceByRail map[string]string) map[string]string {
	out := map[string]string{}
	if len(ifaceByRail) == 0 {
		return out
	}
	// One shell exec per pod: build a tiny script that prints
	// `<rail>=<dev>` per resolved interface. Cheaper than one
	// exec per rail.
	var b strings.Builder
	for rail, iface := range ifaceByRail {
		if iface == "" {
			continue
		}
		// Primary probe: PCI-direct mlx5 netdev (SR-IOV VF, host-device).
		fmt.Fprintf(&b, "dev=$(ls -1 /sys/class/net/%s/device/infiniband/ 2>/dev/null | head -n 1); ", iface)

		// Fallback A (single-NIC): non-PCI-direct netdev (macvlan, ipoib).
		// rdmaSharedDevicePlugin injects /sys/class/infiniband/<mlx5_X>
		// for each rail-resource the pod requested. When the pod
		// requested exactly one rail, there's exactly one mlx5 — pick it.
		fmt.Fprintf(&b, "if [ -z \"$dev\" ] && [ -d /sys/class/net/%s ]; then "+
			"count=$(ls -1 /sys/class/infiniband/ 2>/dev/null | wc -l); "+
			"if [ \"$count\" = \"1\" ]; then dev=$(ls -1 /sys/class/infiniband/ 2>/dev/null); fi; "+
			"fi; ", iface)

		// Fallback B (multi-rail): when the pod was granted multiple
		// rail-resources, /sys/class/infiniband has one entry per rail,
		// sorted by mlx5 index. Use the rail INDEX (extracted from the
		// rail name `rail-N`) to pick the Nth entry. Sort is `sort -t_
		// -k2,2n` (POSIX, works on busybox) rather than `ls -1v` (GNU
		// only) so the probe doesn't silently regress to alphabetic
		// order — which would mis-pick for rails ≥ 10 — if the test
		// container image ever changes from DOCA to a busybox-based
		// distro.
		//
		// Caveats: assumes (a) one rail = one mlx5, (b) mlx5 numbering
		// follows PCI enumeration (kernel default), and (c) rail indexing
		// is contiguous from 0 in the same order. All three hold for
		// every l8k-generated profile today. When false (e.g. extra
		// non-rail mlx5 devices exposed), the heuristic picks wrong;
		// the orchestrator-side master→mlx5 mapping is the real fix and
		// is tracked separately.
		if idx := railIndex(rail); idx >= 0 {
			fmt.Fprintf(&b, "if [ -z \"$dev\" ]; then "+
				"dev=$(ls -1 /sys/class/infiniband/ 2>/dev/null | sort -t_ -k2,2n | sed -n %dp); "+
				"fi; ", idx+1)
		}

		fmt.Fprintf(&b, "if [ -n \"$dev\" ]; then echo %q=$dev; fi; ", rail)
	}
	if b.Len() == 0 {
		return out
	}
	res, err := kubeclient.ExecInPod(ctx, restConfig, namespace, pod, container, []string{"/bin/sh", "-c", b.String()})
	if err != nil {
		return out
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		rail := strings.Trim(strings.TrimSpace(line[:eq]), `"`)
		dev := strings.TrimSpace(line[eq+1:])
		if rail != "" && dev != "" {
			out[rail] = dev
		}
	}
	return out
}

// RunRPing executes the rping server/client dance for one test pair.
// Spawn-fresh-server-per-test (the agreed lifecycle): start the
// server as a background process via `nohup rping -s &`, sleep
// rdmaServerSettleDelay, run the client, then `pkill rping` on the
// server pod. Cleanup runs even if the client errored.
//
// rping returns 0 on success after C iterations. We don't read its
// output for any structured value — the binary OK is enough for
// matrix display. rping verifies the QP pair establishes
// end-to-end; ib_write_bw additionally measures throughput.
func RunRPing(ctx context.Context, restConfig *rest.Config, serverNamespace string, serverPod, serverContainer string, clientNamespace string, clientPod, clientContainer string, test PingTest, iterations int) PingResult {
	if iterations <= 0 {
		iterations = 5
	}
	r := initResult(test)

	tctx, cancel := context.WithTimeout(ctx, rdmaTestTimeout)
	defer cancel()

	if r.Expectation == ExpectRequired {
		r.Route = checkRoute(tctx, restConfig, clientNamespace, clientPod, clientContainer, test)
		if routeMismatch(r.Route, test) {
			r.Err = fmt.Errorf("source route selected dev %q, expected %q (route: %s)",
				r.Route.Dev, test.SrcIface, r.Route.Output)
			finalizeExpectedResult(&r, false, r.Err)
			return r
		}
	}

	serverCmd := fmt.Sprintf("nohup rping -s -a %s -p 9999 -v >/tmp/rping-server.log 2>&1 & echo $!", shellArg(test.DstIP))
	srvRes, err := kubeclient.ExecInPod(tctx, restConfig, serverNamespace, serverPod, serverContainer, []string{"/bin/sh", "-c", serverCmd})
	if err != nil {
		r.Err = fmt.Errorf("rping server start: %w (stderr: %s)", err, srvRes.Stderr)
		return r
	}
	serverPID := strings.TrimSpace(srvRes.Stdout)
	defer killRDMAServer(restConfig, serverNamespace, serverPod, serverContainer, "rping", serverPID)

	// Settle window — give the server time to bind.
	select {
	case <-tctx.Done():
		r.Err = fmt.Errorf("rping settle wait: %w", tctx.Err())
		return r
	case <-time.After(rdmaSettleDelayFor(test)):
	}

	clientCmd := shellWithTimeout(
		fmt.Sprintf("rping -c -I %s -a %s -p 9999 -C %d -v", shellArg(test.SrcIP), shellArg(test.DstIP), iterations),
		commandTimeoutFor(test, 30*time.Second))
	cliRes, cliErr := kubeclient.ExecInPod(tctx, restConfig, clientNamespace, clientPod, clientContainer, []string{"/bin/sh", "-c", clientCmd})
	r.Stdout, r.Stderr = cliRes.Stdout, cliRes.Stderr
	finalizeExpectedResult(&r, cliErr == nil, cliErr)
	return r
}

// RunRPingBatches executes rping tests grouped by ordered pod pair. This keeps
// the matrix result per test, but collapses the API exec traffic from
// start/client/cleanup per cell to start/client/cleanup per pod pair.
func RunRPingBatches(ctx context.Context, restConfig *rest.Config, namespaceByPod, containerByPod map[string]string, tests []PingTest, iterations int) []PingResult {
	if iterations <= 0 {
		iterations = 5
	}
	groups := groupRPingTests(tests)
	out := make([]PingResult, 0, len(tests))
	for _, group := range groups {
		out = append(out, runRPingBatch(ctx, restConfig, namespaceByPod, containerByPod, group, iterations)...)
	}
	return out
}

func runRPingBatch(ctx context.Context, restConfig *rest.Config, namespaceByPod, containerByPod map[string]string, tests []PingTest, iterations int) []PingResult {
	results := make([]PingResult, len(tests))
	for i, test := range tests {
		results[i] = initResult(test)
	}
	if len(tests) == 0 {
		return results
	}

	first := tests[0]
	serverNamespace, serverContainer := namespaceByPod[first.DstPod], containerByPod[first.DstPod]
	clientNamespace, clientContainer := namespaceByPod[first.SrcPod], containerByPod[first.SrcPod]
	if serverNamespace == "" || serverContainer == "" || clientNamespace == "" || clientContainer == "" {
		err := fmt.Errorf("no namespace/container lookup for rping pod pair (%s, %s)", first.SrcPod, first.DstPod)
		for i := range results {
			results[i].Err = err
		}
		return results
	}

	tctx, cancel := context.WithTimeout(ctx, rdmaBatchTimeoutFor(tests))
	defer cancel()

	serverCmd := rpingBatchServerCommand(tests)
	srvRes, err := kubeclient.ExecInPod(tctx, restConfig, serverNamespace, first.DstPod, serverContainer, []string{"/bin/sh", "-c", serverCmd})
	if err != nil {
		batchErr := fmt.Errorf("rping batch server start: %w (stderr: %s)", err, srvRes.Stderr)
		for i := range results {
			results[i].Err = batchErr
		}
		return results
	}
	defer killRDMAServer(restConfig, serverNamespace, first.DstPod, serverContainer, "rping", "")

	select {
	case <-tctx.Done():
		batchErr := fmt.Errorf("rping batch settle wait: %w", tctx.Err())
		for i := range results {
			results[i].Err = batchErr
		}
		return results
	case <-time.After(rdmaBatchSettleDelayFor(tests)):
	}

	clientCmd := rpingBatchClientCommand(tests, iterations)
	cliRes, cliErr := kubeclient.ExecInPod(tctx, restConfig, clientNamespace, first.SrcPod, clientContainer, []string{"/bin/sh", "-c", clientCmd})
	rcByIndex := parseRPingBatchResults(cliRes.Stdout)
	for i := range results {
		results[i].Stdout = cliRes.Stdout
		results[i].Stderr = cliRes.Stderr
		rc, ok := rcByIndex[i]
		if !ok {
			err := cliErr
			if err == nil {
				err = fmt.Errorf("rping batch missing result for test %d", i)
			}
			finalizeExpectedResult(&results[i], false, err)
			continue
		}
		if rc == 0 {
			finalizeExpectedResult(&results[i], true, nil)
			continue
		}
		finalizeExpectedResult(&results[i], false, fmt.Errorf("rping exited with code %d", rc))
	}
	return results
}

func groupRPingTests(tests []PingTest) [][]PingTest {
	type key struct {
		srcPod string
		dstPod string
	}
	var order []key
	byKey := map[key][]PingTest{}
	for _, test := range tests {
		k := key{srcPod: test.SrcPod, dstPod: test.DstPod}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], test)
	}
	out := make([][]PingTest, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

func rpingBatchServerCommand(tests []PingTest) string {
	seen := map[string]bool{}
	var b strings.Builder
	b.WriteString("pkill rping 2>/dev/null || true; rm -f /tmp/l8k-rping-server-*.log; ")
	idx := 0
	for _, test := range tests {
		if test.DstIP == "" || seen[test.DstIP] {
			continue
		}
		seen[test.DstIP] = true
		fmt.Fprintf(&b, "nohup rping -s -a %s -p 9999 -v >/tmp/l8k-rping-server-%d.log 2>&1 & ", shellArg(test.DstIP), idx)
		idx++
	}
	b.WriteString("echo ready")
	return b.String()
}

func rpingBatchClientCommand(tests []PingTest, iterations int) string {
	var b strings.Builder
	b.WriteString(`run_with_timeout() { seconds="$1"; shift; if command -v timeout >/dev/null 2>&1; then timeout --kill-after=2s "${seconds}s" "$@"; else "$@" & pid=$!; (sleep "$seconds"; kill -TERM "$pid" 2>/dev/null; sleep 2; kill -KILL "$pid" 2>/dev/null) & watchdog=$!; wait "$pid"; rc=$?; kill "$watchdog" 2>/dev/null; wait "$watchdog" 2>/dev/null; return "$rc"; fi; }; `)
	for i, test := range tests {
		timeoutSeconds := int(commandTimeoutFor(test, 30*time.Second).Seconds())
		if timeoutSeconds <= 0 {
			timeoutSeconds = 1
		}
		fmt.Fprintf(&b,
			"run_with_timeout %d rping -c -I %s -a %s -p 9999 -C %d -v >/tmp/l8k-rping-client-%d.out 2>/tmp/l8k-rping-client-%d.err; rc=$?; echo %s %d $rc; ",
			timeoutSeconds, shellArg(test.SrcIP), shellArg(test.DstIP), iterations, i, i, rpingBatchResultMarker, i)
	}
	b.WriteString("exit 0")
	return b.String()
}

func parseRPingBatchResults(stdout string) map[int]int {
	out := map[int]int{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 || fields[0] != rpingBatchResultMarker {
			continue
		}
		idx, err1 := strconv.Atoi(fields[1])
		rc, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		out[idx] = rc
	}
	return out
}

func rdmaBatchTimeoutFor(tests []PingTest) time.Duration {
	timeout := 15 * time.Second
	for _, test := range tests {
		timeout += commandTimeoutFor(test, 30*time.Second) + rdmaBatchSettleDelayFor([]PingTest{test})
	}
	return timeout
}

func rdmaBatchSettleDelayFor(tests []PingTest) time.Duration {
	for _, test := range tests {
		if test.Expectation == ExpectRequired {
			return rdmaServerSettleDelay
		}
	}
	return rdmaSettleDelayFor(PingTest{Expectation: ExpectObserve})
}

// RunIbWriteBw runs an ib_write_bw bandwidth test for one src→dst pair.
// Uses `-R` (RDMA-CM connection management — same handshake the
// example DS expects) and `-s 65536 --report_gbits` to produce a
// single-line, single-message-size output that's straightforward to
// parse for a matrix cell. Each test allocates a fresh TCP listener
// port (default 18515 + an orchestrator-supplied offset) to avoid
// collisions with concurrent runs against unrelated rails.
func RunIbWriteBw(ctx context.Context, restConfig *rest.Config, serverNamespace string, serverPod, serverContainer string, clientNamespace string, clientPod, clientContainer string, test PingTest, port, size int, minBandwidthGbps float64) PingResult {
	if port <= 0 {
		port = 18515
	}
	if size <= 0 {
		size = 65536
	}
	r := initResult(test)
	r.MinBandwidthGbps = minBandwidthGbps
	if test.SrcRDMADev == "" || test.DstRDMADev == "" {
		r.Err = fmt.Errorf("ib_write_bw needs RDMA device names; got src=%q dst=%q", test.SrcRDMADev, test.DstRDMADev)
		return r
	}

	tctx, cancel := context.WithTimeout(ctx, rdmaTestTimeout)
	defer cancel()

	if r.Expectation == ExpectRequired {
		r.Route = checkRoute(tctx, restConfig, clientNamespace, clientPod, clientContainer, test)
		if routeMismatch(r.Route, test) {
			r.Err = fmt.Errorf("source route selected dev %q, expected %q (route: %s)",
				r.Route.Dev, test.SrcIface, r.Route.Output)
			finalizeExpectedResult(&r, false, r.Err)
			return r
		}
	}

	// `ib_write_bw -d <dev> -R -s <size> --report_gbits -p <port>`
	// runs as the server when invoked without a peer IP, as the
	// client when invoked with one. The server prints a banner and
	// then waits; the client connects, runs the test, and both
	// sides print the summary table.
	serverCmd := fmt.Sprintf("nohup ib_write_bw -d %s -R -s %d --report_gbits -p %d --bind_source_ip %s >/tmp/ibwritebw-server.log 2>&1 & echo $!",
		shellArg(test.DstRDMADev), size, port, shellArg(test.DstIP))
	srvRes, err := kubeclient.ExecInPod(tctx, restConfig, serverNamespace, serverPod, serverContainer, []string{"/bin/sh", "-c", serverCmd})
	if err != nil {
		r.Err = fmt.Errorf("ib_write_bw server start: %w (stderr: %s)", err, srvRes.Stderr)
		return r
	}
	serverPID := strings.TrimSpace(srvRes.Stdout)
	defer killRDMAServer(restConfig, serverNamespace, serverPod, serverContainer, "ib_write_bw", serverPID)

	select {
	case <-tctx.Done():
		r.Err = fmt.Errorf("ib_write_bw settle wait: %w", tctx.Err())
		return r
	case <-time.After(rdmaSettleDelayFor(test)):
	}

	clientCmd := shellWithTimeout(
		fmt.Sprintf("ib_write_bw -d %s -R -s %d --report_gbits -p %d --bind_source_ip %s %s",
			shellArg(test.SrcRDMADev), size, port, shellArg(test.SrcIP), shellArg(test.DstIP)),
		commandTimeoutFor(test, 45*time.Second))
	cliRes, cliErr := kubeclient.ExecInPod(tctx, restConfig, clientNamespace, clientPod, clientContainer, []string{"/bin/sh", "-c", clientCmd})
	r.Stdout, r.Stderr = cliRes.Stdout, cliRes.Stderr

	bw, msgRate, parseOK := parseIbWriteBwOutput(cliRes.Stdout)
	r.BandwidthGbps = bw
	r.MsgRateMpps = msgRate
	switch {
	case cliErr != nil && !parseOK:
		finalizeExpectedResult(&r, false, cliErr)
	case !parseOK:
		finalizeExpectedResult(&r, false, fmt.Errorf("ib_write_bw output missing summary row"))
	default:
		observedOK, observedErr := bandwidthVerdict(bw, minBandwidthGbps)
		finalizeExpectedResult(&r, observedOK, observedErr)
	}
	return r
}

// RunIbWriteBwBatches executes ib_write_bw tests grouped by ordered pod pair.
// Each test in a group gets a unique TCP listener port so all destination
// listeners can be started with one server-side exec before the client pod runs
// the tests sequentially in one client-side exec.
func RunIbWriteBwBatches(ctx context.Context, restConfig *rest.Config, namespaceByPod, containerByPod map[string]string, tests []PingTest, size int, minBandwidthGbps float64) []PingResult {
	if size <= 0 {
		size = 65536
	}
	groups := groupRPingTests(tests)
	out := make([]PingResult, 0, len(tests))
	for _, group := range groups {
		out = append(out, runIbWriteBwBatch(ctx, restConfig, namespaceByPod, containerByPod, group, size, minBandwidthGbps)...)
	}
	return out
}

func runIbWriteBwBatch(ctx context.Context, restConfig *rest.Config, namespaceByPod, containerByPod map[string]string, tests []PingTest, size int, minBandwidthGbps float64) []PingResult {
	results := make([]PingResult, len(tests))
	for i, test := range tests {
		results[i] = initResult(test)
		results[i].MinBandwidthGbps = minBandwidthGbps
		if test.SrcRDMADev == "" || test.DstRDMADev == "" {
			results[i].Err = fmt.Errorf("ib_write_bw needs RDMA device names; got src=%q dst=%q", test.SrcRDMADev, test.DstRDMADev)
		}
	}
	if len(tests) == 0 {
		return results
	}
	first := tests[0]
	serverNamespace, serverContainer := namespaceByPod[first.DstPod], containerByPod[first.DstPod]
	clientNamespace, clientContainer := namespaceByPod[first.SrcPod], containerByPod[first.SrcPod]
	if serverNamespace == "" || serverContainer == "" || clientNamespace == "" || clientContainer == "" {
		err := fmt.Errorf("no namespace/container lookup for ib_write_bw pod pair (%s, %s)", first.SrcPod, first.DstPod)
		for i := range results {
			results[i].Err = err
		}
		return results
	}

	tctx, cancel := context.WithTimeout(ctx, ibWriteBwBatchTimeoutFor(tests))
	defer cancel()

	serverCmd := ibWriteBwBatchServerCommand(tests, size)
	srvRes, err := kubeclient.ExecInPod(tctx, restConfig, serverNamespace, first.DstPod, serverContainer, []string{"/bin/sh", "-c", serverCmd})
	if err != nil {
		batchErr := fmt.Errorf("ib_write_bw batch server start: %w (stderr: %s)", err, srvRes.Stderr)
		for i := range results {
			results[i].Err = batchErr
		}
		return results
	}
	defer killRDMAServer(restConfig, serverNamespace, first.DstPod, serverContainer, "ib_write_bw", "")

	select {
	case <-tctx.Done():
		batchErr := fmt.Errorf("ib_write_bw batch settle wait: %w", tctx.Err())
		for i := range results {
			results[i].Err = batchErr
		}
		return results
	case <-time.After(rdmaBatchSettleDelayFor(tests)):
	}

	clientCmd := ibWriteBwBatchClientCommand(tests, size)
	cliRes, cliErr := kubeclient.ExecInPod(tctx, restConfig, clientNamespace, first.SrcPod, clientContainer, []string{"/bin/sh", "-c", clientCmd})
	parsed := parseIbWriteBwBatchResults(cliRes.Stdout)
	for i := range results {
		results[i].Stderr = cliRes.Stderr
		if results[i].Err != nil {
			continue
		}
		cell, ok := parsed[i]
		if !ok {
			err := cliErr
			if err == nil {
				err = fmt.Errorf("ib_write_bw batch missing result for test %d", i)
			}
			finalizeExpectedResult(&results[i], false, err)
			continue
		}
		results[i].Stdout = cell.stdout
		bw, msgRate, parseOK := parseIbWriteBwOutput(cell.stdout)
		results[i].BandwidthGbps = bw
		results[i].MsgRateMpps = msgRate
		switch {
		case cell.rc != 0 && !parseOK:
			finalizeExpectedResult(&results[i], false, fmt.Errorf("ib_write_bw exited with code %d", cell.rc))
		case !parseOK:
			finalizeExpectedResult(&results[i], false, fmt.Errorf("ib_write_bw output missing summary row"))
		default:
			observedOK, observedErr := bandwidthVerdict(bw, minBandwidthGbps)
			finalizeExpectedResult(&results[i], observedOK, observedErr)
		}
	}
	return results
}

func ibWriteBwBatchServerCommand(tests []PingTest, size int) string {
	var b strings.Builder
	b.WriteString("pkill ib_write_bw 2>/dev/null || true; rm -f /tmp/l8k-ibwritebw-server-*.log; ")
	for i, test := range tests {
		if test.DstRDMADev == "" || test.DstIP == "" {
			continue
		}
		port := ibWriteBwBatchPort(i)
		fmt.Fprintf(&b,
			"nohup ib_write_bw -d %s -R -s %d --report_gbits -p %d --bind_source_ip %s >/tmp/l8k-ibwritebw-server-%d.log 2>&1 & ",
			shellArg(test.DstRDMADev), size, port, shellArg(test.DstIP), i)
	}
	b.WriteString("echo ready")
	return b.String()
}

func ibWriteBwBatchClientCommand(tests []PingTest, size int) string {
	var b strings.Builder
	b.WriteString(`run_with_timeout() { seconds="$1"; shift; if command -v timeout >/dev/null 2>&1; then timeout --kill-after=2s "${seconds}s" "$@"; else "$@" & pid=$!; (sleep "$seconds"; kill -TERM "$pid" 2>/dev/null; sleep 2; kill -KILL "$pid" 2>/dev/null) & watchdog=$!; wait "$pid"; rc=$?; kill "$watchdog" 2>/dev/null; wait "$watchdog" 2>/dev/null; return "$rc"; fi; }; `)
	for i, test := range tests {
		timeoutSeconds := int(commandTimeoutFor(test, 45*time.Second).Seconds())
		if timeoutSeconds <= 0 {
			timeoutSeconds = 1
		}
		outFile := fmt.Sprintf("/tmp/l8k-ibwritebw-client-%d.out", i)
		errFile := fmt.Sprintf("/tmp/l8k-ibwritebw-client-%d.err", i)
		fmt.Fprintf(&b,
			"run_with_timeout %d ib_write_bw -d %s -R -s %d --report_gbits -p %d --bind_source_ip %s %s >%s 2>%s; rc=$?; echo %s %d $rc; cat %s 2>/dev/null; echo %s %d; ",
			timeoutSeconds, shellArg(test.SrcRDMADev), size, ibWriteBwBatchPort(i), shellArg(test.SrcIP), shellArg(test.DstIP),
			outFile, errFile, ibWriteBwBatchResultMarker, i, outFile, ibWriteBwBatchEndMarker, i)
	}
	b.WriteString("exit 0")
	return b.String()
}

type ibWriteBwBatchCell struct {
	rc     int
	stdout string
}

func parseIbWriteBwBatchResults(stdout string) map[int]ibWriteBwBatchCell {
	out := map[int]ibWriteBwBatchCell{}
	var current *int
	var b strings.Builder
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 3 && fields[0] == ibWriteBwBatchResultMarker {
			idx, err1 := strconv.Atoi(fields[1])
			rc, err2 := strconv.Atoi(fields[2])
			if err1 == nil && err2 == nil {
				out[idx] = ibWriteBwBatchCell{rc: rc}
				current = &idx
				b.Reset()
			}
			continue
		}
		if len(fields) == 2 && fields[0] == ibWriteBwBatchEndMarker {
			idx, err := strconv.Atoi(fields[1])
			if err == nil {
				if cell, ok := out[idx]; ok {
					cell.stdout = strings.TrimSpace(b.String())
					out[idx] = cell
				}
			}
			current = nil
			b.Reset()
			continue
		}
		if current != nil {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return out
}

func ibWriteBwBatchPort(index int) int {
	return 18515 + index
}

func ibWriteBwBatchTimeoutFor(tests []PingTest) time.Duration {
	timeout := 20 * time.Second
	for _, test := range tests {
		timeout += commandTimeoutFor(test, 45*time.Second) + rdmaBatchSettleDelayFor([]PingTest{test})
	}
	return timeout
}

func bandwidthVerdict(bw, minBandwidthGbps float64) (bool, error) {
	switch {
	case bw <= 0:
		return false, fmt.Errorf("observed bandwidth %.1f Gbps is not positive", bw)
	case minBandwidthGbps > 0 && bw < minBandwidthGbps:
		return false, fmt.Errorf("observed bandwidth %.1f Gbps below minimum %.1f Gbps", bw, minBandwidthGbps)
	default:
		return true, nil
	}
}

// killRDMAServer is best-effort cleanup. We try the captured PID
// first (precise) and fall back to an exact process-name `pkill`.
// Errors are swallowed because by this point we've already recorded
// the client's result — leaking a stale server is preferable to
// failing the test on a cleanup hiccup.
func killRDMAServer(restConfig *rest.Config, namespace, pod, container, prog, pid string) {
	// Use a fresh background context so the cleanup runs even if
	// the test's deadline already fired.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := fmt.Sprintf("kill %s 2>/dev/null; pkill %s 2>/dev/null; true", pid, shellArg(prog))
	_, _ = kubeclient.ExecInPod(ctx, restConfig, namespace, pod, container, []string{"/bin/sh", "-c", cmd})
}

// ibWriteBwSummaryRe matches the single-row summary line that
// `ib_write_bw -s 65536 --report_gbits` emits on both client and
// server. Format (whitespace-separated columns, varies slightly
// across perftest versions; this regex matches the common shape):
//
//	#bytes     #iterations  BW peak[Gb/sec]  BW average[Gb/sec]  MsgRate[Mpps]
//	65536      5000          194.39           193.21               0.368
//
// The leading message-size column is always integer; subsequent
// columns are float with optional decimals. We don't anchor the
// regex to start-of-line because perftest sometimes prefixes the
// summary with " ".
var ibWriteBwSummaryRe = regexp.MustCompile(`^\s*\d+\s+\d+\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)\s*$`)

// parseIbWriteBwOutput scans the client's stdout for the summary
// row. Returns the peak Gbps, message rate (Mpps), and ok=true when a
// summary row was found. We return the *peak* rather than average
// because the average can be dragged down by ramp-up cycles on the
// first few iterations; for a "is this rail healthy" matrix the
// peak is the more useful number.
func parseIbWriteBwOutput(stdout string) (peakGbps, msgRateMpps float64, ok bool) {
	for _, line := range strings.Split(stdout, "\n") {
		m := ibWriteBwSummaryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		peak, err1 := strconv.ParseFloat(m[1], 64)
		_, err2 := strconv.ParseFloat(m[2], 64) // average — read for the side effect of validating the regex; not surfaced
		rate, err3 := strconv.ParseFloat(m[3], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		return peak, rate, true
	}
	return 0, 0, false
}
