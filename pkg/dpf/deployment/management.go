// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

type managementRunnerClient interface {
	ResourceClient
	List(context.Context, *unstructured.Unstructured, string) ([]unstructured.Unstructured, error)
	VerifyIdentity(context.Context) error
	CheckManagementPreflight(context.Context, ManagementPreflightRequest) PreflightResult
	checkDPFOperatorConfigPermissions(context.Context, string) []PreflightCheck
	discoverResourceList(context.Context, string) (*metav1.APIResourceList, error)
	ensureOperatorPlan(context.Context, *operatorPlan) error
	matchesBinding(generation.ManagementIdentity, string) bool
}

// managementBootstrapReceipt describes only the operator and configuration
// bootstrap stages. It is intentionally not a complete HBN deployment result.
type managementBootstrapReceipt struct {
	BundleDigest          string
	OperatorNamespace     string
	NamespaceUIDs         map[string]string
	OperatorRelease       string
	ManagerDeploymentName string
	ManagerDeploymentUID  string
	OperatorConfigName    string
	OperatorConfigUID     string
}

type operatorCRDExpectation struct {
	name       string
	group      string
	plural     string
	kind       string
	namespaced bool
	versions   []string
	spec       map[string]any
}

// runManagement validates the complete retained bundle before any management
// API call, then executes only the management bootstrap portion of its fixed
// workflow. Later tenant, enrollment, workload and DPUDeployment stages remain
// unexecuted and are not reported as ready.
func runManagement(ctx context.Context, client managementRunnerClient, retained *bundle.Bundle,
	journal *executionJournal, pollInterval time.Duration,
) (managementBootstrapReceipt, error) {
	if ctx == nil || client == nil || journal == nil || pollInterval <= 0 {
		return managementBootstrapReceipt{}, fmt.Errorf("management runner requires context, client, report and positive polling interval")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return managementBootstrapReceipt{}, fmt.Errorf("management runner requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return managementBootstrapReceipt{}, err
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		return managementBootstrapReceipt{}, fmt.Errorf("validate retained DPF plan before management access: %w", err)
	}
	if journal.digest == "" || journal.digest != plan.Digest() {
		return managementBootstrapReceipt{}, fmt.Errorf("execution report and retained DPF bundle must have the same digest")
	}
	intent := plan.Intent()
	if !client.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return managementBootstrapReceipt{}, fmt.Errorf("management client binding differs from the retained plan; repeat discovery and generation")
	}
	operatorInputs, operatorArchive := plan.Operator()
	operatorPlan, err := prepareOperator(OperatorRequest{
		Archive: operatorArchive,
		Inputs:  operatorInputs,
		Timeout: time.Until(deadline(ctx)),
	})
	if err != nil {
		return managementBootstrapReceipt{}, fmt.Errorf("prepare retained operator installation: %w", err)
	}
	manager, crds, err := operatorReadinessObjects(operatorPlan)
	if err != nil {
		return managementBootstrapReceipt{}, err
	}
	reconciler, err := NewReconciler(operatorPlan.renderedManifest, plan.Digest())
	if err != nil {
		return managementBootstrapReceipt{}, fmt.Errorf("prepare retained DPF resource verifier: %w", err)
	}
	namespaces, operatorNamespace, configObject, err := managementBootstrapObjects(retained, operatorInputs)
	if err != nil {
		return managementBootstrapReceipt{}, err
	}

	preflightEvent := executionEvent{Stage: "management-preflight", Operation: "observe", State: "attempted", ReasonCode: "MANAGEMENT_PREFLIGHT_STARTED", Message: "Verify external prerequisites and caller permissions before management changes."}
	if err := journal.record(preflightEvent); err != nil {
		return managementBootstrapReceipt{}, err
	}
	preflight := client.CheckManagementPreflight(ctx, ManagementPreflightRequest{Bundle: retained})
	preflight.Checks = append(preflight.Checks, client.checkDPFOperatorConfigPermissions(ctx, operatorNamespace)...)
	ready := preflight.Ready && len(preflight.Checks) > 0
	for _, check := range preflight.Checks {
		state := "confirmed"
		if check.State != "passed" {
			state = "failed"
			ready = false
		}
		if err := journal.record(executionEvent{Stage: "management-preflight", Operation: "check", Subject: check.ID,
			State: state, ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
			return managementBootstrapReceipt{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, preflightEvent, false, "MANAGEMENT_PREFLIGHT_CANCELED", "Management preflight did not complete before cancellation.", err)
	}
	if !ready {
		return managementBootstrapReceipt{}, journalFailure(journal, preflightEvent, false, "MANAGEMENT_PREFLIGHT_BLOCKED", "Management prerequisites or caller permissions are not ready.", nil)
	}
	configListEvent := executionEvent{Stage: "management-config-collision-check", Operation: "list", Subject: "DPFOperatorConfig", State: "attempted",
		ReasonCode: "DPF_CONFIG_COLLISION_CHECK_STARTED", Message: "Check cluster-wide DPFOperatorConfig ownership before namespace or Helm effects."}
	if err := journal.record(configListEvent); err != nil {
		return managementBootstrapReceipt{}, err
	}
	existingConfigs, listErr := listOperatorConfigs(ctx, client)
	if listErr != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, configListEvent, false, "DPF_CONFIG_COLLISION_CHECK_FAILED", "Existing DPFOperatorConfigs could not be completely inspected.", listErr)
	}
	if len(existingConfigs) > 1 {
		return managementBootstrapReceipt{}, journalFailure(journal, configListEvent, false, "DPF_CONFIG_COLLISION", "Multiple DPFOperatorConfigs exist; no configuration will be adopted.", nil)
	}
	if len(existingConfigs) == 1 {
		existing := existingConfigs[0].DeepCopy()
		if existing.GetName() != configObject.GetName() || existing.GetNamespace() != configObject.GetNamespace() || reconciler.Verify(configObject, existing) != nil {
			return managementBootstrapReceipt{}, journalFailure(journal, configListEvent, false, "DPF_CONFIG_COLLISION", "An existing DPFOperatorConfig is not owned by this exact retained plan.", nil)
		}
		configListEvent.State, configListEvent.ReasonCode = "confirmed", "DPF_CONFIG_OWNERSHIP_VERIFIED"
		configListEvent.Message = "The existing DPFOperatorConfig matches this retained plan's ownership and complete spec."
	} else {
		configListEvent.State, configListEvent.ReasonCode = "confirmed", "DPF_CONFIG_ABSENT"
		configListEvent.Message = "No DPFOperatorConfig is observed; creation remains gated on operator CRD readiness."
	}
	if err := journal.record(configListEvent); err != nil {
		return managementBootstrapReceipt{}, err
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageNamespaces, Operation: "identity-recheck"}, false,
			"MANAGEMENT_IDENTITY_CHANGED", "Management identity changed after preflight; no namespace or Helm effect may begin.", err)
	}

	namespaceUIDs := make(map[string]string, len(namespaces))
	for _, namespace := range namespaces {
		observed, err := executeNamespace(ctx, client, namespace.GetName(), journal)
		if err != nil {
			return managementBootstrapReceipt{}, err
		}
		namespaceUIDs[observed.GetName()] = string(observed.GetUID())
	}
	namespaceUID := namespaceUIDs[operatorNamespace]
	if namespaceUID == "" {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageNamespaces, Operation: "namespace-recheck", Subject: operatorNamespace}, true,
			"MANAGEMENT_NAMESPACE_UNAVAILABLE", "Operator namespace identity was not established; no Helm effect may begin.", nil)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperator, Operation: "identity-recheck"}, true,
			"MANAGEMENT_IDENTITY_CHANGED", "Management identity changed after namespace setup; inspect partial bootstrap state.", err)
	}
	if err := requireNamespaceUID(ctx, client, operatorNamespace, namespaceUID); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageNamespaces, Operation: "namespace-recheck", Subject: operatorNamespace}, true,
			"MANAGEMENT_NAMESPACE_CHANGED", "The operator namespace identity is no longer valid; inspect partial bootstrap state before retrying.", err)
	}

	helmEvent := executionEvent{Stage: generation.StageOperator, Operation: "install-or-verify", Subject: operatorPlan.namespace + "/" + operatorPlan.releaseName,
		State: "attempted", ReasonCode: "DPF_OPERATOR_STARTED", Message: "Install or verify the exact retained DPF operator release; no upgrade or resource adoption is allowed."}
	if err := journal.record(helmEvent); err != nil {
		return managementBootstrapReceipt{}, err
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, helmEvent, false, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed before the Helm operation.", err)
	}
	if err := client.ensureOperatorPlan(ctx, operatorPlan); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, helmEvent, true, "DPF_OPERATOR_INSTALL_UNCERTAIN", "Helm installation may have changed release or chart resources; inspect the management cluster before retrying.", err)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, helmEvent, true, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed during Helm installation; inspect the release before retrying.", err)
	}
	if err := requireNamespaceUID(ctx, client, operatorNamespace, namespaceUID); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, helmEvent, true, "OPERATOR_NAMESPACE_CHANGED", "The operator namespace identity changed during installation.", err)
	}
	helmEvent.State, helmEvent.ReasonCode = "confirmed", "DPF_OPERATOR_RELEASE_VERIFIED"
	helmEvent.Message = "The exact retained DPF operator release is installed or verified; this does not establish tenant or HBN readiness."
	if err := journal.record(helmEvent); err != nil {
		return managementBootstrapReceipt{}, err
	}

	if err := waitOperatorCRDs(ctx, client, crds, journal, pollInterval); err != nil {
		return managementBootstrapReceipt{}, err
	}
	managerObserved, err := waitManagerDeployment(ctx, client, manager, journal, pollInterval)
	if err != nil {
		return managementBootstrapReceipt{}, err
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperatorConfig, Operation: "identity-recheck"}, true,
			"MANAGEMENT_IDENTITY_CHANGED", "Management identity changed after operator readiness; no configuration may be created.", err)
	}

	// Recheck the cluster-wide singleton immediately before its create-or-verify
	// operation; a config can appear while Helm and CRDs are becoming ready.
	latestConfigs, listErr := listOperatorConfigs(ctx, client)
	if listErr != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, configListEvent, false, "DPF_CONFIG_COLLISION_CHECK_FAILED", "Existing DPFOperatorConfigs could not be completely inspected before configuration.", listErr)
	}
	if len(latestConfigs) > 1 || len(latestConfigs) == 1 && (latestConfigs[0].GetName() != configObject.GetName() || latestConfigs[0].GetNamespace() != configObject.GetNamespace() || reconciler.Verify(configObject, latestConfigs[0].DeepCopy()) != nil) {
		return managementBootstrapReceipt{}, journalFailure(journal, configListEvent, false, "DPF_CONFIG_COLLISION", "A conflicting DPFOperatorConfig appeared before configuration; no resource will be adopted.", nil)
	}

	if err := requireNamespaceUID(ctx, client, operatorNamespace, namespaceUID); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageNamespaces, Operation: "namespace-recheck", Subject: operatorNamespace}, true,
			"MANAGEMENT_NAMESPACE_CHANGED", "The operator namespace identity is no longer valid; inspect partial bootstrap state before retrying.", err)
	}
	currentConfig, err := executeNativeObject(ctx, client, reconciler, configObject, generation.StageOperatorConfig, journal, false, pollInterval)
	if err != nil {
		return managementBootstrapReceipt{}, err
	}
	currentConfig, err = waitOperatorConfigReady(ctx, client, reconciler, configObject, currentConfig, "v"+operatorInputs.ReleaseVersion, journal, pollInterval)
	if err != nil {
		return managementBootstrapReceipt{}, err
	}
	if err := requireNamespaceUID(ctx, client, operatorNamespace, namespaceUID); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageNamespaces, Operation: "namespace-recheck", Subject: operatorNamespace}, true,
			"MANAGEMENT_NAMESPACE_CHANGED", "The operator namespace identity is no longer valid; inspect partial bootstrap state before retrying.", err)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperatorConfig, Operation: "identity-recheck"}, true,
			"MANAGEMENT_IDENTITY_CHANGED", "Management identity changed after configuration readiness; discard the bootstrap receipt.", err)
	}
	for _, namespace := range namespaces {
		if err := requireNamespaceUID(ctx, client, namespace.GetName(), namespaceUIDs[namespace.GetName()]); err != nil {
			return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperatorConfig, Operation: "namespace-recheck", Subject: namespace.GetName()}, false,
				"MANAGEMENT_NAMESPACE_CHANGED", "A namespace used by management bootstrap changed before receipt; discard this result.", err)
		}
	}
	managerCurrent, err := client.Get(ctx, manager.DeepCopy())
	if err != nil || managerCurrent == nil || managerCurrent.GetUID() != managerObserved.GetUID() || !managerDeploymentIntentMatches(manager, managerCurrent) {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperator, Operation: "manager-recheck", Subject: manager.GetNamespace() + "/" + manager.GetName()}, false,
			"DPF_MANAGER_CHANGED", "The ready manager Deployment identity or retained specification changed before receipt.", err)
	}
	if readiness := EvaluateReadiness(managerCurrent); !readiness.Ready {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperator, Operation: "manager-recheck", Subject: manager.GetNamespace() + "/" + manager.GetName()}, false,
			readiness.ReasonCode, readiness.Message, nil)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return managementBootstrapReceipt{}, journalFailure(journal, executionEvent{Stage: generation.StageOperator, Operation: "identity-recheck"}, false,
			"MANAGEMENT_IDENTITY_CHANGED", "Management identity changed before the management bootstrap receipt.", err)
	}
	return managementBootstrapReceipt{
		BundleDigest: plan.Digest(), OperatorNamespace: operatorNamespace, NamespaceUIDs: namespaceUIDs,
		OperatorRelease: operatorPlan.releaseName, ManagerDeploymentName: manager.GetName(),
		ManagerDeploymentUID: string(managerObserved.GetUID()), OperatorConfigName: currentConfig.GetName(),
		OperatorConfigUID: string(currentConfig.GetUID()),
	}, nil
}

func (c *APIClient) checkDPFOperatorConfigPermissions(ctx context.Context, namespace string) []PreflightCheck {
	if c == nil || c.access.Client == nil || ctx == nil {
		return []PreflightCheck{blockedPreflight("caller.rbac.dpfoperatorconfig", "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable.")}
	}
	checks := []PreflightCheck{
		c.checkAccess(ctx, accessRequirement{verb: "list", group: "operator.dpu.nvidia.com", version: "v1alpha1", resource: "dpfoperatorconfigs"}),
		c.checkAccess(ctx, accessRequirement{verb: "get", group: "operator.dpu.nvidia.com", version: "v1alpha1", resource: "dpfoperatorconfigs", namespace: namespace}),
		c.checkAccess(ctx, accessRequirement{verb: "create", group: "operator.dpu.nvidia.com", version: "v1alpha1", resource: "dpfoperatorconfigs", namespace: namespace}),
	}
	for i := range checks {
		checks[i].ID = "caller.rbac.dpfoperatorconfig." + []string{"list", "get", "create"}[i]
	}
	return checks
}

func (c *APIClient) ensureOperatorPlan(ctx context.Context, plan *operatorPlan) error {
	if c == nil || c.access.REST == nil || plan == nil {
		return fmt.Errorf("bound management client and prepared operator plan are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := time.Until(deadline(ctx))
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	config, err := operatorRESTConfig(ctx, c.access.REST, remaining)
	if err != nil {
		return err
	}
	actionConfig, err := helmclient.NewActionConfig(config, plan.namespace, helmclient.StorageDriver)
	if err != nil {
		return fmt.Errorf("initialize DPF Helm action config")
	}
	return ensureOperator(ctx, plan, helmActionClient{config: actionConfig})
}

func (c *APIClient) matchesBinding(identity generation.ManagementIdentity, version string) bool {
	return c != nil && c.expected == identity && c.version == version
}

func listOperatorConfigs(ctx context.Context, client managementRunnerClient) ([]unstructured.Unstructured, error) {
	groupVersion := "operator.dpu.nvidia.com/v1alpha1"
	resources, err := client.discoverResourceList(ctx, groupVersion)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discover DPFOperatorConfig before create: %w", err)
	}
	if resources == nil || resources.GroupVersion != groupVersion {
		return nil, fmt.Errorf("DPFOperatorConfig API discovery is incomplete")
	}
	found := false
	for _, resource := range resources.APIResources {
		if resource.Name == "dpfoperatorconfigs" && resource.Kind == "DPFOperatorConfig" && resource.Namespaced {
			if found {
				return nil, fmt.Errorf("DPFOperatorConfig API mapping is ambiguous")
			}
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("DPFOperatorConfig API is present without its expected namespaced resource")
	}
	return client.List(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": groupVersion, "kind": "DPFOperatorConfig",
	}}, "")
}

func waitOperatorConfigReady(ctx context.Context, client managementRunnerClient, reconciler *Reconciler, desired, initial *unstructured.Unstructured,
	expectedVersion string, journal *executionJournal, interval time.Duration,
) (*unstructured.Unstructured, error) {
	event := executionEvent{Stage: generation.StageOperatorConfig, Operation: "readiness", Subject: desired.GetNamespace() + "/" + desired.GetName(), State: "attempted",
		ReasonCode: "DPF_CONFIG_READINESS_STARTED", Message: "Wait for the exact retained DPFOperatorConfig generation and pinned operator version."}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	uid := initial.GetUID()
	for {
		if err := ctx.Err(); err != nil {
			return nil, journalFailure(journal, event, false, "DPF_CONFIG_READINESS_CANCELED", "DPFOperatorConfig readiness was not established before cancellation.", err)
		}
		current, err := client.Get(ctx, desired.DeepCopy())
		if err != nil {
			if apierrors.IsNotFound(err) || retryableRead(err) {
				event.State, event.ReasonCode, event.Message = "waiting", "DPF_CONFIG_NOT_OBSERVED", "The retained DPFOperatorConfig is not yet observable."
			} else {
				return nil, journalFailure(journal, event, false, "DPF_CONFIG_READ_FAILED", "The retained DPFOperatorConfig could not be read.", err)
			}
		} else {
			if current == nil || current.GetUID() == "" || current.GetUID() != uid || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil || reconciler.Verify(desired, current) != nil {
				return nil, journalFailure(journal, event, false, "DPF_CONFIG_INTENT_CHANGED", "The DPFOperatorConfig identity or retained intent changed while waiting.", nil)
			}
			readiness := EvaluateReadiness(current)
			if readiness.Ready {
				version, versionFound, versionErr := unstructured.NestedString(current.Object, "status", "version")
				target, targetFound, targetErr := unstructured.NestedString(current.Object, "status", "targetVersion")
				if versionErr != nil || targetErr != nil || !versionFound || !targetFound || version != expectedVersion || target != expectedVersion {
					return nil, journalFailure(journal, event, false, "DPF_CONFIG_VERSION_MISMATCH", "DPFOperatorConfig is Ready for a different or unreported DPF version.", nil)
				}
				event.State, event.ReasonCode, event.Message = "ready", "DPF_CONFIG_READY", "The retained DPFOperatorConfig generation is Ready for the selected operator version."
				if err := journal.record(event); err != nil {
					return nil, err
				}
				return current.DeepCopy(), nil
			}
			switch readiness.ReasonCode {
			case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_STATUS_INVALID", "RESOURCE_API_UNSUPPORTED", "RESOURCE_KIND_UNSUPPORTED":
				return nil, journalFailure(journal, event, false, readiness.ReasonCode, readiness.Message, nil)
			}
			event.State, event.ReasonCode, event.Message = "waiting", readiness.ReasonCode, readiness.Message
		}
		if err := journal.record(event); err != nil {
			return nil, err
		}
		if err := waitInterval(ctx, interval); err != nil {
			return nil, journalFailure(journal, event, false, "DPF_CONFIG_READINESS_TIMEOUT", "DPFOperatorConfig did not become Ready before the execution deadline.", err)
		}
	}
}

func managementBootstrapObjects(retained *bundle.Bundle, inputs bootstrap.OperatorRenderInputs) ([]*unstructured.Unstructured, string, *unstructured.Unstructured, error) {
	namespaceStage, ok := retained.Stage(generation.StageNamespaces)
	if !ok {
		return nil, "", nil, fmt.Errorf("retained namespace stage is missing")
	}
	operatorNamespace := inputs.Bootstrap.OperatorNamespace
	var namespaces []*unstructured.Unstructured
	seen := map[string]bool{}
	for _, document := range namespaceStage.Documents() {
		object := document.ObjectCopy()
		if object == nil || object.GetAPIVersion() != "v1" || object.GetKind() != "Namespace" || object.GetName() == "" || object.GetNamespace() != "" || seen[object.GetName()] {
			return nil, "", nil, fmt.Errorf("retained namespace stage has an invalid or duplicate identity")
		}
		seen[object.GetName()] = true
		namespaces = append(namespaces, object)
	}
	if len(namespaces) == 0 || !seen[operatorNamespace] {
		return nil, "", nil, fmt.Errorf("retained namespace stage omits the DPF operator namespace")
	}
	configStage, ok := retained.Stage(generation.StageOperatorConfig)
	if !ok {
		return nil, "", nil, fmt.Errorf("retained DPFOperatorConfig stage is missing")
	}
	documents := configStage.Documents()
	if len(documents) != 1 {
		return nil, "", nil, fmt.Errorf("retained DPFOperatorConfig stage must contain exactly one resource")
	}
	config := documents[0].ObjectCopy()
	if config == nil || config.GetAPIVersion() != "operator.dpu.nvidia.com/v1alpha1" || config.GetKind() != "DPFOperatorConfig" || config.GetName() != inputs.Bootstrap.OperatorConfigName || config.GetNamespace() != operatorNamespace {
		return nil, "", nil, fmt.Errorf("retained DPFOperatorConfig identity differs from the resolved operator inputs")
	}
	return namespaces, operatorNamespace, config, nil
}

func operatorReadinessObjects(plan *operatorPlan) (*unstructured.Unstructured, []operatorCRDExpectation, error) {
	if plan == nil || len(plan.renderedManifest) == 0 {
		return nil, nil, fmt.Errorf("retained DPF operator manifest is required")
	}
	objects, err := decodeOperatorManifest(plan.renderedManifest)
	if err != nil {
		return nil, nil, fmt.Errorf("decode retained DPF operator projection: %w", err)
	}
	var managers []*unstructured.Unstructured
	var crds []operatorCRDExpectation
	seenCRDs := map[string]bool{}
	for _, object := range objects {
		switch {
		case object.GetAPIVersion() == "apps/v1" && object.GetKind() == "Deployment" && object.GetNamespace() == plan.namespace:
			component, _, _ := unstructured.NestedString(object.Object, "metadata", "labels", "app.kubernetes.io/component")
			if component == "manager" {
				managers = append(managers, object)
			}
		case object.GetAPIVersion() == "apiextensions.k8s.io/v1" && object.GetKind() == "CustomResourceDefinition":
			expectation, err := operatorCRDFromObject(object)
			if err != nil || seenCRDs[expectation.name] {
				return nil, nil, fmt.Errorf("retained DPF operator projection has an invalid or duplicate CRD")
			}
			seenCRDs[expectation.name] = true
			crds = append(crds, expectation)
		}
	}
	if len(managers) != 1 || managers[0].GetName() == "" {
		return nil, nil, fmt.Errorf("retained operator projection must contain exactly one manager Deployment")
	}
	managerImages, err := containerImages(managers[0])
	if err != nil || len(managerImages) == 0 {
		return nil, nil, fmt.Errorf("retained manager Deployment has no validated container images")
	}
	if len(crds) == 0 || !seenCRDs["dpfoperatorconfigs.operator.dpu.nvidia.com"] || !seenCRDs["dpudiscoveries.provisioning.dpu.nvidia.com"] {
		return nil, nil, fmt.Errorf("retained operator projection lacks required DPF API definitions")
	}
	return managers[0], crds, nil
}

func decodeOperatorManifest(manifest []byte) ([]*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	var objects []*unstructured.Unstructured
	for ordinal := 1; ; ordinal++ {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read resource document %d", ordinal)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		data, err := sigsyaml.YAMLToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("decode resource document %d", ordinal)
		}
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("decode resource document %d", ordinal)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func operatorCRDFromObject(object *unstructured.Unstructured) (operatorCRDExpectation, error) {
	group, groupFound, groupErr := unstructured.NestedString(object.Object, "spec", "group")
	plural, pluralFound, pluralErr := unstructured.NestedString(object.Object, "spec", "names", "plural")
	kind, kindFound, kindErr := unstructured.NestedString(object.Object, "spec", "names", "kind")
	scope, scopeFound, scopeErr := unstructured.NestedString(object.Object, "spec", "scope")
	versions, versionsFound, versionsErr := unstructured.NestedSlice(object.Object, "spec", "versions")
	if groupErr != nil || pluralErr != nil || kindErr != nil || scopeErr != nil || versionsErr != nil || !groupFound || !pluralFound || !kindFound || !scopeFound || !versionsFound {
		return operatorCRDExpectation{}, fmt.Errorf("CRD spec is incomplete")
	}
	if object.GetName() != plural+"."+group || (scope != "Namespaced" && scope != "Cluster") {
		return operatorCRDExpectation{}, fmt.Errorf("CRD identity is inconsistent")
	}
	spec, specFound, specErr := unstructured.NestedMap(object.Object, "spec")
	if specErr != nil || !specFound {
		return operatorCRDExpectation{}, fmt.Errorf("CRD spec is unavailable")
	}
	expectation := operatorCRDExpectation{name: object.GetName(), group: group, plural: plural, kind: kind, namespaced: scope == "Namespaced", spec: spec}
	seenVersions := map[string]bool{}
	for _, rawVersion := range versions {
		version, ok := rawVersion.(map[string]any)
		if !ok {
			return operatorCRDExpectation{}, fmt.Errorf("CRD version is malformed")
		}
		name, _ := version["name"].(string)
		served, _ := version["served"].(bool)
		if name == "" || seenVersions[name] {
			return operatorCRDExpectation{}, fmt.Errorf("CRD version identity is invalid")
		}
		seenVersions[name] = true
		if served {
			expectation.versions = append(expectation.versions, name)
		}
	}
	if len(expectation.versions) == 0 {
		return operatorCRDExpectation{}, fmt.Errorf("CRD has no served versions")
	}
	return expectation, nil
}

func waitOperatorCRDs(ctx context.Context, client managementRunnerClient, expected []operatorCRDExpectation, journal *executionJournal, interval time.Duration) error {
	event := executionEvent{Stage: generation.StageOperator, Operation: "crd-readiness", State: "attempted", ReasonCode: "DPF_CRD_READINESS_STARTED", Message: "Wait for retained DPF CRDs to be established and discoverable."}
	if err := journal.record(event); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return journalFailure(journal, event, false, "DPF_CRD_READINESS_CANCELED", "DPF API readiness was not established before cancellation.", err)
		}
		pending, failure := false, ""
		for _, crd := range expected {
			desired := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": crd.name}}}
			current, err := client.Get(ctx, desired)
			if err != nil {
				if apierrors.IsNotFound(err) || retryableRead(err) {
					pending = true
					break
				}
				failure = "a required DPF API definition could not be read"
				break
			}
			if !crdIdentityMatches(crd, current) {
				failure = "a required DPF API definition has a conflicting identity or scope"
				break
			}
			for _, version := range crd.versions {
				requirement := preflightAPIRequirement{crd: crd.name, group: crd.group, version: version, resource: crd.plural, kind: crd.kind, namespaced: crd.namespaced}
				if !externalCRDReady(current, requirement) {
					pending = true
					break
				}
				resources, err := client.discoverResourceList(ctx, crd.group+"/"+version)
				if apierrors.IsNotFound(err) {
					pending = true
					break
				}
				if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					return journalFailure(journal, event, false, "DPF_CRD_READINESS_CANCELED", "DPF API discovery did not complete before cancellation.", err)
				}
				if err != nil || !discoveredResourceMatches(resources, requirement) {
					failure = fmt.Sprintf("the retained DPF API %s/%s resource %s could not be verified through discovery", crd.group, version, crd.plural)
					break
				}
			}
			if failure != "" || pending {
				break
			}
		}
		if failure != "" {
			return journalFailure(journal, event, false, "DPF_CRD_READINESS_FAILED", failure, nil)
		}
		if !pending {
			event.State, event.ReasonCode = "ready", "DPF_CRDS_ESTABLISHED"
			event.Message = "Retained DPF CRDs report Established and NamesAccepted, and each served resource is discoverable."
			return journal.record(event)
		}
		event.State, event.ReasonCode = "waiting", "DPF_CRDS_NOT_READY"
		event.Message = "One or more retained DPF CRDs are not yet established and discoverable."
		if err := journal.record(event); err != nil {
			return err
		}
		if err := waitInterval(ctx, interval); err != nil {
			return journalFailure(journal, event, false, "DPF_CRD_READINESS_TIMEOUT", "DPF API readiness was not established before the execution deadline.", err)
		}
	}
}

func crdIdentityMatches(expected operatorCRDExpectation, current *unstructured.Unstructured) bool {
	if current == nil || current.GetAPIVersion() != "apiextensions.k8s.io/v1" || current.GetKind() != "CustomResourceDefinition" || current.GetName() != expected.name || current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
		return false
	}
	group, _, _ := unstructured.NestedString(current.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(current.Object, "spec", "names", "plural")
	kind, _, _ := unstructured.NestedString(current.Object, "spec", "names", "kind")
	scope, _, _ := unstructured.NestedString(current.Object, "spec", "scope")
	wantScope := "Cluster"
	if expected.namespaced {
		wantScope = "Namespaced"
	}
	return group == expected.group && plural == expected.plural && kind == expected.kind && scope == wantScope && crdSpecMatches(expected.spec, current)
}

func crdSpecMatches(expected map[string]any, current *unstructured.Unstructured) bool {
	if len(expected) == 0 || current == nil {
		return false
	}
	data, err := json.Marshal(map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "spec": expected})
	if err != nil {
		return false
	}
	var expectedCRD apiextensionsv1.CustomResourceDefinition
	if err := json.Unmarshal(data, &expectedCRD); err != nil {
		return false
	}
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&expectedCRD)
	currentData, err := json.Marshal(current.Object)
	if err != nil {
		return false
	}
	var currentCRD apiextensionsv1.CustomResourceDefinition
	if err := json.Unmarshal(currentData, &currentCRD); err != nil {
		return false
	}
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&currentCRD)
	want, wantErr := json.Marshal(expectedCRD.Spec)
	got, gotErr := json.Marshal(currentCRD.Spec)
	return wantErr == nil && gotErr == nil && bytes.Equal(want, got)
}

func discoveredResourceMatches(resources *metav1.APIResourceList, requirement preflightAPIRequirement) bool {
	if resources == nil || resources.GroupVersion != requirement.group+"/"+requirement.version {
		return false
	}
	count := 0
	for _, resource := range resources.APIResources {
		if resource.Name == requirement.resource && resource.Kind == requirement.kind && resource.Namespaced == requirement.namespaced {
			count++
		}
	}
	return count == 1
}

func waitManagerDeployment(ctx context.Context, client managementRunnerClient, desired *unstructured.Unstructured, journal *executionJournal, interval time.Duration) (*unstructured.Unstructured, error) {
	event := executionEvent{Stage: generation.StageOperator, Operation: "manager-readiness", Subject: desired.GetNamespace() + "/" + desired.GetName(), State: "attempted", ReasonCode: "DPF_MANAGER_READINESS_STARTED", Message: "Wait for the retained manager Deployment's current generation and replicas."}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	var uid string
	for {
		if err := ctx.Err(); err != nil {
			return nil, journalFailure(journal, event, false, "DPF_MANAGER_READINESS_CANCELED", "The DPF manager Deployment did not become ready before cancellation.", err)
		}
		current, err := client.Get(ctx, desired.DeepCopy())
		if err != nil {
			if apierrors.IsNotFound(err) || retryableRead(err) {
				event.State, event.ReasonCode = "waiting", "DPF_MANAGER_NOT_OBSERVED"
				event.Message = "The projected manager Deployment is not yet observable."
			} else {
				return nil, journalFailure(journal, event, false, "DPF_MANAGER_READ_FAILED", "The projected manager Deployment could not be read.", err)
			}
		} else {
			if current == nil || current.GetAPIVersion() != desired.GetAPIVersion() || current.GetKind() != desired.GetKind() || current.GetName() != desired.GetName() || current.GetNamespace() != desired.GetNamespace() || current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
				return nil, journalFailure(journal, event, false, "DPF_MANAGER_IDENTITY_INVALID", "The projected manager Deployment lacks a stable matching identity.", nil)
			}
			if uid != "" && string(current.GetUID()) != uid {
				return nil, journalFailure(journal, event, false, "DPF_MANAGER_IDENTITY_CHANGED", "The manager Deployment was replaced while waiting for readiness.", nil)
			}
			if uid == "" {
				uid = string(current.GetUID())
			}
			if !managerDeploymentIntentMatches(desired, current) {
				return nil, journalFailure(journal, event, false, "DPF_MANAGER_INTENT_CHANGED", "The live manager specification differs from the retained operator chart after API default normalization.", nil)
			}
			readiness := EvaluateReadiness(current)
			switch readiness.ReasonCode {
			case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_STATUS_INVALID", "RESOURCE_API_UNSUPPORTED", "RESOURCE_KIND_UNSUPPORTED", "DEPLOYMENT_ROLLOUT_FAILED":
				return nil, journalFailure(journal, event, false, readiness.ReasonCode, readiness.Message, nil)
			}
			if readiness.Ready {
				event.State, event.ReasonCode, event.Message = "ready", readiness.ReasonCode, readiness.Message
				if err := journal.record(event); err != nil {
					return nil, err
				}
				return current.DeepCopy(), nil
			}
			event.State, event.ReasonCode, event.Message = "waiting", readiness.ReasonCode, readiness.Message
		}
		if err := journal.record(event); err != nil {
			return nil, err
		}
		if err := waitInterval(ctx, interval); err != nil {
			return nil, journalFailure(journal, event, false, "DPF_MANAGER_READINESS_TIMEOUT", "The DPF manager Deployment was not ready before the execution deadline.", err)
		}
	}
}

func managerDeploymentIntentMatches(desired, current *unstructured.Unstructured) bool {
	want, wantErr := normalizedManagerSpec(desired)
	got, gotErr := normalizedManagerSpec(current)
	return wantErr == nil && gotErr == nil && jsonEqual(want, got)
}

func containerImages(object *unstructured.Unstructured) (map[string]string, error) {
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return nil, fmt.Errorf("deployment containers are missing or malformed")
	}
	images := make(map[string]string, len(containers))
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("deployment container is malformed")
		}
		name, _ := container["name"].(string)
		image, _ := container["image"].(string)
		if name == "" || image == "" || images[name] != "" {
			return nil, fmt.Errorf("deployment container identity or image is invalid")
		}
		images[name] = image
	}
	return images, nil
}

func requireNamespaceUID(ctx context.Context, client managementRunnerClient, name, uid string) error {
	desired := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}}
	current, err := client.Get(ctx, desired)
	if err != nil {
		return safeAPIError("recheck operator namespace identity", err)
	}
	if err := verifyNamespace(name, current); err != nil || string(current.GetUID()) != uid {
		return fmt.Errorf("operator namespace was replaced or is no longer active")
	}
	return nil
}

func waitInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func journalFailure(journal *executionJournal, event executionEvent, uncertain bool, code, message string, cause error) error {
	event.State = "failed"
	if uncertain {
		event.State = "uncertain"
	}
	event.ReasonCode, event.Message = code, message
	recordErr := journal.record(event)
	if cause == nil {
		return errors.Join(recordErr, fmt.Errorf("%s: %s", code, message))
	}
	return errors.Join(recordErr, reconcileError(code, message, safeAPIError("management stage", cause)))
}

func deadline(ctx context.Context) time.Time {
	value, _ := ctx.Deadline()
	return value
}
