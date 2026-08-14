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

import "encoding/json"

type RouteCheck struct {
	Command string
	Output  string
	Dev     string
	OK      bool
	Err     string
}

// PingResult carries the outcome of one src→dst matrix test. The
// matrix runs ICMP, rping, host-memory ib_write_bw, and GPUDirect DMA-BUF;
// fields specific to a kind stay zero for other kinds:
//
//   - rping: OK + Err + Stdout/Stderr; BandwidthGbps stays at 0.
//   - both ib_write_bw families: OK + Err + Stdout/Stderr + BandwidthGbps +
//     MsgRateMpps populated by parseIbWriteBwOutput. MinBandwidthGbps
//     records the configured passing threshold when set.
//
// The struct name is kept for backwards compatibility with the
// historical ICMP-first pipeline; callers and the JSON schema continue
// to reference PingResult.
type PingResult struct {
	Test             PingTest
	OK               bool
	ObservedOK       bool
	Expectation      Expectation
	Route            RouteCheck
	BandwidthGbps    float64 // ib_write_bw families only; 0 when n/a
	MsgRateMpps      float64 // ib_write_bw families only; 0 when n/a
	MinBandwidthGbps float64 // ib_write_bw families only; 0 when no threshold was applied
	Stdout           string
	Stderr           string
	// ServerLog is populated for failed RDMA batch cells before the
	// temporary in-pod server log is removed during cleanup.
	ServerLog string `json:"ServerLog,omitempty"`
	Err       error  `json:"-"`
}

func (r PingResult) MarshalJSON() ([]byte, error) {
	type alias PingResult
	errorText := ""
	if r.Err != nil {
		errorText = r.Err.Error()
	}
	return json.Marshal(struct {
		alias
		Family string `json:"Family"`
		Error  string `json:"Error,omitempty"`
	}{alias: alias(r), Family: resultFamily(r.Test.Kind), Error: errorText})
}

func resultFamily(kind PingTestKind) string {
	switch {
	case kind.IsICMP():
		return "icmp"
	case kind.IsRDMAPing():
		return "rping"
	case kind.IsRDMABw():
		return "ib_write_bw"
	case kind.IsGPUDirectDMABuf():
		return "gpudirect_dmabuf"
	default:
		return "unknown"
	}
}
