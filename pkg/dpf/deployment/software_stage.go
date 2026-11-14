// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type softwareStageManagementClient interface {
	enrollmentManagementClient
	softwareVerificationClient
	checkSoftwarePermissions(context.Context, *unstructured.Unstructured, string) []PreflightCheck
}

var _ softwareStageManagementClient = (*APIClient)(nil)

// softwarePreparationReceipt reports only staged cache verification. It does
// not report workload or DPUDeployment readiness.
type softwarePreparationReceipt struct {
	BundleDigest string
	ControlPlane controlPlaneReceipts
	Software     softwareVerificationReceipt
}

// runSoftwarePreparation owns the retained execution across control-plane and
// software staging. It stops after verifying selected software bytes; it does
// not create the remaining workload resources or DPUDeployment.
func runSoftwarePreparation(ctx context.Context, client softwareStageManagementClient, retained *bundle.Bundle,
	journal *executionJournal, probes probe.Runner, request config.Request, interval, requestTimeout time.Duration, maxBytes int64,
) (softwarePreparationReceipt, error) {
	if ctx == nil || client == nil || journal == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return softwarePreparationReceipt{}, fmt.Errorf("software preparation requires a bounded context, management client and durable report")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return softwarePreparationReceipt{}, fmt.Errorf("software preparation requires an execution deadline")
	}
	prepared, err := prepareControlPlaneRun(ctx, client, retained, journal, probes, request, interval, requestTimeout)
	if err != nil {
		return softwarePreparationReceipt{}, err
	}
	receipts, err := runPreparedControlPlane(ctx, client, prepared, probes, journal, interval, requestTimeout)
	if err != nil {
		return softwarePreparationReceipt{BundleDigest: prepared.execution.plan.Digest(), ControlPlane: receipts}, err
	}
	software, err := runSoftwareStage(ctx, client, prepared.execution, receipts, prepared.selected, prepared.password, journal,
		interval, requestTimeout, maxBytes)
	return softwarePreparationReceipt{BundleDigest: prepared.execution.plan.Digest(), ControlPlane: receipts, Software: software}, err
}

// runSoftwareStage creates or verifies only the retained BlueFieldSoftware
// object and validates the bytes currently served from the DPF cache. The
// receipts are fresh identity boundaries, not a lease on later DPU operations.
func runSoftwareStage(ctx context.Context, client softwareStageManagementClient, execution *managementExecution,
	receipts controlPlaneReceipts, selected []enrollmentBMC, password string, journal *executionJournal,
	interval, requestTimeout time.Duration, maxBytes int64,
) (softwareVerificationReceipt, error) {
	if ctx == nil || client == nil || execution == nil || execution.plan == nil || execution.reconciler == nil || journal == nil ||
		interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return softwareVerificationReceipt{}, fmt.Errorf("software stage requires the retained execution, selected enrollment and bounded controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return softwareVerificationReceipt{}, fmt.Errorf("software stage requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_VERIFICATION_CANCELED", "Software verification was canceled before its receipt could be issued.", err)
	}
	digest := execution.plan.Digest()
	if err := validateEnrollmentStageInputs(ctx, client, execution, receipts.management, receipts.tenant, selected, password,
		journal, interval, requestTimeout); err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_RECEIPT_MISMATCH", "Control-plane receipts or selected enrollment do not match the retained plan.", err)
	}
	if journal.digest != digest || receipts.enrollment.BundleDigest != digest || receipts.enrollment.TenantUID == "" ||
		receipts.enrollment.TenantUID != receipts.tenant.ClusterUID || receipts.enrollment.Secret.UID == "" ||
		len(receipts.enrollment.Devices) != len(selected) ||
		!softwareEnrollmentReceiptMatches(receipts.enrollment.Devices, selected) {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_RECEIPT_MISMATCH", "Software staging receipts do not identify the same retained bundle, tenant and selected devices.", nil)
	}
	intent := execution.plan.Intent()
	if !softwareSelectedMatchesIntent(intent, selected) {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_SELECTED_SCOPE_CHANGED", "Selected runtime BMCs do not match the retained device cohort.", nil)
	}
	if !client.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "The bound management client differs from the retained software plan.", nil)
	}
	desired, err := retainedBlueFieldSoftware(execution)
	if err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_PLAN_INVALID", "The retained workload stage does not contain one exact BlueFieldSoftware resource.", err)
	}
	checks := client.checkSoftwarePermissions(ctx, desired.DeepCopy(), execution.operatorNamespace)
	if err := journalSoftwareChecks(journal, checks); err != nil {
		return softwareVerificationReceipt{}, err
	}
	if len(checks) == 0 {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_PREFLIGHT_INCOMPLETE", "Software API and registry permissions were not verified.", nil)
	}
	for _, check := range checks {
		if check.State != "passed" {
			return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_PREFLIGHT_BLOCKED", "Software API or registry permissions are not ready.", nil)
		}
	}
	release, err := releases.Resolve(intent.Release)
	if err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_RELEASE_INVALID", "The selected software release is unavailable in the retained catalog.", err)
	}
	if err := verifySoftwareStageBoundary(ctx, client, execution, receipts, selected, requestTimeout, password, journal); err != nil {
		return softwareVerificationReceipt{}, err
	}
	registryNamespace := execution.operatorNamespace
	registry, err := client.observeRegistrySnapshot(ctx, registryNamespace)
	if err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_REGISTRY_UNAVAILABLE", "The retained provisioning registry could not be freshly inspected before download activation.", err)
	}
	if registry.namespace != registryNamespace || registry.manager.container.image != release.Operator.Image {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_MANAGER_RELEASE_MISMATCH", "The active provisioning manager does not match the retained DPF release and namespace.", nil)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed before BlueFieldSoftware creation.", err)
	}
	current, err := executeNativeObject(ctx, client, execution.reconciler, desired, generation.StageWorkload, journal, true, interval)
	if err != nil {
		return softwareVerificationReceipt{}, err
	}
	if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "verify-software-cache", Subject: desired.GetNamespace() + "/" + desired.GetName(),
		State: "attempted", ReasonCode: "SOFTWARE_CACHE_VERIFICATION_STARTED", Message: "Read selected cached components through the current release-bound DPF registry and compare retained digests."}); err != nil {
		return softwareVerificationReceipt{}, err
	}
	verified, err := verifyStagedSoftware(ctx, client, execution.reconciler, desired, current,
		intent.Software, selectedSoftwarePSIDs(intent), registryNamespace, release.Operator.Image, maxBytes)
	if err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_BYTES_UNVERIFIED", "The selected cached software bytes could not be verified against retained digests.", err)
	}
	if err := verifySoftwareStageBoundary(ctx, client, execution, receipts, selected, requestTimeout, password, journal); err != nil {
		return softwareVerificationReceipt{}, err
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed before the software receipt.", err)
	}
	for _, component := range verified.Components {
		subject := component.Component
		if component.PSID != "" {
			subject += "/" + component.PSID
		}
		if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "software-component", Subject: subject,
			State: "confirmed", ReasonCode: "SOFTWARE_COMPONENT_DIGEST_VERIFIED",
			Message: fmt.Sprintf("Verified %d bytes with retained SHA-256 %s.", component.Bytes, component.SHA256)}); err != nil {
			return softwareVerificationReceipt{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_VERIFICATION_CANCELED", "Software verification was canceled before its receipt could be issued.", err)
	}
	event := executionEvent{Stage: generation.StageWorkload, Operation: "verify-software-cache", Subject: desired.GetNamespace() + "/" + desired.GetName(),
		State: "confirmed", ReasonCode: "SOFTWARE_BYTES_VERIFIED", Message: "The selected cached software bytes matched retained digests at the verified DPF registry endpoint."}
	if err := journal.record(event); err != nil {
		return softwareVerificationReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return softwareVerificationReceipt{}, softwareStageFailure(journal, "SOFTWARE_VERIFICATION_CANCELED", "Software verification was canceled before its receipt could be issued.", err)
	}
	return verified, nil
}

func verifySoftwareStageBoundary(ctx context.Context, client enrollmentManagementClient, execution *managementExecution,
	receipts controlPlaneReceipts, selected []enrollmentBMC, requestTimeout time.Duration, password string, journal *executionJournal,
) error {
	if receipts.enrollment.BundleDigest != execution.plan.Digest() || !softwareEnrollmentReceiptMatches(receipts.enrollment.Devices, selected) {
		return softwareStageFailure(journal, "SOFTWARE_RECEIPT_MISMATCH", "Selected enrollment receipt does not match the retained hardware cohort.", nil)
	}
	_, ready, err := observeSelectedEnrollment(ctx, client, execution, selected, receipts.enrollment.Devices, journal)
	if err != nil {
		return softwareStageFailure(journal, "SOFTWARE_ENROLLMENT_UNVERIFIED", "Selected Device and DPUNode identities could not be refreshed before software staging.", err)
	}
	if !ready {
		return softwareStageFailure(journal, "SOFTWARE_ENROLLMENT_CHANGED", "A selected DPU lost its recorded Device or DPUNode readiness before software staging.", nil)
	}
	return verifyEnrollmentBoundary(ctx, client, execution, receipts.management, receipts.tenant, selected,
		requestTimeout, receipts.enrollment.Secret, password, journal)
}

func softwareEnrollmentReceiptMatches(receipts []enrolledDeviceReceipt, selected []enrollmentBMC) bool {
	if len(receipts) != len(selected) {
		return false
	}
	for i, receipt := range receipts {
		device := selected[i].device
		if receipt.Serial != device.Serial || receipt.DeviceUID == "" || receipt.DPUNodeUID == "" ||
			receipt.BMCIP != device.BMCIP || receipt.BMCPort != device.BMCPort {
			return false
		}
	}
	return true
}

func softwareSelectedMatchesIntent(intent generation.ResolvedIntent, selected []enrollmentBMC) bool {
	if len(intent.Devices) == 0 || len(intent.Devices) != len(selected) {
		return false
	}
	for i, wanted := range intent.Devices {
		got := selected[i].device
		if got.Serial != wanted.Serial || got.PSID != wanted.PSID || got.BMCIP != wanted.BMCIP || got.BMCPort != wanted.BMCPort {
			return false
		}
	}
	return true
}

func retainedBlueFieldSoftware(execution *managementExecution) (*unstructured.Unstructured, error) {
	if execution == nil || execution.plan == nil {
		return nil, fmt.Errorf("validated retained plan is required")
	}
	intent := execution.plan.Intent()
	var result *unstructured.Unstructured
	foundStage := false
	for _, view := range execution.plan.Stages() {
		stage := view.Stage()
		if stage.ID != generation.StageWorkload {
			continue
		}
		if foundStage || stage.Destination != bundle.Management || stage.Namespace != intent.Namespace {
			return nil, fmt.Errorf("retained workload stage identity is ambiguous")
		}
		foundStage = true
		for _, document := range view.Documents() {
			object := document.ObjectCopy()
			if object.GetKind() != "BlueFieldSoftware" {
				continue
			}
			if result != nil || object.GetAPIVersion() != blueFieldSoftwareAPIVersion || object.GetNamespace() != stage.Namespace || object.GetName() == "" {
				return nil, fmt.Errorf("retained workload stage BlueFieldSoftware identity is invalid or duplicated")
			}
			result = object
		}
	}
	if !foundStage || result == nil {
		return nil, fmt.Errorf("retained workload stage or BlueFieldSoftware is missing")
	}
	return result, nil
}

func selectedSoftwarePSIDs(intent generation.ResolvedIntent) []string {
	psids := make([]string, 0, len(intent.Devices))
	for _, device := range intent.Devices {
		psids = append(psids, device.PSID)
	}
	return psids
}

func journalSoftwareChecks(journal *executionJournal, checks []PreflightCheck) error {
	for _, check := range checks {
		state := "confirmed"
		if check.State != "passed" {
			state = check.State
			if state == "" {
				state = "unknown"
			}
		}
		if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "preflight", Subject: check.ID,
			State: state, ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
			return err
		}
	}
	return nil
}

func softwareStageFailure(journal *executionJournal, code, message string, cause error) error {
	return journalFailure(journal, executionEvent{Stage: generation.StageWorkload, Operation: "software-stage", State: "failed",
		ReasonCode: code, Message: message}, false, code, message, cause)
}
