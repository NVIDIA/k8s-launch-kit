// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package validation owns standalone DPF validation coverage and acceptance.
// Observers supply current, read-only evidence; they do not decide acceptance.
package validation

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"
)

type Mode string

const (
	ModeUnknown   Mode = "unknown"
	ModeFull      Mode = "full"
	ModeResources Mode = "resources"
)

type State string

const (
	Passed     State = "passed"
	Failed     State = "failed"
	Pending    State = "pending"
	Unverified State = "unverified"
	Skipped    State = "skipped"
)

type Reason string

const (
	ReasonNone          Reason = ""
	ReasonHealth        Reason = "health"
	ReasonMissing       Reason = "missing"
	ReasonDenied        Reason = "denied"
	ReasonUnavailable   Reason = "unavailable"
	ReasonUnsupported   Reason = "unsupported"
	ReasonIncomplete    Reason = "incomplete"
	ReasonProgressing   Reason = "progressing"
	ReasonDependency    Reason = "dependency"
	ReasonDisabled      Reason = "disabled"
	ReasonInapplicable  Reason = "inapplicable"
	ReasonCanceled      Reason = "canceled"
	ReasonTimedOut      Reason = "timed_out"
	ReasonIdentityDrift Reason = "identity_drift"
	ReasonInvalidInput  Reason = "invalid_input"
)

type CheckID string
type DeviceID string
type HostID string

// Area identifies a fixed DPF resource region, not an arbitrary dependency graph.
type Area string

const (
	AreaPreparation Area = "preparation"
	AreaManagement  Area = "management"
	AreaOperator    Area = "operator"
	AreaDefinitions Area = "definitions"
	AreaSystem      Area = "system"
	AreaTenant      Area = "tenant"
	AreaDevices     Area = "devices"
	AreaSoftware    Area = "software"
	AreaHBN         Area = "hbn"
	AreaWorkloads   Area = "workloads"
	AreaPhysical    Area = "physical"
)

// CheckSpec is derived from retained intent. Coverage checks must prove complete
// selected child sets, including expected children which do not exist yet.
// Subject must be a source-safe display name, never credentials or raw output.
type CheckSpec struct {
	ID       CheckID `json:"id"`
	Area     Area    `json:"area"`
	Subject  string  `json:"subject"`
	Required bool    `json:"required"`
	FullOnly bool    `json:"fullOnly,omitempty"`
	Coverage bool    `json:"coverage,omitempty"`
	// CohortBound resource evidence is invalidated by a later runtime fence.
	CohortBound bool `json:"cohortBound,omitempty"`
}

type DirectedDevicePair struct {
	Source      DeviceID `json:"source"`
	Destination DeviceID `json:"destination"`
}
type DirectedHostPair struct {
	Source      HostID `json:"source"`
	Destination HostID `json:"destination"`
}

// RuntimeScope contains safe selected identities, not commands or transports.
// A session returns independent copies. Routes and Traffic contain the complete
// directed matrices of the retained selected Devices and Hosts respectively.
// Resource-only results omit these unrequested matrices and disclose their
// exact excluded measurement counts through Result.DisabledRuntime.
type RuntimeScope struct {
	Devices []DeviceID           `json:"devices"`
	Hosts   []HostID             `json:"hosts"`
	Routes  []DirectedDevicePair `json:"routes"`
	Traffic []DirectedHostPair   `json:"traffic"`
}

type PlanInput struct {
	Mode    Mode
	Checks  []CheckSpec
	Runtime RuntimeScope
}

// Outcome is a bounded safe projection. Summary and Action may contain only
// reviewed source-safe messages, never errors, transport output or API bodies.
type Outcome struct {
	ID      CheckID       `json:"id"`
	State   State         `json:"state"`
	Reason  Reason        `json:"reason,omitempty"`
	Summary string        `json:"summary,omitempty"`
	Action  string        `json:"action,omitempty"`
	BGP     *BGPFacts     `json:"bgp,omitempty"`
	Route   *RouteFacts   `json:"route,omitempty"`
	Traffic *TrafficFacts `json:"traffic,omitempty"`
}

type BGPFacts struct {
	ExpectedPeers    uint32 `json:"expectedPeers"`
	EstablishedPeers uint32 `json:"establishedPeers"`
	LocalASN         uint32 `json:"localASN,omitempty"`
}
type RouteFacts struct {
	Prefix    netip.Prefix `json:"prefix"`
	Installed bool         `json:"installed"`
}
type TrafficFacts struct {
	Transmitted  uint32 `json:"transmitted"`
	Received     uint32 `json:"received"`
	PayloadBytes uint32 `json:"payloadBytes"`
}

type ResourceSnapshot struct{ Checks []Outcome }
type BGPObservation struct{ Outcome Outcome }
type RouteObservation struct{ Outcome Outcome }
type TrafficObservation struct {
	Ordinary Outcome
	DF       Outcome
}

type ProgressPhase string

const (
	Running   ProgressPhase = "running"
	Completed ProgressPhase = "completed"
)

type ProgressEvent struct {
	Check   CheckSpec
	Phase   ProgressPhase
	Outcome Outcome
}
type Progress func(ProgressEvent)

// Observer is the consumer-owned read/diagnostic port. The adapter is bound to
// the same frozen Plan and mode as Runner. Ordinary missing/forbidden/unhealthy
// evidence belongs in snapshots. Errors are for invocation-wide safety failures;
// a partial snapshot returned with an error is retained.
//
// ObserveResources reports running/completed events around individual checks.
// The supplied sink is safe for concurrent calls and must not be retained after
// return. OpenRuntime is called once, after resource polling, only in full mode.
// Every underlying API/SSH request uses the adapter's frozen 1s–2m timeout
// (15s default). Snapshot lists and selected subjects are finite and bounded.
// Request timeouts are separate from the runner's wait and total runtime budgets.
type Observer interface {
	ObserveResources(context.Context, Progress) (ResourceSnapshot, error)
	OpenRuntime(context.Context, Progress) (RuntimeSession, error)
}

// RuntimeSession permanently anchors one fresh cohort. Every measurement must
// fence the cohort before and after its transport calls. Identity drift poisons
// the session permanently; no replacement or smaller cohort may be re-anchored.
// Health failures with stable identity do not poison independent measurements.
type RuntimeSession interface {
	Scope() RuntimeScope
	ObserveBGP(context.Context, DeviceID) (BGPObservation, error)
	ObserveRoute(context.Context, DirectedDevicePair) (RouteObservation, error)
	ProbeTraffic(context.Context, DirectedHostPair) (TrafficObservation, error)
	VerifyCohort(context.Context) error
}

type FatalCategory string

const (
	FatalInput       FatalCategory = "invalid_input"
	FatalAccess      FatalCategory = "access"
	FatalIdentity    FatalCategory = "identity_drift"
	FatalCanceled    FatalCategory = "canceled"
	FatalTimeout     FatalCategory = "timed_out"
	FatalObservation FatalCategory = "observation"
)

// ObservationError keeps the cause available through errors.Is without exposing
// it to formatting, JSON, progress or reports. Unknown errors become incomplete.
type ObservationError struct {
	Category   FatalCategory
	Cause      error `json:"-"`
	Diagnostic *Diagnostic
}

// DiagnosticStage identifies a finite native observation boundary.
type DiagnosticStage string

const (
	StageRuntimeHostNetwork  DiagnosticStage = "runtime-host-network"
	StageRuntimePhysical     DiagnosticStage = "runtime-physical"
	StageRuntimeCurrentGraph DiagnosticStage = "runtime-current-graph"
	StageRuntimeAllocation   DiagnosticStage = "runtime-allocation"
	StageRuntimeHBN          DiagnosticStage = "runtime-hbn-binding"
	StageRuntimeSFC          DiagnosticStage = "runtime-sfc-anchors"
	StageRuntimeIdentity     DiagnosticStage = "runtime-identity"
	StageRuntimeCohort       DiagnosticStage = "runtime-cohort-fence"
)

// Label is the shared human-facing stage name; JSON keeps the stable value.
func (stage DiagnosticStage) Label() string {
	switch stage {
	case StageRuntimePhysical:
		return "Physical identity and host prerequisites"
	case StageRuntimeHostNetwork:
		return "Host network qualification"
	case StageRuntimeCurrentGraph:
		return "DPU controller state"
	case StageRuntimeAllocation:
		return "Workload allocation"
	case StageRuntimeHBN:
		return "HBN workload binding"
	case StageRuntimeSFC:
		return "Service chains and interfaces"
	case StageRuntimeIdentity:
		return "Management and tenant identity"
	case StageRuntimeCohort:
		return "Runtime cohort verification"
	default:
		return "Runtime observation"
	}
}

// Diagnostic contains trusted, source-safe prose, never an error's raw text.
// It explains an incomplete observation without changing measured outcomes.
type Diagnostic struct {
	Stage   DiagnosticStage `json:"stage"`
	Reason  Reason          `json:"reason"`
	Summary string          `json:"summary"`
	Action  string          `json:"action,omitempty"`
}

func (e *ObservationError) Error() string {
	return "DPF validation observation could not complete safely"
}
func (e *ObservationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Options struct {
	Wait                   time.Duration
	ConnectivityTimeout    time.Duration
	ConnectivityTimeoutSet bool
	PollInterval           time.Duration
	Progress               Progress
}

type Verdict string

const (
	VerdictPassed          Verdict = "passed"
	VerdictResourcesPassed Verdict = "resource_checks_passed"
	VerdictFailed          Verdict = "failed"
	VerdictIncomplete      Verdict = "incomplete"
)

type CheckResult struct {
	CheckSpec
	Outcome Outcome `json:"outcome"`
	// PriorOutcome preserves measurement-time evidence invalidated by a later fence.
	PriorOutcome *Outcome `json:"priorOutcome,omitempty"`
}
type ReportStatus string

const (
	ReportPending     ReportStatus = "pending"
	ReportDisabled    ReportStatus = "disabled"
	ReportUnavailable ReportStatus = "unavailable"
	ReportPublished   ReportStatus = "published"
	ReportFailed      ReportStatus = "failed"
)

type ReportDisposition struct {
	Status ReportStatus `json:"status"`
	Path   string       `json:"path,omitempty"`
}

// RuntimeCoverage describes intentionally excluded measurements without
// expanding an unrequested directed matrix into individual result rows.
type RuntimeCoverage struct {
	BGP             int `json:"bgp"`
	Routes          int `json:"routes"`
	OrdinaryTraffic int `json:"ordinaryTraffic"`
	DFTraffic       int `json:"dfTraffic"`
}

// Result is the one evaluation model for text, JSON, HTML and exit decisions.
// Cause is library-only. Publication never changes the measured verdict.
type Result struct {
	Mode               Mode              `json:"mode"`
	Scope              RuntimeScope      `json:"scope"`
	DisabledRuntime    *RuntimeCoverage  `json:"disabledRuntime,omitempty"`
	Verdict            Verdict           `json:"verdict"`
	Checks             []CheckResult     `json:"checks"`
	RuntimeBudget      time.Duration     `json:"-"`
	Fatal              FatalCategory     `json:"fatal,omitempty"`
	Report             ReportDisposition `json:"report"`
	OutputFailed       bool              `json:"outputFailed,omitempty"`
	Cause              error             `json:"-"`
	Diagnostic         *Diagnostic       `json:"diagnostic,omitempty"`
	diagnosticPriority int
}

// MarshalJSON keeps the canonical duration useful to callers while presenting
// an explicit human unit, rather than an unexplained nanosecond count.
func (r Result) MarshalJSON() ([]byte, error) {
	type fields Result
	r.Diagnostic = NormalizeDiagnostic(r.Diagnostic)
	return json.Marshal(struct {
		fields
		RuntimeBudget string `json:"runtimeBudget"`
	}{fields: fields(r), RuntimeBudget: r.RuntimeBudget.String()})
}
