// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const operationLabel = "nvidia.kubernetes-launch-kit/dpf-operation"
const maxOperationBytes = 256 << 10

// OperationRef locates one durable initial deployment. It grants no authority
// independently of its live management, namespace, UID and bundle bindings.
// Pass the returned reference verbatim to RunRequest.Resume.
type OperationRef struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

// String is the portable CLI --resume reference. No local file is authority.
func (r OperationRef) String() string { return r.Namespace + "/" + r.Name + "/" + string(r.UID) }

// ParseOperationRef rejects incomplete or ambiguous operation references.
func ParseOperationRef(value string) (*OperationRef, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 || len(validation.IsDNS1123Label(parts[0])) != 0 || len(validation.IsDNS1123Subdomain(parts[1])) != 0 || !validUUID(parts[2]) || !strings.HasPrefix(parts[1], "dpf-op-") {
		return nil, fmt.Errorf("resume requires the reported namespace/name/UID operation reference")
	}
	return &OperationRef{Namespace: parts[0], Name: parts[1], UID: types.UID(parts[2])}, nil
}

type operationBinding struct {
	Version        int
	ID             string
	BundleDigest   string
	Management     generation.ManagementIdentity
	NamespaceUID   string
	DeploymentName string
	CohortDigest   string
}

type operationProgress struct {
	NodeName             string
	MaintenanceUID       types.UID
	ReleasedVersion      string
	ResetAcknowledged    bool
	Sets, DPUs, Versions map[string]types.UID
}

type operationRound struct {
	UID                types.UID
	Generation         int64
	Source             string
	Requested, Startup time.Time
	KernelBoot         string
}

type operationReset struct {
	Request ActivationRequest

	Rounds       map[types.UID]operationRound
	Baseline     string
	Observation  ActivationObservation
	ClaimUID     types.UID
	ArmProofs    map[types.UID]operationRound
	ACKIntent    bool
	Acknowledged bool
}

type operationHost struct {
	Request sshprobe.HostNetworkRequest
	Result  *HostWorkloadNetworkResult
}

// operationState is a bounded nonsecret checkpoint, not a workflow engine.
// Tenant credentials, access clients, raw manifests and host files never enter it.
type operationState struct {
	Binding         operationBinding
	Management      managementBootstrapReceipt
	TenantUID       types.UID
	TenantSystemUID string
	TenantSources   map[string]types.UID
	Enrollment      enrollmentStageReceipt
	Software        softwareVerificationReceipt
	Workload        []workloadResourceReceipt
	DeploymentUID   types.UID
	Children        map[string]activationObjectReceipt
	HoldIntents     map[string]operationProgress
	Progress        map[string]operationProgress
	Resets          map[string]operationReset
	Hosts           map[string]operationHost
}

type operationStore interface {
	getOperation(context.Context, string, string) (*corev1.ConfigMap, error)
	createOperation(context.Context, *corev1.ConfigMap) (*corev1.ConfigMap, error)
	updateOperation(context.Context, *corev1.ConfigMap) (*corev1.ConfigMap, error)
}

type deploymentOperation struct {
	store  operationStore
	object *corev1.ConfigMap
	state  operationState
	// Once persistence is uncertain this attempt must never start another effect.
	failed bool
}

func operationName(deployment *unstructured.Unstructured) string {
	return "dpf-op-" + strings.TrimPrefix(inventory.Digest("dpf-operation-v1", deployment.GetNamespace()+"/"+deployment.GetName()), "sha256:")[:40]
}

func encodeOperation(state operationState) (string, error) {
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxOperationBytes {
		return "", fmt.Errorf("operation checkpoint exceeds its bounded schema")
	}
	return string(data), nil
}

func decodeOperation(data string) (operationState, error) {
	var state operationState
	if len(data) == 0 || len(data) > maxOperationBytes {
		return state, fmt.Errorf("operation checkpoint size invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("operation checkpoint schema invalid")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return state, fmt.Errorf("operation checkpoint has trailing data")
	}
	if state.Binding.Version != 1 || state.Binding.ID == "" || !validSHA256Digest(state.Binding.BundleDigest) || state.Binding.NamespaceUID == "" ||
		state.Management.BundleDigest != state.Binding.BundleDigest || state.TenantUID == "" || state.TenantSystemUID == "" || len(state.TenantSources) == 0 ||
		state.Enrollment.BundleDigest != state.Binding.BundleDigest || state.Enrollment.TenantUID != state.TenantUID || state.Enrollment.Secret.UID == "" ||
		state.Software.SoftwareUID == "" || len(state.Workload) == 0 || state.Children == nil || state.HoldIntents == nil || state.Progress == nil || state.Resets == nil || state.Hosts == nil {
		return state, fmt.Errorf("operation checkpoint binding/receipts incomplete")
	}
	if len(state.Enrollment.Devices) == 0 || len(state.Workload) == 0 || len(state.Hosts) > len(state.Enrollment.Devices) || len(state.Progress) > len(state.Enrollment.Devices) {
		return state, fmt.Errorf("operation checkpoint exceeds reviewed cohort bounds")
	}
	for node, progress := range state.Progress {
		if node != progress.NodeName || progress.MaintenanceUID == "" || progress.ReleasedVersion == "" && !progress.ResetAcknowledged || len(progress.Sets) == 0 || len(progress.DPUs) == 0 || len(progress.Versions) == 0 {
			return state, fmt.Errorf("operation activation history incomplete")
		}
	}
	for _, reset := range state.Resets {
		if len(reset.Rounds) == 0 || len(reset.Rounds) > len(state.Enrollment.Devices) {
			return state, fmt.Errorf("operation reset cohort invalid")
		}
		for uid, round := range reset.Rounds {
			if uid == "" || uid != round.UID || round.Generation < 1 || !supportedActivationRebootSourcePhase(round.Source) || round.Requested.IsZero() || round.Startup.IsZero() || !validUUID(round.KernelBoot) {
				return state, fmt.Errorf("operation reset baseline invalid")
			}
		}
	}
	return state, nil
}

func operationObjectMatches(object *corev1.ConfigMap, namespace, name string) bool {
	return object != nil && object.Namespace == namespace && object.Name == name && validUUID(string(object.UID)) && object.ResourceVersion != "" && object.DeletionTimestamp == nil &&
		object.Labels[operationLabel] == "v1" && len(object.Data) == 1 && len(object.BinaryData) == 0 && len(object.OwnerReferences) == 0
}

func (o *deploymentOperation) ref() *OperationRef {
	if o == nil || o.object == nil {
		return nil
	}
	return &OperationRef{Namespace: o.object.Namespace, Name: o.object.Name, UID: o.object.UID}
}

func (o *deploymentOperation) save(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if o.failed || o.object == nil {
		return fmt.Errorf("OPERATION_CHECKPOINT_UNCERTAIN: stop before another effect")
	}
	previous, err := decodeOperation(o.object.Data["checkpoint.json"])
	if err != nil || previous.Binding != o.state.Binding || !reflect.DeepEqual(previous.Management, o.state.Management) || previous.TenantUID != o.state.TenantUID || previous.TenantSystemUID != o.state.TenantSystemUID || !reflect.DeepEqual(previous.TenantSources, o.state.TenantSources) || !reflect.DeepEqual(previous.Enrollment, o.state.Enrollment) || !reflect.DeepEqual(previous.Software, o.state.Software) || !reflect.DeepEqual(previous.Workload, o.state.Workload) || previous.DeploymentUID != "" && previous.DeploymentUID != o.state.DeploymentUID {
		o.failed = true
		return fmt.Errorf("OPERATION_BINDING_CONFLICT: immutable checkpoint bindings changed")
	}
	data, err := encodeOperation(o.state)
	if err != nil {
		o.failed = true
		return err
	}
	desired := o.object.DeepCopy()
	desired.Data = map[string]string{"checkpoint.json": data}
	// Kubernetes can elide an unchanged update without advancing the resource
	// version. Still perform the CAS: a stale or replaced checkpoint must fail
	// even when this caller has no new progress to persist.
	unchanged := data == o.object.Data["checkpoint.json"]
	updated, err := o.store.updateOperation(ctx, desired)
	if err != nil || !operationObjectMatches(updated, desired.Namespace, desired.Name) || updated.UID != desired.UID || updated.ResourceVersion == desired.ResourceVersion && !unchanged || updated.Data["checkpoint.json"] != data {
		o.failed = true
		return fmt.Errorf("OPERATION_CHECKPOINT_UNCERTAIN: checkpoint CAS failed; resume from live evidence before another effect")
	}
	o.object = updated.DeepCopy()
	return nil
}

func (tenant *tenantClient) operationSources(ctx context.Context) (map[string]types.UID, error) {
	sources, err := tenant.source.observeTenantSources(ctx, tenant.expected)
	if err != nil {
		return nil, err
	}
	result := map[string]types.UID{}
	objects := []*unstructured.Unstructured{sources.observed.DPUCluster, sources.observed.TenantControlPlane, sources.observed.Service}
	objects = append(objects, sources.observed.Keepalived...)
	for _, object := range objects {
		if object == nil || object.GetUID() == "" {
			return nil, fmt.Errorf("tenant source identity missing")
		}
		result[object.GetKind()+"/"+object.GetNamespace()+"/"+object.GetName()] = object.GetUID()
	}
	if sources.secret == nil || sources.secret.UID == "" {
		return nil, fmt.Errorf("tenant credential source identity missing")
	}
	result["Secret/"+sources.secret.Namespace+"/"+sources.secret.Name] = sources.secret.UID
	return result, nil
}

func reserveDeploymentOperation(ctx context.Context, client *APIClient, prepared *preparedControlPlaneRun, workload workloadPreparationReceipt) (*deploymentOperation, error) {
	deployment, err := retainedDeploymentObject(prepared.execution.verificationInputs())
	if err != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_PLAN_INVALID", err)
	}
	// A preexisting parent without this operation's reservation is never adopted.
	parent, err := client.Get(ctx, deployment)
	if ctx.Err() != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_CANCELED", err)
	}
	if err == nil && parent != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_PARENT_EXISTS", nil)
	}
	if !apierrors.IsNotFound(err) {
		return nil, operationReservationFailure(ctx, "OPERATION_PARENT_UNMEASURED", err)
	}
	sources, err := workload.ControlPlane.tenant.Client.operationSources(ctx)
	if err != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_TENANT_SOURCES_UNMEASURED", err)
	}
	namespace := deployment.GetNamespace()
	state := operationState{
		Binding: operationBinding{Version: 1, ID: rand.Text(), BundleDigest: prepared.execution.plan.Digest(), Management: prepared.execution.plan.Intent().Management,
			NamespaceUID: workload.ControlPlane.management.NamespaceUIDs[namespace], DeploymentName: deployment.GetName(), CohortDigest: inventory.Digest("dpf-operation-cohort-v1", prepared.execution.plan.Intent().Devices)},
		Management: workload.ControlPlane.management, TenantUID: workload.ControlPlane.tenant.ClusterUID, TenantSystemUID: workload.ControlPlane.tenant.Client.systemIdentity(),
		TenantSources: sources, Enrollment: workload.ControlPlane.enrollment, Software: workload.Software, Workload: workload.Resources,
		Children: map[string]activationObjectReceipt{}, HoldIntents: map[string]operationProgress{}, Progress: map[string]operationProgress{}, Resets: map[string]operationReset{}, Hosts: map[string]operationHost{},
	}
	// Detach the complete setup before encoding; this same owned snapshot is
	// retained only after the create response matches those exact bytes.
	operation := newDeploymentOperation(client, nil, state)
	data, err := encodeOperation(operation.state)
	if err != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_CHECKPOINT_INVALID", err)
	}
	if _, err := decodeOperation(data); err != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_CHECKPOINT_INVALID", err)
	}
	desired := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: operationName(deployment), Labels: map[string]string{operationLabel: "v1"}}, Data: map[string]string{"checkpoint.json": data}}
	created, err := client.createOperation(ctx, desired)
	if err != nil {
		existing, readErr := client.getOperation(ctx, namespace, desired.Name)
		if readErr == nil && operationObjectMatches(existing, namespace, desired.Name) {
			return nil, operationReservationFailure(ctx, "OPERATION_RESERVATION_EXISTS", err)
		}
		return nil, operationReservationFailure(ctx, "OPERATION_RESERVATION_UNCERTAIN", errors.Join(err, readErr))
	}
	if !operationObjectMatches(created, namespace, desired.Name) || created.Data["checkpoint.json"] != data {
		return nil, operationReservationFailure(ctx, "OPERATION_RESERVATION_UNCERTAIN", nil)
	}
	if _, err := decodeOperation(data); err != nil {
		return nil, operationReservationFailure(ctx, "OPERATION_CHECKPOINT_INVALID", err)
	}
	operation.object = created.DeepCopy()
	return operation, nil
}

func loadDeploymentOperation(ctx context.Context, client *APIClient, execution *managementExecution, ref *OperationRef) (*deploymentOperation, error) {
	deployment, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		return nil, reconcileError("OPERATION_RESUME_PLAN_INVALID", operationResumeMessage("OPERATION_RESUME_PLAN_INVALID"), err)
	}
	if ref == nil || ref.Namespace != deployment.GetNamespace() || ref.Name != operationName(deployment) || !validUUID(string(ref.UID)) {
		return nil, reconcileError("OPERATION_RESUME_REFERENCE_INVALID", operationResumeMessage("OPERATION_RESUME_REFERENCE_INVALID"), nil)
	}
	object, err := client.getOperation(ctx, ref.Namespace, ref.Name)
	if err != nil || !operationObjectMatches(object, ref.Namespace, ref.Name) || object.UID != ref.UID {
		return nil, reconcileError("OPERATION_RESUME_RECORD_UNAVAILABLE", operationResumeMessage("OPERATION_RESUME_RECORD_UNAVAILABLE"), err)
	}
	state, err := decodeOperation(object.Data["checkpoint.json"])
	if err != nil {
		return nil, reconcileError("OPERATION_RESUME_CHECKPOINT_INVALID", operationResumeMessage("OPERATION_RESUME_CHECKPOINT_INVALID"), err)
	}
	intent := execution.plan.Intent()
	if state.Binding.BundleDigest != execution.plan.Digest() || state.Binding.Management != intent.Management || state.Binding.DeploymentName != deployment.GetName() || state.Binding.CohortDigest != inventory.Digest("dpf-operation-cohort-v1", intent.Devices) {
		return nil, reconcileError("OPERATION_RESUME_BINDING_CHANGED", operationResumeMessage("OPERATION_RESUME_BINDING_CHANGED"), nil)
	}
	ns, err := client.access.Client.CoreV1().Namespaces().Get(ctx, ref.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, reconcileError("OPERATION_RESUME_NAMESPACE_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_NAMESPACE_UNVERIFIED"), err)
	}
	if ns == nil || string(ns.UID) != state.Binding.NamespaceUID || ns.DeletionTimestamp != nil {
		return nil, reconcileError("OPERATION_RESUME_NAMESPACE_CHANGED", operationResumeMessage("OPERATION_RESUME_NAMESPACE_CHANGED"), err)
	}
	return newDeploymentOperation(client, object, state), nil
}

func restoreOperationReceipts(ctx context.Context, client receiptRestorationReader, prepared *preparedControlPlaneRun, journal *executionJournal, interval, timeout time.Duration) (controlPlaneReceipts, workloadPreparationReceipt, error) {
	state := prepared.operation.preparationReceipts()
	control := controlPlaneReceipts{management: state.Management, enrollment: state.Enrollment}
	if err := waitManagementBootstrap(ctx, client, prepared.execution.verificationInputs(), state.Management, journal, interval); err != nil {
		return control, workloadPreparationReceipt{}, reconcileError("OPERATION_RESUME_BOOTSTRAP_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_BOOTSTRAP_UNVERIFIED"), err)
	}
	tenant, err := client.connectTenant(ctx, prepared.execution.plan, state.TenantUID, prepared.execution.reconciler, journal, interval, timeout)
	if err != nil {
		return control, workloadPreparationReceipt{}, reconcileError("OPERATION_RESUME_TENANT_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_TENANT_UNVERIFIED"), err)
	}
	sources, err := tenant.operationSources(ctx)
	if err != nil || tenant.systemIdentity() != state.TenantSystemUID || !reflect.DeepEqual(sources, state.TenantSources) {
		return control, workloadPreparationReceipt{}, reconcileError("OPERATION_RESUME_TENANT_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_TENANT_UNVERIFIED"), err)
	}
	control.tenant = tenantStageReceipt{ClusterUID: state.TenantUID, Client: tenant}
	workload := workloadPreparationReceipt{BundleDigest: state.BundleDigest, ControlPlane: control, Software: state.Software, Resources: state.Workload}
	objects, err := retainedWorkloadObjects(prepared.execution.verificationInputs())
	if err != nil {
		return control, workload, reconcileError("OPERATION_RESUME_PLAN_INVALID", operationResumeMessage("OPERATION_RESUME_PLAN_INVALID"), err)
	}
	if err := verifyActivationSoftware(ctx, client, prepared.execution.verificationInputs(), workload.Software); err != nil {
		return control, workload, reconcileError("OPERATION_RESUME_SOFTWARE_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_SOFTWARE_UNVERIFIED"), err)
	}
	if err := verifyWorkloadReceiptSet(ctx, client, prepared.execution.verificationInputs(), tenant, objects, workload.Resources); err != nil {
		return control, workload, reconcileError("OPERATION_RESUME_WORKLOAD_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_WORKLOAD_UNVERIFIED"), err)
	}
	return control, workload, nil
}

func (o *deploymentOperation) restoreProgress() map[string]activationNodeProgress {
	result := map[string]activationNodeProgress{}
	if o != nil {
		for key, p := range o.state.Progress {
			result[key] = activationNodeProgress{nodeName: p.NodeName, maintenanceUID: p.MaintenanceUID, releasedVersion: p.ReleasedVersion, resetAcknowledged: p.ResetAcknowledged, sets: maps.Clone(p.Sets), dpus: maps.Clone(p.DPUs), versions: maps.Clone(p.Versions)}
		}
	}
	return result
}

func (c *APIClient) getOperation(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return c.access.Client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}
func (c *APIClient) createOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return c.access.Client.CoreV1().ConfigMaps(object.Namespace).Create(ctx, object.DeepCopy(), metav1.CreateOptions{})
}
func (c *APIClient) updateOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return c.access.Client.CoreV1().ConfigMaps(object.Namespace).Update(ctx, object.DeepCopy(), metav1.UpdateOptions{})
}

func (c *APIClient) checkOperationPermissions(ctx context.Context, execution *managementVerificationInputs) error {
	deployment, err := retainedDeploymentObject(execution)
	if err != nil {
		return err
	}
	namespace, name := deployment.GetNamespace(), operationName(deployment)
	for _, verb := range []string{"get", "create", "update"} {
		resourceName := name
		if verb == "create" {
			resourceName = ""
		}
		check := c.checkAccess(ctx, accessRequirement{verb: verb, version: "v1", resource: "configmaps", namespace: namespace, resourceName: resourceName})
		if check.State != "passed" {
			code := "OPERATION_PERMISSION_UNVERIFIED"
			if check.ReasonCode == "CALLER_PERMISSION_MISSING" {
				code = "OPERATION_PERMISSION_DENIED"
			}
			return reconcileError(code, fmt.Sprintf("Durable deployment requires verified ConfigMap %s permission in its operator namespace.", verb), nil)
		}
	}
	return nil
}

// Defensive structural comparison used by recovery without exposing raw JSON.
func sameOperationRequest(a, b sshprobe.HostNetworkRequest) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// prepareResume binds access without the fresh-only enrollment policy. The
// caller must run the complete resume activation scope before any next effect.
func prepareResume(execution *managementExecution, request config.Request, operation *deploymentOperation) (*preparedControlPlaneRun, error) {
	selected, password, err := bindEnrollmentBMCs(execution.plan.Intent().Release, execution.plan.Intent().Devices, request.Targets)
	if err != nil {
		return nil, reconcileError("OPERATION_RESUME_ENROLLMENT_UNVERIFIED", operationResumeMessage("OPERATION_RESUME_ENROLLMENT_UNVERIFIED"), err)
	}
	if !softwareEnrollmentReceiptMatches(operation.enrolledDevices(), selected) {
		return nil, reconcileError("OPERATION_RESUME_ENROLLMENT_CHANGED", operationResumeMessage("OPERATION_RESUME_ENROLLMENT_CHANGED"), nil)
	}
	return &preparedControlPlaneRun{execution: execution, selected: selected, password: password, operation: operation}, nil
}

func (o *deploymentOperation) bindChildren(ctx context.Context, observed activationObservation) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	if o.state.DeploymentUID == "" {
		return fmt.Errorf("OPERATION_PARENT_UNBOUND: children require a bound deployment")
	}
	children := maps.Clone(o.state.Children)
	for _, group := range []struct {
		kind    string
		objects map[string]*unstructured.Unstructured
	}{{"DPUSet", observed.sets}, {"DPU", observed.dpus}, {"DPUNode", observed.nodes}} {
		for key, prior := range o.state.Children {
			if !strings.HasPrefix(key, group.kind+"/") {
				continue
			}
			current := group.objects[prior.Name]
			if current == nil || current.GetUID() != prior.UID || current.GetGeneration() != prior.Generation {
				return o.bindingConflict("OPERATION_CHILD_CHANGED: selected child missing/replaced/changed")
			}
		}
		for name, current := range group.objects {
			if current == nil || name == "" || current.GetName() != name || current.GetUID() == "" || current.GetGeneration() < 1 {
				return fmt.Errorf("OPERATION_CHILD_CHANGED: selected child identity incomplete")
			}
			children[group.kind+"/"+name] = activationObjectReceipt{Name: name, UID: current.GetUID(), Generation: current.GetGeneration()}
		}
	}
	if reflect.DeepEqual(children, o.state.Children) {
		return nil
	}
	o.state.Children = children
	return o.save(ctx)
}

func (o *deploymentOperation) verifyHoldHistory(observed activationObservation) error {
	if o == nil {
		return nil
	}
	for node, intent := range o.state.HoldIntents {
		if _, confirmed := o.state.Progress[node]; confirmed {
			continue
		}
		maintenance := observed.maintenance[node+"-hold"]
		if maintenance == nil || maintenance.GetUID() != intent.MaintenanceUID || maintenance.GetAnnotations()[activationHoldKey] != "true" {
			return fmt.Errorf("HOLD_ACTION_REQUIRED: prior hold outcome lacks a confirmed durable receipt; current absence/false is not completion history")
		}
	}
	return nil
}

// rebootHistory preserves already proved physical effects across attempts;
// it is history, not a substitute for current activation/runtime fences.
func (o *deploymentOperation) rebootHistory() []activationRebootReceipt {
	var history []activationRebootReceipt
	if o == nil {
		return history
	}
	for _, reset := range o.state.Resets {
		proof := reset.Observation
		if !proof.Reconnected || !validUUID(proof.PreviousBootID) || !validUUID(proof.CurrentBootID) || proof.PreviousBootID == proof.CurrentBootID {
			continue
		}
		nodes := make([]string, 0, len(reset.Request.DPUNodes))
		for _, node := range reset.Request.DPUNodes {
			nodes = append(nodes, node.NodeName)
		}
		history = append(history, activationRebootReceipt{HostInputRef: reset.Request.HostInputRef, NodeNames: nodes, PreviousBoot: proof.PreviousBootID, CurrentBoot: proof.CurrentBootID})
	}
	sort.Slice(history, func(i, j int) bool { return history[i].HostInputRef < history[j].HostInputRef })
	return history
}

// verifyFirstDeploymentSources refreshes old checkpoint byte observations only
// before a genuinely absent, unbound parent can trigger a first reimage. It has
// read-only dependencies and never reruns software/resource creation stages.
func verifyFirstDeploymentSources(ctx context.Context, client softwareVerificationClient, execution *managementVerificationInputs, receipt softwareVerificationReceipt, chart workloadChartSourceVerifier, journal *executionJournal, maxBytes int64) error {
	if err := checkWorkloadChartSource(ctx, execution.plan, chart, journal, "chart-source-before-first-resumed-deployment"); err != nil {
		return err
	}
	desired, err := retainedBlueFieldSoftware(execution)
	if err != nil {
		return err
	}
	current, err := client.Get(ctx, desired.DeepCopy())
	if err != nil || current == nil || current.GetUID() != receipt.SoftwareUID || current.GetGeneration() != receipt.Generation {
		return fmt.Errorf("RESUME_SOFTWARE_IDENTITY_CHANGED: verified software resource no longer matches checkpoint")
	}
	release, err := releases.Resolve(execution.plan.Intent().Release)
	if err != nil {
		return err
	}
	fresh, err := verifyStagedSoftware(ctx, client, execution.reconciler, desired, current, execution.plan.Intent().Software, selectedSoftwarePSIDs(execution.plan.Intent()), execution.operatorNamespace, release.Operator.Image, maxBytes)
	if err != nil || !reflect.DeepEqual(fresh.Components, receipt.Components) {
		return fmt.Errorf("RESUME_SOFTWARE_BYTES_UNVERIFIED: current cached bytes do not match the retained locks and receipt")
	}
	// Keep the immutable checkpoint receipt as historical provenance; this fresh
	// read barrier authorizes only the immediately following first DD creation.
	return journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "verify-software-before-first-resumed-deployment", State: "confirmed", ReasonCode: "RESUME_SOFTWARE_BYTES_VERIFIED", Message: "Current selected cached ISO/PLDM bytes matched retained locks before first DPUDeployment creation."})
}
