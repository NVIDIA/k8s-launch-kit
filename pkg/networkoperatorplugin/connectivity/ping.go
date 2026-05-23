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

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"k8s.io/client-go/rest"
)

// PingResult carries the outcome of one src→dst matrix test. Used for
// every test kind (ICMP / rping / ib_write_bw); fields that don't
// apply to a kind stay zero.
//
//   - ICMP: PacketLoss + RTTAvgMs populated by ping parser
//   - rping: only OK + Err set (rping output is verbose; binary
//     pass/fail is enough for the matrix view)
//   - ib_write_bw: BandwidthGbps + MsgRateMpps populated by the
//     ib_write_bw parser; PacketLoss stays at -1 (n/a)
//
// PacketLoss = -1 means the value was not parseable / not applicable;
// callers treat that as fail only when OK is also false.
type PingResult struct {
	Test           PingTest
	OK             bool
	PacketLoss     int     // ICMP only; 0-100; -1 when n/a
	RTTAvgMs       float64 // ICMP only; 0 when unknown
	BandwidthGbps  float64 // ib_write_bw only; 0 when n/a
	MsgRateMpps    float64 // ib_write_bw only; 0 when n/a
	Stdout         string
	Stderr         string
	Err            error
}

// pingSummaryRe matches the trailing summary line of `ping -c N` output,
// e.g.:
//
//	3 packets transmitted, 3 received, 0% packet loss, time 2003ms
//	5 packets transmitted, 0 received, 100% packet loss, time 4099ms
//
// Both BusyBox ping and iputils ping share this format; the
// `mellanox/rping-test` image ships iputils.
var pingSummaryRe = regexp.MustCompile(`(\d+)\s+packets transmitted,\s*(\d+)\s+(?:packets\s+)?received,\s*(\d+)%\s+packet loss`)

// pingRTTRe matches the optional rtt summary line, e.g.:
//
//	rtt min/avg/max/mdev = 0.123/0.456/0.789/0.100 ms
var pingRTTRe = regexp.MustCompile(`rtt min/avg/max/mdev = [\d.]+/([\d.]+)/[\d.]+/[\d.]+ ms`)

// RunPing execs `ping -c <count> -W 1 -I <srcIP> <dstIP>` inside the
// source pod and parses the summary line. `-I` is critical for the
// rail-based matrix — without it, ping would use the pod's default-CNI
// interface and never traverse the secondary network we're testing.
//
// Returned PingResult is always populated; PingResult.Err carries any
// exec-level failure (RBAC, pod gone, kubelet timeout). PingResult.OK
// is true only when the exec succeeded *and* packet loss < 100%.
func RunPing(ctx context.Context, restConfig *rest.Config, namespace, container string, test PingTest, count int) PingResult {
	if count <= 0 {
		count = 3
	}
	cmd := []string{
		"ping",
		"-c", strconv.Itoa(count),
		"-W", "1",
		"-I", test.SrcIP,
		test.DstIP,
	}
	res, err := kubeclient.ExecInPod(ctx, restConfig, namespace, test.SrcPod, container, cmd)

	r := PingResult{
		Test:       test,
		PacketLoss: -1,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		Err:        err,
	}

	// Even on exec error we try to parse stdout — ping returns
	// non-zero when packet loss is 100%, and the SPDY executor
	// surfaces that as an error with stdout still populated.
	if m := pingSummaryRe.FindStringSubmatch(res.Stdout); m != nil {
		if loss, perr := strconv.Atoi(m[3]); perr == nil {
			r.PacketLoss = loss
		}
	}
	if m := pingRTTRe.FindStringSubmatch(res.Stdout); m != nil {
		if avg, perr := strconv.ParseFloat(m[1], 64); perr == nil {
			r.RTTAvgMs = avg
		}
	}

	switch {
	case err != nil && r.PacketLoss < 0:
		// Couldn't even parse output — treat as fail and keep the
		// exec error for the report.
		r.OK = false
	case r.PacketLoss < 0:
		// Stdout present but no summary line — unusual; surface
		// it.
		r.OK = false
		r.Err = fmt.Errorf("ping output missing summary line")
	default:
		r.OK = r.PacketLoss < 100
	}
	return r
}
