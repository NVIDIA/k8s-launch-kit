// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Runner has no transport or lifecycle authority. It owns coverage, waiting,
// independent measurement scheduling and the terminal evaluation.
type Runner struct {
	plan     Plan
	observer Observer
	options  Options
	pause    func(context.Context, time.Duration) error
}

func NewRunner(plan Plan, observer Observer, options Options) (*Runner, error) {
	if observer == nil || (reflect.ValueOf(observer).Kind() == reflect.Pointer && reflect.ValueOf(observer).IsNil()) {
		return nil, fmt.Errorf("validation observer is required")
	}
	// Revalidate even a zero-value Plan, then take another independent snapshot.
	frozen, err := NewPlan(plan.Input())
	if err != nil {
		return nil, err
	}
	if options.Wait < 0 || options.PollInterval < 0 {
		return nil, fmt.Errorf("validation durations cannot be negative")
	}
	if options.PollInterval == 0 {
		options.PollInterval = 2 * time.Second
	}
	if _, err := RuntimeBudget(len(frozen.input.Runtime.Hosts), options.ConnectivityTimeout, options.ConnectivityTimeoutSet, frozen.Mode()); err != nil {
		return nil, err
	}
	return &Runner{plan: frozen, observer: observer, options: options, pause: pause}, nil
}

func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) Run(ctx context.Context) Result {
	if r == nil || ctx == nil {
		return PreparationFailure(FatalInput, errors.New("validation runner and context are required"))
	}
	result := Result{Mode: r.plan.Mode(), Scope: r.plan.RuntimeScope(), Verdict: VerdictIncomplete, Report: ReportDisposition{Status: ReportPending}}
	result.RuntimeBudget, _ = RuntimeBudget(len(result.Scope.Hosts), r.options.ConnectivityTimeout, r.options.ConnectivityTimeoutSet, result.Mode)
	for _, spec := range r.plan.checks {
		o := Outcome{ID: spec.ID, State: Unverified, Reason: ReasonDependency, Summary: "Required observation has not completed"}
		if result.Mode == ModeResources && spec.FullOnly {
			o = Outcome{ID: spec.ID, State: Skipped, Reason: ReasonDisabled, Summary: "Outside requested resource-only scope"}
		}
		if spec.ID == "runtime/cross-host-coverage" && result.Mode == ModeFull {
			o = Outcome{ID: spec.ID, State: Unverified, Reason: ReasonIncomplete, Summary: "Cross-host traffic requires at least two selected hosts", Action: "Select at least two physical hosts to measure cross-host coverage"}
		}
		result.Checks = append(result.Checks, CheckResult{CheckSpec: spec, Outcome: o})
	}
	progress := newProgress(r.options.Progress, r.plan.checks, result.Mode)
	resourceCtx := ctx
	cancel := func() {}
	if r.options.Wait > 0 {
		resourceCtx, cancel = context.WithTimeout(ctx, r.options.Wait)
	}
	r.observeResources(resourceCtx, &result, progress)
	cancel()
	if result.Fatal == "" && ctx.Err() != nil {
		setFatal(&result, ctx.Err())
	}
	if result.Mode == ModeFull && result.Fatal == "" {
		// Missing/unhealthy resource regions may make runtime unavailable, but the
		// adapter can still bind independently safe regions. It owns this decision.
		r.observeRuntime(ctx, &result, progress)
	}
	if result.Fatal != "" {
		markRemaining(&result, reasonForFatal(result.Fatal))
	}
	for _, row := range result.Checks {
		progress.emit(ProgressEvent{Check: row.CheckSpec, Phase: Completed, Outcome: row.Outcome})
	}
	result.evaluate()
	return result
}

func (r *Runner) observeResources(ctx context.Context, result *Result, progress *progressSink) {
	for {
		if ctx.Err() != nil {
			setFatal(result, ctx.Err())
			return
		}
		snapshot, err := r.observer.ObserveResources(ctx, progress.emit)
		if err == nil {
			err = ctx.Err()
		}
		if !r.applySnapshot(result, snapshot, progress, err) {
			return
		}
		if err != nil {
			setFatal(result, err)
			if classify(err) == FatalIdentity {
				invalidate(result, ReasonIdentityDrift, false)
			}
			return
		}
		pending, blocker := false, false
		for _, row := range result.Checks {
			if !r.isResource(row.CheckSpec) || !row.Required {
				continue
			}
			if row.Outcome.State == Pending {
				pending = true
			} else if row.Outcome.State != Passed {
				blocker = true
			}
		}
		if r.options.Wait == 0 || !pending || blocker {
			return
		}
		if err := r.pause(ctx, r.options.PollInterval); err != nil {
			setFatal(result, err)
			return
		}
	}
}

func (r *Runner) isResource(spec CheckSpec) bool {
	return !strings.HasPrefix(string(spec.ID), "runtime/") && (r.plan.Mode() != ModeResources || !spec.FullOnly)
}

func (r *Runner) applySnapshot(result *Result, snapshot ResourceSnapshot, progress *progressSink, observationErr error) bool {
	observed := make(map[CheckID]Outcome, len(snapshot.Checks))
	valid := true
	for _, o := range snapshot.Checks {
		spec, ok := progress.specs[o.ID]
		if !ok || !r.isResource(spec) {
			valid = false
			continue
		}
		if _, duplicate := observed[o.ID]; duplicate {
			valid = false
			continue
		}
		observed[o.ID] = normalize(o, o.ID)
		if o.Reason == ReasonIdentityDrift {
			setFatal(result, &ObservationError{Category: FatalIdentity})
		}
	}
	for i := range result.Checks {
		row := &result.Checks[i]
		if !r.isResource(row.CheckSpec) {
			continue
		}
		o, ok := observed[row.ID]
		if !ok {
			reason := ReasonIncomplete
			if observationErr != nil {
				reason = reasonForFatal(classify(observationErr))
			}
			o = Outcome{ID: row.ID, State: Unverified, Reason: reason, Summary: "Planned observation was not returned", Action: "Restore this observation source and validate again"}
			preservePrior(row)
		}
		row.Outcome = o
		progress.emit(ProgressEvent{Check: row.CheckSpec, Phase: Completed, Outcome: o})
	}
	if !valid {
		setFatal(result, &ObservationError{Category: FatalObservation})
		return false
	}
	if result.Fatal == FatalIdentity {
		invalidate(result, ReasonIdentityDrift, false)
		return false
	}
	return true
}

func (r *Runner) observeRuntime(ctx context.Context, result *Result, progress *progressSink) {
	runtimeCtx, cancel := context.WithTimeout(ctx, result.RuntimeBudget)
	defer cancel()
	session, err := r.observer.OpenRuntime(runtimeCtx, progress.emit)
	if err == nil {
		err = runtimeCtx.Err()
	}
	if err != nil {
		setFatal(result, err)
		if classify(err) == FatalIdentity {
			invalidate(result, ReasonIdentityDrift, true)
		}
		return
	}
	if session == nil || (reflect.ValueOf(session).Kind() == reflect.Pointer && reflect.ValueOf(session).IsNil()) {
		setFatal(result, &ObservationError{Category: FatalObservation})
		return
	}
	if !sameScope(session.Scope(), r.plan.input.Runtime) {
		setFatal(result, &ObservationError{Category: FatalIdentity})
		invalidate(result, ReasonIdentityDrift, true)
		return
	}
	measure := func(ids []CheckID, observe func(context.Context) ([]Outcome, error)) bool {
		if runtimeCtx.Err() != nil {
			setFatal(result, runtimeCtx.Err())
			return false
		}
		for _, id := range ids {
			progress.emit(ProgressEvent{Check: progress.specs[id], Phase: Running})
		}
		outcomes, callErr := observe(runtimeCtx)
		if callErr == nil {
			callErr = runtimeCtx.Err()
		}
		for i, id := range ids {
			var partial *Outcome
			o := Outcome{ID: id, State: Unverified, Reason: ReasonIncomplete, Summary: "Measurement did not return evidence"}
			if i < len(outcomes) {
				if outcomes[i].Reason == ReasonIdentityDrift {
					callErr = &ObservationError{Category: FatalIdentity, Cause: callErr}
				}
				if outcomes[i].ID != "" && outcomes[i].ID != id {
					callErr = errors.Join(callErr, &ObservationError{Category: FatalObservation})
				} else {
					o = normalize(outcomes[i], id)
				}
			}
			if callErr != nil && (o.State == Passed || o.State == Pending) {
				if o.State == Passed {
					previous := o
					partial = &previous
				}
				o = Outcome{ID: id, State: Unverified, Reason: reasonForFatal(classify(callErr)), Summary: "Measurement did not complete safely"}
			}
			if row := setOutcome(result, o); row != nil {
				row.PriorOutcome = partial
			}
			progress.emit(ProgressEvent{Check: progress.specs[id], Phase: Completed, Outcome: o})
			if o.Reason == ReasonIdentityDrift {
				callErr = errors.Join(callErr, &ObservationError{Category: FatalIdentity})
			}
		}
		if callErr != nil {
			category := classify(callErr)
			if category == FatalIdentity {
				setFatal(result, callErr)
				invalidate(result, ReasonIdentityDrift, true)
				return false
			}
			if category == FatalInput || category == FatalCanceled || runtimeCtx.Err() != nil {
				setFatal(result, callErr)
				return false
			}
			// Access/ordinary observation failures retain their category but do not
			// suppress unrelated selected peers, routes or host pairs.
			setFatal(result, callErr)
		}
		return true
	}
	for _, device := range r.plan.input.Runtime.Devices {
		if !measure([]CheckID{BGPCheckID(device)}, func(c context.Context) ([]Outcome, error) {
			o, e := session.ObserveBGP(c, device)
			return []Outcome{o.Outcome}, e
		}) {
			return
		}
	}
	for _, pair := range r.plan.input.Runtime.Routes {
		if !measure([]CheckID{RouteCheckID(pair)}, func(c context.Context) ([]Outcome, error) {
			o, e := session.ObserveRoute(c, pair)
			return []Outcome{o.Outcome}, e
		}) {
			return
		}
	}
	for _, pair := range r.plan.input.Runtime.Traffic {
		if !measure([]CheckID{TrafficCheckID(pair, false), TrafficCheckID(pair, true)}, func(c context.Context) ([]Outcome, error) {
			o, e := session.ProbeTraffic(c, pair)
			return []Outcome{o.Ordinary, o.DF}, e
		}) {
			return
		}
	}
	err = session.VerifyCohort(runtimeCtx)
	if err == nil {
		err = runtimeCtx.Err()
	}
	if err != nil {
		setFatal(result, err)
		invalidate(result, reasonForFatal(classify(err)), true)
	}
}

func sameScope(a, b RuntimeScope) bool {
	// Order is not identity. Validate completeness, then compare membership.
	if validateScope(a) != nil || len(a.Devices) != len(b.Devices) {
		return false
	}
	devices, hosts := map[DeviceID]bool{}, map[HostID]bool{}
	for _, d := range b.Devices {
		devices[d] = true
	}
	for _, h := range b.Hosts {
		hosts[h] = true
	}
	for _, d := range a.Devices {
		if !devices[d] {
			return false
		}
	}
	for _, h := range a.Hosts {
		if !hosts[h] {
			return false
		}
	}
	return true
}

func setOutcome(result *Result, o Outcome) *CheckResult {
	for i := range result.Checks {
		if result.Checks[i].ID == o.ID {
			result.Checks[i].Outcome = o
			return &result.Checks[i]
		}
	}
	return nil
}
func preservePrior(row *CheckResult) {
	if row.Outcome.State == Passed || row.Outcome.State == Failed {
		previous := row.Outcome
		row.PriorOutcome = &previous
	}
}
func invalidate(result *Result, reason Reason, runtimeOnly bool) {
	for i := range result.Checks {
		row := &result.Checks[i]
		if row.Outcome.State == Skipped {
			continue
		}
		if runtimeOnly && !row.CohortBound {
			continue
		}
		preservePrior(row)
		row.Outcome = Outcome{ID: row.ID, State: Unverified, Reason: reason, Summary: "Cohort verification did not complete; earlier evidence cannot qualify this run", Action: "Restore the selected identities and validate again"}
	}
}
func markRemaining(result *Result, reason Reason) {
	for i := range result.Checks {
		row := &result.Checks[i]
		if row.Outcome.State == Pending || (row.Outcome.State == Unverified && row.Outcome.Reason == ReasonDependency) {
			row.Outcome = Outcome{ID: row.ID, State: Unverified, Reason: reason, Summary: "Planned observation could not complete"}
		}
	}
}

func classify(err error) FatalCategory {
	var observation *ObservationError
	if errors.As(err, &observation) && observation != nil {
		switch observation.Category {
		case FatalInput, FatalAccess, FatalIdentity, FatalCanceled, FatalTimeout, FatalObservation:
			return observation.Category
		}
	}
	if errors.Is(err, context.Canceled) {
		return FatalCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FatalTimeout
	}
	return FatalObservation
}
func fatalPriority(c FatalCategory) int {
	switch c {
	case FatalInput:
		return 4
	case FatalAccess, FatalIdentity:
		return 3
	case FatalCanceled, FatalTimeout, FatalObservation:
		return 2
	}
	return 0
}
func setFatal(result *Result, err error) {
	if err == nil {
		return
	}
	category := classify(err)
	if result.Fatal == "" || fatalPriority(category) > fatalPriority(result.Fatal) || (category == FatalIdentity && result.Fatal == FatalAccess) {
		result.Fatal = category
	}
	result.Cause = errors.Join(result.Cause, err)
}
func reasonForFatal(c FatalCategory) Reason {
	switch c {
	case FatalInput:
		return ReasonInvalidInput
	case FatalAccess:
		return ReasonUnavailable
	case FatalIdentity:
		return ReasonIdentityDrift
	case FatalCanceled:
		return ReasonCanceled
	case FatalTimeout:
		return ReasonTimedOut
	default:
		return ReasonIncomplete
	}
}

func normalize(o Outcome, id CheckID) Outcome {
	o.ID = id
	if o.BGP != nil {
		facts := *o.BGP
		o.BGP = &facts
	}
	if o.Route != nil {
		facts := *o.Route
		o.Route = &facts
	}
	if o.Traffic != nil {
		facts := *o.Traffic
		o.Traffic = &facts
	}
	o.Summary = safeText(o.Summary)
	o.Action = safeText(o.Action)
	validReason := false
	switch o.Reason {
	case ReasonHealth, ReasonMissing, ReasonDenied, ReasonUnavailable, ReasonUnsupported, ReasonIncomplete, ReasonProgressing, ReasonDependency, ReasonDisabled, ReasonInapplicable, ReasonCanceled, ReasonTimedOut, ReasonIdentityDrift, ReasonInvalidInput:
		validReason = true
	}
	switch o.State {
	case Passed:
		if o.Reason != ReasonNone {
			o.State = Unverified
			o.Reason = ReasonIncomplete
		}
	case Pending:
		if o.Reason != ReasonProgressing {
			o.State = Unverified
			if !validReason {
				o.Reason = ReasonIncomplete
			}
		}
	case Failed, Unverified:
		if !validReason {
			o.State = Unverified
			o.Reason = ReasonIncomplete
		}
		if o.State == Failed && o.Reason == ReasonProgressing {
			o.State = Unverified
			o.Reason = ReasonIncomplete
		}
	case Skipped:
		// An observer cannot turn a requested required check into a disabled or
		// inapplicable check. The frozen plan alone controls disabled scope.
		o.State = Unverified
		o.Reason = ReasonIncomplete
	default:
		o.State = Unverified
		o.Reason = ReasonIncomplete
	}
	return o
}

// PreparationFailure provides the same terminal envelope before a retained plan
// can be loaded. No release, selection, expected count or successful report is
// invented. The raw cause is available only to library callers.
func PreparationFailure(category FatalCategory, cause error) Result {
	result := Result{Mode: ModeFull, Verdict: VerdictIncomplete, Report: ReportDisposition{Status: ReportUnavailable}}
	setFatal(&result, &ObservationError{Category: category, Cause: cause})
	spec := CheckSpec{ID: "preparation", Area: AreaManagement, Subject: "Validation preparation", Required: true}
	result.Checks = []CheckResult{{CheckSpec: spec, Outcome: Outcome{ID: spec.ID, State: Unverified, Reason: reasonForFatal(result.Fatal), Summary: "Validation preparation could not complete", Action: "Check the retained inputs and explicit access configuration"}}}
	return result
}
func safeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxText {
		s = s[:maxText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

type progressSink struct {
	mu       sync.Mutex
	callback Progress
	specs    map[CheckID]CheckSpec
	mode     Mode
	last     map[CheckID]ProgressEvent
}

func newProgress(callback Progress, specs []CheckSpec, mode Mode) *progressSink {
	p := &progressSink{callback: callback, specs: map[CheckID]CheckSpec{}, mode: mode, last: map[CheckID]ProgressEvent{}}
	for _, s := range specs {
		p.specs[s.ID] = s
	}
	return p
}
func (p *progressSink) emit(event ProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	spec, ok := p.specs[event.Check.ID]
	if !ok || (p.mode == ModeResources && spec.FullOnly) || (event.Phase != Running && event.Phase != Completed) {
		return
	}
	event.Check = spec
	if event.Phase == Completed {
		event.Outcome = normalize(event.Outcome, spec.ID)
	} else {
		event.Outcome = Outcome{ID: spec.ID}
	}
	previous, exists := p.last[spec.ID]
	if event.Phase == Running && exists {
		return
	}
	if event.Phase == Completed && exists && reflect.DeepEqual(previous, event) {
		return
	}
	if event.Phase == Completed && !exists {
		p.call(ProgressEvent{Check: spec, Phase: Running, Outcome: Outcome{ID: spec.ID}})
	}
	p.last[spec.ID] = event
	p.call(event)
}
func (p *progressSink) call(event ProgressEvent) {
	if p.callback != nil {
		if event.Phase == Completed {
			event.Outcome = normalize(event.Outcome, event.Check.ID)
		}
		func() { defer func() { _ = recover() }(); p.callback(event) }()
	}
}

func (r *Result) evaluate() {
	failed, incomplete, required := false, r.Fatal != "", 0
	for _, row := range r.Checks {
		if !row.Required || (r.Mode == ModeResources && row.FullOnly) {
			continue
		}
		required++
		if row.Outcome.State == Failed {
			failed = true
		} else if row.Outcome.State != Passed {
			incomplete = true
		}
	}
	if failed {
		r.Verdict = VerdictFailed
	} else if incomplete || required == 0 {
		r.Verdict = VerdictIncomplete
	} else if r.Mode == ModeResources {
		r.Verdict = VerdictResourcesPassed
	} else {
		r.Verdict = VerdictPassed
	}
}

// ExitCode separates observation/evaluation from report/output disposition.
func (r Result) ExitCode() int {
	inputFailure, accessFailure := r.Fatal == FatalInput, r.Fatal == FatalAccess || r.Fatal == FatalIdentity
	for _, row := range r.Checks {
		if !row.Required || (r.Mode == ModeResources && row.FullOnly) || row.Outcome.State == Passed {
			continue
		}
		switch row.Outcome.Reason {
		case ReasonInvalidInput:
			inputFailure = true
		case ReasonDenied, ReasonUnavailable, ReasonIdentityDrift:
			accessFailure = true
		}
	}
	if inputFailure {
		return 2
	}
	if accessFailure {
		return 3
	}
	if r.Verdict != VerdictPassed && r.Verdict != VerdictResourcesPassed {
		return 4
	}
	if r.Report.Status == ReportFailed || r.Report.Status == ReportUnavailable || r.OutputFailed {
		return 1
	}
	return 0
}
