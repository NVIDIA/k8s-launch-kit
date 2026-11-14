// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type enrollmentStageReceipt struct {
	BundleDigest string
	TenantUID    types.UID
	Secret       enrollmentSecretReceipt
	Devices      []enrolledDeviceReceipt
}

type enrolledDeviceReceipt struct {
	Serial     string
	DeviceUID  types.UID
	DPUNodeUID types.UID
	BMCIP      string
	BMCPort    int
}

type enrollmentManagementClient interface {
	managementRunnerClient
	tenantSourceReader
	tenantStageManagementClient
	checkTenantNetwork(context.Context, bootstrap.Tenant, tenantNetworkRunner, string, time.Duration) ([]inventory.Check, error)
	verifyEnrollmentBMCs(context.Context, []enrollmentBMC, time.Duration, *executionJournal) error
	executeEnrollmentSecret(context.Context, ResourceClient, string, string, *executionJournal) (enrollmentSecretReceipt, error)
	getOperatorCRD(context.Context, string) (*unstructured.Unstructured, error)
}

func (c *APIClient) verifyEnrollmentBMCs(ctx context.Context, selected []enrollmentBMC, timeout time.Duration, journal *executionJournal) error {
	return verifyEnrollmentBMCs(ctx, selected, timeout, journal)
}

func (c *APIClient) executeEnrollmentSecret(ctx context.Context, client ResourceClient, namespace, password string, journal *executionJournal) (enrollmentSecretReceipt, error) {
	return executeEnrollmentSecret(ctx, client, namespace, password, journal)
}

func (c *APIClient) getOperatorCRD(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	if c == nil || c.dynamic == nil || ctx == nil || name == "" {
		return nil, fmt.Errorf("bound API client and CRD identity are required")
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	object, err := c.dynamic.Resource(schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, safeAPIError("read retained DPF CRD", err)
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return object, nil
}

// A missing companion can be transient while the crawler is creating it.
// After a completed scan, DPF cannot repair it: later scans skip the device IP.
var errEnrollmentNodeMissing = errors.New("a selected DPUDevice exists without its matching DPUNode; DPF rescans cannot repair this partial enrollment, so inspect the controller failure before retrying")

func guardEnrollmentScope(ctx context.Context, client enrollmentManagementClient, execution *managementExecution, selected []enrollmentBMC) error {
	return inspectEnrollmentScope(ctx, client, execution, selected, false)
}

func inspectEnrollmentScope(ctx context.Context, client enrollmentManagementClient, execution *managementExecution, selected []enrollmentBMC, requireInstalled bool) error {
	if ctx == nil || client == nil || execution == nil || execution.plan == nil || len(selected) == 0 {
		return fmt.Errorf("selected enrollment scope is incomplete")
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return fmt.Errorf("management identity changed before enrollment scope inspection")
	}
	resources := []enrollmentResource{
		{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", plural: "dpudiscoveries", kind: "DPUDiscovery", namespaced: true},
		{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", plural: "dpudevices", kind: "DPUDevice", namespaced: true},
		{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", plural: "dpunodes", kind: "DPUNode", namespaced: true},
		{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", plural: "dpus", kind: "DPU", namespaced: true},
		{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", plural: "dpusets", kind: "DPUSet", namespaced: true},
		{group: "svc.dpu.nvidia.com", version: "v1alpha1", plural: "dpudeployments", kind: "DPUDeployment", namespaced: true},
	}
	devices, nodes := map[string]bool{}, map[string]bool{}
	for _, required := range resources {
		if err := ctx.Err(); err != nil {
			return err
		}
		expectation, found := retainedCRD(execution.crds, required.group, required.kind)
		if !found || expectation.plural != required.plural || expectation.namespaced != required.namespaced || !containsEnrollmentString(expectation.versions, required.version) {
			return fmt.Errorf("retained operator projection does not contain the expected %s CRD", required.kind)
		}
		current, err := client.getOperatorCRD(ctx, expectation.name)
		if apierrors.IsNotFound(err) {
			if requireInstalled {
				return fmt.Errorf("the required %s CRD disappeared after operator bootstrap", required.kind)
			}
			// Only the exact CRD object's NotFound proves the operator has not yet
			// installed this API. A missing or unavailable group-version does not.
			continue
		}
		if err != nil || !crdIdentityMatches(expectation, current) {
			return fmt.Errorf("the exact %s CRD is unreadable or differs from the retained operator", required.kind)
		}
		requirement := preflightAPIRequirement{crd: expectation.name, group: required.group, version: required.version, resource: required.plural, kind: required.kind, namespaced: required.namespaced}
		if !externalCRDReady(current, requirement) {
			return fmt.Errorf("the %s CRD is not established and serving the required version", required.kind)
		}
		resources, err := client.discoverResourceList(ctx, required.group+"/"+required.version)
		if err != nil || !discoveredResourceMatches(resources, requirement) {
			return fmt.Errorf("the %s API is not completely discoverable", required.kind)
		}
		objects, err := client.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": required.group + "/" + required.version, "kind": required.kind}}, "")
		if err != nil {
			return fmt.Errorf("the all-namespace %s list is incomplete", required.kind)
		}
		if err := validateEnrollmentObjects(required, objects, execution, selected); err != nil {
			return err
		}
		for _, object := range objects {
			if required.kind == "DPUDevice" {
				devices[object.GetName()] = true
			}
			if required.kind == "DPUNode" {
				nodes[strings.TrimPrefix(object.GetName(), "dpu-node-")] = true
			}
		}
	}
	for device := range devices {
		if !nodes[device] {
			return errEnrollmentNodeMissing
		}
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return fmt.Errorf("management identity changed during enrollment scope inspection")
	}
	return nil
}

type enrollmentResource struct {
	group, version, plural, kind string
	namespaced                   bool
}

func retainedCRD(crds []operatorCRDExpectation, group, kind string) (operatorCRDExpectation, bool) {
	var found operatorCRDExpectation
	count := 0
	for _, crd := range crds {
		if crd.group == group && crd.kind == kind {
			found, count = crd, count+1
		}
	}
	return found, count == 1
}

func validateEnrollmentObjects(required enrollmentResource, objects []unstructured.Unstructured, execution *managementExecution, selected []enrollmentBMC) error {
	desiredDiscoveries := retainedEnrollmentDiscoveries(execution)
	wanted := make(map[string]enrollmentBMC, len(selected))
	for _, item := range selected {
		wanted[strings.ToLower(item.device.Serial)] = item
	}
	for i := range objects {
		object := &objects[i]
		if object.GetAPIVersion() != required.group+"/"+required.version || object.GetKind() != required.kind || object.GetName() == "" || object.GetNamespace() == "" || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
			return fmt.Errorf("the all-namespace %s observation contains an invalid or deleting object", required.kind)
		}
		switch required.kind {
		case "DPUDiscovery":
			desired := desiredDiscoveries[object.GetNamespace()+"/"+object.GetName()]
			if desired == nil || execution.reconciler.Verify(desired, object) != nil {
				return fmt.Errorf("an unrelated DPUDiscovery could be activated by the shared credential Secret")
			}
		case "DPUDevice":
			item, ok := wanted[object.GetName()]
			if !ok {
				return fmt.Errorf("an unrelated DPUDevice exists")
			}
			if err := validateEnrolledDevice(object, item, execution.operatorNamespace, false); err != nil {
				return err
			}
		case "DPUNode":
			item, ok := wanted[strings.TrimPrefix(object.GetName(), "dpu-node-")]
			if !ok {
				return fmt.Errorf("an unrelated DPUNode exists")
			}
			if err := validateEnrolledNode(object, item, execution.operatorNamespace); err != nil {
				return err
			}
		case "DPU", "DPUSet", "DPUDeployment":
			return fmt.Errorf("existing %s objects are outside the selected-device enrollment path", required.kind)
		}
	}
	return nil
}

func retainedEnrollmentDiscoveries(execution *managementExecution) map[string]*unstructured.Unstructured {
	result := map[string]*unstructured.Unstructured{}
	if execution == nil || execution.retained == nil {
		return result
	}
	stage, found := execution.retained.Stage(generation.StageEnrollment)
	if !found {
		return result
	}
	for _, document := range stage.Documents() {
		object := document.ObjectCopy()
		if object != nil && object.GetKind() == "DPUDiscovery" {
			result[object.GetNamespace()+"/"+object.GetName()] = object
		}
	}
	return result
}

func runEnrollmentStage(ctx context.Context, management enrollmentManagementClient, execution *managementExecution,
	managementReceipt managementBootstrapReceipt, tenantReceipt tenantStageReceipt, selected []enrollmentBMC, password string,
	journal *executionJournal, interval, requestTimeout time.Duration,
) (enrollmentStageReceipt, error) {
	if err := validateEnrollmentStageInputs(ctx, management, execution, managementReceipt, tenantReceipt, selected, password, journal, interval, requestTimeout); err != nil {
		return enrollmentStageReceipt{}, err
	}
	if err := verifyManagementBootstrap(ctx, management, execution, managementReceipt); err != nil {
		return enrollmentStageReceipt{}, enrollmentStageFailure(journal, "ENROLLMENT_BOOTSTRAP_CHANGED", "Management bootstrap no longer matches its receipt; enrollment is stopped.", err)
	}
	if err := tenantReceipt.Client.VerifyIdentity(ctx); err != nil {
		return enrollmentStageReceipt{}, enrollmentStageFailure(journal, "TENANT_API_IDENTITY_CHANGED", "Tenant API identity changed before selected-device enrollment.", err)
	}
	if err := management.VerifyIdentity(ctx); err != nil {
		return enrollmentStageReceipt{}, enrollmentStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed before enrollment.", err)
	}
	if err := management.verifyEnrollmentBMCs(ctx, selected, requestTimeout, journal); err != nil {
		return enrollmentStageReceipt{}, err
	}
	if err := inspectEnrollmentScope(ctx, management, execution, selected, true); err != nil {
		return enrollmentStageReceipt{}, enrollmentStageFailure(journal, "ENROLLMENT_SCOPE_CHANGED", err.Error(), nil)
	}
	discoveries, err := selectedEnrollmentDiscoveries(execution, selected)
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	secret, err := management.executeEnrollmentSecret(ctx, management, execution.operatorNamespace, password, journal)
	receipt := enrollmentStageReceipt{BundleDigest: execution.plan.Digest(), TenantUID: tenantReceipt.ClusterUID, Secret: secret}
	if err != nil {
		return receipt, err
	}
	for _, desired := range discoveries {
		if err := verifyEnrollmentBoundary(ctx, management, execution, managementReceipt, tenantReceipt, selected, requestTimeout, secret, password, journal); err != nil {
			return receipt, err
		}
		created, err := executeNativeObject(ctx, management, execution.reconciler, desired, generation.StageEnrollment, journal, false, interval)
		if err != nil {
			return receipt, err
		}
		if err := waitEnrollmentDiscovery(ctx, management, execution, selected, desired, created.GetUID(), interval, journal); err != nil {
			return receipt, err
		}
	}
	devices, err := waitSelectedEnrollment(ctx, management, execution, selected, interval, journal)
	receipt.Devices = devices
	if err != nil {
		return receipt, err
	}
	if err := verifyEnrollmentBoundary(ctx, management, execution, managementReceipt, tenantReceipt, selected, requestTimeout, secret, password, journal); err != nil {
		return receipt, err
	}
	// A device that was ready before the final boundary checks must still be
	// the same ready device when the receipt is issued.
	_, ready, err := observeSelectedEnrollment(ctx, management, execution, selected, devices, journal)
	if err != nil {
		return receipt, err
	}
	if !ready {
		return receipt, enrollmentStageFailure(journal, "ENROLLMENT_READINESS_CHANGED", "Selected devices lost readiness before the enrollment receipt.", nil)
	}
	tenantErr := tenantReceipt.Client.VerifyIdentity(ctx)
	if err := ctx.Err(); err != nil {
		return receipt, enrollmentStageFailure(journal, "ENROLLMENT_READINESS_CANCELED", "Enrollment was canceled before its completed receipt could be issued.", err)
	}
	if tenantErr != nil {
		return receipt, enrollmentStageFailure(journal, "TENANT_API_IDENTITY_CHANGED", "Tenant API or source identity changed during the final selected-device observation.", tenantErr)
	}
	// This is a verified observation across independent APIs, not a lease or
	// transaction that prevents external changes after these checks return.
	return receipt, nil
}

func verifyEnrollmentBoundary(ctx context.Context, management enrollmentManagementClient, execution *managementExecution,
	managementReceipt managementBootstrapReceipt, tenantReceipt tenantStageReceipt, selected []enrollmentBMC, requestTimeout time.Duration,
	secret enrollmentSecretReceipt, password string, journal *executionJournal,
) error {
	if err := verifyManagementBootstrap(ctx, management, execution, managementReceipt); err != nil {
		return enrollmentStageFailure(journal, "ENROLLMENT_BOOTSTRAP_CHANGED", "Management bootstrap changed before selected-device discovery.", err)
	}
	if err := tenantReceipt.Client.VerifyIdentity(ctx); err != nil {
		return enrollmentStageFailure(journal, "TENANT_API_IDENTITY_CHANGED", "Tenant API identity changed before selected-device discovery.", err)
	}
	if err := management.verifyEnrollmentBMCs(ctx, selected, requestTimeout, journal); err != nil {
		return err
	}
	if err := inspectEnrollmentScope(ctx, management, execution, selected, true); err != nil {
		return enrollmentStageFailure(journal, "ENROLLMENT_SCOPE_CHANGED", err.Error(), nil)
	}
	current, err := management.Get(ctx, secretIdentity(secret.Namespace))
	if err != nil {
		return enrollmentStageFailure(journal, "ENROLLMENT_SECRET_CHANGED", "The shared BMC credential could not be reverified before discovery.", err)
	}
	verified, err := verifyEnrollmentSecret(current, secret.Namespace, password)
	if err != nil || verified.UID != secret.UID {
		return enrollmentStageFailure(journal, "ENROLLMENT_SECRET_CHANGED", "The shared BMC credential changed before discovery.", err)
	}
	return nil
}

func validateEnrollmentStageInputs(ctx context.Context, management enrollmentManagementClient, execution *managementExecution,
	managementReceipt managementBootstrapReceipt, tenantReceipt tenantStageReceipt, selected []enrollmentBMC, password string,
	journal *executionJournal, interval, requestTimeout time.Duration,
) error {
	if ctx == nil || management == nil || execution == nil || execution.plan == nil || execution.reconciler == nil || journal == nil || len(selected) == 0 || password == "" || tenantReceipt.Client == nil {
		return fmt.Errorf("enrollment stage requires selected BMCs, verified tenant API, retained plan and report")
	}
	if _, bounded := ctx.Deadline(); !bounded || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return fmt.Errorf("enrollment stage requires a deadline and bounded polling/request controls")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantReceipt.ClusterUID == "" || tenantReceipt.Client.dpuClusterUID != string(tenantReceipt.ClusterUID) {
		return fmt.Errorf("tenant API receipt is incomplete or bound to a different DPUCluster")
	}
	expected, _, err := tenantPlanInputs(execution.plan)
	if err != nil || !reflect.DeepEqual(expected, tenantReceipt.Client.expected) || tenantReceipt.Client.source != management {
		return fmt.Errorf("tenant API source and expected configuration differ from the retained enrollment plan")
	}
	if _, err := selectedEnrollmentDiscoveries(execution, selected); err != nil {
		return err
	}
	if execution.plan.Digest() != journal.digest || managementReceipt.BundleDigest != journal.digest || execution.reconciler.BundleDigest() != journal.digest {
		return fmt.Errorf("enrollment report, bootstrap, tenant and retained plan identities differ")
	}
	if !management.matchesBinding(execution.plan.Intent().Management, execution.plan.Intent().KubernetesVersion) {
		return fmt.Errorf("management client binding differs from the retained enrollment plan")
	}
	return nil
}

func waitSelectedEnrollment(ctx context.Context, client enrollmentManagementClient, execution *managementExecution, selected []enrollmentBMC,
	interval time.Duration, journal *executionJournal,
) ([]enrolledDeviceReceipt, error) {
	observed := make([]enrolledDeviceReceipt, len(selected))
	for {
		if err := ctx.Err(); err != nil {
			return observed, enrollmentStageFailure(journal, "ENROLLMENT_READINESS_CANCELED", "Selected-device enrollment did not become ready before cancellation; inspect recorded partial effects.", err)
		}
		// Recheck the complete scope, including every DPUNode association, on
		// each poll. Reading only the deterministic node name misses duplicates.
		if err := inspectEnrollmentScope(ctx, client, execution, selected, true); err != nil {
			return observed, enrollmentStageFailure(journal, "ENROLLMENT_SCOPE_CHANGED", err.Error(), nil)
		}
		current, allReady, err := observeSelectedEnrollment(ctx, client, execution, selected, observed, journal)
		observed = current
		if err != nil || allReady {
			return observed, err
		}
		if err := waitInterval(ctx, interval); err != nil {
			return observed, enrollmentStageFailure(journal, "ENROLLMENT_READINESS_TIMEOUT", "Selected devices did not reach verified readiness before the deadline.", err)
		}
	}
}

// observeSelectedEnrollment observes the entire cohort on every pass; a prior
// pass is used only to detect replacement, never to cache readiness.
func observeSelectedEnrollment(ctx context.Context, client enrollmentManagementClient, execution *managementExecution, selected []enrollmentBMC, previous []enrolledDeviceReceipt, journal *executionJournal) ([]enrolledDeviceReceipt, bool, error) {
	current := append([]enrolledDeviceReceipt(nil), previous...)
	if len(current) != len(selected) {
		return nil, false, fmt.Errorf("selected enrollment receipt count differs from the selected devices")
	}
	allReady := true
	for i, item := range selected {
		current[i].Serial, current[i].BMCIP, current[i].BMCPort = item.device.Serial, item.device.BMCIP, item.device.BMCPort
		for _, kind := range []string{"DPUDevice", "DPUNode"} {
			name := strings.ToLower(item.device.Serial)
			uid := &current[i].DeviceUID
			if kind == "DPUNode" {
				name, uid = "dpu-node-"+name, &current[i].DPUNodeUID
			}
			identity := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": execution.operatorNamespace}}}
			object, err := client.Get(ctx, identity)
			check := result("ENROLLMENT_RESOURCE_PENDING", "The selected enrollment resource has not been created.")
			if apierrors.IsNotFound(err) && *uid == "" {
				allReady = false
			} else if err != nil {
				return current, false, enrollmentStageFailure(journal, "ENROLLMENT_RESOURCE_READ_FAILED", "A selected enrollment resource disappeared or could not be read.", err)
			} else {
				if kind == "DPUDevice" {
					err = validateEnrolledDevice(object, item, execution.operatorNamespace, false)
				} else {
					err = validateEnrolledNode(object, item, execution.operatorNamespace)
				}
				if err != nil {
					return current, false, enrollmentStageFailure(journal, "ENROLLED_IDENTITY_MISMATCH", err.Error(), nil)
				}
				if *uid != "" && *uid != object.GetUID() {
					return current, false, enrollmentStageFailure(journal, "ENROLLED_IDENTITY_CHANGED", "A selected DPUDevice or DPUNode was replaced during enrollment.", nil)
				}
				*uid = object.GetUID()
				if kind == "DPUDevice" {
					check = enrolledDeviceReadiness(object)
					if check.Ready {
						if err := validateEnrolledDevice(object, item, execution.operatorNamespace, true); err != nil {
							return current, false, enrollmentStageFailure(journal, "ENROLLED_STATUS_MISMATCH", err.Error(), nil)
						}
					}
				} else {
					check = enrolledNodeReadiness(object)
				}
				allReady = allReady && check.Ready
			}
			event := executionEvent{Stage: generation.StageEnrollment, Operation: "device-readiness", Subject: kind + "/" + execution.operatorNamespace + "/" + name, State: "waiting", ReasonCode: check.ReasonCode, Message: check.Message}
			if check.Ready {
				event.State = "ready"
			}
			if err := journal.record(event); err != nil {
				return current, false, err
			}
			if terminalEnrollmentReadiness(check) {
				return current, false, enrollmentStageFailure(journal, check.ReasonCode, check.Message, nil)
			}
		}
	}
	return current, allReady, nil
}

func terminalEnrollmentReadiness(check Readiness) bool {
	for _, code := range []string{"RESOURCE_TERMINAL_FAILURE", "RESOURCE_STATUS_INVALID", "RESOURCE_DELETING"} {
		if check.ReasonCode == code || strings.HasPrefix(check.ReasonCode, code+":") {
			return true
		}
	}
	return false
}

func waitEnrollmentDiscovery(ctx context.Context, client enrollmentManagementClient, execution *managementExecution, selected []enrollmentBMC, desired *unstructured.Unstructured, uid types.UID, interval time.Duration, journal *executionJournal) error {
	event := executionEvent{Stage: generation.StageEnrollment, Operation: "discovery-readiness", Subject: desired.GetNamespace() + "/" + desired.GetName(), State: "attempted", ReasonCode: "DPU_DISCOVERY_SCAN_STARTED", Message: "Wait for a successful DPUDiscovery scan of the retained selected BMC endpoint."}
	if err := journal.record(event); err != nil {
		return err
	}
	partial := false
	for {
		if err := ctx.Err(); err != nil {
			if partial {
				return journalFailure(journal, event, false, "ENROLLMENT_PARTIAL_NODE_MISSING", errEnrollmentNodeMissing.Error(), err)
			}
			return journalFailure(journal, event, false, "DPU_DISCOVERY_SCAN_CANCELED", "The selected BMC scan did not complete before cancellation.", err)
		}
		current, err := client.Get(ctx, desired.DeepCopy())
		if err != nil {
			return journalFailure(journal, event, false, "DPU_DISCOVERY_READ_FAILED", "The retained DPUDiscovery disappeared or could not be read.", err)
		}
		if current == nil || current.GetUID() != uid || current.GetResourceVersion() == "" {
			return journalFailure(journal, event, false, "DPU_DISCOVERY_IDENTITY_CHANGED", "The selected DPUDiscovery was replaced or has no verifiable identity.", nil)
		}
		if err := execution.reconciler.Verify(desired, current); err != nil {
			return journalFailure(journal, event, false, "DPU_DISCOVERY_INTENT_CHANGED", "The retained DPUDiscovery specification or ownership changed while scanning.", err)
		}
		readiness := EvaluateReadiness(current)
		if terminalEnrollmentReadiness(readiness) {
			return journalFailure(journal, event, false, readiness.ReasonCode, readiness.Message, nil)
		}
		scopeErr := inspectEnrollmentScope(ctx, client, execution, selected, true)
		partial = errors.Is(scopeErr, errEnrollmentNodeMissing)
		if scopeErr != nil && !partial {
			return journalFailure(journal, event, false, "ENROLLMENT_SCOPE_CHANGED", scopeErr.Error(), nil)
		}
		if partial {
			if readiness.Ready || readiness.ReasonCode == "DISCOVERY_NO_DPUS_FOUND" {
				return journalFailure(journal, event, false, "ENROLLMENT_PARTIAL_NODE_MISSING", errEnrollmentNodeMissing.Error(), nil)
			}
			readiness = result("ENROLLMENT_NODE_CREATION_PENDING", "A selected DPUDevice exists but its DPUNode is still absent. If the crawler's node creation fails, a rescan cannot repair this partial enrollment.")
		}
		// The crawler skips already-enrolled BMC IPs. Zero newly found devices
		// can therefore complete a scan, but never establishes enrollment: the
		// next stage still verifies every selected Device/Node and condition.
		if readiness.Ready || readiness.ReasonCode == "DISCOVERY_NO_DPUS_FOUND" {
			event.State, event.ReasonCode = "ready", "DPU_DISCOVERY_SCAN_READY"
			event.Message = "The retained DPUDiscovery completed a current scan; selected-device identity and readiness are checked separately."
			return journal.record(event)
		}
		event.State, event.ReasonCode, event.Message = "waiting", readiness.ReasonCode, readiness.Message
		if err := journal.record(event); err != nil {
			return err
		}
		if err := waitInterval(ctx, interval); err != nil {
			if partial {
				return journalFailure(journal, event, false, "ENROLLMENT_PARTIAL_NODE_MISSING", errEnrollmentNodeMissing.Error(), err)
			}
			return journalFailure(journal, event, false, "DPU_DISCOVERY_SCAN_TIMEOUT", "The selected DPUDiscovery did not complete before the deadline.", err)
		}
	}
}

func containsEnrollmentString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func enrollmentStageFailure(journal *executionJournal, code, message string, cause error) error {
	var safe error
	if cause != nil {
		safe = safeAPIError("selected-device enrollment", cause)
	}
	return errors.Join(journal.record(executionEvent{Stage: generation.StageEnrollment, Operation: "check", State: "failed", ReasonCode: code, Message: message}), reconcileError(code, message, safe))
}
