// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const deploymentAcceptanceWait = 10 * time.Minute

// runDeploymentAcceptance uses exactly the standalone plan and scheduler. This
// decorator adds operation authority only; it never measures BGP, routes or traffic.
func runDeploymentAcceptance(ctx context.Context, plan validation.Plan, observer validation.Observer, client acceptanceReceiptReadSource,
	verification *controlPlaneVerificationInputs, tenant acceptanceTenantReadSource, activation activationStageReceipt,
	hosts []HostWorkloadNetworkResult, journal *executionJournal, timeout, interval time.Duration, leases acceptanceLeaseSnapshotReader,
	publish func(validation.Result) (validation.ReportDisposition, error), display validation.Progress,
) (result validation.Result, acceptanceErr error) {
	result = validation.PreparationFailure(validation.ModeFull, validation.FatalInput, nil)
	defer func() {
		acceptanceErr = errors.Join(acceptanceErr, publishDeploymentAcceptance(&result, journal, publish))
		if result.ExitCode() != 0 && !onlySingleHostCoverageIncomplete(result) {
			acceptanceErr = errors.Join(acceptanceErr, fmt.Errorf("full deployment acceptance did not pass; inspect validation checks and report"))
		}
	}()
	fences, err := newDeploymentAcceptanceFences(client, verification, tenant, activation, hosts, journal, timeout, leases)
	if err != nil {
		return result, err
	}
	wrapped := &deploymentAcceptanceObserver{inner: observer, receipts: fences.receipts, physical: fences.physical, scope: plan.RuntimeScope()}
	progress := func(event validation.ProgressEvent) {
		state := "attempted"
		if event.Phase == validation.Completed {
			state = string(event.Outcome.State)
		}
		// The callback cannot return errors; the enclosing observer checks the sticky
		// journal failure before/after reads and the final durable result checks it too.
		_ = journal.record(executionEvent{Stage: hbnRuntimeStage, Operation: string(event.Check.ID), Subject: event.Check.Subject, State: state, ReasonCode: "DEPLOYMENT_ACCEPTANCE_CHECK", Message: event.Outcome.Summary})
		if display != nil {
			display(event)
		}
	}
	wrapped.output = func() error { journal.mu.Lock(); defer journal.mu.Unlock(); return journal.failed }
	runner, err := validation.NewRunner(plan, wrapped, validation.Options{Wait: deploymentAcceptanceWait, PollInterval: interval, Progress: progress})
	if err != nil {
		return result, err
	}
	result = runner.Run(ui.WithOutput(ctx, ui.NewSilent()))
	return result, nil
}

// The legacy one-host provisioned outcome is retained only for this exact gap.
func onlySingleHostCoverageIncomplete(result validation.Result) bool {
	if len(result.Scope.Hosts) != 1 || result.Mode != validation.ModeFull || result.Verdict != validation.VerdictIncomplete || result.Fatal != "" || result.OutputFailed || result.Report.Status != validation.ReportPublished {
		return false
	}
	gap := false
	for _, row := range result.Checks {
		if !row.Required {
			continue
		}
		if row.ID == "runtime/cross-host-coverage" && row.Outcome.State == validation.Unverified && row.Outcome.Reason == validation.ReasonIncomplete {
			gap = true
			continue
		}
		if row.Outcome.State != validation.Passed {
			return false
		}
	}
	return gap
}

type deploymentAcceptanceObserver struct {
	scope              validation.RuntimeScope
	inner              validation.Observer
	receipts, physical func(context.Context) error
	output             func() error
	poisoned           error
}

func (o *deploymentAcceptanceObserver) fence(ctx context.Context, physical bool) error {
	if o.poisoned != nil {
		return o.poisoned
	}
	err := o.output()
	if err == nil && physical {
		err = o.physical(ctx)
	}
	if err == nil {
		err = o.receipts(ctx)
	}
	if err != nil {
		o.poisoned = validationRuntimeStageError(ctx, validation.StageRuntimeCohort, err)
	}
	return o.poisoned
}
func (o *deploymentAcceptanceObserver) ObserveResources(ctx context.Context, progress validation.Progress) (validation.ResourceSnapshot, error) {
	if err := o.fence(ctx, false); err != nil {
		return validation.ResourceSnapshot{}, err
	}
	snapshot, err := o.inner.ObserveResources(ctx, progress)
	return snapshot, errors.Join(err, o.fence(ctx, false))
}
func (o *deploymentAcceptanceObserver) OpenRuntime(ctx context.Context, progress validation.Progress) (validation.RuntimeSession, error) {
	if ctx == nil {
		return nil, &validation.ObservationError{Category: validation.FatalInput}
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return nil, &validation.ObservationError{Category: validation.FatalInput}
	}
	if err := hbn.ValidateRuntimeTrafficBudget(len(o.scope.Hosts), time.Until(deadline)); err != nil {
		return nil, validationRuntimeError(ctx, err, false)
	}
	if err := o.fence(ctx, true); err != nil {
		return nil, err
	}
	inner, err := o.inner.OpenRuntime(ctx, progress)
	if err != nil {
		return nil, err
	}
	if err := o.fence(ctx, false); err != nil {
		return nil, err
	}
	return &deploymentAcceptanceSession{inner: inner, observer: o}, nil
}

type deploymentAcceptanceSession struct {
	mu       sync.Mutex
	inner    validation.RuntimeSession
	observer *deploymentAcceptanceObserver
}

func (s *deploymentAcceptanceSession) Scope() validation.RuntimeScope { return s.inner.Scope() }
func (s *deploymentAcceptanceSession) ObserveBGP(ctx context.Context, device validation.DeviceID) (result validation.BGPObservation, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.observer.fence(ctx, false); err != nil {
		return result, err
	}
	result, err = s.inner.ObserveBGP(ctx, device)
	return result, errors.Join(err, s.observer.fence(ctx, false))
}
func (s *deploymentAcceptanceSession) ObserveRoute(ctx context.Context, pair validation.DirectedDevicePair) (result validation.RouteObservation, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.observer.fence(ctx, false); err != nil {
		return result, err
	}
	result, err = s.inner.ObserveRoute(ctx, pair)
	return result, errors.Join(err, s.observer.fence(ctx, false))
}
func (s *deploymentAcceptanceSession) ProbeTraffic(ctx context.Context, pair validation.DirectedHostPair) (result validation.TrafficObservation, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.observer.fence(ctx, false); err != nil {
		return result, err
	}
	result, err = s.inner.ProbeTraffic(ctx, pair)
	return result, errors.Join(err, s.observer.fence(ctx, false))
}
func (s *deploymentAcceptanceSession) VerifyCohort(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Receipt/physical closure precedes the inner final graph fence. Runner owns
	// invalidating every prior cohort-bound measurement when either closure fails.
	if err := s.observer.fence(ctx, true); err != nil {
		return err
	}
	return s.inner.VerifyCohort(ctx)
}

// A publisher receives no aliases into the canonical acceptance result.
func acceptancePublicationSnapshot(result validation.Result) validation.Result {
	result.Scope.Devices = append([]validation.DeviceID(nil), result.Scope.Devices...)
	result.Scope.Hosts = append([]validation.HostID(nil), result.Scope.Hosts...)
	result.Scope.Routes = append([]validation.DirectedDevicePair(nil), result.Scope.Routes...)
	result.Scope.Traffic = append([]validation.DirectedHostPair(nil), result.Scope.Traffic...)
	result.Checks = append([]validation.CheckResult(nil), result.Checks...)
	copyOutcome := func(o validation.Outcome) validation.Outcome {
		if o.BGP != nil {
			value := *o.BGP
			o.BGP = &value
		}
		if o.Route != nil {
			value := *o.Route
			o.Route = &value
		}
		if o.Traffic != nil {
			value := *o.Traffic
			o.Traffic = &value
		}
		return o
	}
	for i := range result.Checks {
		result.Checks[i].Outcome = copyOutcome(result.Checks[i].Outcome)
		if prior := result.Checks[i].PriorOutcome; prior != nil {
			value := copyOutcome(*prior)
			result.Checks[i].PriorOutcome = &value
		}
	}
	if result.Diagnostic != nil {
		value := *result.Diagnostic
		result.Diagnostic = &value
	}
	if result.DisabledRuntime != nil {
		value := *result.DisabledRuntime
		result.DisabledRuntime = &value
	}
	result.Cause = nil
	return result
}

func publishDeploymentAcceptance(result *validation.Result, journal *executionJournal, publish func(validation.Result) (validation.ReportDisposition, error)) error {
	disposition, publicationErr := publish(acceptancePublicationSnapshot(*result))
	if publicationErr == nil && disposition.Status != validation.ReportPublished {
		disposition = validation.ReportDisposition{Status: validation.ReportFailed}
		publicationErr = fmt.Errorf("deployment acceptance requires a published report")
	}
	validation.ApplyReportPublication(result, disposition, publicationErr)
	journalErr := journal.record(executionEvent{Stage: hbnRuntimeStage, Operation: "acceptance", State: string(result.Verdict), ReasonCode: "DEPLOYMENT_ACCEPTANCE_RESULT", Message: "Full validation result and report disposition retained.", Validation: result})
	if journalErr != nil {
		result.OutputFailed = true
		result.Cause = errors.Join(result.Cause, journalErr)
	}
	return errors.Join(publicationErr, journalErr)
}

type deploymentAcceptanceFences struct {
	receipts, physical func(context.Context) error
}

// newDeploymentAcceptanceFences freezes this operation's authority receipts.
// It observes no runtime measurement and cannot acquire deployment authority.
func newDeploymentAcceptanceFences(client acceptanceReceiptReadSource, verification *controlPlaneVerificationInputs, tenant acceptanceTenantReadSource, activation activationStageReceipt, hosts []HostWorkloadNetworkResult, journal *executionJournal, timeout time.Duration, leases acceptanceLeaseSnapshotReader) (deploymentAcceptanceFences, error) {
	frozen, err := copyActivationVerificationReceipt(activation)
	if err != nil {
		return deploymentAcceptanceFences{}, err
	}
	hosts = append([]HostWorkloadNetworkResult(nil), hosts...)
	objects, err := retainedWorkloadObjects(verification.execution)
	if err != nil {
		return deploymentAcceptanceFences{}, err
	}
	physical := func(ctx context.Context) error {
		return verifyActivationBoundary(ctx, client, verification, tenant, frozen.selected, frozen.DeploymentUID, objects, journal, timeout)
	}
	var anchor *hostNetworkLeaseFence
	receiptFence := func(ctx context.Context) error {
		var current hostNetworkLeaseFence
		err := client.withAcceptanceReceiptReads(ctx, tenant, func(reads activationControlReader, tenantReads acceptanceTenantReceiptReader) error {
			observe, err := newRuntimeAllocationFenceObserver(ctx, reads, verification, tenantReads, frozen, journal, timeout, func(observeCtx context.Context) (hostWorkloadLeaseSnapshot, error) {
				return leases(observeCtx, reads, tenantReads)
			}, hbnRuntimeStage, "DEPLOYMENT_ACCEPTANCE")
			if err != nil {
				return err
			}
			current, err = observe()
			return err
		})
		if err != nil {
			return err
		}
		if anchor != nil && !sameHostNetworkAllocation(*anchor, current) {
			return fmt.Errorf("deployment acceptance allocation changed: %w", errValidationResourceChanged)
		}
		intent := verification.execution.plan.Intent()
		if err := validateHBNRuntimeHostReceipts(frozen, frozen.selected, current, hosts, intent.Networking.WorkloadMTU, intent.Networking.WorkloadCIDR); err != nil {
			return err
		}
		if anchor == nil {
			anchor = &current
		}
		return nil
	}
	return deploymentAcceptanceFences{receipts: receiptFence, physical: physical}, nil
}
