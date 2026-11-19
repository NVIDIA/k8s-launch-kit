// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

const maxText = 512

// Plan freezes expected checks before observation. Missing children must be
// covered by required parent completeness checks derived from retained intent.
type Plan struct {
	input  PlanInput
	checks []CheckSpec
}

// NewPlan rejects incomplete structural coverage, duplicate IDs, unknown areas,
// and reduced runtime matrices. It never infers expectations from live objects.
func NewPlan(input PlanInput) (Plan, error) {
	p := Plan{input: cloneInput(input)}
	if input.Mode != ModeFull && input.Mode != ModeResources {
		return Plan{}, fmt.Errorf("validation mode must be full or resources")
	}
	areas := map[Area]bool{AreaManagement: false, AreaOperator: false, AreaDefinitions: false, AreaSystem: false, AreaTenant: false, AreaDevices: false, AreaSoftware: false, AreaHBN: false, AreaWorkloads: false, AreaPhysical: false}
	ids := map[CheckID]bool{}
	for _, c := range p.input.Checks {
		if _, ok := areas[c.Area]; !ok {
			return Plan{}, fmt.Errorf("unknown validation resource area")
		}
		if !validID(string(c.ID)) || !validID(c.Subject) || ids[c.ID] {
			return Plan{}, fmt.Errorf("validation checks require unique safe IDs and subjects")
		}
		if strings.HasPrefix(string(c.ID), "runtime/") {
			return Plan{}, fmt.Errorf("resource checks cannot use reserved runtime IDs")
		}
		if c.Area == AreaPhysical && !c.FullOnly {
			return Plan{}, fmt.Errorf("physical observations require full mode")
		}
		if c.Required && c.Coverage && (!c.FullOnly || c.Area == AreaPhysical) {
			areas[c.Area] = true
		}
		ids[c.ID] = true
	}
	for _, area := range []Area{AreaManagement, AreaOperator, AreaDefinitions, AreaSystem, AreaTenant, AreaDevices, AreaSoftware, AreaHBN, AreaWorkloads, AreaPhysical} {
		if !areas[area] {
			return Plan{}, fmt.Errorf("missing required completeness obligation for %s", area)
		}
	}
	if err := validateScope(p.input.Runtime); err != nil {
		return Plan{}, err
	}
	p.checks = append([]CheckSpec(nil), p.input.Checks...)
	if input.Mode == ModeResources {
		return p, nil
	}
	for _, d := range p.input.Runtime.Devices {
		p.checks = append(p.checks, runtimeSpec(BGPCheckID(d), "BGP peers — DPU "+string(d)))
	}
	for _, pair := range p.input.Runtime.Routes {
		p.checks = append(p.checks, runtimeSpec(RouteCheckID(pair), "Installed route — DPU "+string(pair.Source)+" to "+string(pair.Destination)))
	}
	for _, pair := range p.input.Runtime.Traffic {
		p.checks = append(p.checks, runtimeSpec(TrafficCheckID(pair, false), "Ordinary traffic — hosts attached to DPUs "+string(pair.Source)+" to "+string(pair.Destination)), runtimeSpec(TrafficCheckID(pair, true), "MTU-sized DF traffic — hosts attached to DPUs "+string(pair.Source)+" to "+string(pair.Destination)))
	}
	if len(p.input.Runtime.Hosts) < 2 {
		p.checks = append(p.checks, runtimeSpec("runtime/cross-host-coverage", "cross-host coverage"))
	}
	return p, nil
}

func runtimeSpec(id CheckID, subject string) CheckSpec {
	return CheckSpec{ID: id, Area: AreaHBN, Subject: subject, Required: true, FullOnly: true, CohortBound: true}
}

func validID(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validateScope(s RuntimeScope) error {
	if _, err := hbn.MinimumRuntimeTrafficDuration(len(s.Hosts)); err != nil {
		return fmt.Errorf("invalid selected host count: %w", err)
	}
	if len(s.Devices) != len(s.Hosts) {
		return fmt.Errorf("HBN requires one selected device per selected physical host")
	}
	devices, hosts := map[DeviceID]bool{}, map[HostID]bool{}
	for _, d := range s.Devices {
		if !validID(string(d)) || devices[d] {
			return fmt.Errorf("invalid or duplicate selected device")
		}
		devices[d] = true
	}
	for _, h := range s.Hosts {
		if !validID(string(h)) || hosts[h] {
			return fmt.Errorf("invalid or duplicate selected host")
		}
		hosts[h] = true
	}
	routes, traffic := map[DirectedDevicePair]bool{}, map[DirectedHostPair]bool{}
	for _, p := range s.Routes {
		if !devices[p.Source] || !devices[p.Destination] || p.Source == p.Destination || routes[p] {
			return fmt.Errorf("invalid directed route scope")
		}
		routes[p] = true
	}
	for _, p := range s.Traffic {
		if !hosts[p.Source] || !hosts[p.Destination] || p.Source == p.Destination || traffic[p] {
			return fmt.Errorf("invalid directed traffic scope")
		}
		traffic[p] = true
	}
	n := len(s.Hosts)
	if len(routes) != n*(n-1) || len(traffic) != n*(n-1) {
		return fmt.Errorf("runtime scope must contain every selected directed pair")
	}
	return nil
}

func cloneScope(s RuntimeScope) RuntimeScope {
	return RuntimeScope{Devices: append([]DeviceID(nil), s.Devices...), Hosts: append([]HostID(nil), s.Hosts...), Routes: append([]DirectedDevicePair(nil), s.Routes...), Traffic: append([]DirectedHostPair(nil), s.Traffic...)}
}
func cloneInput(i PlanInput) PlanInput {
	return PlanInput{Mode: i.Mode, Checks: append([]CheckSpec(nil), i.Checks...), Runtime: cloneScope(i.Runtime)}
}
func (p Plan) Input() PlanInput           { return cloneInput(p.input) }
func (p Plan) Checks() []CheckSpec        { return append([]CheckSpec(nil), p.checks...) }
func (p Plan) Mode() Mode                 { return p.input.Mode }
func (p Plan) RuntimeScope() RuntimeScope { return cloneScope(p.input.Runtime) }

func BGPCheckID(d DeviceID) CheckID { return CheckID("runtime/bgp/" + url.PathEscape(string(d))) }
func RouteCheckID(p DirectedDevicePair) CheckID {
	return CheckID("runtime/route/" + url.PathEscape(string(p.Source)) + "/" + url.PathEscape(string(p.Destination)))
}
func TrafficCheckID(p DirectedHostPair, df bool) CheckID {
	suffix := "/ordinary"
	if df {
		suffix = "/df"
	}
	return CheckID("runtime/traffic/" + url.PathEscape(string(p.Source)) + "/" + url.PathEscape(string(p.Destination)) + suffix)
}

// RuntimeBudget applies validation's total-stage policy, independently of bounded
// per-request timeouts. Explicit zero follows automatic policy; its presence is
// still rejected in resource-only mode.
func RuntimeBudget(hosts int, requested time.Duration, explicit bool, mode Mode) (time.Duration, error) {
	minimum, err := hbn.MinimumRuntimeTrafficDuration(hosts)
	if err != nil {
		return 0, err
	}
	if requested < 0 {
		return 0, fmt.Errorf("connectivity timeout cannot be negative")
	}
	if mode == ModeResources {
		if explicit || requested != 0 {
			return 0, fmt.Errorf("connectivity timeout is unused in resource-only mode")
		}
		return 0, nil
	}
	if mode != ModeFull {
		return 0, fmt.Errorf("invalid validation mode")
	}
	if requested > 0 {
		if requested <= minimum {
			return 0, fmt.Errorf("connectivity timeout must exceed %s packet-spacing lower bound for %d hosts", minimum, hosts)
		}
		return requested, nil
	}
	// hosts has already been bounded to [1,256] before multiplication.
	n := time.Duration(hosts)
	return 5*time.Minute + 2*time.Minute*n + 30*time.Second*n*(n-1), nil
}
