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

// PingResult carries the outcome of one src→dst matrix test. The
// matrix currently runs rping and ib_write_bw — fields specific to a
// kind stay zero for tests of the other kind:
//
//   - rping: OK + Err + Stdout/Stderr; BandwidthGbps stays at 0.
//   - ib_write_bw: OK + Err + Stdout/Stderr + BandwidthGbps +
//     MsgRateMpps populated by parseIbWriteBwOutput. MinBandwidthGbps
//     records the configured passing threshold when set.
//
// The struct name is kept for backwards compatibility with the
// historical ICMP-first pipeline; the matrix is RDMA-only now but
// callers and the JSON schema continue to reference PingResult.
type PingResult struct {
	Test             PingTest
	OK               bool
	BandwidthGbps    float64 // ib_write_bw only; 0 when n/a
	MsgRateMpps      float64 // ib_write_bw only; 0 when n/a
	MinBandwidthGbps float64 // ib_write_bw only; 0 when no threshold was applied
	Stdout           string
	Stderr           string
	Err              error
}
