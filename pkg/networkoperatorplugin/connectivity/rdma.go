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

// DiscoverRDMADevices runs once per test pod to map each multus
// secondary-network interface to its RDMA device. Reads
// /sys/class/net/<iface>/device/infiniband/ inside the pod — that
// directory exists for any mlx5 VF and contains exactly one entry
// (the device name, e.g. "mlx5_4").
//
// nets is the parsed multus annotation; ifaceByRail tells the
// mapping which net interface (e.g. "net1") backs each rail.
// Returns map[rail]→<rdmaDev>; rails whose interface couldn't be
// resolved are absent from the map.
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
		// `ls -1` prints one filename per line. Capture the
		// first line — the mlx5 VF directory always contains
		// exactly one entry, but `head -n 1` is a cheap safety
		// belt against future kernel changes.
		fmt.Fprintf(&b, "dev=$(ls -1 /sys/class/net/%s/device/infiniband/ 2>/dev/null | head -n 1); ", iface)
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
func RunRPing(ctx context.Context, restConfig *rest.Config, namespace string, serverPod, serverContainer string, clientPod, clientContainer string, test PingTest, iterations int) PingResult {
	if iterations <= 0 {
		iterations = 5
	}
	r := PingResult{Test: test}

	tctx, cancel := context.WithTimeout(ctx, rdmaTestTimeout)
	defer cancel()

	serverCmd := fmt.Sprintf("nohup rping -s -a %s -p 9999 -v >/tmp/rping-server.log 2>&1 & echo $!", test.DstIP)
	srvRes, err := kubeclient.ExecInPod(tctx, restConfig, namespace, serverPod, serverContainer, []string{"/bin/sh", "-c", serverCmd})
	if err != nil {
		r.Err = fmt.Errorf("rping server start: %w (stderr: %s)", err, srvRes.Stderr)
		return r
	}
	serverPID := strings.TrimSpace(srvRes.Stdout)
	defer killRDMAServer(restConfig, namespace, serverPod, serverContainer, "rping", serverPID)

	// Settle window — give the server time to bind.
	select {
	case <-tctx.Done():
		r.Err = fmt.Errorf("rping settle wait: %w", tctx.Err())
		return r
	case <-time.After(rdmaServerSettleDelay):
	}

	clientCmd := fmt.Sprintf("rping -c -a %s -p 9999 -C %d -v", test.DstIP, iterations)
	cliRes, cliErr := kubeclient.ExecInPod(tctx, restConfig, namespace, clientPod, clientContainer, []string{"/bin/sh", "-c", clientCmd})
	r.Stdout, r.Stderr = cliRes.Stdout, cliRes.Stderr
	r.Err = cliErr
	// rping exits 0 only on a clean run; non-zero exit propagates
	// to cliErr.
	r.OK = cliErr == nil
	return r
}

// RunIbWriteBw runs an ib_write_bw bandwidth test for one src→dst pair.
// Uses `-R` (RDMA-CM connection management — same handshake the
// example DS expects) and `-s 65536 --report_gbits` to produce a
// single-line, single-message-size output that's straightforward to
// parse for a matrix cell. Each test allocates a fresh TCP listener
// port (default 18515 + an orchestrator-supplied offset) to avoid
// collisions with concurrent runs against unrelated rails.
func RunIbWriteBw(ctx context.Context, restConfig *rest.Config, namespace string, serverPod, serverContainer string, clientPod, clientContainer string, test PingTest, port int) PingResult {
	if port <= 0 {
		port = 18515
	}
	r := PingResult{Test: test}
	if test.SrcRDMADev == "" || test.DstRDMADev == "" {
		r.Err = fmt.Errorf("ib_write_bw needs RDMA device names; got src=%q dst=%q", test.SrcRDMADev, test.DstRDMADev)
		return r
	}

	tctx, cancel := context.WithTimeout(ctx, rdmaTestTimeout)
	defer cancel()

	// `ib_write_bw -d <dev> -R -s 65536 --report_gbits -p <port>`
	// runs as the server when invoked without a peer IP, as the
	// client when invoked with one. The server prints a banner and
	// then waits; the client connects, runs the test, and both
	// sides print the summary table.
	serverCmd := fmt.Sprintf("nohup ib_write_bw -d %s -R -s 65536 --report_gbits -p %d >/tmp/ibwritebw-server.log 2>&1 & echo $!",
		test.DstRDMADev, port)
	srvRes, err := kubeclient.ExecInPod(tctx, restConfig, namespace, serverPod, serverContainer, []string{"/bin/sh", "-c", serverCmd})
	if err != nil {
		r.Err = fmt.Errorf("ib_write_bw server start: %w (stderr: %s)", err, srvRes.Stderr)
		return r
	}
	serverPID := strings.TrimSpace(srvRes.Stdout)
	defer killRDMAServer(restConfig, namespace, serverPod, serverContainer, "ib_write_bw", serverPID)

	select {
	case <-tctx.Done():
		r.Err = fmt.Errorf("ib_write_bw settle wait: %w", tctx.Err())
		return r
	case <-time.After(rdmaServerSettleDelay):
	}

	clientCmd := fmt.Sprintf("ib_write_bw -d %s -R -s 65536 --report_gbits -p %d %s",
		test.SrcRDMADev, port, test.DstIP)
	cliRes, cliErr := kubeclient.ExecInPod(tctx, restConfig, namespace, clientPod, clientContainer, []string{"/bin/sh", "-c", clientCmd})
	r.Stdout, r.Stderr = cliRes.Stdout, cliRes.Stderr

	bw, msgRate, parseOK := parseIbWriteBwOutput(cliRes.Stdout)
	r.BandwidthGbps = bw
	r.MsgRateMpps = msgRate
	switch {
	case cliErr != nil && !parseOK:
		r.Err = cliErr
		r.OK = false
	case !parseOK:
		r.Err = fmt.Errorf("ib_write_bw output missing summary row")
		r.OK = false
	default:
		// Treat any non-zero bandwidth as success — partial-bw
		// numbers are still informative (slow rail, MTU
		// mismatch, etc.) and the cell value tells the story.
		r.OK = bw > 0
	}
	return r
}

// killRDMAServer is best-effort cleanup. We try the captured PID
// first (precise) and fall back to a wildcard `pkill -f <prog>` for
// the cases where the PID didn't survive the exec round-trip.
// Errors are swallowed because by this point we've already recorded
// the client's result — leaking a stale server is preferable to
// failing the test on a cleanup hiccup.
func killRDMAServer(restConfig *rest.Config, namespace, pod, container, prog, pid string) {
	// Use a fresh background context so the cleanup runs even if
	// the test's deadline already fired.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := fmt.Sprintf("kill %s 2>/dev/null; pkill -f %q 2>/dev/null; true", pid, prog)
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
