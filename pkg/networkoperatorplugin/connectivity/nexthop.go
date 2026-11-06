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
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"k8s.io/client-go/rest"
)

// NextHopDiagnostic is an advisory neighbor-table observation after a failed
// test. It never changes the test verdict or proves end-to-end reachability.
type NextHopDiagnostic struct {
	Address    string
	Interface  string
	Gateway    bool
	Status     string
	State      string
	Message    string
	Command    []string
	Output     string
	Error      string `json:",omitempty"`
	ObservedAt time.Time
}

type nextHopKey struct {
	namespace, pod, container, address, device string
}

// annotateNextHops owns only the NextHop field of these completed stage results.
// Observations are shared within this stage, never across protocol stages.
func annotateNextHops(ctx context.Context, config *rest.Config, namespaces, containers map[string]string,
	results []PingResult, exec execInPodFunc) {
	cache := map[nextHopKey]NextHopDiagnostic{}
	for i := range results {
		r := &results[i]
		if r.ObservedOK || r.BandwidthGbps > 0 || !r.Route.OK || r.Route.Err != "" ||
			r.Route.Dev == "" || r.Route.Dev != r.Test.SrcIface || r.Test.sourceRouteErr != nil {
			continue
		}
		diagnostic := nextHopFromRoute(r.Route, r.Test.DstIP)
		key := nextHopKey{namespaces[r.Test.SrcPod], r.Test.SrcPod, containers[r.Test.SrcPod], diagnostic.Address, diagnostic.Interface}
		if diagnostic.Error == "" {
			if cached, ok := cache[key]; ok {
				// Gateway describes this route, rather than the shared neighbor entry.
				gateway := diagnostic.Gateway
				diagnostic = cached
				diagnostic.Gateway = gateway
			} else {
				diagnostic = inspectNextHop(ctx, config, key, diagnostic, exec)
				cache[key] = diagnostic
			}
		}
		r.NextHop = &diagnostic
		testLogger(r.Test).V(1).Info("next-hop neighbor observation",
			"address", diagnostic.Address, "device", diagnostic.Interface,
			"status", diagnostic.Status, "state", diagnostic.State, "error", diagnostic.Error)
		testLogger(r.Test).V(2).Info("next-hop neighbor evidence", "command", diagnostic.Command, "output", diagnostic.Output)
	}
}

func nextHopFromRoute(route RouteCheck, destination string) NextHopDiagnostic {
	d := NextHopDiagnostic{Address: destination, Interface: route.Dev, Status: "unverified"}
	dst, err := netip.ParseAddr(destination)
	if err != nil {
		d.Error = "invalid destination IP"
	} else {
		fields := strings.Fields(route.Output)
		for i, field := range fields {
			if field != "via" {
				continue
			}
			d.Gateway = true
			j := i + 1
			if j < len(fields) && (fields[j] == "inet" || fields[j] == "inet6") {
				j++
			}
			if j >= len(fields) {
				d.Error = "route has no gateway address"
				break
			}
			gateway, parseErr := netip.ParseAddr(fields[j])
			if parseErr != nil || gateway.Is4() != dst.Is4() {
				d.Error = "route has an invalid or incompatible gateway address"
			} else {
				d.Address = gateway.String()
			}
			break
		}
	}
	if d.Error != "" {
		d.Message = "next-hop observation unavailable: " + d.Error
	}
	return d
}

func inspectNextHop(ctx context.Context, config *rest.Config, key nextHopKey, d NextHopDiagnostic, exec execInPodFunc) NextHopDiagnostic {
	d.ObservedAt = time.Now().UTC()
	d.Command = []string{"ip", "-j", "neigh", "show", "to", d.Address, "dev", d.Interface}
	switch {
	case ctx.Err() != nil:
		d.Error = ctx.Err().Error()
	case key.namespace == "" || key.pod == "" || key.container == "":
		d.Error = "missing test pod namespace or diagnostic container"
	default:
		probeCtx, cancel := context.WithTimeout(ctx, nextHopTimeout)
		defer cancel()
		res, err := exec(probeCtx, config, key.namespace, key.pod, key.container, d.Command)
		d.Output = boundedTraceOutput(strings.TrimSpace(res.Stdout + "\n" + res.Stderr))
		switch {
		case err != nil:
			d.Error = boundedTraceOutput(err.Error())
		case probeCtx.Err() != nil:
			d.Error = probeCtx.Err().Error()
		case len(res.Stdout) > traceOutputLimit:
			d.Error = "neighbor output exceeds diagnostic limit"
		default:
			d.Status, d.State, d.Message = classifyNeighbor(res.Stdout, d.Address, d.Interface)
		}
	}
	if d.Error != "" {
		d.Message = "next-hop observation unavailable: " + d.Error
	}
	return d
}

type neighborEntry struct {
	Destination string   `json:"dst"`
	Device      string   `json:"dev"`
	LinkAddress string   `json:"lladdr"`
	State       []string `json:"state"`
}

func classifyNeighbor(output, address, device string) (status, state, message string) {
	var entries []neighborEntry
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return "unverified", "", "neighbor output is not valid ip JSON"
	}
	if len(entries) == 0 {
		return "unverified", "", "no neighbor entry; absence alone does not establish resolution failure"
	}
	if len(entries) != 1 {
		return "unverified", "", "ambiguous neighbor entries"
	}
	entry := entries[0]
	got, err := netip.ParseAddr(entry.Destination)
	want, wantErr := netip.ParseAddr(address)
	if err != nil || wantErr != nil || got != want || (entry.Device != "" && entry.Device != device) {
		return "unverified", "", "neighbor entry does not match the selected next hop"
	}
	state = strings.Join(entry.State, ",")
	if len(entry.State) != 1 {
		return "unverified", state, "neighbor state is missing or ambiguous"
	}
	switch entry.State[0] {
	case "FAILED":
		return "unresolved", state, "next-hop address resolution failed (neighbor state FAILED); this observation does not identify the underlying fabric cause"
	case "INCOMPLETE":
		return "pending", state, "next-hop address resolution is incomplete; no final resolution verdict"
	case "REACHABLE", "STALE", "DELAY", "PROBE", "PERMANENT", "NOARP":
		if entry.LinkAddress != "" {
			return "resolved", state, "next-hop link-layer mapping exists; this does not prove end-to-end connectivity"
		}
	}
	return "unverified", state, fmt.Sprintf("neighbor state %q does not establish a usable link-layer mapping", state)
}

func nextHopResults(results []PingResult) []PingResult {
	var out []PingResult
	for _, r := range results {
		if r.NextHop != nil {
			out = append(out, r)
		}
	}
	return out
}
