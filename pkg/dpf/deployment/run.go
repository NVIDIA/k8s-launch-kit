// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/types"
)

const (
	defaultRunPollInterval   = 5 * time.Second
	defaultRunRequestTimeout = 30 * time.Second
	maxRunSoftwareBytes      = int64(16 << 30)
	maxRunDuration           = 2 * time.Hour
)

// RunStateFailed means one or more deployment stages did not complete.
const RunStateFailed = "failed"

// RunStateProvisioned means DPF provisioning completed but HBN traffic remains unverified.
const RunStateProvisioned = "provisioned"

// RunStateValidated means the complete selected HBN runtime matrix passed.
const RunStateValidated = "validated"

// PrerequisiteVerification is a fresh, point-in-time observation made by the
// invoking adapter. The returned inventory is kept private to this run and is
// used to bind later physical actions.
type PrerequisiteVerification struct {
	Inventory *inventory.Inventory
	Checks    []inventory.Check
}

// PrerequisiteVerifier must freshly bind the current site to the retained plan
// and report every deploy-phase prerequisite. It must not mutate its inputs.
type PrerequisiteVerifier func(context.Context, config.Request, *generation.Plan) (PrerequisiteVerification, error)

// ChartSourceVerifier compares the currently selected chart source with the
// exact archive retained in the bundle. It is repeated at stage boundaries.
type ChartSourceVerifier func(context.Context, *generation.Plan) (PreflightCheck, error)

// ActivationDPU identifies one selected DPU without exposing BMC credentials.
type ActivationDPU struct {
	SerialNumber   string
	DPUName        string
	DPUUID         types.UID
	HostAdapterKey string
	AdapterSerial  string
}

// ActivationNode identifies one selected DPUNode covered by a host reboot.
type ActivationNode struct {
	NodeName     string
	NodeUID      types.UID
	RebootMethod string
	SelectedDPUs []ActivationDPU
}

// ActivationRequest binds one external reboot to an observed host adapter and
// the exact selected DPUNode cohort. The callback resolves private SSH access
// from its own explicit request using HostInputRef and HostEndpoint.
type ActivationRequest struct {
	HostInputRef      string
	HostEndpoint      string
	HostSystemUUID    string
	HostAdapterKey    string
	HostAdapterSerial string
	DPUNodes          []ActivationNode
	// ResetSafetyExpiresAt is the earliest local BMC proof expiry across this
	// host's cohort. It is transient attempt evidence, not a claim identity.
	ResetSafetyExpiresAt time.Time
}

// ActivationObservation contains bounded post-reboot identity observations.
type ActivationObservation struct {
	HostSystemUUID      string
	HostAdapterSerial   string
	PreviousBootID      string
	CurrentBootID       string
	Reconnected         bool
	KubernetesNodeReady *bool
}

// ExternalRebooter performs the configured, explicitly selected external
// reboot operation. It receives no credentials; the adapter owns access
// resolution and must validate host and adapter identity before the reboot.
// It must enforce ResetSafetyExpiresAt at command submission. After dispatch,
// timeout is an unknown outcome; the physical command must never be retried.
type ExternalRebooter interface {
	RebootExternal(context.Context, ActivationRequest) (ActivationObservation, error)
}

// RebooterFunc adapts a function to ExternalRebooter.
type RebooterFunc func(context.Context, ActivationRequest) (ActivationObservation, error)

func (f RebooterFunc) RebootExternal(ctx context.Context, request ActivationRequest) (ActivationObservation, error) {
	return f(ctx, request)
}

// RunRequest contains one retained bundle and explicit, already-resolved site
// access. Run never loads ambient kubeconfig or resolves credentials itself.
type RunRequest struct {
	Bundle              *bundle.Bundle
	Access              config.Request
	ReportPath          string
	ProbeJournalPath    string
	VerifyPrerequisites PrerequisiteVerifier
	VerifyChartSource   ChartSourceVerifier
	Rebooter            ExternalRebooter
	PollInterval        time.Duration
	RequestTimeout      time.Duration
	MaxSoftwareBytes    int64
}

// RunResult reports only safe identifiers and typed checks. Detailed stage
// events are retained in the append-only report at ReportPath.
type RunResult struct {
	BundleDigest       string                      `json:"bundleDigest"`
	State              string                      `json:"state"`
	LastCompletedStage string                      `json:"lastCompletedStage,omitempty"`
	ReportPath         string                      `json:"reportPath"`
	TrafficValidated   bool                        `json:"trafficValidated"`
	HBNRuntime         *HBNRuntimeResult           `json:"hbnRuntime,omitempty"`
	Activation         *ActivationSummary          `json:"activation,omitempty"`
	HostNetworking     []HostWorkloadNetworkResult `json:"hostNetworking,omitempty"`
	Checks             []inventory.Check           `json:"checks"`
}

// ActivationSummary contains only the object identities confirmed by the last
// stage. It is not a networking or traffic health assertion.
// RebootedHosts retains verified physical reboots even when a later controller
// acknowledgement or readiness check fails.
type ActivationSummary struct {
	DPUDeploymentUID string               `json:"dpuDeploymentUID"`
	DPUSets          []ActivationObjectID `json:"dpuSets"`
	Nodes            []ActivationObjectID `json:"nodes"`
	DPUs             []ActivationObjectID `json:"dpus"`
	RebootedHosts    []string             `json:"rebootedHosts"`
}

// ActivationObjectID is the safe name/UID pair of a confirmed DPF object.
type ActivationObjectID struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// Run validates the retained bundle before access, checks fresh prerequisites,
// and executes the fixed management, tenant, enrollment, software, workload,
// DPUDeployment, external-activation, selected host-network and native HBN runtime stages against
// one identity-bound client and the explicit selected host access.
// A complete cross-host matrix establishes point-in-time HBN workload traffic;
// a one-host run remains provisioned with cross-host traffic unmeasured.
func Run(ctx context.Context, request RunRequest) (result RunResult, runErr error) {
	if ctx == nil {
		return RunResult{}, fmt.Errorf("deployment run requires a context")
	}
	if _, ok := ctx.Deadline(); !ok {
		return RunResult{}, fmt.Errorf("deployment run requires a deadline")
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > maxRunDuration {
		return RunResult{}, fmt.Errorf("deployment deadline must be in the next two hours")
	}
	if err := validateRunRequest(request); err != nil {
		return RunResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}
	execution, err := prepareManagementExecution(request.Bundle, remaining)
	if err != nil {
		return RunResult{}, fmt.Errorf("validate retained deployment bundle: %w", err)
	}
	plan := execution.plan
	request.ReportPath, err = canonicalNewFilePath(request.ReportPath)
	if err != nil {
		return RunResult{}, fmt.Errorf("validate execution report path")
	}
	request.ProbeJournalPath, err = canonicalNewFilePath(request.ProbeJournalPath)
	if err != nil || request.ReportPath == request.ProbeJournalPath {
		return RunResult{}, fmt.Errorf("execution and probe journals must use distinct output files")
	}
	result = RunResult{BundleDigest: plan.Digest(), State: RunStateFailed, ReportPath: request.ReportPath, Checks: []inventory.Check{}}
	journal, err := newExecutionJournal(request.ReportPath, plan.Digest())
	if err != nil {
		return result, err
	}
	probeLog, err := openProbeExecutionJournal(request.ProbeJournalPath)
	if err != nil {
		_ = journal.close()
		return result, err
	}
	progress := ui.FromContext(ctx).StartProgress("Preparing HBN DPF deployment")
	defer func() {
		runErr = finishRunReport(&result, runErr, probeLog.close, journal, plan.Intent().Namespace)
		if runErr != nil {
			progress.Fail("HBN DPF deployment stopped; inspect the execution report")
		} else if result.TrafficValidated {
			progress.Success("HBN runtime validated: BGP, installed routes and every directed host traffic pair passed")
		} else {
			progress.Success("DPF resources are provisioned; cross-host traffic remains unmeasured")
		}
	}()

	interval, requestTimeout, maxBytes := normalizeRunControls(request)
	access := request.Access
	access.RequestTimeout = requestTimeout
	intent := plan.Intent()
	if err := journal.record(executionEvent{Stage: "fresh-prerequisites", Operation: "observe", State: "attempted",
		ReasonCode: "FRESH_PREREQUISITES_STARTED", Message: "Bind current management and host observations to the retained deployment plan."}); err != nil {
		return result, err
	}
	progress.Update("Verifying fresh management and host prerequisites")
	observerRequest, err := cloneRequestForPrerequisites(access)
	if err != nil {
		return result, fmt.Errorf("copy prerequisite request")
	}
	verification, err := request.VerifyPrerequisites(ctx, observerRequest, execution.plan)
	result.Checks = appendRunChecks(result.Checks, verification.Checks)
	if len(verification.Checks) > 0 {
		if journalErr := journalPrerequisiteChecks(journal, verification.Checks); journalErr != nil {
			return result, journalErr
		}
	}
	if err != nil {
		result.Checks = append(result.Checks, inventory.Check{
			ID: "dpf.hbn.deployment.fresh-prerequisites", Subject: intent.Namespace, Vantage: "management-cluster",
			RequiredPhase: "deploy", State: "unknown", ReasonCode: "FRESH_PREREQUISITES_UNAVAILABLE",
			Message: "Fresh management identity and deploy prerequisites could not be established.",
		})
		if journalErr := journal.record(executionEvent{Stage: "fresh-prerequisites", Operation: "check", State: "unknown",
			ReasonCode: "FRESH_PREREQUISITES_UNAVAILABLE", Message: "Fresh management identity and deploy prerequisites could not be established."}); journalErr != nil {
			return result, journalErr
		}
		return result, fmt.Errorf("fresh deployment prerequisite observation failed")
	}
	if err := validatePrerequisiteVerification(execution.plan, verification); err != nil {
		if journalErr := journal.record(executionEvent{Stage: "fresh-prerequisites", Operation: "check", State: "failed",
			ReasonCode: "FRESH_PREREQUISITES_BLOCKED", Message: "Fresh management identity or required deployment checks do not match the retained plan."}); journalErr != nil {
			return result, journalErr
		}
		return result, err
	}
	if err := validateSelectedInventory(execution.plan, verification.Inventory); err != nil {
		return result, err
	}
	currentInventory, err := cloneInventory(verification.Inventory)
	if err != nil {
		return result, fmt.Errorf("copy fresh management inventory")
	}
	access.Input.Site.Inventory = currentInventory
	if err := journal.record(executionEvent{Stage: "fresh-prerequisites", Operation: "check", State: "confirmed",
		ReasonCode: "FRESH_PREREQUISITES_PASSED", Message: "Fresh management identity and deploy-phase prerequisites match the retained plan."}); err != nil {
		return result, err
	}
	client, err := NewAPIClient(ctx, access, intent.Management, intent.KubernetesVersion)
	if err != nil {
		return result, fmt.Errorf("bind management client to retained identity: %w", err)
	}
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: probeLog.record}
	prepared, err := prepareControlPlaneWithExecution(ctx, client, execution, journal, probes, access, interval, requestTimeout)
	if err != nil {
		return result, err
	}
	progress.Update("Checking retained workload permissions and chart source")
	objects, err := checkWorkloadPreparationInputs(ctx, client, prepared, journal, workloadChartSourceVerifier(request.VerifyChartSource))
	if err != nil {
		return result, err
	}
	controlPlane, err := runPreparedControlPlane(ctx, client, prepared, probes, journal, interval, requestTimeout)
	if err != nil {
		return result, err
	}
	result.LastCompletedStage = generation.StageEnrollment
	progress.Update("Staging software and HBN workload resources")
	workload, err := runPreparedWorkload(ctx, client, prepared, controlPlane, probes, journal, interval, requestTimeout, maxBytes,
		workloadChartSourceVerifier(request.VerifyChartSource), objects)
	if err != nil {
		return result, err
	}
	result.LastCompletedStage = generation.StageWorkload
	progress.Update("Activating selected DPU nodes and waiting for DPF readiness")
	activation, err := runActivationStage(ctx, client, prepared, controlPlane, workload, currentInventory, access, journal, interval, requestTimeout, request.Rebooter)
	result.Activation = activationSummary(activation)
	if err != nil {
		return result, err
	}
	result.LastCompletedStage = generation.StageDeployment
	progress.Update("Configuring selected host workload interfaces from fresh DPF allocations")
	result.HostNetworking, err = runHostWorkloadNetworkStage(ctx, client, prepared, controlPlane, workload, activation, journal, requestTimeout,
		func(observeCtx context.Context) (hostWorkloadLeaseSnapshot, error) {
			return observeHostWorkloadLeaseSnapshot(observeCtx, client, controlPlane.tenant.Client, execution.plan, execution.reconciler, workload, requestTimeout)
		}, sshprobe.ConfigureHostWorkloadNetwork)
	if err != nil {
		return result, err
	}
	result.LastCompletedStage = hostWorkloadNetworkStage
	progress.Update("Validating HBN BGP, installed routes and all directed host traffic pairs")
	runtimeResult, err := runHBNRuntimeStage(ctx, client, prepared, controlPlane, workload, activation, result.HostNetworking, journal, requestTimeout, hbnRuntimeTransports{
		readLeases: func(observeCtx context.Context) (hostWorkloadLeaseSnapshot, error) {
			return observeHostWorkloadLeaseSnapshot(observeCtx, client, controlPlane.tenant.Client, execution.plan, execution.reconciler, workload, requestTimeout)
		},
		readCommand: controlPlane.tenant.Client.hbnExec.read, probeTraffic: sshprobe.ProbeHostWorkloadTraffic,
	})
	result.HBNRuntime = &runtimeResult
	if err != nil {
		return result, err
	}
	result.TrafficValidated = runtimeResult.TrafficValidated
	if result.TrafficValidated {
		result.LastCompletedStage = hbnRuntimeStage
	}
	return result, nil
}

func activationSummary(receipt activationStageReceipt) *ActivationSummary {
	if receipt.BundleDigest == "" && receipt.DeploymentUID == "" && len(receipt.DPUSets) == 0 && len(receipt.Nodes) == 0 && len(receipt.DPUs) == 0 && len(receipt.Reboots) == 0 {
		return nil
	}
	summary := &ActivationSummary{DPUDeploymentUID: string(receipt.DeploymentUID), DPUSets: []ActivationObjectID{}, Nodes: []ActivationObjectID{}, DPUs: []ActivationObjectID{}, RebootedHosts: []string{}}
	for _, object := range receipt.DPUSets {
		summary.DPUSets = append(summary.DPUSets, ActivationObjectID{Name: object.Name, UID: object.UID})
	}
	for _, object := range receipt.Nodes {
		summary.Nodes = append(summary.Nodes, ActivationObjectID{Name: object.Name, UID: object.UID})
	}
	for _, object := range receipt.DPUs {
		summary.DPUs = append(summary.DPUs, ActivationObjectID{Name: object.Name, UID: object.UID})
	}
	for _, reboot := range receipt.Reboots {
		summary.RebootedHosts = append(summary.RebootedHosts, reboot.HostInputRef)
	}
	return summary
}

func validateRunRequest(request RunRequest) error {
	if request.Bundle == nil || request.VerifyPrerequisites == nil || request.VerifyChartSource == nil || request.Rebooter == nil {
		return fmt.Errorf("deployment run requires a retained bundle and prerequisite, chart-source and external-reboot callbacks")
	}
	if request.ReportPath == "" || request.ProbeJournalPath == "" {
		return fmt.Errorf("deployment run requires separate durable report paths")
	}
	if request.PollInterval < 0 || request.PollInterval > time.Minute || request.RequestTimeout < 0 ||
		(request.RequestTimeout != 0 && (request.RequestTimeout < time.Second || request.RequestTimeout > 2*time.Minute)) ||
		request.MaxSoftwareBytes < 0 || request.MaxSoftwareBytes > maxRunSoftwareBytes {
		return fmt.Errorf("deployment polling, request and software-size controls exceed supported bounds")
	}
	return nil
}

func canonicalNewFilePath(filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("filename is required")
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(absolute)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("output directory must exist")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(absolute)), nil
}

func normalizeRunControls(request RunRequest) (time.Duration, time.Duration, int64) {
	interval := request.PollInterval
	if interval == 0 {
		interval = defaultRunPollInterval
	}
	requestTimeout := request.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultRunRequestTimeout
	}
	maxBytes := request.MaxSoftwareBytes
	if maxBytes == 0 {
		maxBytes = maxRunSoftwareBytes
	}
	return interval, requestTimeout, maxBytes
}

func cloneInventory(in *inventory.Inventory) (*inventory.Inventory, error) {
	if in == nil {
		return nil, fmt.Errorf("inventory required")
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out inventory.Inventory
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if in.Management != nil && out.Management != nil {
		for i := range in.Management.Nodes {
			if i < len(out.Management.Nodes) {
				out.Management.Nodes[i].Labels = maps.Clone(in.Management.Nodes[i].Labels)
			}
		}
	}
	return &out, nil
}

func cloneRequestForPrerequisites(request config.Request) (config.Request, error) {
	targets := append([]config.Target(nil), request.Targets...)
	for i := range targets {
		if targets[i].SSH != nil {
			ssh := *targets[i].SSH
			targets[i].SSH = &ssh
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return config.Request{}, err
	}
	var cloned config.Request
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return config.Request{}, err
	}
	// Access deliberately contains private resolved credentials. Preserve its
	// immutable value snapshots without serializing or reconstructing them.
	cloned.Targets = targets
	return cloned, nil
}

type probeExecutionJournal struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	failed error
	closed bool
}

func openProbeExecutionJournal(filename string) (*probeExecutionJournal, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create new probe resource journal: %w", err)
	}
	journal := &probeExecutionJournal{file: file, writer: bufio.NewWriter(file)}
	if err := journal.record([]probe.Receipt{}); err != nil {
		_ = journal.close()
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(filename))
	if err != nil {
		_ = journal.close()
		return nil, fmt.Errorf("open probe journal directory for persistence")
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		_ = journal.close()
		return nil, fmt.Errorf("persist probe journal directory: %w", err)
	}
	return journal, nil
}

func (j *probeExecutionJournal) record(receipts []probe.Receipt) error {
	if j == nil {
		return fmt.Errorf("probe resource journal is unavailable")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.file == nil {
		return fmt.Errorf("probe resource journal is closed")
	}
	if j.failed != nil {
		return j.failed
	}
	data, err := json.Marshal(struct {
		Receipts []probe.Receipt `json:"receipts"`
	}{Receipts: receipts})
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = j.writer.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = j.writer.Flush()
	}
	if err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.failed = fmt.Errorf("persist probe resource journal: %w", err)
		return j.failed
	}
	return nil
}

func (j *probeExecutionJournal) close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.writer != nil {
		if err := j.writer.Flush(); err != nil {
			j.failed = errors.Join(j.failed, err, j.file.Close())
			return j.failed
		}
	}
	j.failed = errors.Join(j.failed, j.file.Close())
	return j.failed
}

func validatePrerequisiteVerification(plan *generation.Plan, verification PrerequisiteVerification) error {
	if plan == nil || verification.Inventory == nil || verification.Inventory.Management == nil {
		return fmt.Errorf("fresh management inventory is required before deployment")
	}
	if verification.Inventory.FormatVersion != inventory.FormatVersion {
		return fmt.Errorf("fresh inventory format is unsupported")
	}
	intent := plan.Intent()
	management := verification.Inventory.Management
	if management.APIEndpoint != intent.Management.APIEndpoint || management.SystemNamespaceUID != intent.Management.SystemNamespaceUID ||
		management.TrustDigest != intent.Management.TrustDigest || management.KubernetesVersion != intent.KubernetesVersion {
		return fmt.Errorf("fresh management inventory does not match the retained bundle identity")
	}
	if len(verification.Checks) == 0 {
		return fmt.Errorf("fresh deploy prerequisites were not reported")
	}
	seen := make(map[string]struct{}, len(verification.Checks))
	deployChecks, failed := 0, false
	for _, check := range verification.Checks {
		if check.ID == "" || check.Subject == "" || check.Vantage == "" || check.RequiredPhase == "" {
			return fmt.Errorf("fresh deploy prerequisite check is incomplete")
		}
		if check.RequiredPhase != "generate" && check.RequiredPhase != "deploy" {
			return fmt.Errorf("fresh prerequisite has an unsupported required phase")
		}
		if check.State != "passed" && check.State != "failed" && check.State != "unknown" {
			return fmt.Errorf("fresh deploy prerequisite has an unsupported state")
		}
		key := check.ID + "\x00" + check.Subject + "\x00" + check.Vantage
		if _, ok := seen[key]; ok {
			return fmt.Errorf("fresh deploy prerequisites contain duplicate check identities")
		}
		seen[key] = struct{}{}
		if check.State != "passed" && (check.ReasonCode == "" || check.Message == "") {
			return fmt.Errorf("non-passing prerequisite lacks an actionable reason")
		}
		if check.RequiredPhase == "deploy" {
			deployChecks++
			failed = failed || check.State != "passed"
		}
	}
	if deployChecks == 0 {
		return fmt.Errorf("fresh deploy prerequisites contain no deploy-phase checks")
	}
	if failed {
		return fmt.Errorf("fresh deploy prerequisites are not ready")
	}
	return nil
}

func journalPrerequisiteChecks(journal *executionJournal, checks []inventory.Check) error {
	for _, check := range checks {
		state := "confirmed"
		if check.State == "unknown" {
			state = "unknown"
		} else if check.State != "passed" {
			state = "failed"
		}
		if err := journal.record(executionEvent{
			Stage: "fresh-prerequisites", Operation: "check", Subject: check.ID + "/" + check.Subject,
			State: state, ReasonCode: check.ReasonCode, Message: check.Message,
		}); err != nil {
			return err
		}
	}
	return nil
}

func appendRunChecks(destination, source []inventory.Check) []inventory.Check {
	for _, check := range source {
		check.EvidenceRefs = nil
		destination = append(destination, check)
	}
	return destination
}

func validateSelectedInventory(plan *generation.Plan, current *inventory.Inventory) error {
	if plan == nil || current == nil || current.FormatVersion != inventory.FormatVersion {
		return fmt.Errorf("fresh inventory is required in the current format")
	}
	intent := plan.Intent()
	selected := make(map[string]generation.ResolvedDevice, len(intent.Devices))
	selectedEndpoints := make(map[netip.AddrPort]string, len(intent.Devices))
	for _, device := range intent.Devices {
		if device.Serial == "" {
			return fmt.Errorf("retained DPU identity is incomplete")
		}
		if _, exists := selected[device.Serial]; exists {
			return fmt.Errorf("retained DPU identities are ambiguous")
		}
		selected[device.Serial] = device
		ip, err := netip.ParseAddr(device.BMCIP)
		if err != nil || !ip.Is4() || device.BMCPort < 1 || device.BMCPort > 65535 {
			return fmt.Errorf("retained DPU endpoint is incomplete")
		}
		endpoint := netip.AddrPortFrom(ip, uint16(device.BMCPort))
		if _, duplicate := selectedEndpoints[endpoint]; duplicate {
			return fmt.Errorf("retained DPU endpoints are ambiguous")
		}
		selectedEndpoints[endpoint] = device.Serial
	}
	observed := make(map[string]int, len(selected))
	for _, device := range current.DPUs {
		if device.BMCEndpoint != "" {
			endpoint, err := enrollmentEndpoint(device.BMCEndpoint)
			if err == nil {
				if serial, selectedEndpoint := selectedEndpoints[endpoint]; selectedEndpoint && serial != device.SerialNumber {
					return fmt.Errorf("fresh inventory assigns a selected BMC endpoint to another DPU identity")
				}
			}
		}
		want, selectedDevice := selected[device.SerialNumber]
		if !selectedDevice {
			continue
		}
		endpoint, err := enrollmentEndpoint(device.BMCEndpoint)
		if err != nil {
			return fmt.Errorf("fresh selected DPU endpoint is invalid")
		}
		if endpoint.Addr().String() != want.BMCIP || int(endpoint.Port()) != want.BMCPort || device.ObservationState != "observed" || device.ID == "" {
			return fmt.Errorf("fresh selected DPU identity or BMC endpoint differs from the retained bundle")
		}
		observed[device.SerialNumber]++
	}
	for serial := range selected {
		if observed[serial] != 1 {
			return fmt.Errorf("fresh inventory must contain each selected DPU exactly once")
		}
	}
	return nil
}

// finishRunReport is the final durability gate shared by every Run return path.
// A close/sync failure preserves typed partial receipts but revokes the cohort
// pass before the caller sees RunResult.
func finishRunReport(result *RunResult, runErr error, closeProbe func() error, journal *executionJournal, namespace string) error {
	var reportErr error
	if err := closeProbe(); err != nil {
		reportErr = errors.Join(reportErr, fmt.Errorf("close probe resource journal: %w", err))
		runErr = errors.Join(runErr, reportErr)
	}
	state, code, message := "failed", "DEPLOYMENT_RUN_FAILED", "Deployment stopped; inspect the preceding stage checks and execution report."
	if runErr == nil {
		state, code, message = "passed", "DPF_RESOURCES_READY", "DPF resources and selected host workload configuration reached readiness; cross-host HBN traffic remains unmeasured."
		result.State = RunStateProvisioned
		if result.TrafficValidated {
			result.State = RunStateValidated
			code = "HBN_RUNTIME_COHORT_VALIDATED"
			message = "All selected HBN BGP sessions, remote FIB routes and directed host ordinary/DF traffic probes passed with final cohort fences."
		}
	}
	result.Checks = append(result.Checks, inventory.Check{
		ID: "dpf.hbn.deployment.dpf-resources", Subject: namespace, Vantage: "management-cluster",
		RequiredPhase: "deploy", State: state, ReasonCode: code, Message: message,
	})
	if err := journal.record(executionEvent{Stage: "run", Operation: "complete", State: state, ReasonCode: code, Message: message}); err != nil {
		reportErr = errors.Join(reportErr, err)
		runErr = errors.Join(runErr, err)
	}
	if err := journal.close(); err != nil {
		reportErr = errors.Join(reportErr, fmt.Errorf("close execution report: %w", err))
		runErr = errors.Join(runErr, reportErr)
	}
	if runErr != nil {
		result.State = RunStateFailed
		result.TrafficValidated = false
		if result.HBNRuntime != nil {
			result.HBNRuntime.TrafficValidated = false
			if result.HBNRuntime.State == "passed" {
				result.HBNRuntime.State = "failed"
				result.HBNRuntime.ReasonCode = "HBN_RUNTIME_REPORT_UNCERTAIN"
			}
		}
		if result.LastCompletedStage == hbnRuntimeStage {
			result.LastCompletedStage = hostWorkloadNetworkStage
		}
		check := &result.Checks[len(result.Checks)-1]
		if reportErr != nil {
			check.ReasonCode = "EXECUTION_REPORT_UNCERTAIN"
			check.Message = "The run report could not be durably completed; inspect its last recorded stage before retrying."
		} else if runErr != nil {
			check.ReasonCode = "DEPLOYMENT_RUN_FAILED"
			check.Message = "A deployment stage did not complete; inspect the stage checks and execution report."
		}
		check.State = "failed"
	}
	return runErr
}
