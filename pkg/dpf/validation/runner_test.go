// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
)

func TestValidation(t *testing.T) {
	RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "DPF validation coverage")
}

type fakeObserver struct {
	input                    PlanInput
	resources                func(context.Context, Progress) (ResourceSnapshot, error)
	open                     func(context.Context, Progress) (RuntimeSession, error)
	resourceCalls, openCalls int
	session                  *fakeSession
}

func (f *fakeObserver) ObserveResources(c context.Context, p Progress) (ResourceSnapshot, error) {
	f.resourceCalls++
	if f.resources != nil {
		return f.resources(c, p)
	}
	s := ResourceSnapshot{}
	for _, check := range f.input.Checks {
		if f.input.Mode != ModeResources || !check.FullOnly {
			p(ProgressEvent{Check: check, Phase: Running})
			o := Outcome{ID: check.ID, State: Passed}
			s.Checks = append(s.Checks, o)
			p(ProgressEvent{Check: check, Phase: Completed, Outcome: o})
		}
	}
	return s, nil
}
func (f *fakeObserver) OpenRuntime(c context.Context, p Progress) (RuntimeSession, error) {
	f.openCalls++
	if f.open != nil {
		return f.open(c, p)
	}
	return f.session, nil
}

type fakeSession struct {
	scope                                           RuntimeScope
	bgp                                             func(context.Context, DeviceID) (BGPObservation, error)
	route                                           func(context.Context, DirectedDevicePair) (RouteObservation, error)
	traffic                                         func(context.Context, DirectedHostPair) (TrafficObservation, error)
	verify                                          func(context.Context) error
	bgpCalls, routeCalls, trafficCalls, verifyCalls int
}

func (f *fakeSession) Scope() RuntimeScope { return cloneScope(f.scope) }
func (f *fakeSession) ObserveBGP(c context.Context, d DeviceID) (BGPObservation, error) {
	f.bgpCalls++
	if f.bgp != nil {
		return f.bgp(c, d)
	}
	return BGPObservation{Outcome: Outcome{State: Passed}}, nil
}
func (f *fakeSession) ObserveRoute(c context.Context, p DirectedDevicePair) (RouteObservation, error) {
	f.routeCalls++
	if f.route != nil {
		return f.route(c, p)
	}
	return RouteObservation{Outcome: Outcome{State: Passed}}, nil
}
func (f *fakeSession) ProbeTraffic(c context.Context, p DirectedHostPair) (TrafficObservation, error) {
	f.trafficCalls++
	if f.traffic != nil {
		return f.traffic(c, p)
	}
	return TrafficObservation{Ordinary: Outcome{State: Passed}, DF: Outcome{State: Passed}}, nil
}
func (f *fakeSession) VerifyCohort(c context.Context) error {
	f.verifyCalls++
	if f.verify != nil {
		return f.verify(c)
	}
	return nil
}

func fixture(mode Mode, n int) (PlanInput, *fakeObserver) {
	i := PlanInput{Mode: mode}
	for _, area := range []Area{AreaManagement, AreaOperator, AreaDefinitions, AreaSystem, AreaTenant, AreaDevices, AreaSoftware, AreaHBN, AreaWorkloads, AreaPhysical} {
		i.Checks = append(i.Checks, CheckSpec{ID: CheckID(area) + "/coverage", Area: area, Subject: string(area), Required: true, Coverage: true, FullOnly: area == AreaPhysical})
	}
	for k := 0; k < n; k++ {
		identity := string(rune('a' + k))
		if k >= 26 {
			identity = fmt.Sprintf("selected-%03d", k)
		}
		i.Runtime.Devices = append(i.Runtime.Devices, DeviceID(identity))
		i.Runtime.Hosts = append(i.Runtime.Hosts, HostID(identity))
	}
	for _, a := range i.Runtime.Devices {
		for _, b := range i.Runtime.Devices {
			if a != b {
				i.Runtime.Routes = append(i.Runtime.Routes, DirectedDevicePair{Source: a, Destination: b})
			}
		}
	}
	for _, a := range i.Runtime.Hosts {
		for _, b := range i.Runtime.Hosts {
			if a != b {
				i.Runtime.Traffic = append(i.Runtime.Traffic, DirectedHostPair{Source: a, Destination: b})
			}
		}
	}
	f := &fakeObserver{input: cloneInput(i), session: &fakeSession{scope: cloneScope(i.Runtime)}}
	return i, f
}
func runner(i PlanInput, f Observer, o Options) *Runner {
	p, e := NewPlan(i)
	Expect(e).NotTo(HaveOccurred())
	r, e := NewRunner(p, f, o)
	Expect(e).NotTo(HaveOccurred())
	return r
}
func healthySnapshot(i PlanInput) ResourceSnapshot {
	s := ResourceSnapshot{}
	for _, c := range i.Checks {
		if i.Mode != ModeResources || !c.FullOnly {
			s.Checks = append(s.Checks, Outcome{ID: c.ID, State: Passed})
		}
	}
	return s
}
func check(result Result, id CheckID) CheckResult {
	for _, c := range result.Checks {
		if c.ID == id {
			return c
		}
	}
	ginkgo.Fail("missing check " + string(id))
	return CheckResult{}
}

var _ = ginkgo.Describe("Frozen expected coverage", func() {
	ginkgo.It("accepts long real resource identities without truncation and keeps opaque runtime pair IDs distinct", func() {
		i, _ := fixture(ModeFull, 2)
		i.Checks[0].ID = CheckID("customresourcedefinition/" + strings.Repeat("a", 253))
		i.Checks[0].Subject = strings.Repeat("b", 500)
		p, e := NewPlan(i)
		Expect(e).NotTo(HaveOccurred())
		Expect(p.Checks()[0].ID).To(Equal(i.Checks[0].ID))
		Expect(RouteCheckID(DirectedDevicePair{Source: "a/b", Destination: "c"})).NotTo(Equal(RouteCheckID(DirectedDevicePair{Source: "a", Destination: "b/c"})))
		i.Checks[0].ID = "unsafe\nidentity"
		_, e = NewPlan(i)
		Expect(e).To(HaveOccurred())
	})
	ginkgo.It("owns independent copies of every input and accessor slice", func() {
		i, f := fixture(ModeFull, 2)
		p, e := NewPlan(i)
		Expect(e).NotTo(HaveOccurred())
		i.Checks[0].ID = "changed"
		i.Runtime.Devices[0] = "changed"
		copy := p.Input()
		copy.Checks[0].ID = "also-changed"
		copy.Runtime.Hosts[0] = "changed"
		checks := p.Checks()
		checks[0].ID = "changed"
		r, e := NewRunner(p, f, Options{})
		Expect(e).NotTo(HaveOccurred())
		result := r.Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictPassed))
		Expect(result.Scope.Devices[0]).To(Equal(DeviceID("a")))
	})
	ginkgo.It("rejects empty plans, unknown regions, absent completeness obligations and reduced matrices", func() {
		i, _ := fixture(ModeFull, 2)
		_, e := NewPlan(PlanInput{})
		Expect(e).To(HaveOccurred())
		bad := cloneInput(i)
		bad.Checks = bad.Checks[1:]
		_, e = NewPlan(bad)
		Expect(e).To(HaveOccurred())
		bad = cloneInput(i)
		bad.Checks[0].Area = "invented"
		_, e = NewPlan(bad)
		Expect(e).To(HaveOccurred())
		bad = cloneInput(i)
		bad.Runtime.Routes = bad.Runtime.Routes[:1]
		_, e = NewPlan(bad)
		Expect(e).To(HaveOccurred())
		bad = cloneInput(i)
		bad.Checks = append(bad.Checks, bad.Checks[0])
		_, e = NewPlan(bad)
		Expect(e).To(HaveOccurred())
	})
	ginkgo.It("never passes when any required expected ID is omitted", func() {
		i, f := fixture(ModeResources, 2)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			s.Checks = s.Checks[1:]
			return s, nil
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictIncomplete))
		Expect(check(result, i.Checks[0].ID).Outcome.Reason).To(Equal(ReasonIncomplete))
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("rejects duplicate and unsolicited observations", func() {
		for _, duplicate := range []bool{false, true} {
			i, f := fixture(ModeResources, 2)
			f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
				s := healthySnapshot(i)
				o := Outcome{ID: "unplanned", State: Passed}
				if duplicate {
					o = s.Checks[0]
				}
				s.Checks = append(s.Checks, o)
				return s, nil
			}
			result := runner(i, f, Options{}).Run(context.Background())
			Expect(result.Verdict).To(Equal(VerdictIncomplete))
			Expect(result.Fatal).To(Equal(FatalObservation))
		}
	})
	ginkgo.It("does not normalize a contradictory or unknown observation into pass", func() {
		for _, o := range []Outcome{{State: Passed, Reason: ReasonDenied}, {State: "unknown"}, {State: Skipped, Reason: ReasonInapplicable}} {
			i, f := fixture(ModeResources, 2)
			f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
				s := healthySnapshot(i)
				o.ID = s.Checks[0].ID
				s.Checks[0] = o
				return s, nil
			}
			result := runner(i, f, Options{}).Run(context.Background())
			Expect(result.Verdict).To(Equal(VerdictIncomplete))
			Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Unverified))
		}
	})
})

var _ = ginkgo.Describe("Independent scheduling and modes", func() {
	ginkgo.It("applies joined drift poisoning consistently at resource, session-open and final-fence boundaries", func() {
		for _, stage := range []string{"resources", "open", "final"} {
			for _, reverse := range []bool{false, true} {
				i, f := fixture(ModeFull, 2)
				i.Checks[5].CohortBound = true
				f.input = cloneInput(i)
				access := &ObservationError{Category: FatalAccess}
				identity := &ObservationError{Category: FatalIdentity}
				joined := errors.Join(access, identity)
				if reverse {
					joined = errors.Join(identity, access)
				}
				switch stage {
				case "resources":
					f.resources = func(context.Context, Progress) (ResourceSnapshot, error) { return healthySnapshot(i), joined }
				case "open":
					f.open = func(context.Context, Progress) (RuntimeSession, error) { return nil, joined }
				case "final":
					f.session.verify = func(context.Context) error { return joined }
				}
				result := runner(i, f, Options{}).Run(context.Background())
				Expect(result.Fatal).To(Equal(FatalIdentity))
				Expect(check(result, i.Checks[5].ID).Outcome.Reason).To(Equal(ReasonIdentityDrift))
				if stage == "resources" {
					Expect(f.openCalls).To(BeZero())
					Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Unverified))
				} else {
					Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Passed))
				}
				if stage == "final" {
					Expect(check(result, BGPCheckID("a")).PriorOutcome.State).To(Equal(Passed))
				}
				Expect(result.ExitCode()).To(Equal(3))
			}
		}
	})
	ginkgo.It("stops after an earlier independent access failure when a later joined error contains drift", func() {
		i, f := fixture(ModeFull, 2)
		f.session.bgp = func(context.Context, DeviceID) (BGPObservation, error) {
			if f.session.bgpCalls == 1 {
				return BGPObservation{}, &ObservationError{Category: FatalAccess}
			}
			return BGPObservation{}, errors.Join(&ObservationError{Category: FatalObservation}, &ObservationError{Category: FatalIdentity})
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(result.Fatal).To(Equal(FatalIdentity))
		Expect(f.session.routeCalls).To(BeZero())
		Expect(f.session.trafficCalls).To(BeZero())
		Expect(result.ExitCode()).To(Equal(3))
	})
	ginkgo.It("finds identity drift in every joined branch independently of exit precedence and order", func() {
		for _, otherCategory := range []FatalCategory{FatalAccess, FatalInput} {
			for _, reverse := range []bool{false, true} {
				i, f := fixture(ModeFull, 2)
				i.Checks[5].CohortBound = true
				f.input = cloneInput(i)
				drift := &ObservationError{Category: FatalIdentity}
				other := &ObservationError{Category: otherCategory}
				joined := errors.Join(other, fmt.Errorf("wrapped drift: %w", drift))
				if reverse {
					joined = errors.Join(drift, other)
				}
				f.session.bgp = func(context.Context, DeviceID) (BGPObservation, error) {
					if f.session.bgpCalls == 1 {
						return BGPObservation{Outcome: Outcome{State: Passed}}, nil
					}
					return BGPObservation{}, joined
				}
				result := runner(i, f, Options{}).Run(context.Background())
				Expect(f.session.routeCalls).To(BeZero())
				Expect(f.session.trafficCalls).To(BeZero())
				Expect(f.session.verifyCalls).To(BeZero())
				first := check(result, BGPCheckID(i.Runtime.Devices[0]))
				Expect(first.Outcome.Reason).To(Equal(ReasonIdentityDrift))
				Expect(first.PriorOutcome.State).To(Equal(Passed))
				Expect(check(result, i.Checks[5].ID).Outcome.Reason).To(Equal(ReasonIdentityDrift))
				Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Passed))
				Expect(errors.Is(result.Cause, drift)).To(BeTrue())
				Expect(errors.Is(result.Cause, other)).To(BeTrue())
				if otherCategory == FatalInput {
					Expect(result.Fatal).To(Equal(FatalInput))
					Expect(result.ExitCode()).To(Equal(2))
				} else {
					Expect(result.Fatal).To(Equal(FatalIdentity))
					Expect(result.ExitCode()).To(Equal(3))
				}
			}
		}
	})
	ginkgo.It("accumulates the maximum selected full matrix through the planned constant-time index", func() {
		i, f := fixture(ModeFull, 256)
		r := runner(i, f, Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result := r.Run(ctx)
		Expect(result.Verdict).To(Equal(VerdictPassed))
		Expect(result.Checks).To(HaveLen(len(i.Checks) + 196096))
		Expect(f.session.bgpCalls).To(Equal(256))
		Expect(f.session.routeCalls).To(Equal(65280))
		Expect(f.session.trafficCalls).To(Equal(65280))
		Expect(f.session.verifyCalls).To(Equal(1))
		Expect(result.Checks[len(i.Checks)].ID).To(Equal(BGPCheckID(i.Runtime.Devices[0])))
		last := result.Checks[len(result.Checks)-1]
		Expect(last.ID).To(Equal(TrafficCheckID(i.Runtime.Traffic[len(i.Runtime.Traffic)-1], true)))
		Expect(last.Outcome.State).To(Equal(Passed))
	})
	ginkgo.It("keeps maximum resource-only results bounded while disclosing every excluded measurement count", func() {
		i, f := fixture(ModeResources, 256)
		p, e := NewPlan(i)
		Expect(e).NotTo(HaveOccurred())
		Expect(p.Checks()).To(HaveLen(len(i.Checks)))
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictResourcesPassed))
		Expect(result.Checks).To(HaveLen(len(i.Checks)))
		Expect(f.openCalls).To(BeZero())
		Expect(result.DisabledRuntime).To(Equal(&RuntimeCoverage{BGP: 256, Routes: 65280, OrdinaryTraffic: 65280, DFTraffic: 65280}))
		Expect(result.Scope.Devices).To(HaveLen(256))
		Expect(result.Scope.Routes).To(BeEmpty())
		Expect(result.Scope.Traffic).To(BeEmpty())
		data, e := json.Marshal(result)
		Expect(e).NotTo(HaveOccurred())
		Expect(len(data)).To(BeNumerically("<", 25000))
	})
	ginkgo.It("keeps independent resource evidence while invalidating cohort-bound resource rows", func() {
		i, f := fixture(ModeFull, 2)
		i.Checks[5].CohortBound = true
		f.input = cloneInput(i)
		f.session.verify = func(context.Context) error {
			return &ObservationError{Category: FatalIdentity, Cause: context.Canceled}
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Passed))
		bound := check(result, i.Checks[5].ID)
		Expect(bound.Outcome.State).To(Equal(Unverified))
		Expect(bound.PriorOutcome.State).To(Equal(Passed))
		Expect(result.Fatal).To(Equal(FatalIdentity))
		Expect(result.ExitCode()).To(Equal(3))
	})
	ginkgo.It("continues independent runtime requests after a local transport timeout", func() {
		i, f := fixture(ModeFull, 2)
		f.session.bgp = func(context.Context, DeviceID) (BGPObservation, error) {
			return BGPObservation{}, context.DeadlineExceeded
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(f.session.bgpCalls).To(Equal(2))
		Expect(f.session.routeCalls).To(Equal(2))
		Expect(f.session.trafficCalls).To(Equal(2))
		Expect(f.session.verifyCalls).To(Equal(1))
		Expect(result.Fatal).To(Equal(FatalTimeout))
		Expect(check(result, TrafficCheckID(i.Runtime.Traffic[0], false)).Outcome.State).To(Equal(Passed))
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("uses timeout rather than drift when final cohort verification times out", func() {
		i, f := fixture(ModeFull, 2)
		f.session.verify = func(context.Context) error { return context.DeadlineExceeded }
		result := runner(i, f, Options{}).Run(context.Background())
		row := check(result, BGPCheckID("a"))
		Expect(row.Outcome.State).To(Equal(Unverified))
		Expect(row.Outcome.Reason).To(Equal(ReasonTimedOut))
		Expect(row.PriorOutcome.State).To(Equal(Passed))
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("runs all BGP, directed route and ordinary/DF pairs after an unhealthy peer", func() {
		i, f := fixture(ModeFull, 2)
		f.session.bgp = func(context.Context, DeviceID) (BGPObservation, error) {
			return BGPObservation{Outcome: Outcome{State: Failed, Reason: ReasonHealth}}, nil
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictFailed))
		Expect(f.session.bgpCalls).To(Equal(2))
		Expect(f.session.routeCalls).To(Equal(2))
		Expect(f.session.trafficCalls).To(Equal(2))
		Expect(f.session.verifyCalls).To(Equal(1))
		Expect(check(result, TrafficCheckID(i.Runtime.Traffic[1], true)).Outcome.State).To(Equal(Passed))
	})
	ginkgo.It("retains independent runtime evidence after unavailable resource access", func() {
		i, f := fixture(ModeFull, 2)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			s.Checks[3] = Outcome{ID: s.Checks[3].ID, State: Unverified, Reason: ReasonDenied}
			return s, nil
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(f.openCalls).To(Equal(1))
		Expect(f.session.trafficCalls).To(Equal(2))
		Expect(result.Verdict).To(Equal(VerdictIncomplete))
		Expect(result.ExitCode()).To(Equal(3))
	})
	ginkgo.It("can pass explicit resources scope without opening a runtime session", func() {
		i, f := fixture(ModeResources, 1)
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictResourcesPassed))
		Expect(result.ExitCode()).To(Equal(0))
		Expect(f.openCalls).To(BeZero())
		Expect(check(result, "physical/coverage").Outcome).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{"State": Equal(Skipped), "Reason": Equal(ReasonDisabled)}))
	})
	ginkgo.It("keeps single-host BGP evidence and incomplete cross-host coverage", func() {
		i, f := fixture(ModeFull, 1)
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(f.session.bgpCalls).To(Equal(1))
		Expect(f.session.trafficCalls).To(BeZero())
		Expect(check(result, BGPCheckID("a")).Outcome.State).To(Equal(Passed))
		Expect(result.Verdict).To(Equal(VerdictIncomplete))
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("stops permanently on identity drift and preserves measurement-time evidence", func() {
		i, f := fixture(ModeFull, 2)
		f.session.route = func(context.Context, DirectedDevicePair) (RouteObservation, error) {
			return RouteObservation{}, &ObservationError{Category: FatalIdentity}
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(f.session.routeCalls).To(Equal(1))
		Expect(f.session.trafficCalls).To(BeZero())
		row := check(result, BGPCheckID("a"))
		Expect(row.Outcome.State).To(Equal(Unverified))
		Expect(row.Outcome.Reason).To(Equal(ReasonIdentityDrift))
		Expect(row.PriorOutcome).NotTo(BeNil())
		Expect(row.PriorOutcome.State).To(Equal(Passed))
		Expect(result.ExitCode()).To(Equal(3))
	})
	ginkgo.It("rejects a reduced or replaced runtime cohort before any measurement", func() {
		i, f := fixture(ModeFull, 2)
		smaller, _ := fixture(ModeFull, 1)
		f.session.scope = smaller.Runtime
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(f.session.bgpCalls).To(BeZero())
		Expect(result.Fatal).To(Equal(FatalIdentity))
		Expect(result.Verdict).To(Equal(VerdictIncomplete))
	})
	ginkgo.It("invalidates prior passing evidence on final verification failure", func() {
		i, f := fixture(ModeFull, 2)
		f.session.verify = func(context.Context) error { return &ObservationError{Category: FatalIdentity} }
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(f.session.trafficCalls).To(Equal(2))
		Expect(result.Verdict).To(Equal(VerdictIncomplete))
		row := check(result, TrafficCheckID(i.Runtime.Traffic[1], true))
		Expect(row.PriorOutcome.State).To(Equal(Passed))
		Expect(row.Outcome.State).To(Equal(Unverified))
	})
})

var _ = ginkgo.Describe("Bounded waiting and cancellation", func() {
	ginkgo.It("retains earlier source-access precedence when cancellation invalidates its cohort-bound row", func() {
		i, f := fixture(ModeFull, 2)
		i.Checks[5].CohortBound = true
		f.input = cloneInput(i)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			s.Checks[5] = Outcome{ID: s.Checks[5].ID, State: Unverified, Reason: ReasonDenied}
			return s, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.session.route = func(context.Context, DirectedDevicePair) (RouteObservation, error) {
			cancel()
			return RouteObservation{}, context.Canceled
		}
		result := runner(i, f, Options{}).Run(ctx)
		row := check(result, i.Checks[5].ID)
		Expect(row.Outcome.Reason).To(Equal(ReasonCanceled))
		Expect(row.PriorOutcome.Reason).To(Equal(ReasonDenied))
		Expect(result.ExitCode()).To(Equal(3))
	})
	ginkgo.It("invalidates earlier cohort successes when cancellation prevents the final fence", func() {
		i, f := fixture(ModeFull, 2)
		i.Checks[5].CohortBound = true
		f.input = cloneInput(i)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.session.route = func(context.Context, DirectedDevicePair) (RouteObservation, error) {
			cancel()
			return RouteObservation{}, context.Canceled
		}
		result := runner(i, f, Options{}).Run(ctx)
		Expect(f.session.bgpCalls).To(Equal(2))
		Expect(f.session.routeCalls).To(Equal(1))
		Expect(f.session.trafficCalls).To(BeZero())
		Expect(f.session.verifyCalls).To(BeZero())
		for _, device := range i.Runtime.Devices {
			row := check(result, BGPCheckID(device))
			Expect(row.Outcome.State).To(Equal(Unverified))
			Expect(row.Outcome.Reason).To(Equal(ReasonCanceled))
			Expect(row.PriorOutcome.State).To(Equal(Passed))
		}
		Expect(check(result, i.Checks[5].ID).Outcome.Reason).To(Equal(ReasonCanceled))
		Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Passed))
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("stops runtime after caller cancellation and leaves remaining pairs explicitly unverified", func() {
		i, f := fixture(ModeFull, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.session.bgp = func(context.Context, DeviceID) (BGPObservation, error) {
			cancel()
			return BGPObservation{Outcome: Outcome{State: Passed}}, nil
		}
		result := runner(i, f, Options{}).Run(ctx)
		Expect(f.session.bgpCalls).To(Equal(1))
		Expect(f.session.routeCalls).To(BeZero())
		Expect(f.session.trafficCalls).To(BeZero())
		Expect(check(result, TrafficCheckID(i.Runtime.Traffic[0], false)).Outcome.Reason).To(Equal(ReasonCanceled))
		Expect(errors.Is(result.Cause, context.Canceled)).To(BeTrue())
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("does not poll a contradictory pending denied row", func() {
		i, f := fixture(ModeResources, 2)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			s.Checks[0] = Outcome{ID: s.Checks[0].ID, State: Pending, Reason: ReasonDenied}
			return s, nil
		}
		r := runner(i, f, Options{Wait: time.Hour})
		r.pause = func(context.Context, time.Duration) error {
			ginkgo.Fail("terminal access failure was polled")
			return nil
		}
		result := r.Run(context.Background())
		Expect(f.resourceCalls).To(Equal(1))
		Expect(result.ExitCode()).To(Equal(3))
	})
	ginkgo.It("takes exactly one snapshot for zero wait and does not poll terminal blockers", func() {
		for _, wait := range []time.Duration{0, time.Hour} {
			i, f := fixture(ModeResources, 2)
			f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
				s := healthySnapshot(i)
				s.Checks[0] = Outcome{ID: s.Checks[0].ID, State: Pending, Reason: ReasonProgressing}
				if wait > 0 {
					s.Checks[1] = Outcome{ID: s.Checks[1].ID, State: Unverified, Reason: ReasonDenied}
				}
				return s, nil
			}
			r := runner(i, f, Options{Wait: wait})
			r.pause = func(context.Context, time.Duration) error { ginkgo.Fail("unexpected polling"); return nil }
			result := r.Run(context.Background())
			Expect(f.resourceCalls).To(Equal(1))
			Expect(result.Verdict).To(Equal(VerdictIncomplete))
		}
	})
	ginkgo.It("refreshes pending checks while preserving the frozen plan", func() {
		i, f := fixture(ModeResources, 2)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			if f.resourceCalls == 1 {
				s.Checks[0] = Outcome{ID: s.Checks[0].ID, State: Pending, Reason: ReasonProgressing}
			}
			return s, nil
		}
		r := runner(i, f, Options{Wait: time.Hour})
		pauses := 0
		r.pause = func(c context.Context, _ time.Duration) error {
			_, ok := c.Deadline()
			Expect(ok).To(BeTrue())
			pauses++
			return nil
		}
		result := r.Run(context.Background())
		Expect(pauses).To(Equal(1))
		Expect(f.resourceCalls).To(Equal(2))
		Expect(f.openCalls).To(BeZero())
		Expect(result.Verdict).To(Equal(VerdictResourcesPassed))
	})
	ginkgo.It("retains partial refresh evidence without presenting stale passes", func() {
		i, f := fixture(ModeResources, 2)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			if f.resourceCalls == 1 {
				s.Checks[0] = Outcome{ID: s.Checks[0].ID, State: Pending, Reason: ReasonProgressing}
				return s, nil
			}
			return ResourceSnapshot{Checks: []Outcome{{ID: i.Checks[0].ID, State: Passed}}}, context.DeadlineExceeded
		}
		r := runner(i, f, Options{Wait: time.Hour})
		r.pause = func(context.Context, time.Duration) error { return nil }
		result := r.Run(context.Background())
		Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Passed))
		stale := check(result, i.Checks[1].ID)
		Expect(stale.Outcome.State).To(Equal(Unverified))
		Expect(stale.PriorOutcome.State).To(Equal(Passed))
		Expect(result.Fatal).To(Equal(FatalTimeout))
		Expect(errors.Is(result.Cause, context.DeadlineExceeded)).To(BeTrue())
	})
	ginkgo.It("marks unfinished pending rows timed out when the wait budget expires", func() {
		i, f := fixture(ModeResources, 2)
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			s.Checks[0] = Outcome{ID: s.Checks[0].ID, State: Pending, Reason: ReasonProgressing}
			return s, nil
		}
		r := runner(i, f, Options{Wait: time.Hour})
		r.pause = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
		result := r.Run(context.Background())
		Expect(check(result, i.Checks[0].ID).Outcome.Reason).To(Equal(ReasonTimedOut))
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("preserves a partial snapshot and cancellation identity", func() {
		i, f := fixture(ModeFull, 2)
		ctx, cancel := context.WithCancel(context.Background())
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			cancel()
			return ResourceSnapshot{Checks: []Outcome{{ID: i.Checks[0].ID, State: Passed}}}, ctx.Err()
		}
		result := runner(i, f, Options{}).Run(ctx)
		Expect(check(result, i.Checks[0].ID).Outcome.State).To(Equal(Passed))
		Expect(errors.Is(result.Cause, context.Canceled)).To(BeTrue())
		Expect(result.Fatal).To(Equal(FatalCanceled))
		Expect(f.openCalls).To(BeZero())
		Expect(result.ExitCode()).To(Equal(4))
	})
	ginkgo.It("honors an already expired caller deadline without observing", func() {
		i, f := fixture(ModeFull, 2)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		result := runner(i, f, Options{}).Run(ctx)
		Expect(f.resourceCalls).To(BeZero())
		Expect(result.Fatal).To(Equal(FatalTimeout))
	})
	ginkgo.It("gives the whole runtime phase its computed deadline without imposing a request ceiling", func() {
		i, f := fixture(ModeFull, 2)
		f.resources = func(c context.Context, _ Progress) (ResourceSnapshot, error) {
			_, ok := c.Deadline()
			Expect(ok).To(BeFalse())
			return healthySnapshot(i), nil
		}
		f.open = func(c context.Context, _ Progress) (RuntimeSession, error) {
			deadline, ok := c.Deadline()
			Expect(ok).To(BeTrue())
			Expect(time.Until(deadline)).To(BeNumerically(">", 9*time.Minute))
			return f.session, nil
		}
		result := runner(i, f, Options{}).Run(context.Background())
		Expect(result.RuntimeBudget).To(Equal(10 * time.Minute))
		Expect(result.Verdict).To(Equal(VerdictPassed))
	})
})

var _ = ginkgo.Describe("Safe progress and result disposition", func() {
	ginkgo.It("does not allow observer-disabled reasons to hide a requested blocker", func() {
		for _, state := range []State{Failed, Unverified, Pending} {
			i, f := fixture(ModeResources, 2)
			f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
				s := healthySnapshot(i)
				s.Checks[0] = Outcome{ID: s.Checks[0].ID, State: state, Reason: ReasonDisabled}
				return s, nil
			}
			result := runner(i, f, Options{}).Run(context.Background())
			row := check(result, i.Checks[0].ID)
			Expect(row.Outcome.State).To(Equal(Unverified))
			Expect(row.Outcome.Reason).To(Equal(ReasonIncomplete))
			Expect(result.ExitCode()).To(Equal(4))
		}
	})
	ginkgo.It("preserves requested or explicitly unknown scope through real preparation failures", func() {
		for _, mode := range []Mode{ModeFull, ModeResources, ModeUnknown, Mode("invalid")} {
			result := PreparationFailure(mode, FatalInput, errors.New("private input"))
			expected := mode
			if mode == "invalid" {
				expected = ModeUnknown
			}
			Expect(result.Mode).To(Equal(expected))
			Expect(result.Checks[0].Area).To(Equal(AreaPreparation))
			Expect(result.Scope.Hosts).To(BeEmpty())
			Expect(result.DisabledRuntime).To(BeNil())
			data, e := json.Marshal(result)
			Expect(e).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring(`"mode":"` + string(expected) + `"`))
			Expect(string(data)).NotTo(ContainSubstring("private input"))
		}
		var absent *Runner
		Expect(absent.Run(context.Background()).Mode).To(Equal(ModeUnknown))
		i, f := fixture(ModeResources, 2)
		var missingContext context.Context
		Expect(runner(i, f, Options{}).Run(missingContext).Mode).To(Equal(ModeResources))
	})
	ginkgo.It("streams per-check running and completion, deduplicates and contains callback panic", func() {
		i, f := fixture(ModeResources, 2)
		events := map[CheckID][]ProgressPhase{}
		r := runner(i, f, Options{Progress: func(e ProgressEvent) {
			events[e.Check.ID] = append(events[e.Check.ID], e.Phase)
			panic("callback failure")
		}})
		result := r.Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictResourcesPassed))
		for _, c := range i.Checks {
			if !c.FullOnly {
				Expect(events[c.ID]).To(Equal([]ProgressPhase{Running, Completed}))
			}
		}
	})
	ginkgo.It("serializes concurrent progress and projects only planned safe subjects", func() {
		i, f := fixture(ModeResources, 2)
		seen := 0
		f.resources = func(_ context.Context, p Progress) (ResourceSnapshot, error) {
			var wg sync.WaitGroup
			for _, c := range i.Checks {
				if c.FullOnly {
					continue
				}
				wg.Go(func() {
					p(ProgressEvent{Check: CheckSpec{ID: c.ID, Subject: "forged"}, Phase: Running})
					p(ProgressEvent{Check: CheckSpec{ID: c.ID}, Phase: Completed, Outcome: Outcome{State: Passed}})
				})
			}
			wg.Wait()
			return healthySnapshot(i), nil
		}
		result := runner(i, f, Options{Progress: func(e ProgressEvent) { seen++; Expect(e.Check.Subject).NotTo(Equal("forged")) }}).Run(context.Background())
		Expect(result.Verdict).To(Equal(VerdictResourcesPassed))
		Expect(seen).To(Equal(18))
	})
	ginkgo.It("bounds safe summaries and keeps raw causes out of JSON and formatting", func() {
		i, f := fixture(ModeResources, 2)
		secret := errors.New("SECRET-CANARY")
		f.resources = func(context.Context, Progress) (ResourceSnapshot, error) {
			s := healthySnapshot(i)
			s.Checks[0].Summary = strings.Repeat("a", 700) + "\n"
			return s, &ObservationError{Category: FatalAccess, Cause: secret}
		}
		result := runner(i, f, Options{}).Run(context.Background())
		data, e := json.Marshal(result)
		Expect(e).NotTo(HaveOccurred())
		Expect(string(data)).NotTo(ContainSubstring("SECRET-CANARY"))
		Expect(string(data)).To(ContainSubstring(`"runtimeBudget":"0s"`))
		Expect(errors.Is(result.Cause, secret)).To(BeTrue())
		Expect(len(check(result, i.Checks[0].ID).Outcome.Summary)).To(BeNumerically("<=", 512))
		Expect((&ObservationError{Category: FatalAccess, Cause: secret}).Error()).NotTo(ContainSubstring("SECRET-CANARY"))
	})
	ginkgo.It("keeps callback metric mutation away from acceptance evidence", func() {
		i, f := fixture(ModeFull, 2)
		f.session.bgp = func(context.Context, DeviceID) (BGPObservation, error) {
			return BGPObservation{Outcome: Outcome{State: Passed, BGP: &BGPFacts{ExpectedPeers: 2, EstablishedPeers: 2}}}, nil
		}
		result := runner(i, f, Options{Progress: func(e ProgressEvent) {
			if e.Outcome.BGP != nil {
				e.Outcome.BGP.ExpectedPeers = 0
			}
		}}).Run(context.Background())
		Expect(check(result, BGPCheckID("a")).Outcome.BGP.ExpectedPeers).To(Equal(uint32(2)))
	})
	ginkgo.It("gives input/access/evaluation precedence over independent publication failure", func() {
		i, f := fixture(ModeResources, 2)
		result := runner(i, f, Options{}).Run(context.Background())
		result.Report.Status = ReportFailed
		Expect(result.Verdict).To(Equal(VerdictResourcesPassed))
		Expect(result.ExitCode()).To(Equal(1))
		result.Verdict = VerdictIncomplete
		Expect(result.ExitCode()).To(Equal(4))
		result.Fatal = FatalAccess
		Expect(result.ExitCode()).To(Equal(3))
		result.Fatal = FatalInput
		Expect(result.ExitCode()).To(Equal(2))
	})
	ginkgo.It("handles nil public inputs and pre-plan failures as safe terminal results", func() {
		var nilRunner *Runner
		Expect(nilRunner.Run(context.Background()).ExitCode()).To(Equal(2))
		i, f := fixture(ModeFull, 2)
		var missingContext context.Context
		Expect(runner(i, f, Options{}).Run(missingContext).ExitCode()).To(Equal(2))
		p, e := NewPlan(i)
		Expect(e).NotTo(HaveOccurred())
		var nilObserver *fakeObserver
		_, e = NewRunner(p, nilObserver, Options{})
		Expect(e).To(HaveOccurred())
		result := PreparationFailure(ModeFull, FatalInput, errors.New("private"))
		Expect(result.Checks).To(HaveLen(1))
		Expect(result.Report.Status).To(Equal(ReportUnavailable))
		Expect(result.Scope.Hosts).To(BeEmpty())
	})
})

var _ = ginkgo.Describe("Connectivity budget policy", func() {
	ginkgo.It("uses the exact automatic formula across supported sizes", func() {
		for _, n := range []int{1, 2, 13, 43, 256} {
			actual, e := RuntimeBudget(n, 0, false, ModeFull)
			Expect(e).NotTo(HaveOccurred())
			expected := 5*time.Minute + 2*time.Minute*time.Duration(n) + 30*time.Second*time.Duration(n*(n-1))
			Expect(actual).To(Equal(expected))
		}
	})
	ginkgo.It("rejects impossible and unused bounds but accepts a feasible explicit bound", func() {
		for _, n := range []int{0, 257, int(^uint(0) >> 1)} {
			_, e := RuntimeBudget(n, 0, false, ModeFull)
			Expect(e).To(HaveOccurred())
		}
		for _, d := range []time.Duration{-1, 8 * time.Second} {
			_, e := RuntimeBudget(2, d, true, ModeFull)
			Expect(e).To(HaveOccurred())
		}
		actual, e := RuntimeBudget(2, 8*time.Second+1, true, ModeFull)
		Expect(e).NotTo(HaveOccurred())
		Expect(actual).To(Equal(8*time.Second + 1))
		_, e = RuntimeBudget(2, 0, true, ModeResources)
		Expect(e).To(HaveOccurred())
		i, f := fixture(ModeFull, 2)
		p, e := NewPlan(i)
		Expect(e).NotTo(HaveOccurred())
		_, e = NewRunner(p, f, Options{Wait: -1})
		Expect(e).To(HaveOccurred())
	})
})
