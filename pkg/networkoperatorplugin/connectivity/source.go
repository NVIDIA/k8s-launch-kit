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

var routeDevRe = regexp.MustCompile(`(?:^|\s)dev\s+(\S+)`)

const routeSkipMarker = "__L8K_ROUTE_SKIP__"

func initResult(test PingTest) PingResult {
	exp := test.Expectation
	if exp == "" {
		exp = ExpectRequired
	}
	return PingResult{Test: test, Expectation: exp}
}

func shellArg(s string) string {
	return strconv.Quote(s)
}

func shellWithTimeout(command string, timeout time.Duration) string {
	seconds := int(timeout.Seconds())
	if seconds <= 0 {
		seconds = 1
	}
	return fmt.Sprintf(
		`%s & pid=$!; (`+
			`sleep %d; `+
			`kill -TERM "$pid" 2>/dev/null; `+
			`sleep 2; `+
			`kill -KILL "$pid" 2>/dev/null`+
			`) & watchdog=$!; `+
			`wait "$pid"; rc=$?; `+
			`kill "$watchdog" 2>/dev/null; `+
			`wait "$watchdog" 2>/dev/null; `+
			`exit "$rc"`,
		command, seconds)
}

func commandTimeoutFor(test PingTest, defaultTimeout time.Duration) time.Duration {
	if test.Expectation == ExpectForbidden || test.Expectation == ExpectObserve {
		return 5 * time.Second
	}
	return defaultTimeout
}

func rdmaSettleDelayFor(test PingTest) time.Duration {
	if test.Expectation == ExpectForbidden {
		return 1 * time.Second
	}
	return rdmaServerSettleDelay
}

func checkRoute(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, test PingTest) RouteCheck {
	cmd := fmt.Sprintf(
		`ipcmd=""; for p in ip /sbin/ip /usr/sbin/ip /bin/ip /usr/bin/ip; do `+
			`if command -v "$p" >/dev/null 2>&1 || [ -x "$p" ]; then ipcmd="$p"; break; fi; `+
			`done; `+
			`if [ -z "$ipcmd" ]; then echo %s; exit 0; fi; `+
			`"$ipcmd" -o route get %s from %s`,
		shellArg(routeSkipMarker), shellArg(test.DstIP), shellArg(test.SrcIP))
	out := RouteCheck{Command: cmd}
	if test.SrcIP == "" || test.DstIP == "" {
		out.Err = "missing source or destination IP"
		return out
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := kubeclient.ExecInPod(tctx, restConfig, namespace, pod, container, []string{"/bin/sh", "-c", cmd})
	out.Output = strings.TrimSpace(strings.Join([]string{res.Stdout, res.Stderr}, "\n"))
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if strings.Contains(out.Output, routeSkipMarker) {
		out.Err = "ip command not found in validation container"
		return out
	}
	if m := routeDevRe.FindStringSubmatch(out.Output); len(m) == 2 {
		out.Dev = m[1]
	}
	out.OK = out.Dev != ""
	return out
}

func routeMatchesSourceInterface(route RouteCheck, test PingTest) bool {
	return route.OK && route.Dev == test.SrcIface
}

func routeMismatch(route RouteCheck, test PingTest) bool {
	return route.OK && !routeMatchesSourceInterface(route, test)
}

func finalizeExpectedResult(r *PingResult, observedOK bool, observedErr error) {
	r.ObservedOK = observedOK
	switch r.Expectation {
	case ExpectObserve:
		r.OK = true
		r.Err = observedErr
	case ExpectForbidden:
		r.OK = !observedOK
		if observedOK {
			r.Err = fmt.Errorf("cross-rail traffic succeeded but profile routing expects isolation")
		} else {
			r.Err = nil
		}
	default:
		r.OK = observedOK
		r.Err = observedErr
	}
}
