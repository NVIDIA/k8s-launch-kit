// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"k8s.io/client-go/rest"
)

// planGatewayTests covers every source endpoint independently of pod pairs.
// Discovery is deferred to the runner so it shares the execution deadline and
// contributes an explicit failed/skipped row when a gateway cannot be measured.
func planGatewayTests(pods []TestPod) []PingTest {
	out := make([]PingTest, 0)
	seen := make(map[string]bool)
	for _, pod := range pods {
		for _, rail := range pod.RailOrder {
			key := strings.Join([]string{pod.Namespace, pod.Name, rail, pod.IPsByRail[rail], pod.InterfacesByRail[rail]}, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, PingTest{
				Kind: Gateway, SrcNamespace: pod.Namespace, SrcPod: pod.Name,
				SrcNetwork: pod.NetworksByRail[rail], gatewayEndpointErr: pod.GatewayErrorsByRail[rail],
				sourceSelection: pod.networkSelection,
				SrcNode:         pod.Node, Rail: rail, SrcRail: rail,
				SrcIP: pod.IPsByRail[rail], SrcIface: pod.InterfacesByRail[rail],
				Expectation: ExpectRequired,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return strings.Join([]string{a.SrcNamespace, a.SrcPod, a.SrcRail, a.SrcIP, a.SrcIface}, "\x00") <
			strings.Join([]string{b.SrcNamespace, b.SrcPod, b.SrcRail, b.SrcIP, b.SrcIface}, "\x00")
	})
	return out
}

func gatewayContainer(pods []testPodWithDS, test PingTest) string {
	for _, pod := range pods {
		if pod.pod.Namespace == test.SrcNamespace && pod.pod.Name == test.SrcPod {
			return pod.ref.ICMPContainer
		}
	}
	return ""
}

// gatewayFromRoutes accepts only the JSON representation requested from iproute2.
// A genuinely on-link interface has no next hop. Unsupported indirection and
// malformed/ambiguous evidence fail rather than being treated as that skip.
func gatewayFromRoutes(raw, iface, source string) (string, error) {
	src, err := netip.ParseAddr(source)
	if err != nil || !src.IsGlobalUnicast() {
		return "", fmt.Errorf("invalid gateway source IP %q", source)
	}
	var routes []*struct {
		Dev      string          `json:"dev"`
		Dst      string          `json:"dst"`
		Gateway  json.RawMessage `json:"gateway"`
		Via      json.RawMessage `json:"via"`
		Nexthops json.RawMessage `json:"nexthops"`
		NHID     json.RawMessage `json:"nhid"`
	}
	if err := json.Unmarshal([]byte(raw), &routes); err != nil || routes == nil {
		return "", fmt.Errorf("malformed gateway route JSON for interface %q", iface)
	}
	gateways := map[string]bool{}
	for _, route := range routes {
		if route == nil {
			return "", fmt.Errorf("malformed null gateway route for interface %q", iface)
		}
		// iproute2 omits dev when the query already filters by that device.
		if route.Dev != "" && route.Dev != iface {
			return "", fmt.Errorf("gateway route evidence has device %q, expected %q", route.Dev, iface)
		}
		if route.Via != nil || route.Nexthops != nil || route.NHID != nil {
			return "", fmt.Errorf("unsupported gateway route indirection for interface %q (via/nexthops/nhid)", iface)
		}
		if route.Gateway == nil {
			if route.Dst == "" {
				return "", fmt.Errorf("malformed gateway route without destination for interface %q", iface)
			}
			continue
		}
		var rawGateway string
		if err := json.Unmarshal(route.Gateway, &rawGateway); err != nil || rawGateway == "" {
			return "", fmt.Errorf("malformed gateway value for interface %q", iface)
		}
		gateway, err := validateGatewayAddress(rawGateway, src)
		if err != nil {
			return "", err
		}
		gateways[gateway.String()] = true
	}
	if len(gateways) > 1 {
		return "", fmt.Errorf("ambiguous gateway route evidence: %d distinct gateways on interface %q", len(gateways), iface)
	}
	for gateway := range gateways {
		return gateway, nil
	}
	return "", nil
}

func validateGatewayAddress(raw string, source netip.Addr) (netip.Addr, error) {
	gateway, err := netip.ParseAddr(raw)
	if err != nil || gateway.Zone() != "" || gateway.Is4() != source.Is4() ||
		(!gateway.IsGlobalUnicast() && !gateway.IsLinkLocalUnicast()) || gateway == source {
		return netip.Addr{}, fmt.Errorf("invalid gateway %q for source %q", raw, source)
	}
	return gateway, nil
}

func gatewayRouteCommand(test PingTest) string {
	family := "-4"
	if ipv6Flag(test.SrcIP, "yes") != "" {
		family = "-6"
	}
	// Single quotes prevent shell expansion even for unusual interface names.
	iface := "'" + strings.ReplaceAll(test.SrcIface, "'", "'\"'\"'") + "'"
	return fmt.Sprintf("ip -j %s route show table all dev %s", family, iface)
}

var gatewayInterfaceName = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`)

func runGateway(ctx context.Context, cfg *rest.Config, namespace, pod, container string, test PingTest, exec execInPodFunc, target gatewayTarget) PingResult {
	r := initResult(test)
	r.GatewayEvidence = target.Evidence
	if test.gatewayEndpointErr != nil {
		r.Err = test.gatewayEndpointErr
		return r
	}
	if namespace == "" || pod == "" || container == "" || test.SrcIface == "" {
		r.Err = fmt.Errorf("gateway check needs source namespace, pod, helper container and interface for rail %q", test.SrcRail)
		return r
	}
	if !gatewayInterfaceName.MatchString(test.SrcIface) {
		r.Err = fmt.Errorf("unsupported gateway source interface name %q", test.SrcIface)
		return r
	}
	if src, err := netip.ParseAddr(test.SrcIP); err != nil || !src.IsGlobalUnicast() {
		r.Err = fmt.Errorf("invalid gateway source IP %q", test.SrcIP)
		return r
	}
	gateway := target.Gateway
	if gateway == "" {
		queryCtx, cancel := context.WithTimeout(ctx, routeCheckTimeout)
		res, err := exec(queryCtx, cfg, namespace, pod, container, []string{"/bin/sh", "-c", gatewayRouteCommand(test)})
		if err == nil {
			err = queryCtx.Err()
		}
		cancel()
		r.GatewayEvidence += "\n" + gatewayRouteCommand(test) + "\n" + strings.TrimSpace(res.Stdout+"\n"+res.Stderr)
		r.Stderr = res.Stderr
		if err != nil {
			r.Err = fmt.Errorf("discover gateway on interface %q: %w", test.SrcIface, err)
			return r
		}
		gateway, err = gatewayFromRoutes(res.Stdout, test.SrcIface, test.SrcIP)
		if err != nil {
			r.Err = err
			return r
		}
	}
	if gateway == "" {
		r.SkipReason = "no configured gateway on secondary interface " + test.SrcIface
		return r
	}
	test.DstIP = gateway
	r.Test = test
	r.Route.Command = routeCommand(test)
	addr, _ := netip.ParseAddr(gateway)
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		// IPv6 link-local addresses require an explicit zone. oif scopes only
		// this lookup; global gateway lookups retain natural source routing.
		r.Route.Command += " oif '" + strings.ReplaceAll(test.SrcIface, "'", "'\"'\"'") + "'"
	}
	routeCtx, routeCancel := context.WithTimeout(ctx, routeCheckTimeout)
	res, err := exec(routeCtx, cfg, namespace, pod, container, []string{"/bin/sh", "-c", r.Route.Command})
	if err == nil {
		err = routeCtx.Err()
	}
	routeCancel()
	r.Route.Output = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if err != nil {
		r.Route.Err = err.Error()
		r.Err = fmt.Errorf("qualify gateway source route: %w", err)
		return r
	}
	if match := routeDevRe.FindStringSubmatch(r.Route.Output); len(match) == 2 {
		r.Route.Dev, r.Route.OK = match[1], true
	}
	if r.Err = sourceRouteValidationError(r.Route, test); r.Err != nil {
		return r
	}
	if strings.Contains(" "+r.Route.Output+" ", " via ") {
		r.Err = fmt.Errorf("gateway %s is not directly reachable on source interface %q (route: %s)", gateway, test.SrcIface, r.Route.Output)
		return r
	}
	probe := test
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		probe.DstIP += "%" + test.SrcIface
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, icmpCommandTimeout)
	defer probeCancel()
	res, err = exec(probeCtx, cfg, namespace, pod, container, []string{"/bin/sh", "-c", shellWithTimeout(icmpCommand(probe), icmpCommandTimeout)})
	if err == nil {
		err = probeCtx.Err()
	}
	r.Stdout, r.Stderr = res.Stdout, res.Stderr
	r.OK, r.ObservedOK = err == nil, err == nil
	if err != nil {
		r.Err = fmt.Errorf("gateway ICMP reachability failed (does not establish whether neighbor resolution failed): %w", err)
	}
	return r
}
