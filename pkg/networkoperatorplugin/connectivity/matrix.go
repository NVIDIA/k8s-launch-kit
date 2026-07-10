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
	"fmt"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// Mode controls matrix coverage and cross-rail gating.
type Mode string

const (
	ModeQuick  Mode = "quick"
	ModeFull   Mode = "full"
	ModeStrict Mode = "strict"
)

// Expectation controls how a test result contributes to the verdict.
type Expectation string

const (
	ExpectRequired  Expectation = "required"
	ExpectForbidden Expectation = "forbidden"
	ExpectObserve   Expectation = "observe"
)

// TestPod is one entry in the matrix's source set — a pod with its
// per-rail secondary-network IPs already parsed from the multus
// annotation. Rails are keyed by their multus NetworkAttachmentDefinition
// name (e.g. "rail-0", "rail-1") so the matrix can match same-rail
// endpoints across pods.
type TestPod struct {
	Name      string
	Namespace string
	Node      string
	// IPsByRail is "<rail>" → IPv4 address (first IPv4 only — pods
	// typically have one v4 per attachment). Rails that didn't yield
	// an IP are absent from the map.
	IPsByRail map[string]string
	// RDMADevsByRail is "<rail>" → RDMA device name (e.g.
	// "mlx5_0") for the VF attached to that secondary network.
	// Populated by DiscoverRDMADevices(); rails without an
	// RDMA-capable device or where the lookup failed are absent.
	// Used by RunIbWriteBw / RunRPing to pass `-d <dev>`.
	RDMADevsByRail map[string]string
	// InterfacesByRail is "<rail>" → pod netdev name (e.g. "net1").
	// Every runner uses this to pin or verify the source interface.
	InterfacesByRail map[string]string
	// RailOrder is the deterministic iteration order over IPsByRail
	// — needed because Go map iteration is randomized and the cross
	// -rail canary picks "rail 0" vs "rail 1" by position.
	RailOrder []string
}

// PingTest is one src→dst test the matrix will execute. Kind tags the
// test bucket. ICMP and two RDMA test families are scheduled in stages by the
// orchestrator:
//
//   - Kind = RDMAPingSameRail / RDMAPingCrossRail — `rping -c -a
//     <ip>` against an `rping -s -a <ip>` server. Verifies the QP
//     pair establishes end-to-end (RDMA-CM TCP handshake + RDMA
//     write).
//   - Kind = RDMABwSameRail / RDMABwCrossRail — `ib_write_bw -d
//     <dev> -R --report_gbits [<server-ip>]` against an
//     `ib_write_bw -d <dev> -R --report_gbits` server. Measures
//     bandwidth in Gbps.
//
// SrcPod/DstPod carry the actual k8s pod names — needed by the
// orchestrator to issue the SPDY exec against the right pod. SrcNode/
// DstNode are the kubelet's `Spec.NodeName` for each endpoint and are
// used everywhere the matrix is rendered for humans (operators
// recognize hostnames; DaemonSet-generated pod suffixes are noise).
// SrcRDMADev / DstRDMADev carry the per-pod RDMA device names so the
// test runner can pass `-d <dev>` for ib_write_bw.
type PingTest struct {
	Kind        PingTestKind
	SrcPod      string
	DstPod      string
	SrcNode     string
	DstNode     string
	Rail        string // for same-rail tests; "<srcRail>→<dstRail>" for cross
	SrcIP       string
	DstIP       string
	SrcRail     string
	DstRail     string
	SrcIface    string
	DstIface    string
	SrcRDMADev  string
	DstRDMADev  string
	Expectation Expectation
}

// PingTestKind enumerates the buckets the matrix renders into. Order
// follows the orchestrator's staged-execution flow: rping first
// (QP-establishment canary), ib_write_bw second (bandwidth).
type PingTestKind int

const (
	ICMPSameRail PingTestKind = iota
	ICMPCrossRail
	RDMAPingSameRail
	RDMAPingCrossRail
	RDMABwSameRail
	RDMABwCrossRail
)

func (k PingTestKind) IsICMP() bool {
	return k == ICMPSameRail || k == ICMPCrossRail
}

// IsRDMAPing reports whether the test kind is an rping (RDMA-CM QP)
// test.
func (k PingTestKind) IsRDMAPing() bool {
	return k == RDMAPingSameRail || k == RDMAPingCrossRail
}

// IsRDMABw reports whether the test kind is an ib_write_bw bandwidth
// test.
func (k PingTestKind) IsRDMABw() bool {
	return k == RDMABwSameRail || k == RDMABwCrossRail
}

// IsCrossRail reports whether the test kind is one of the cross-rail
// canary variants (regardless of family).
func (k PingTestKind) IsCrossRail() bool {
	return k == ICMPCrossRail || k == RDMAPingCrossRail || k == RDMABwCrossRail
}

// MatrixSkip captures the soft-skip case where fewer than 2 schedulable
// test pods were found — the matrix returns an empty TestSet plus this
// reason for the operator to see.
type MatrixSkip struct {
	Reason string
}

// MatrixPlan is the full set of tests the orchestrator will execute.
// Generated by Plan() from a sorted list of TestPods so the order is
// deterministic regardless of the cluster's pod-listing order.
//
// Pairs without RDMA device names on both ends (the discovery step
// couldn't resolve the secondary network → mlx5_X mapping) are
// silently dropped: there's no `-d <dev>` to pass and the test
// would fail with a confusing error.
type MatrixPlan struct {
	ICMPSameRail    []PingTest
	ICMPCrossRail   []PingTest
	RDMASameRail    []PingTest
	RDMACrossRail   []PingTest
	RDMABwSameRail  []PingTest
	RDMABwCrossRail []PingTest
	Skip            *MatrixSkip
}

// Plan builds the test plan from the given pods. The matrix generation
// is pure (no k8s access) so it's straightforward to unit-test against
// fixture pod lists.
//
//   - Same-rail: for every ordered pod pair (A, B) with A≠B, emit one
//     rping + one ib_write_bw test per rail both endpoints share. This
//     is the data-plane verification proper.
//   - Cross-rail canary: for every ordered pod pair (A, B), emit one
//     rping + one ib_write_bw test pinging A.rail[0] → B.rail[1] when
//     both pods have ≥2 rails. Catches routing misconfigurations that
//     allow accidental rail-to-rail leakage (or, when blocked by
//     design, confirms the isolation).
//   - Soft-skip on <2 pods: no tests are emitted and Skip is set so
//     the caller can render "matrix skipped: only N schedulable test
//     pod(s)".
func Plan(pods []TestPod) MatrixPlan {
	return PlanWithOptions(pods, ModeQuick, config.RoutingDestinationBased)
}

func PlanWithOptions(pods []TestPod, mode Mode, routing string) MatrixPlan {
	if len(pods) < 2 {
		return MatrixPlan{Skip: &MatrixSkip{
			Reason: fmt.Sprintf("only %d schedulable test pod(s) — need ≥2 for a matrix", len(pods)),
		}}
	}

	// Sort pods by name so the plan is stable across runs.
	sorted := make([]TestPod, len(pods))
	copy(sorted, pods)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var plan MatrixPlan
	crossExpectation := crossRailExpectation(mode, routing)
	for i, src := range sorted {
		for j, dst := range sorted {
			if i == j {
				continue
			}
			// Same-rail tests: rails the two endpoints both
			// attach to, where RDMA devices are known on both
			// ends.
			for _, rail := range src.RailOrder {
				srcIP, ok1 := src.IPsByRail[rail]
				dstIP, ok2 := dst.IPsByRail[rail]
				if !ok1 || !ok2 || srcIP == "" || dstIP == "" {
					continue
				}
				srcIface := src.InterfacesByRail[rail]
				dstIface := dst.InterfacesByRail[rail]
				if srcIface == "" || dstIface == "" {
					continue
				}
				base := PingTest{
					SrcPod: src.Name, DstPod: dst.Name,
					SrcNode: src.Node, DstNode: dst.Node,
					Rail: rail, SrcIP: srcIP, DstIP: dstIP,
					SrcRail: rail, DstRail: rail,
					SrcIface: srcIface, DstIface: dstIface,
					Expectation: ExpectRequired,
				}
				icmpT := base
				icmpT.Kind = ICMPSameRail
				plan.ICMPSameRail = append(plan.ICMPSameRail, icmpT)
				srcDev, hasSrcDev := src.RDMADevsByRail[rail]
				dstDev, hasDstDev := dst.RDMADevsByRail[rail]
				if !hasSrcDev || !hasDstDev || srcDev == "" || dstDev == "" {
					continue
				}
				base.SrcRDMADev = srcDev
				base.DstRDMADev = dstDev
				rpingT := base
				rpingT.Kind = RDMAPingSameRail
				plan.RDMASameRail = append(plan.RDMASameRail, rpingT)
				bwT := base
				bwT.Kind = RDMABwSameRail
				plan.RDMABwSameRail = append(plan.RDMABwSameRail, bwT)
			}
			for _, pair := range crossRailPairs(src, dst, mode, i, j) {
				srcRail, dstRail := pair.srcRail, pair.dstRail
				srcIP, ok1 := src.IPsByRail[srcRail]
				dstIP, ok2 := dst.IPsByRail[dstRail]
				srcIface := src.InterfacesByRail[srcRail]
				dstIface := dst.InterfacesByRail[dstRail]
				if !ok1 || !ok2 || srcIP == "" || dstIP == "" || srcIface == "" || dstIface == "" {
					continue
				}
				base := PingTest{
					SrcPod: src.Name, DstPod: dst.Name,
					SrcNode: src.Node, DstNode: dst.Node,
					Rail:  fmt.Sprintf("%s→%s", srcRail, dstRail),
					SrcIP: srcIP, DstIP: dstIP,
					SrcRail: srcRail, DstRail: dstRail,
					SrcIface: srcIface, DstIface: dstIface,
					Expectation: crossExpectation,
				}
				icmpC := base
				icmpC.Kind = ICMPCrossRail
				plan.ICMPCrossRail = append(plan.ICMPCrossRail, icmpC)
				srcDev, hasSrcDev := src.RDMADevsByRail[srcRail]
				dstDev, hasDstDev := dst.RDMADevsByRail[dstRail]
				if !hasSrcDev || !hasDstDev || srcDev == "" || dstDev == "" {
					continue
				}
				base.SrcRDMADev = srcDev
				base.DstRDMADev = dstDev
				rpingC := base
				rpingC.Kind = RDMAPingCrossRail
				plan.RDMACrossRail = append(plan.RDMACrossRail, rpingC)
				bwC := base
				bwC.Kind = RDMABwCrossRail
				plan.RDMABwCrossRail = append(plan.RDMABwCrossRail, bwC)
			}
		}
	}

	return plan
}

type railPair struct {
	srcRail string
	dstRail string
}

func crossRailPairs(src, dst TestPod, mode Mode, srcIdx, dstIdx int) []railPair {
	if len(src.RailOrder) < 2 || len(dst.RailOrder) < 2 {
		return nil
	}
	switch mode {
	case ModeFull, ModeStrict:
		var out []railPair
		for _, srcRail := range src.RailOrder {
			for _, dstRail := range dst.RailOrder {
				if srcRail == dstRail {
					continue
				}
				out = append(out, railPair{srcRail: srcRail, dstRail: dstRail})
			}
		}
		return out
	default:
		// Quick mode runs one non-gating canary per rail mapping, not per
		// pod pair. Deterministically use the first ordered pod pair.
		if srcIdx != 0 || dstIdx != 1 {
			return nil
		}
		var out []railPair
		for _, srcRail := range src.RailOrder {
			for _, dstRail := range dst.RailOrder {
				if srcRail == dstRail {
					continue
				}
				out = append(out, railPair{srcRail: srcRail, dstRail: dstRail})
			}
		}
		return out
	}
}

func crossRailExpectation(mode Mode, routing string) Expectation {
	if mode != ModeStrict {
		return ExpectObserve
	}
	if routing == config.RoutingSourceBased {
		return ExpectRequired
	}
	return ExpectForbidden
}
