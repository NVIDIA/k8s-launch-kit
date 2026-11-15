// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementnetwork"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

type tenantNetworkObserver func(context.Context, bootstrap.Tenant) ([]inventory.Check, error)

type tenantStageManagementClient interface {
	ResourceClient
	VerifyIdentity(context.Context) error
	List(context.Context, *unstructured.Unstructured, string) ([]unstructured.Unstructured, error)
	checkTenantStorage(context.Context, string) PreflightCheck
	matchesBinding(generation.ManagementIdentity, string) bool
	connectTenant(context.Context, *generation.Plan, types.UID, *Reconciler, *executionJournal, time.Duration, time.Duration) (*tenantClient, error)
}

func (c *APIClient) connectTenant(ctx context.Context, plan *generation.Plan, uid types.UID, reconciler *Reconciler,
	journal *executionJournal, interval, requestTimeout time.Duration,
) (*tenantClient, error) {
	return connectRetainedTenant(ctx, c, plan, uid, reconciler, journal, interval, requestTimeout)
}

type tenantStageReceipt struct {
	ClusterUID types.UID
	Client     *tenantClient
}

// runTenantStage advances one validated plan from the completed management
// bootstrap to an exactly owned DPUCluster and verified tenant API. It does
// not prove external VIP reservation or VRID uniqueness; the network
// observations establish only current selected-node facts.
func runTenantStage(ctx context.Context, management tenantStageManagementClient, execution *managementExecution, bootstrapReceipt managementBootstrapReceipt,
	journal *executionJournal, interval, requestTimeout time.Duration,
	observeNetwork tenantNetworkObserver,
) (tenantStageReceipt, error) {
	if err := validateTenantStageInputs(ctx, management, execution, bootstrapReceipt, journal, interval, requestTimeout, observeNetwork); err != nil {
		return tenantStageReceipt{}, err
	}
	plan, reconciler := execution.plan, execution.reconciler
	expected, desired, err := tenantPlanInputs(plan)
	if err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_PLAN_INVALID", "The retained tenant stage is invalid.", err)
	}
	if err := journalTenantChecks(journal, "allocation-check", managementnetwork.CheckTenantAllocation(expectedTenant(plan))); err != nil {
		return tenantStageReceipt{}, err
	}
	if err := verifyManagementBootstrap(ctx, management, execution, bootstrapReceipt); err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_BOOTSTRAP_RECEIPT_INVALID", "The management bootstrap receipt no longer matches live retained resources; tenant effects are stopped.", err)
	}
	if err := management.VerifyIdentity(ctx); err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "The bound management identity changed before tenant-stage checks.", err)
	}

	if err := checkTenantStorageForStage(ctx, management, journal, execution.kamajiNamespace); err != nil {
		return tenantStageReceipt{}, err
	}

	clusters, err := listTenantClusters(ctx, management)
	if err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_COLLISION_LIST_INCOMPLETE", "The all-namespace DPUCluster list is incomplete; tenant creation is stopped.", err)
	}
	target, err := checkTenantRegistry(clusters, desired, expected.VIP)
	if err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_REGISTRY_CONFLICT", err.Error(), nil)
	}

	if target == nil {
		if err := observeAndJournalTenantNetwork(ctx, observeNetwork, expectedTenant(plan), journal); err != nil {
			return tenantStageReceipt{}, err
		}
		// Probes may take time, so refresh the complete collision domain before
		// the final storage and management receipt checks below.
		clusters, err = listTenantClusters(ctx, management)
		if err != nil {
			return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_COLLISION_LIST_INCOMPLETE", "The final all-namespace DPUCluster list is incomplete; tenant creation is stopped.", err)
		}
		target, err = checkTenantRegistry(clusters, desired, expected.VIP)
		if err != nil {
			return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_REGISTRY_CONFLICT", err.Error(), nil)
		}
	}
	// This is the last backing-resource observation before the effect. For a
	// new target it follows the probe and final collision scan; for an existing
	// target it confirms storage again before the exact ownership check.
	if err := checkTenantStorageForStage(ctx, management, journal, execution.kamajiNamespace); err != nil {
		return tenantStageReceipt{}, err
	}
	if err := verifyManagementBootstrap(ctx, management, execution, bootstrapReceipt); err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_BOOTSTRAP_RECEIPT_CHANGED", "Management bootstrap identity changed before tenant reconciliation.", err)
	}

	if err := management.VerifyIdentity(ctx); err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "The bound management identity changed before the DPUCluster create-or-verify boundary.", err)
	}
	resourceClient := ResourceClient(management)
	if target != nil {
		resourceClient = existingOnlyResourceClient{ResourceClient: management}
	}
	cluster, err := executeNativeObject(ctx, resourceClient, reconciler, desired, generation.StageTenant, journal, false, interval)
	if err != nil {
		return tenantStageReceipt{}, err
	}
	if target != nil && cluster.GetUID() != target.GetUID() {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_IDENTITY_CHANGED", "The selected DPUCluster changed after the collision observation.", nil)
	}
	if err := management.VerifyIdentity(ctx); err != nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "The bound management identity changed after DPUCluster reconciliation.", err)
	}
	tenant, err := management.connectTenant(ctx, plan, cluster.GetUID(), reconciler, journal, interval, requestTimeout)
	if err != nil {
		return tenantStageReceipt{}, err
	}
	if tenant == nil {
		return tenantStageReceipt{}, tenantStageFailure(journal, "TENANT_API_RECEIPT_INCOMPLETE", "Tenant API access returned no verified client; dependent stages remain stopped.", nil)
	}
	return tenantStageReceipt{ClusterUID: cluster.GetUID(), Client: tenant}, nil
}

func validateTenantStageInputs(ctx context.Context, management tenantStageManagementClient, execution *managementExecution, receipt managementBootstrapReceipt,
	journal *executionJournal, interval, requestTimeout time.Duration, observeNetwork tenantNetworkObserver,
) error {
	if ctx == nil || management == nil || execution == nil || execution.plan == nil || execution.operator == nil || execution.reconciler == nil || execution.configObject == nil || execution.manager == nil || len(execution.namespaces) == 0 || journal == nil || observeNetwork == nil {
		return fmt.Errorf("tenant stage requires a retained plan, bound management client, report and fresh network observer")
	}
	if _, ok := ctx.Deadline(); !ok || interval <= 0 || requestTimeout <= 0 {
		return fmt.Errorf("tenant stage requires an execution deadline and positive polling and request timeouts")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	plan := execution.plan
	if plan.Digest() == "" || journal.digest != plan.Digest() || execution.reconciler.BundleDigest() != plan.Digest() || receipt.BundleDigest != plan.Digest() {
		return fmt.Errorf("tenant stage, bootstrap receipt, reconciler and report must identify the same retained bundle")
	}
	intent := plan.Intent()
	if !management.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return fmt.Errorf("management client binding differs from the retained tenant plan")
	}
	if len(validation.IsDNS1123Label(execution.kamajiNamespace)) != 0 || receipt.OperatorNamespace == "" || receipt.OperatorConfigUID == "" || receipt.ManagerDeploymentUID == "" || receipt.ManagerDeploymentName == "" {
		return fmt.Errorf("tenant stage requires a complete management bootstrap receipt and Kamaji namespace")
	}
	if receipt.OperatorNamespace != intent.Bootstrap.OperatorNamespace || receipt.OperatorConfigName != intent.Bootstrap.OperatorConfigName {
		return fmt.Errorf("management bootstrap receipt identity differs from retained intent")
	}
	return nil
}

func verifyManagementBootstrap(ctx context.Context, client tenantStageManagementClient, execution *managementExecution, receipt managementBootstrapReceipt) error {
	if err := client.VerifyIdentity(ctx); err != nil {
		return err
	}
	if execution == nil || execution.operator == nil || execution.manager == nil || execution.configObject == nil || execution.reconciler == nil {
		return fmt.Errorf("prepared management execution is incomplete")
	}
	if receipt.OperatorRelease != execution.operator.releaseName || receipt.ManagerDeploymentName != execution.manager.GetName() || receipt.OperatorNamespace != execution.operator.namespace {
		return fmt.Errorf("bootstrap receipt manager identity differs from the retained operator plan")
	}
	if len(execution.namespaces) == 0 || len(receipt.NamespaceUIDs) != len(execution.namespaces) {
		return fmt.Errorf("bootstrap receipt namespace set differs from the retained plan")
	}
	for _, wanted := range execution.namespaces {
		uid := receipt.NamespaceUIDs[wanted.GetName()]
		if uid == "" {
			return fmt.Errorf("bootstrap receipt omits a retained namespace identity")
		}
		current, err := client.Get(ctx, wanted.DeepCopy())
		if err != nil || current == nil || current.GetUID() != types.UID(uid) || verifyNamespace(wanted.GetName(), current) != nil {
			return fmt.Errorf("a management namespace no longer matches the bootstrap receipt")
		}
	}
	desiredConfig := execution.configObject
	if desiredConfig == nil || desiredConfig.GetNamespace() != receipt.OperatorNamespace || desiredConfig.GetName() != receipt.OperatorConfigName {
		return fmt.Errorf("retained DPFOperatorConfig identity differs from bootstrap receipt")
	}
	config, err := client.Get(ctx, desiredConfig.DeepCopy())
	if err != nil || config == nil || config.GetUID() != types.UID(receipt.OperatorConfigUID) || execution.reconciler.Verify(desiredConfig, config) != nil {
		return fmt.Errorf("DPFOperatorConfig no longer matches the bootstrap receipt and retained intent")
	}
	if readiness := EvaluateReadiness(config); !readiness.Ready || !operatorConfigVersionMatches(config, execution.operatorVersion) {
		return fmt.Errorf("DPFOperatorConfig is no longer ready for the retained operator release")
	}
	currentManager, err := client.Get(ctx, execution.manager.DeepCopy())
	if err != nil || currentManager == nil || currentManager.GetUID() != types.UID(receipt.ManagerDeploymentUID) || !managerDeploymentIntentMatches(execution.manager, currentManager) {
		return fmt.Errorf("DPF manager Deployment no longer matches the bootstrap receipt")
	}
	if readiness := EvaluateReadiness(currentManager); !readiness.Ready {
		return fmt.Errorf("DPF manager Deployment is no longer ready: %s", readiness.ReasonCode)
	}
	return client.VerifyIdentity(ctx)
}

func checkTenantStorageForStage(ctx context.Context, client tenantStageManagementClient, journal *executionJournal, namespace string) error {
	event := executionEvent{Stage: generation.StageTenant, Operation: "storage-check", Subject: namespace, State: "attempted", ReasonCode: "TENANT_STORAGE_CHECK_STARTED", Message: "Recheck current Kamaji backing resources immediately before tenant reconciliation."}
	if err := journal.record(event); err != nil {
		return err
	}
	check := client.checkTenantStorage(ctx, namespace)
	if err := journal.record(executionEvent{Stage: event.Stage, Operation: "storage-check", Subject: namespace, State: check.State,
		ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
		return err
	}
	if check.State != "passed" {
		return fmt.Errorf("tenant storage check did not pass: %s", check.ReasonCode)
	}
	return nil
}

func listTenantClusters(ctx context.Context, client tenantStageManagementClient) ([]unstructured.Unstructured, error) {
	template := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster"}}
	return client.List(ctx, template, "")
}

func checkTenantRegistry(clusters []unstructured.Unstructured, desired *unstructured.Unstructured, vip string) (*unstructured.Unstructured, error) {
	wantedVIP, err := netip.ParseAddr(vip)
	if err != nil || !wantedVIP.Is4() || wantedVIP.Is4In6() || !wantedVIP.IsGlobalUnicast() {
		return nil, fmt.Errorf("retained tenant VIP is invalid")
	}
	wantedRouter, found, err := unstructured.NestedInt64(desired.Object, "spec", "clusterEndpoint", "keepalived", "virtualRouterID")
	if err != nil || !found || wantedRouter < 1 || wantedRouter > 255 {
		return nil, fmt.Errorf("retained tenant Keepalived virtual router ID is invalid")
	}
	wantedSelector, found, err := unstructured.NestedStringMap(desired.Object, "spec", "clusterEndpoint", "keepalived", "nodeSelector")
	if err != nil || !found || len(wantedSelector) == 0 {
		return nil, fmt.Errorf("retained tenant Keepalived node selector is invalid")
	}
	var target *unstructured.Unstructured
	for i := range clusters {
		cluster := &clusters[i]
		if cluster.GetAPIVersion() != desired.GetAPIVersion() || cluster.GetKind() != "DPUCluster" || cluster.GetName() == "" || cluster.GetNamespace() == "" || cluster.GetUID() == "" || cluster.GetDeletionTimestamp() != nil {
			return nil, fmt.Errorf("all-namespace DPUCluster observation contains an invalid or deleting object")
		}
		if cluster.GetNamespace() == desired.GetNamespace() && cluster.GetName() == desired.GetName() {
			if target != nil {
				return nil, fmt.Errorf("selected DPUCluster identity appeared more than once")
			}
			target = cluster.DeepCopy()
			continue
		}
		clusterType, found, err := unstructured.NestedString(cluster.Object, "spec", "type")
		if err != nil || !found || clusterType == "" {
			return nil, fmt.Errorf("an existing DPUCluster has an unsupported or missing type")
		}
		_, endpointFound, endpointErr := unstructured.NestedMap(cluster.Object, "spec", "clusterEndpoint")
		if endpointErr != nil {
			return nil, fmt.Errorf("an existing DPUCluster has a malformed endpoint")
		}
		if !endpointFound {
			// The pinned CRD permits static and Kamaji DPUClusters without a
			// Keepalived endpoint. Custom controller types have unknown endpoint
			// semantics, so their absence cannot establish a collision-free view.
			if clusterType == "kamaji" || clusterType == "static" {
				continue
			}
			return nil, fmt.Errorf("a custom DPUCluster type has no inspectable endpoint")
		}
		_, keepalivedFound, keepalivedErr := unstructured.NestedMap(cluster.Object, "spec", "clusterEndpoint", "keepalived")
		if keepalivedErr != nil {
			return nil, fmt.Errorf("an existing DPUCluster has malformed Keepalived configuration")
		}
		if !keepalivedFound {
			continue
		}
		observedVIP, found, err := unstructured.NestedString(cluster.Object, "spec", "clusterEndpoint", "keepalived", "vip")
		if err != nil || !found || observedVIP == "" {
			return nil, fmt.Errorf("an existing DPUCluster has no usable VIP for collision checking")
		}
		addr, err := netip.ParseAddr(observedVIP)
		if err != nil || !addr.Is4() || addr.Is4In6() || !addr.IsGlobalUnicast() {
			return nil, fmt.Errorf("an existing DPUCluster has an invalid VIP")
		}
		if addr.Unmap() == wantedVIP.Unmap() {
			return nil, fmt.Errorf("the requested VIP is already assigned to another DPUCluster")
		}
		if routerID, found, err := unstructured.NestedInt64(cluster.Object, "spec", "clusterEndpoint", "keepalived", "virtualRouterID"); err != nil || !found || routerID < 1 || routerID > 255 {
			return nil, fmt.Errorf("an existing DPUCluster has no valid Keepalived virtual router ID")
		} else if selector, selectorFound, selectorErr := unstructured.NestedStringMap(cluster.Object, "spec", "clusterEndpoint", "keepalived", "nodeSelector"); selectorErr != nil {
			return nil, fmt.Errorf("an existing DPUCluster has an invalid Keepalived node selector")
		} else {
			if !selectorFound {
				selector = map[string]string{} // An omitted selector may cover every node.
			}
			if routerID == wantedRouter && selectorsMayOverlap(wantedSelector, selector) {
				return nil, fmt.Errorf("the requested Keepalived virtual router ID overlaps another DPUCluster selector")
			}
		}
	}
	return target, nil
}

func selectorsMayOverlap(first, second map[string]string) bool {
	for key, value := range first {
		if other, exists := second[key]; exists && other != value {
			return false
		}
	}
	return true
}

func observeAndJournalTenantNetwork(ctx context.Context, observer tenantNetworkObserver, tenant bootstrap.Tenant, journal *executionJournal) error {
	event := executionEvent{Stage: generation.StageTenant, Operation: "network-check", Subject: tenant.Namespace + "/" + tenant.Name, State: "attempted", ReasonCode: "TENANT_NETWORK_CHECK_STARTED", Message: "Collect fresh selected-node network observations before creating a new DPUCluster."}
	if err := journal.record(event); err != nil {
		return err
	}
	checks, err := observer(ctx, tenant)
	var checkErr error
	if len(checks) > 0 {
		checkErr = journalTenantChecks(journal, "network-check", checks)
	}
	if err != nil {
		return errors.Join(checkErr, tenantStageFailure(journal, "TENANT_NETWORK_OBSERVATION_FAILED", "Fresh selected-node network observations could not be completed.", err))
	}
	if checkErr != nil {
		return checkErr
	}
	if len(checks) == 0 {
		return tenantStageFailure(journal, "TENANT_NETWORK_OBSERVATION_EMPTY", "Fresh selected-node network observations returned no checks.", nil)
	}
	return nil
}

func journalTenantChecks(journal *executionJournal, operation string, checks []inventory.Check) error {
	if len(checks) == 0 {
		return tenantStageFailure(journal, "TENANT_REQUIRED_CHECKS_UNAVAILABLE", "A required tenant-stage check returned no observations.", nil)
	}
	if len(checks) > 256 {
		return tenantStageFailure(journal, "TENANT_CHECKS_UNBOUNDED", "Required tenant-stage checks exceeded the supported bound.", nil)
	}
	passed := true
	for _, check := range checks {
		state, reasonCode, message := check.State, check.ReasonCode, check.Message
		switch check.State {
		case "passed":
			state = "confirmed"
		case "failed", "unknown":
			passed = false
		default:
			state, reasonCode, message, passed = "unknown", "TENANT_CHECK_STATE_INVALID", "A required tenant-stage check returned an unsupported state.", false
		}
		if err := journal.record(executionEvent{Stage: generation.StageTenant, Operation: operation, Subject: check.Subject,
			State: state, ReasonCode: reasonCode, Message: message}); err != nil {
			return err
		}
	}
	if !passed {
		return tenantStageFailure(journal, "TENANT_REQUIRED_CHECKS_BLOCKED", "Required tenant-stage checks did not all pass; tenant creation is stopped.", nil)
	}
	return nil
}

func tenantStageFailure(journal *executionJournal, code, message string, cause error) error {
	event := executionEvent{Stage: generation.StageTenant, Operation: "stage-check", State: "failed", ReasonCode: code, Message: message}
	if cause == nil {
		return errors.Join(journal.record(event), fmt.Errorf("%s: %s", code, message))
	}
	return errors.Join(journal.record(event), fmt.Errorf("%s: %s", code, message), safeAPIError("tenant stage", cause))
}

func expectedTenant(plan *generation.Plan) bootstrap.Tenant {
	if plan == nil {
		return bootstrap.Tenant{}
	}
	return plan.Intent().Bootstrap.Tenant
}

type existingOnlyResourceClient struct{ ResourceClient }

func (c existingOnlyResourceClient) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	current, err := c.ResourceClient.Get(ctx, desired)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("selected DPUCluster disappeared after the all-namespace collision check")
	}
	return current, err
}

func (c existingOnlyResourceClient) Create(context.Context, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("selected DPUCluster disappeared after the all-namespace collision check; retry from fresh observations")
}
