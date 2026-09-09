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

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"k8s.io/client-go/rest"
)

func RunICMP(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, test PingTest) PingResult {
	r := initResult(test)
	if r.Err != nil {
		finalizeICMPRouteError(&r)
		return r
	}
	if test.SrcIface == "" {
		r.Err = fmt.Errorf("icmp needs source interface for rail %q", test.SrcRail)
		finalizeExpectedResult(&r, false, r.Err)
		return r
	}
	// RunMatrix pre-computes and caches ICMP source routes across stages.
	// Keep the standalone runner behavior for direct callers.
	if r.Route.Command == "" {
		r.Route = checkRoute(ctx, restConfig, namespace, pod, container, test)
	}
	if r.Err = sourceRouteValidationError(r.Route, test); r.Err != nil {
		finalizeICMPRouteError(&r)
		return r
	}
	cmd := shellWithTimeout(icmpCommand(test),
		commandTimeoutFor(test, icmpCommandTimeout))
	testLogger(test).V(2).Info("ICMP command", "command", boundedTraceOutput(cmd))
	res, err := kubeclient.ExecInPod(ctx, restConfig, namespace, pod, container, []string{"/bin/sh", "-c", cmd})
	r.Stdout, r.Stderr = res.Stdout, res.Stderr
	finalizeExpectedResult(&r, err == nil, err)
	return r
}

func finalizeICMPRouteError(result *PingResult) {
	if isSourceRouteMismatchError(result.Err) {
		finalizeExpectedResult(result, false, result.Err)
		return
	}
	result.OK = false
	result.ObservedOK = false
}

func icmpCommand(test PingTest) string {
	return fmt.Sprintf("ping -c 1 -W 1 -I %s %s", shellArg(test.SrcIP), shellArg(test.DstIP))
}
