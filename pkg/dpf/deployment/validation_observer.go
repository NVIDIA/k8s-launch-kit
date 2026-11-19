// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/chartfetch"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"github.com/nvidia/k8s-launch-kit/pkg/helmclient"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// validationManagementReader deliberately excludes creation, SSAR, installation,
// tunnel, journal and reset capabilities. Existing transports stay behind this port.
type validationManagementReader interface {
	resourceReader
	resourceLister
	tenantSourceReader
	VerifyIdentity(context.Context) error
	discoverResourceList(context.Context, string) (*metav1.APIResourceList, error)
	listHBNRuntimeObjects(context.Context, string, string, string) ([]unstructured.Unstructured, error)
	getHBNRuntimeApplication(context.Context, string, string) (*unstructured.Unstructured, error)
	observeRegistrySnapshot(context.Context, string) (registrySnapshot, error)
	observeValidationTenantStorage(context.Context, string) managementprobe.TenantStorageResult
	inspectValidationOperator(context.Context, *operatorDesired) error
}

type validationObserver struct {
	physical         validationPhysicalReaders
	desired          *managementDesired
	request          config.Request
	unavailable      []config.AccessIssue
	mode             validation.Mode
	checks           []validation.CheckSpec
	runtimeScope     validation.RuntimeScope
	systemMu         sync.Mutex
	systemArtifacts  *systemArtifacts
	fetchSystemChart func(context.Context, string) ([]byte, error)
	newManagement    func(context.Context) (validationManagementReader, error)
}

// NewValidationObserver compiles immutable expected coverage offline. Connections
// are opened only by observation; a validated plan is reused without regeneration.
func NewValidationObserver(retained *bundle.Bundle, plan *generation.Plan, request config.Request, unavailable []config.AccessIssue, mode validation.Mode) (validation.Plan, validation.Observer, error) {
	desired, err := prepareManagementDesiredFromPlan(retained, plan)
	if err != nil {
		return validation.Plan{}, nil, err
	}
	if mode != validation.ModeFull && mode != validation.ModeResources {
		return validation.Plan{}, nil, fmt.Errorf("validation mode must be full or resources")
	}
	if request.RequestTimeout == 0 {
		request.RequestTimeout = 15 * time.Second
	}
	if request.RequestTimeout < time.Second || request.RequestTimeout > 2*time.Minute {
		return validation.Plan{}, nil, fmt.Errorf("request timeout must be between one second and two minutes")
	}
	request, err = config.SnapshotRequestForValidation(request)
	if err != nil {
		return validation.Plan{}, nil, err
	}
	if err := validateValidationPhysicalScope(desired, request); err != nil {
		return validation.Plan{}, nil, err
	}
	o := &validationObserver{desired: desired, request: request, unavailable: append([]config.AccessIssue(nil), unavailable...), mode: mode}
	runtimeChecks, scope, err := validationRuntimeChecks(plan)
	if err != nil {
		return validation.Plan{}, nil, err
	}
	o.runtimeScope = scope
	o.fetchSystemChart = (chartfetch.Downloader{}).Fetch
	o.checks = append(o.validationResourceChecks(), runtimeChecks...)
	o.checks = append(o.checks, o.physicalValidationChecks()...)
	compiled, err := validation.NewPlan(validation.PlanInput{Mode: mode, Checks: o.checks, Runtime: scope})
	if err != nil {
		return validation.Plan{}, nil, err
	}
	// Freeze explicit credential/trust files once without network access. A
	// local access failure remains an observation obligation, not lost coverage.
	access, accessErr := managementaccess.Open(request, nil)
	o.newManagement = func(ctx context.Context) (validationManagementReader, error) {
		if accessErr != nil {
			return nil, accessErr
		}
		client, err := apiClientForAccess(access, plan.Intent().Management, plan.Intent().KubernetesVersion, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		if err := client.VerifyIdentity(ctx); err != nil {
			return nil, err
		}
		return client, nil
	}
	return compiled, o, nil
}

func (o *validationObserver) ObserveResources(ctx context.Context, progress validation.Progress) (validation.ResourceSnapshot, error) {
	active := make([]validation.CheckSpec, 0, len(o.checks))
	for _, spec := range o.checks {
		if o.mode != validation.ModeResources || !spec.FullOnly {
			active = append(active, spec)
		}
	}
	r := newValidationRecorder(active, progress)
	if ctx == nil {
		return r.snapshot(), &validation.ObservationError{Category: validation.FatalInput}
	}
	var fatal error
	r.start("management/identity")
	management, err := o.newManagement(ctx)
	if err != nil {
		r.recordError("management/identity", err)
		if validationIdentityChanged(err) {
			fatal = &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
		}
	} else {
		r.record("management/identity", validation.Passed, validation.ReasonNone, "Management API identity, trust and Kubernetes version match retained intent.", "")
		fatal = o.observeManagementResources(ctx, management, r)
		if fatal == nil {
			tenant, tenantErr := o.observeTenantResources(ctx, management, r)
			if validationIdentityChanged(tenantErr) {
				fatal = &validation.ObservationError{Category: validation.FatalIdentity, Cause: tenantErr}
			}
			if fatal == nil {
				fatal = o.observeSystemResources(ctx, management, tenant, r)
			}
			if fatal == nil {
				fatal = o.observeValidationRuntimeResources(ctx, management, tenant, r)
			}
		}
	}
	if fatal == nil && management != nil {
		if err := management.VerifyIdentity(ctx); err != nil {
			r.recordError("management/identity", err)
			if validationIdentityChanged(err) {
				fatal = &validation.ObservationError{Category: validation.FatalIdentity, Cause: err}
			}
		}
	}
	// Physical sources are independent of unavailable management/tenant access.
	if ctx.Err() == nil {
		if err := o.observePhysical(ctx, r); err != nil {
			if fatal == nil {
				fatal = err
			} else {
				fatal = errors.Join(fatal, err)
			}
		}
	}

	r.blockRemaining()
	if fatal == nil && ctx.Err() != nil {
		fatal = &validation.ObservationError{Category: validation.FatalCanceled, Cause: ctx.Err()}
	}
	return r.snapshot(), fatal
}

var errValidationResourceChanged = errors.New("selected validation resource changed during observation")

func validationIdentityChanged(err error) bool {
	return errors.Is(err, errManagementIdentityChanged) || errors.Is(err, errTenantIdentityChanged) || errors.Is(err, errValidationResourceChanged)
}

// inspectValidationOperator only retrieves Helm storage and compares retained
// chart, values and manifest. No install preparation or callback is constructed.
func (c *APIClient) inspectValidationOperator(ctx context.Context, desired *operatorDesired) error {
	if err := c.VerifyIdentity(ctx); err != nil {
		return err
	}
	cfg, err := operatorRESTConfig(ctx, c.access.REST, c.access.REST.Timeout)
	if err != nil {
		return err
	}
	actions, err := helmclient.NewActionConfig(cfg, desired.namespace, helmclient.StorageDriver)
	if err != nil {
		return err
	}
	installed, err := (helmActionClient{config: actions}).Get(ctx, desired.releaseName)
	if err != nil {
		return err
	}
	if mismatch := operatorDesiredReleaseMismatchReason(installed, desired); mismatch != "" {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Installed Helm release differs from the retained chart, values or manifest.")
	}
	return c.VerifyIdentity(ctx)
}

type validationCheckError struct {
	state   validation.State
	reason  validation.Reason
	summary string
}

func (e *validationCheckError) Error() string { return "validation check did not pass" }
func validationFailure(state validation.State, reason validation.Reason, summary string) error {
	return &validationCheckError{state: state, reason: reason, summary: summary}
}

type validationRecorder struct {
	specs    map[validation.CheckID]validation.CheckSpec
	order    []validation.CheckID
	outcomes map[validation.CheckID]validation.Outcome
	progress validation.Progress
}

func newValidationRecorder(checks []validation.CheckSpec, progress validation.Progress) *validationRecorder {
	r := &validationRecorder{specs: map[validation.CheckID]validation.CheckSpec{}, outcomes: map[validation.CheckID]validation.Outcome{}, progress: progress}
	for _, s := range checks {
		r.specs[s.ID] = s
		r.order = append(r.order, s.ID)
	}
	return r
}
func (r *validationRecorder) start(id validation.CheckID) {
	if _, ok := r.specs[id]; !ok {
		return
	}
	if r.progress != nil {
		r.progress(validation.ProgressEvent{Check: r.specs[id], Phase: validation.Running})
	}
}
func (r *validationRecorder) record(id validation.CheckID, state validation.State, reason validation.Reason, summary, action string) {
	if _, ok := r.specs[id]; !ok {
		return
	}
	out := validation.Outcome{ID: id, State: state, Reason: reason, Summary: summary, Action: action}
	r.outcomes[id] = out
	if r.progress != nil {
		r.progress(validation.ProgressEvent{Check: r.specs[id], Phase: validation.Completed, Outcome: out})
	}
}
func (r *validationRecorder) recordError(id validation.CheckID, err error) {
	var check *validationCheckError
	switch {
	case errors.As(err, &check):
		r.record(id, check.state, check.reason, check.summary, "Inspect the selected dependency and repeat validation.")
	case validationIdentityChanged(err):
		r.record(id, validation.Unverified, validation.ReasonIdentityDrift, "Bound resource identity changed during observation.", "Repeat discovery and generation after inspecting the replacement.")
	case apierrors.IsNotFound(err) || errors.Is(err, driver.ErrReleaseNotFound):
		r.record(id, validation.Failed, validation.ReasonMissing, "A required selected resource is absent.", "Inspect the responsible controller and selected resource.")
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		r.record(id, validation.Unverified, validation.ReasonDenied, "The caller cannot inspect all required evidence.", "Grant read access to the selected source and repeat validation.")
	case errors.Is(err, context.Canceled):
		r.record(id, validation.Unverified, validation.ReasonCanceled, "Observation was canceled.", "Repeat validation.")
	case errors.Is(err, context.DeadlineExceeded):
		r.record(id, validation.Unverified, validation.ReasonTimedOut, "A bounded observation request timed out.", "Inspect source reachability and repeat validation.")
	default:
		r.record(id, validation.Unverified, validation.ReasonUnavailable, "Required current evidence could not be completely inspected.", "Inspect the selected source and repeat validation.")
	}
}
func (r *validationRecorder) run(id validation.CheckID, check func() error) error {
	r.start(id)
	err := check()
	if err != nil {
		r.recordError(id, err)
	} else {
		r.record(id, validation.Passed, validation.ReasonNone, "Current selected evidence matches the retained expectation.", "")
	}
	return err
}
func (r *validationRecorder) blockRemaining() {
	for _, id := range r.order {
		if _, ok := r.outcomes[id]; !ok {
			r.record(id, validation.Unverified, validation.ReasonDependency, "A required dependency could not be verified.", "Resolve the reported dependency and repeat validation.")
		}
	}
}
func (r *validationRecorder) snapshot() validation.ResourceSnapshot {
	result := validation.ResourceSnapshot{}
	for _, id := range r.order {
		if out, ok := r.outcomes[id]; ok {
			result.Checks = append(result.Checks, out)
		}
	}
	return result
}
