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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const traceOutputLimit = 4096

func connectivityLogger() logr.Logger {
	return log.Log.WithName("connectivity")
}

func testLogger(test PingTest) logr.Logger {
	return connectivityLogger().WithValues(
		"family", resultFamily(test.Kind),
		"crossRail", test.Kind.IsCrossRail(),
		"expectation", test.Expectation,
		"srcPod", test.SrcPod,
		"srcNode", test.SrcNode,
		"srcRail", test.SrcRail,
		"srcIP", test.SrcIP,
		"srcIface", test.SrcIface,
		"srcRDMADev", test.SrcRDMADev,
		"dstPod", test.DstPod,
		"dstNode", test.DstNode,
		"dstRail", test.DstRail,
		"dstIP", test.DstIP,
		"dstIface", test.DstIface,
		"dstRDMADev", test.DstRDMADev,
	)
}

func boundedTraceOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= traceOutputLimit {
		return value
	}
	cut := traceOutputLimit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... [truncated %d bytes]", value[:cut], len(value)-cut)
}

func remainingTimeout(ctx context.Context) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "unbounded"
	}
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	return remaining.Round(time.Millisecond).String()
}
