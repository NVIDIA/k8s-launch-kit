// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const workloadChartSourceCheckID = "hbn-chart-source-matches-archive"

// workloadChartSourceVerifier checks the current selected repository response
// against the archive retained in the validated plan. A pass is a point-in-time
// source check; it does not attest what the DPF controller later downloads.
type workloadChartSourceVerifier func(context.Context, *generation.Plan) (PreflightCheck, error)

type workloadStageClient interface {
	softwareStageManagementClient
	List(context.Context, *unstructured.Unstructured, string) ([]unstructured.Unstructured, error)
	checkCallerPermissions(context.Context, preflightPlan) PreflightResult
	getTenantWorkloadChild(context.Context, *tenantClient, *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

var _ workloadStageClient = (*APIClient)(nil)

// workloadPreparationReceipt covers only retained workload prerequisites. It
// never includes or implies creation/readiness of DPUDeployment.
type workloadPreparationReceipt struct {
	BundleDigest string
	ControlPlane controlPlaneReceipts
	Software     softwareVerificationReceipt
	Resources    []workloadResourceReceipt
}

type workloadResourceReceipt struct {
	Kind       string
	Namespace  string
	Name       string
	UID        types.UID
	Generation int64
	ChildUID   types.UID
}

// runWorkloadPreparation validates one retained execution, checks the chart
// source and all required workload permissions before effects, then carries the
// same execution through management, tenant, enrollment, cached-software and
// workload-prerequisite stages. It deliberately stops before DPUDeployment.
func runWorkloadPreparation(ctx context.Context, client workloadStageClient, retained *bundle.Bundle,
	journal *executionJournal, probes probe.Runner, request config.Request, interval, requestTimeout time.Duration,
	maxBytes int64, verifyChartSource workloadChartSourceVerifier,
) (workloadPreparationReceipt, error) {
	if ctx == nil || client == nil || journal == nil || verifyChartSource == nil || interval <= 0 ||
		requestTimeout < time.Second || requestTimeout > 2*time.Minute || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return workloadPreparationReceipt{}, fmt.Errorf("workload preparation requires a bound client, retained plan, chart verifier, durable report and bounded controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return workloadPreparationReceipt{}, fmt.Errorf("workload preparation requires an execution deadline")
	}
	prepared, err := prepareControlPlaneRun(ctx, client, retained, journal, probes, request, interval, requestTimeout)
	if err != nil {
		return workloadPreparationReceipt{}, err
	}
	objects, err := checkWorkloadPreparationInputs(ctx, client, prepared, journal, verifyChartSource)
	if err != nil {
		return workloadPreparationReceipt{}, err
	}
	execution := prepared.execution
	controlPlane, err := runPreparedControlPlane(ctx, client, prepared, probes, journal, interval, requestTimeout)
	if err != nil {
		return workloadPreparationReceipt{BundleDigest: execution.plan.Digest(), ControlPlane: controlPlane}, err
	}
	return runPreparedWorkload(ctx, client, prepared, controlPlane, probes, journal, interval, requestTimeout, maxBytes, verifyChartSource, objects)
}

// checkWorkloadPreparationInputs performs all workload checks that must happen
// before the management bootstrap can have controller side effects.
func checkWorkloadPreparationInputs(ctx context.Context, client workloadStageClient, prepared *preparedControlPlaneRun,
	journal *executionJournal, verifyChartSource workloadChartSourceVerifier,
) ([]*unstructured.Unstructured, error) {
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil ||
		journal == nil || verifyChartSource == nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_INPUT_INVALID", "Workload preparation requires one retained execution, chart verifier and durable report.", nil)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, workloadStageFailure(journal, "WORKLOAD_DEADLINE_MISSING", "Workload preparation requires an execution deadline.", nil)
	}
	if journal.digest != prepared.execution.plan.Digest() {
		return nil, workloadStageFailure(journal, "WORKLOAD_BUNDLE_MISMATCH", "The execution report does not identify the retained plan.", nil)
	}
	objects, err := retainedWorkloadObjects(prepared.execution)
	if err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_PLAN_INVALID", "The retained workload prerequisite set is invalid.", err)
	}
	if err := checkWorkloadChartSource(ctx, prepared.execution.plan, verifyChartSource, journal, "chart-source-preflight"); err != nil {
		return nil, err
	}
	preflight := client.checkCallerPermissions(ctx, prepared.execution.preflight)
	if err := journalWorkloadChecks(journal, preflight.Checks); err != nil {
		return nil, err
	}
	if len(preflight.Checks) == 0 || !preflight.Ready {
		return nil, workloadStageFailure(journal, "WORKLOAD_PREFLIGHT_BLOCKED", "The caller lacks a required retained-resource permission or a required API is unavailable.", nil)
	}
	for _, check := range preflight.Checks {
		if check.State != "passed" {
			return nil, workloadStageFailure(journal, "WORKLOAD_PREFLIGHT_BLOCKED", "The caller lacks a required workload resource permission or API is unavailable.", nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_PREPARATION_CANCELED", "Workload preparation was canceled before management effects.", err)
	}
	return objects, nil
}

// runPreparedWorkload carries the already prepared control-plane execution
// through software verification and retained workload prerequisites.
func runPreparedWorkload(ctx context.Context, client workloadStageClient, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, probes probe.Runner, journal *executionJournal, interval, requestTimeout time.Duration,
	maxBytes int64, verifyChartSource workloadChartSourceVerifier, objects []*unstructured.Unstructured,
) (workloadPreparationReceipt, error) {
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil ||
		journal == nil || verifyChartSource == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute ||
		maxBytes <= 0 || maxBytes == math.MaxInt64 || len(objects) != 9 {
		return workloadPreparationReceipt{}, workloadStageFailure(journal, "WORKLOAD_INPUT_INVALID", "Workload stage requires one retained execution, complete prerequisites and bounded controls.", nil)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return workloadPreparationReceipt{}, workloadStageFailure(journal, "WORKLOAD_DEADLINE_MISSING", "Workload stage requires an execution deadline.", nil)
	}
	execution := prepared.execution
	if journal.digest != execution.plan.Digest() || controlPlane.management.BundleDigest != execution.plan.Digest() ||
		controlPlane.tenant.ClusterUID == "" || controlPlane.enrollment.BundleDigest != execution.plan.Digest() {
		return workloadPreparationReceipt{}, workloadStageFailure(journal, "WORKLOAD_RECEIPT_MISMATCH", "The prepared control-plane receipts do not identify this retained plan.", nil)
	}
	if err := checkWorkloadChartSource(ctx, execution.plan, verifyChartSource, journal, "chart-source-before-workload"); err != nil {
		return workloadPreparationReceipt{BundleDigest: execution.plan.Digest(), ControlPlane: controlPlane}, err
	}
	software, err := runSoftwareStage(ctx, client, execution, controlPlane, prepared.selected, prepared.password, journal, interval, requestTimeout, maxBytes)
	if err != nil {
		return workloadPreparationReceipt{BundleDigest: execution.plan.Digest(), ControlPlane: controlPlane}, err
	}
	resources, err := runWorkloadStage(ctx, client, execution, controlPlane, software, prepared.selected, prepared.password, journal, interval, requestTimeout, objects)
	return workloadPreparationReceipt{BundleDigest: execution.plan.Digest(), ControlPlane: controlPlane, Software: software, Resources: resources}, err
}

func checkWorkloadChartSource(ctx context.Context, plan *generation.Plan, verifier workloadChartSourceVerifier, journal *executionJournal, operation string) error {
	if err := ctx.Err(); err != nil {
		return workloadStageFailure(journal, "HBN_CHART_SOURCE_CANCELED", "The HBN chart source check was canceled before workload writes.", err)
	}
	event := executionEvent{Stage: generation.StageWorkload, Operation: operation, Subject: workloadChartSourceCheckID, State: "attempted",
		ReasonCode: "HBN_CHART_SOURCE_CHECK_STARTED", Message: "Compare current HBN chart source bytes with the archive retained by this bundle before workload writes."}
	if err := journal.record(event); err != nil {
		return err
	}
	check, err := verifier(ctx, plan)
	if err != nil {
		return workloadStageFailure(journal, "HBN_CHART_SOURCE_UNVERIFIED", "The HBN chart source did not pass the retained-archive check; no workload resources were created.", err)
	}
	validReason := safeWorkloadReasonCode(check.ReasonCode)
	valid := check.ID == workloadChartSourceCheckID && check.State == "passed" && validReason && check.Message != ""
	if !valid {
		return workloadStageFailure(journal, "HBN_CHART_SOURCE_UNVERIFIED", "The HBN chart source did not match a complete retained-archive check; no workload resources were created.", nil)
	}
	event.State, event.ReasonCode = "confirmed", check.ReasonCode
	event.Message = "The current HBN chart source bytes matched the archive retained by this bundle before workload writes."
	if err := journal.record(event); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return workloadStageFailure(journal, "HBN_CHART_SOURCE_CANCELED", "The HBN chart source check was canceled before workload writes.", err)
	}
	return nil
}

func safeWorkloadReasonCode(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func retainedWorkloadObjects(execution *managementExecution) ([]*unstructured.Unstructured, error) {
	if execution == nil || execution.plan == nil {
		return nil, fmt.Errorf("validated retained plan is required")
	}
	intent := execution.plan.Intent()
	view, ok := execution.planStage(generation.StageWorkload)
	if !ok {
		return nil, fmt.Errorf("retained workload stage is missing")
	}
	stage := view.Stage()
	if stage.Destination != bundle.Management || stage.Namespace != intent.Namespace {
		return nil, fmt.Errorf("retained workload stage destination differs from the selected intent")
	}
	counts := map[string]int{}
	objects := make([]*unstructured.Unstructured, 0, 9)
	for _, document := range view.Documents() {
		object := document.ObjectCopy()
		if object == nil || object.GetNamespace() != intent.Namespace || object.GetName() == "" || object.GetAPIVersion() == "" {
			return nil, fmt.Errorf("retained workload document has incomplete identity")
		}
		switch object.GetKind() {
		case "BlueFieldSoftware", "DPUFlavor", "DPUServiceConfiguration", "DPUServiceTemplate", "DPUServiceIPAM", "DPUServiceInterface":
			counts[object.GetKind()]++
		case "DPUDeployment":
			return nil, fmt.Errorf("DPUDeployment must remain in the separate deployment stage")
		default:
			return nil, fmt.Errorf("retained workload stage contains an unsupported resource kind")
		}
		objects = append(objects, object)
	}
	want := map[string]int{"BlueFieldSoftware": 1, "DPUFlavor": 1, "DPUServiceConfiguration": 1, "DPUServiceTemplate": 1, "DPUServiceIPAM": 2, "DPUServiceInterface": 3}
	if len(objects) != 9 || !reflect.DeepEqual(counts, want) {
		return nil, fmt.Errorf("retained workload stage does not contain the fixed HBN prerequisite set")
	}
	return objects, nil
}

func (e *managementExecution) planStage(id string) (bundle.StageView, bool) {
	if e == nil || e.plan == nil {
		return bundle.StageView{}, false
	}
	for _, stage := range e.plan.Stages() {
		if stage.Stage().ID == id {
			return stage, true
		}
	}
	return bundle.StageView{}, false
}

type workloadCheckMode uint8

const (
	workloadNoReadiness workloadCheckMode = iota
	workloadTemplateReadiness
	workloadIPAMReadiness
	workloadInterfaceReadiness
)

type workloadResourceSpec struct {
	object *unstructured.Unstructured
	mode   workloadCheckMode
}

// runWorkloadStage creates or verifies only the fixed HBN prerequisites. It
// never reads or applies StageDeployment and never reports dataplane readiness.
func runWorkloadStage(ctx context.Context, client workloadStageClient, execution *managementExecution,
	controlPlane controlPlaneReceipts, software softwareVerificationReceipt, selected []enrollmentBMC, password string,
	journal *executionJournal, interval, requestTimeout time.Duration, objects []*unstructured.Unstructured,
) ([]workloadResourceReceipt, error) {
	if ctx == nil || client == nil || execution == nil || execution.plan == nil || execution.reconciler == nil || journal == nil ||
		interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute || len(objects) != 9 {
		return nil, workloadStageFailure(journal, "WORKLOAD_INPUT_INVALID", "Workload preparation requires a retained execution and complete prerequisite set.", nil)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, workloadStageFailure(journal, "WORKLOAD_DEADLINE_MISSING", "Workload preparation requires an execution deadline.", nil)
	}
	if err := validateEnrollmentStageInputs(ctx, client, execution, controlPlane.management, controlPlane.tenant, selected, password,
		journal, interval, requestTimeout); err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_RECEIPT_MISMATCH", "Control-plane and selected-device receipts do not match the retained workload plan.", err)
	}
	if software.SoftwareUID == "" || software.Generation <= 0 || len(software.Components) == 0 || len(selected) == 0 {
		return nil, workloadStageFailure(journal, "WORKLOAD_SOFTWARE_RECEIPT_INVALID", "Verified cached software bytes and selected devices are required before workload resources.", nil)
	}
	if err := verifySoftwareStageBoundary(ctx, client, execution, controlPlane, selected, requestTimeout, password, journal); err != nil {
		return nil, err
	}
	if err := verifyStagedSoftwareResource(ctx, client, execution, software); err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_SOFTWARE_RESOURCE_CHANGED", "The verified BlueFieldSoftware resource changed before workload preparation.", err)
	}
	if err := validateWorkloadTargeting(ctx, client, execution, controlPlane.tenant, selected, objects); err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_TARGET_SCOPE_INVALID", "Retained workload selectors do not target only the selected DPUCluster and devices.", err)
	}
	if err := verifyWorkloadAPIs(ctx, client, objects); err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_API_UNAVAILABLE", "A required DPF workload API is not currently served as retained.", err)
	}
	specs, err := orderWorkloadObjects(objects)
	if err != nil {
		return nil, workloadStageFailure(journal, "WORKLOAD_PLAN_INVALID", "The retained workload prerequisite set is invalid.", err)
	}
	receipts := make([]workloadResourceReceipt, 0, len(specs))
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return receipts, workloadStageFailure(journal, "WORKLOAD_CANCELED", "Workload preparation stopped before all retained prerequisites were verified.", err)
		}
		current, err := executeNativeObject(ctx, client, execution.reconciler, spec.object, generation.StageWorkload, journal, false, interval)
		if err != nil {
			return receipts, err
		}
		if current.GetGeneration() <= 0 || current.GetResourceVersion() == "" {
			return receipts, workloadStageFailure(journal, "WORKLOAD_RESOURCE_RECEIPT_INCOMPLETE", "A workload prerequisite has no stable generation or resource version.", nil)
		}
		if spec.mode != workloadNoReadiness {
			childUID, err := waitWorkloadResource(ctx, client, execution.reconciler, controlPlane.tenant.Client, spec, current.GetUID(), journal, interval)
			if err != nil {
				return receipts, err
			}
			receipts = append(receipts, workloadResourceReceipt{Kind: spec.object.GetKind(), Namespace: spec.object.GetNamespace(), Name: spec.object.GetName(), UID: current.GetUID(), Generation: current.GetGeneration(), ChildUID: childUID})
		} else {
			receipts = append(receipts, workloadResourceReceipt{Kind: spec.object.GetKind(), Namespace: spec.object.GetNamespace(), Name: spec.object.GetName(), UID: current.GetUID(), Generation: current.GetGeneration()})
		}
	}
	if err := verifySoftwareStageBoundary(ctx, client, execution, controlPlane, selected, requestTimeout, password, journal); err != nil {
		return receipts, err
	}
	if err := verifyStagedSoftwareResource(ctx, client, execution, software); err != nil {
		return receipts, workloadStageFailure(journal, "WORKLOAD_SOFTWARE_RESOURCE_CHANGED", "The verified BlueFieldSoftware resource changed before the workload-preparation receipt.", err)
	}
	if err := verifyWorkloadReceiptSet(ctx, client, execution, controlPlane.tenant.Client, objects, receipts); err != nil {
		return receipts, workloadStageFailure(journal, "WORKLOAD_RESOURCE_SET_CHANGED", workloadChildVerificationMessage("A previously verified workload prerequisite or tenant child changed before the preparation receipt.", err), err)
	}
	if err := validateWorkloadTargeting(ctx, client, execution, controlPlane.tenant, selected, objects); err != nil {
		return receipts, workloadStageFailure(journal, "WORKLOAD_TARGET_SCOPE_CHANGED", "The selected DPUCluster target scope changed during workload preparation.", err)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return receipts, workloadStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "Management cluster identity changed before the workload-preparation receipt.", err)
	}
	if err := ctx.Err(); err != nil {
		return receipts, workloadStageFailure(journal, "WORKLOAD_CANCELED", "Workload preparation was canceled before its receipt could be issued.", err)
	}
	if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "workload-preparation", Subject: execution.plan.Digest(), State: "confirmed",
		ReasonCode: "WORKLOAD_PREREQUISITES_PREPARED", Message: "The retained DPF workload prerequisites and controller-owned tenant objects match this bundle; DPUDeployment was not created."}); err != nil {
		return receipts, err
	}
	if err := ctx.Err(); err != nil {
		return receipts, workloadStageFailure(journal, "WORKLOAD_CANCELED", "Workload preparation was canceled before its receipt could be issued.", err)
	}
	return receipts, nil
}

func verifyStagedSoftwareResource(ctx context.Context, client workloadStageClient, execution *managementExecution, receipt softwareVerificationReceipt) error {
	if ctx == nil || client == nil || execution == nil || execution.plan == nil || execution.reconciler == nil || receipt.SoftwareUID == "" || receipt.Generation <= 0 {
		return fmt.Errorf("verified software identity and retained plan are required")
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return err
	}
	desired, err := retainedBlueFieldSoftware(execution)
	if err != nil {
		return err
	}
	current, err := client.Get(ctx, desired.DeepCopy())
	if err != nil {
		return safeAPIError("read verified BlueFieldSoftware", err)
	}
	if current == nil || current.GetUID() != receipt.SoftwareUID || current.GetGeneration() != receipt.Generation || execution.reconciler.Verify(desired, current) != nil {
		return fmt.Errorf("verified BlueFieldSoftware identity or retained spec changed")
	}
	if readiness := EvaluateReadiness(current); !readiness.Ready {
		return fmt.Errorf("verified BlueFieldSoftware is no longer ready: %s", readiness.ReasonCode)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func orderWorkloadObjects(objects []*unstructured.Unstructured) ([]workloadResourceSpec, error) {
	order := map[string]int{"DPUFlavor": 0, "DPUServiceConfiguration": 1, "DPUServiceTemplate": 2, "DPUServiceIPAM": 3, "DPUServiceInterface": 4}
	result := make([]workloadResourceSpec, 0, len(objects)-1)
	seenNames := map[string]bool{}
	for _, object := range objects {
		if object == nil || object.GetKind() == "BlueFieldSoftware" {
			continue
		}
		if seenNames[object.GetKind()+"/"+object.GetNamespace()+"/"+object.GetName()] {
			return nil, fmt.Errorf("retained workload contains a duplicate resource")
		}
		seenNames[object.GetKind()+"/"+object.GetNamespace()+"/"+object.GetName()] = true
		if _, ok := order[object.GetKind()]; !ok {
			return nil, fmt.Errorf("unsupported workload resource kind")
		}
		mode := workloadNoReadiness
		switch object.GetKind() {
		case "DPUServiceTemplate":
			mode = workloadTemplateReadiness
		case "DPUServiceIPAM":
			mode = workloadIPAMReadiness
		case "DPUServiceInterface":
			mode = workloadInterfaceReadiness
		}
		result = append(result, workloadResourceSpec{object: object.DeepCopy(), mode: mode})
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := order[result[i].object.GetKind()], order[result[j].object.GetKind()]
		if left != right {
			return left < right
		}
		return result[i].object.GetName() < result[j].object.GetName()
	})
	if len(result) != 8 {
		return nil, fmt.Errorf("retained workload does not contain eight non-software prerequisites")
	}
	return result, nil
}

func validateWorkloadTargeting(ctx context.Context, client workloadStageClient, execution *managementExecution, tenant tenantStageReceipt,
	selected []enrollmentBMC, objects []*unstructured.Unstructured,
) error {
	if ctx == nil || client == nil || execution == nil || execution.plan == nil || tenant.Client == nil || tenant.ClusterUID == "" || len(selected) == 0 {
		return fmt.Errorf("selected workload target is unavailable")
	}
	expected, desiredCluster, err := tenantPlanInputs(execution.plan)
	if err != nil || tenant.Client.dpuClusterUID != string(tenant.ClusterUID) || !reflect.DeepEqual(expected, tenant.Client.expected) {
		return fmt.Errorf("tenant API binding differs from the selected plan")
	}
	current, err := client.Get(ctx, desiredCluster.DeepCopy())
	if err != nil || current == nil || current.GetUID() != tenant.ClusterUID || execution.reconciler.Verify(desiredCluster, current) != nil {
		return fmt.Errorf("selected DPUCluster changed before workload creation")
	}
	selectorLabels := tenant.Client.expected.Labels
	if len(selectorLabels) == 0 {
		return fmt.Errorf("selected DPUCluster has no stable selector labels")
	}
	for key, value := range selectorLabels {
		if current.GetLabels()[key] != value {
			return fmt.Errorf("selected DPUCluster no longer has its retained selector labels")
		}
	}
	clusters, err := client.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster"}}, "")
	if err != nil {
		return fmt.Errorf("complete DPUCluster selector domain is unavailable")
	}
	selectedClusters := 0
	for i := range clusters {
		if labels.Set(selectorLabels).AsSelector().Matches(labels.Set(clusters[i].GetLabels())) {
			selectedClusters++
			if clusters[i].GetUID() != tenant.ClusterUID || clusters[i].GetNamespace() != current.GetNamespace() || clusters[i].GetName() != current.GetName() {
				return fmt.Errorf("retained tenant selector also matches another DPUCluster")
			}
		}
	}
	if selectedClusters != 1 {
		return fmt.Errorf("retained tenant selector does not resolve exactly one DPUCluster")
	}
	expectedClusterSelector := map[string]any{"matchLabels": cloneWorkloadStringMap(selectorLabels)}
	for _, object := range objects {
		if object.GetKind() == "DPUServiceIPAM" || object.GetKind() == "DPUServiceInterface" {
			selector, found, err := unstructured.NestedMap(object.Object, "spec", "dpuClusterSelector")
			if err != nil || !found || !reflect.DeepEqual(selector, expectedClusterSelector) {
				return fmt.Errorf("%s %s has no exact selected-tenant cluster selector", object.GetKind(), object.GetName())
			}
		}
		if object.GetKind() == "DPUServiceInterface" {
			if err := validateInterfaceNodeSelector(object, len(selected)); err != nil {
				return err
			}
		}
	}
	return client.VerifyIdentity(ctx)
}

func validateInterfaceNodeSelector(object *unstructured.Unstructured, selectedCount int) error {
	selector, found, err := unstructured.NestedMap(object.Object, "spec", "template", "spec", "nodeSelector")
	if err != nil || !found || len(selector) != 1 || selectedCount <= 0 {
		return fmt.Errorf("DPUServiceInterface %s does not have one bounded selected-node selector", object.GetName())
	}
	expressions, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "nodeSelector", "matchExpressions")
	if err != nil || !found || len(expressions) != 1 {
		return fmt.Errorf("DPUServiceInterface %s selector is malformed", object.GetName())
	}
	expression, ok := expressions[0].(map[string]any)
	if !ok || expression["key"] != "kubernetes.io/hostname" || expression["operator"] != "In" {
		return fmt.Errorf("DPUServiceInterface %s selector is not bounded to selected hostnames", object.GetName())
	}
	values, ok := expression["values"].([]any)
	if !ok || len(values) != selectedCount {
		return fmt.Errorf("DPUServiceInterface %s selector does not cover the selected devices", object.GetName())
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		name, ok := value.(string)
		if !ok || len(validation.IsValidLabelValue(name)) != 0 {
			return fmt.Errorf("DPUServiceInterface %s has an invalid hostname selector", object.GetName())
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("DPUServiceInterface %s repeats a selected hostname", object.GetName())
		}
		seen[name] = struct{}{}
	}
	return nil
}

func verifyWorkloadReceiptSet(ctx context.Context, client workloadStageClient, execution *managementExecution, tenant *tenantClient,
	objects []*unstructured.Unstructured, receipts []workloadResourceReceipt,
) error {
	if ctx == nil || client == nil || execution == nil || execution.reconciler == nil || tenant == nil || len(receipts) != 8 {
		return fmt.Errorf("complete workload receipt set is required")
	}
	specs, err := orderWorkloadObjects(objects)
	if err != nil || len(specs) != len(receipts) {
		return fmt.Errorf("retained workload prerequisites differ from their receipts")
	}
	for i, spec := range specs {
		receipt := receipts[i]
		desired := spec.object
		if receipt.Kind != desired.GetKind() || receipt.Namespace != desired.GetNamespace() || receipt.Name != desired.GetName() || receipt.UID == "" || receipt.Generation <= 0 {
			return fmt.Errorf("workload receipt order or identity is invalid")
		}
		current, err := client.Get(ctx, desired.DeepCopy())
		if err != nil {
			return safeAPIError("recheck workload prerequisite", err)
		}
		if current == nil || current.GetUID() != receipt.UID || current.GetGeneration() != receipt.Generation || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
			return fmt.Errorf("workload prerequisite %s/%s was replaced or changed", desired.GetKind(), desired.GetName())
		}
		if err := execution.reconciler.Verify(desired, current); err != nil {
			return fmt.Errorf("workload prerequisite %s/%s no longer matches the retained plan", desired.GetKind(), desired.GetName())
		}
		if spec.mode != workloadNoReadiness {
			if readiness := workloadResourceReadiness(current, spec.mode); !readiness.Ready {
				return fmt.Errorf("workload prerequisite %s/%s no longer has current required reconciliation", desired.GetKind(), desired.GetName())
			}
		}
		if spec.mode == workloadIPAMReadiness || spec.mode == workloadInterfaceReadiness {
			if receipt.ChildUID == "" {
				return fmt.Errorf("workload prerequisite %s/%s has no tenant-child receipt", desired.GetKind(), desired.GetName())
			}
			child, err := client.getTenantWorkloadChild(ctx, tenant, current)
			if err != nil {
				return safeAPIError("recheck tenant workload child", err)
			}
			var verificationErr error
			if child != nil && child.GetUID() == receipt.ChildUID {
				verificationErr = verifyWorkloadChild(current, child, tenant.expected)
			}
			if child == nil || child.GetUID() != receipt.ChildUID || verificationErr != nil {
				message := fmt.Sprintf("tenant child for %s/%s changed before the preparation receipt", desired.GetKind(), desired.GetName())
				if reason := workloadChildVerificationReason(verificationErr); reason != "" {
					return reconcileError(reason, message, nil)
				}
				return errors.New(message)
			}
		} else if receipt.ChildUID != "" {
			return fmt.Errorf("unexpected tenant-child receipt for %s/%s", desired.GetKind(), desired.GetName())
		}
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func cloneWorkloadStringMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func verifyWorkloadAPIs(ctx context.Context, client workloadStageClient, objects []*unstructured.Unstructured) error {
	resources := map[string]*metav1.APIResourceList{}
	for _, object := range objects {
		gv := object.GroupVersionKind().GroupVersion().String()
		resourceList := resources[gv]
		if resourceList == nil {
			var err error
			resourceList, err = client.discoverResourceList(ctx, gv)
			if err != nil {
				return fmt.Errorf("workload API %s is unavailable", gv)
			}
			resources[gv] = resourceList
		}
		requirement := workloadAPIRequirement(object.GroupVersionKind())
		if !discoveredResourceMatches(resourceList, requirement) {
			return fmt.Errorf("workload API %s does not serve the retained %s resource with namespaced scope", gv, object.GetKind())
		}
	}
	return ctx.Err()
}

func workloadAPIRequirement(gvk schema.GroupVersionKind) preflightAPIRequirement {
	plurals := map[string]string{
		"BlueFieldSoftware": "bluefieldsoftwares", "DPUFlavor": "dpuflavors",
		"DPUServiceConfiguration": "dpuserviceconfigurations", "DPUServiceTemplate": "dpuservicetemplates",
		"DPUServiceIPAM": "dpuserviceipams", "DPUServiceInterface": "dpuserviceinterfaces",
	}
	return preflightAPIRequirement{group: gvk.Group, version: gvk.Version, resource: plurals[gvk.Kind], kind: gvk.Kind, namespaced: true}
}

func journalWorkloadChecks(journal *executionJournal, checks []PreflightCheck) error {
	for _, check := range checks {
		state := "confirmed"
		if check.State != "passed" {
			state = "failed"
		}
		if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "preflight", Subject: check.ID,
			State: state, ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
			return err
		}
	}
	return nil
}

func workloadStageFailure(journal *executionJournal, reason, message string, cause error) error {
	return journalFailure(journal, executionEvent{Stage: generation.StageWorkload, Operation: "workload-preparation", State: "failed",
		ReasonCode: reason, Message: message}, false, reason, message, cause)
}
