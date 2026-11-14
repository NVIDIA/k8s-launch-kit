// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"k8s.io/apimachinery/pkg/util/version"
)

type managementCheck struct {
	id, subject, state, reason, message string
}

type managementCRDContract struct {
	name, group, plural, kind string
	fields                    []string
}

var hbnManagementCRDs = []managementCRDContract{
	{name: "bluefieldsoftwares.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "bluefieldsoftwares", kind: "BlueFieldSoftware", fields: []string{"spec.osIso", "spec.pldmFwBundle"}},
	{name: "dpudeployments.svc.dpu.nvidia.com", group: "svc.dpu.nvidia.com", plural: "dpudeployments", kind: "DPUDeployment", fields: []string{"spec.dpus.bfb", "spec.dpus.blueFieldSoftware", "spec.dpus.dpuSets[].nameSuffix", "spec.dpus.dpuSetStrategy", "spec.dpus.nodeEffect"}},
	{name: "dpuflavors.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpuflavors", kind: "DPUFlavor", fields: []string{"spec.configFiles"}},
	{name: "dpfoperatorconfigs.operator.dpu.nvidia.com", group: "operator.dpu.nvidia.com", plural: "dpfoperatorconfigs", kind: "DPFOperatorConfig", fields: []string{"spec.deploymentMode", "status.conditions"}},
	{name: "dpuclusters.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpuclusters", kind: "DPUCluster", fields: []string{"spec.type", "spec.clusterEndpoint.keepalived.vip", "spec.clusterEndpoint.keepalived.interface", "spec.clusterEndpoint.keepalived.virtualRouterID", "status.phase", "status.nodesCount"}},
	{name: "dpudevices.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpudevices", kind: "DPUDevice", fields: []string{"spec.serialNumber", "status.serialNumber", "status.psid"}},
	{name: "dpus.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpus", kind: "DPU", fields: []string{"spec.serialNumber", "spec.dpuDeviceName", "status.phase"}},
}

// managementHBNChecks evaluates management-cluster provisioning facts for HBN.
// It is pure: it reads the snapshot and inventory without changing either.
func managementHBNChecks(snapshot *managementprobe.Snapshot, inv *inventory.Inventory, namespace string) ([]inventory.Check, *inventory.HBNManagement) {
	projection := projectHBNManagement(snapshot, inv, namespace)
	checks := make([]inventory.Check, 0, 10)
	add := func(c managementCheck) {
		checks = append(checks, inventory.Check{ID: "dpf.hbn.management." + c.id, Subject: c.subject, Vantage: "executor", RequiredPhase: "deploy", State: c.state, ReasonCode: c.reason, Message: c.message, EvidenceRefs: []string{}})
	}
	if snapshot == nil {
		for _, c := range []managementCheck{
			{"source-completeness", namespace, "unknown", "MANAGEMENT_OBSERVATION_UNAVAILABLE", "Management-cluster discovery did not return a snapshot."},
			{"operator", namespace, "unknown", "DPF_OPERATOR_UNOBSERVED", "DPF operator state was not observed."},
			{"operator-config", namespace, "unknown", "DPF_OPERATOR_CONFIG_UNOBSERVED", "DPFOperatorConfig state was not observed."},
			{"apis", namespace, "unknown", "DPF_APIS_UNOBSERVED", "DPF API schemas were not observed."},
			{"enrollments", namespace, "unknown", "DPU_ENROLLMENTS_UNOBSERVED", "Existing DPU ownership was not observed."},
			{"rbac", namespace, "unknown", "DPF_RBAC_UNMEASURED", "The permissions required by the DPF operator and deployment workflow have not been verified."},
			{"storage-health", namespace, "unknown", "STORAGE_HEALTH_UNMEASURED", "StorageClass presence does not establish provisioner or volume health."},
		} {
			add(c)
		}
		add(kubernetesRangeCheck(inv))
		add(controlPlaneCheck(inv))
		return checks, projection
	}
	if snapshot.Namespace != namespace {
		projection = &inventory.HBNManagement{Namespace: namespace, OperatorConfigs: []inventory.HBNManagementConfig{}, TenantClusters: []inventory.HBNManagementCluster{}, Enrollments: []inventory.HBNManagementEnrollment{}, Issues: []inventory.HBNManagementIssue{{Source: "collector", Code: "MANAGEMENT_NAMESPACE_MISMATCH", Message: "The management snapshot was collected from a different namespace."}}}
		add(managementCheck{"source-completeness", namespace, "failed", "MANAGEMENT_NAMESPACE_MISMATCH", "The management snapshot namespace does not match the requested DPF namespace."})
		for _, id := range []string{"kubernetes-version", "control-plane", "apis", "operator", "operator-config", "enrollments"} {
			add(managementCheck{id, namespace, "unknown", "MANAGEMENT_NAMESPACE_MISMATCH", "The management observation was scoped to a different namespace."})
		}
		add(managementCheck{"rbac", namespace, "unknown", "DPF_RBAC_UNMEASURED", "The permissions required by the DPF operator and deployment workflow have not been verified."})
		add(managementCheck{"storage-health", namespace, "unknown", "STORAGE_HEALTH_UNMEASURED", "StorageClass presence does not establish provisioner or volume health."})
		return checks, projection
	}

	add(sourceCompletenessCheck(snapshot))
	add(kubernetesRangeCheck(inv))
	add(controlPlaneCheck(inv))
	add(apiCapabilityCheck(snapshot, inv))
	add(operatorCheck(snapshot, inv, namespace))
	add(operatorConfigCheck(snapshot, inv, namespace))
	add(enrollmentCheck(snapshot, inv))
	add(managementCheck{"rbac", namespace, "unknown", "DPF_RBAC_UNMEASURED", "The permissions required by the DPF operator and deployment workflow have not been verified."})
	add(tenantStorageCheck(snapshot, namespace))
	return checks, projection
}

func tenantStorageCheck(snapshot *managementprobe.Snapshot, namespace string) managementCheck {
	check := managementCheck{"storage-health", namespace, "unknown", "STORAGE_HEALTH_UNMEASURED", "Kamaji datastore and backing storage health were not observed."}
	if snapshot == nil || snapshot.TenantStorage == nil {
		return check
	}
	storage := snapshot.TenantStorage
	if storage.State != "passed" && storage.State != "failed" && storage.State != "unknown" || storage.ReasonCode == "" || storage.Message == "" {
		check.reason, check.message = "STORAGE_RESULT_INVALID", "The storage observation is incomplete or has an unsupported result."
		return check
	}
	check.state, check.reason, check.message = storage.State, storage.ReasonCode, storage.Message
	return check
}

func projectHBNManagement(snapshot *managementprobe.Snapshot, inv *inventory.Inventory, namespace string) *inventory.HBNManagement {
	if snapshot == nil {
		return &inventory.HBNManagement{Namespace: namespace, OperatorConfigs: []inventory.HBNManagementConfig{}, TenantClusters: []inventory.HBNManagementCluster{}, Enrollments: []inventory.HBNManagementEnrollment{}, Issues: []inventory.HBNManagementIssue{}}
	}
	result := &inventory.HBNManagement{Namespace: namespace, OperatorConfigs: []inventory.HBNManagementConfig{}, TenantClusters: []inventory.HBNManagementCluster{}, Enrollments: []inventory.HBNManagementEnrollment{}, Issues: []inventory.HBNManagementIssue{}}
	selectedImage := ""
	if inv != nil && inv.Software != nil {
		selectedImage = inv.Software.Operator.Image
	}
	var operatorDeployments []managementprobe.Deployment
	for _, item := range snapshot.Deployments.Items {
		if item.Name == "dpf-operator-controller-manager" || imageRepositoryMatch(item.Images, selectedImage) {
			operatorDeployments = append(operatorDeployments, item)
		}
	}
	if len(operatorDeployments) == 1 {
		item := operatorDeployments[0]
		image := selectedOperatorImage(item.Images, selectedImage)
		result.Operator = &inventory.HBNManagementOperator{Name: item.Name, UID: item.UID, Image: image, Version: imageVersion(image), Ready: deploymentReady(item), ReadyReplicas: item.ReadyReplicas, DesiredReplicas: item.Replicas}
	} else if len(operatorDeployments) > 1 {
		result.Issues = append(result.Issues, inventory.HBNManagementIssue{Source: "deployments", Code: "DPF_OPERATOR_AMBIGUOUS", Message: "Multiple deployments match the selected DPF operator identity."})
	}
	for _, item := range snapshot.OperatorConfigs.Items {
		projected := inventory.HBNManagementConfig{Name: item.Name, UID: item.UID, DeploymentMode: deref(item.DeploymentMode), Owners: ownerSummaries(item.Owners)}
		projected.Ready = readyCondition(item.Generation, item.Conditions)
		if item.StatusFreshness == "stale" {
			projected.Ready = nil
		}
		result.OperatorConfigs = append(result.OperatorConfigs, projected)
	}
	// Tenant clusters are projected by hbnBootstrapChecks after it resolves the
	// exact target and evaluates the complete cross-namespace list. Copying all
	// tenants here would expose unrelated namespace, endpoint, and ownership data.
	result.Enrollments = projectEnrollments(snapshot, selectedSerials(inv))
	for _, issue := range snapshot.Issues {
		if issue.Namespace != "" && !projectedIssueObject(result, issue) {
			continue
		}
		result.Issues = append(result.Issues, inventory.HBNManagementIssue{Source: issue.Source, Resource: issue.Resource, Namespace: issue.Namespace, Field: issue.Field, Code: issue.Code, Message: issue.Message})
	}
	return result
}

func projectedIssueObject(projection *inventory.HBNManagement, issue managementprobe.Issue) bool {
	if projection == nil {
		return false
	}
	switch issue.Source {
	case "dpfoperatorconfigs.operator.dpu.nvidia.com":
		for _, item := range projection.OperatorConfigs {
			if issue.Namespace == projection.Namespace && issue.Resource == item.Name {
				return true
			}
		}
	case "dpudevices.provisioning.dpu.nvidia.com":
		return projectedEnrollment(projection.Enrollments, issue, "DPUDevice")
	case "dpus.provisioning.dpu.nvidia.com":
		return projectedEnrollment(projection.Enrollments, issue, "DPU")
	}
	return false
}

func projectedEnrollment(enrollments []inventory.HBNManagementEnrollment, issue managementprobe.Issue, kind string) bool {
	for _, item := range enrollments {
		if item.Kind == kind && item.Namespace == issue.Namespace && item.Name == issue.Resource {
			return true
		}
	}
	return false
}

func sourceCompletenessCheck(s *managementprobe.Snapshot) managementCheck {
	states := []struct {
		name  string
		state managementprobe.CollectionState
	}{
		{"deployments", s.Deployments.State}, {"operator configs", s.OperatorConfigs.State}, {"DPU clusters", s.DPUClusters.State},
		{"DPU devices", s.DPUDevices.State}, {"DPUs", s.DPUs.State}, {"StorageClasses", s.StorageClasses.State},
	}
	for _, source := range states {
		if source.state == managementprobe.CollectionIncomplete {
			return managementCheck{"source-completeness", source.name, "failed", "MANAGEMENT_SOURCE_INCOMPLETE", "The bounded management observation is incomplete; resolve the source issue before generation."}
		}
		if source.state == managementprobe.CollectionUnavailable {
			return managementCheck{"source-completeness", source.name, "unknown", "MANAGEMENT_SOURCE_UNAVAILABLE", "The management API source could not be observed."}
		}
	}
	for _, crd := range s.CRDs {
		if crd.State == "unavailable" {
			return managementCheck{"source-completeness", crd.Name, "unknown", "MANAGEMENT_SOURCE_UNAVAILABLE", "The CRD API source could not be observed."}
		}
	}
	return managementCheck{"source-completeness", s.Namespace, "passed", "MANAGEMENT_SOURCES_OBSERVED", "Management observations completed within their bounded coverage."}
}

func kubernetesRangeCheck(inv *inventory.Inventory) managementCheck {
	check := managementCheck{"kubernetes-version", "management cluster", "unknown", "KUBERNETES_VERSION_RANGE_UNKNOWN", "The selected release or observed Kubernetes version range is unknown."}
	if inv == nil || inv.Software == nil || inv.Management == nil || inv.Management.KubernetesVersion == "" || inv.Software.SupportedKubernetes == nil {
		return check
	}
	observed, err := version.ParseSemantic(inv.Management.KubernetesVersion)
	if err != nil {
		return check
	}
	if strings.Contains(observed.String(), "-") {
		check.state, check.reason, check.message = "failed", "KUBERNETES_PRERELEASE_UNSUPPORTED", "The selected release catalog does not declare support for a pre-release Kubernetes version."
		return check
	}
	minimum, minErr := version.ParseGeneric(inv.Software.SupportedKubernetes.Minimum)
	maximum, maxErr := version.ParseGeneric(inv.Software.SupportedKubernetes.Maximum)
	if minErr != nil || maxErr != nil {
		return check
	}
	observedMinor, _ := version.ParseGeneric(fmt.Sprintf("%d.%d", observed.Major(), observed.Minor()))
	minimumMinor, _ := version.ParseGeneric(fmt.Sprintf("%d.%d", minimum.Major(), minimum.Minor()))
	maximumMinor, _ := version.ParseGeneric(fmt.Sprintf("%d.%d", maximum.Major(), maximum.Minor()))
	if observedMinor.LessThan(minimumMinor) || observedMinor.GreaterThan(maximumMinor) {
		check.state, check.reason = "failed", "KUBERNETES_VERSION_UNSUPPORTED"
		check.message = fmt.Sprintf("Observed Kubernetes %s is outside selected release range %s through %s.", inv.Management.KubernetesVersion, inv.Software.SupportedKubernetes.Minimum, inv.Software.SupportedKubernetes.Maximum)
		return check
	}
	check.state, check.reason, check.message = "passed", "KUBERNETES_VERSION_SUPPORTED", "Observed Kubernetes version is within the selected release range."
	return check
}

func controlPlaneCheck(inv *inventory.Inventory) managementCheck {
	check := managementCheck{"control-plane", "management cluster", "unknown", "CONTROL_PLANE_READINESS_UNKNOWN", "Control-plane node readiness was not observed."}
	if inv == nil || inv.Management == nil {
		return check
	}
	count := 0
	for _, node := range inv.Management.Nodes {
		if !node.ControlPlane {
			continue
		}
		count++
		if !node.Ready {
			check.state, check.reason, check.message = "failed", "CONTROL_PLANE_NODE_NOT_READY", "At least one observed control-plane node is not Ready."
			return check
		}
	}
	if count == 0 {
		return check
	}
	check.state, check.reason, check.message = "passed", "CONTROL_PLANE_NODES_READY", "All observed control-plane nodes are Ready."
	return check
}

func apiCapabilityCheck(s *managementprobe.Snapshot, inv *inventory.Inventory) managementCheck {
	check := managementCheck{"apis", s.Namespace, "unknown", "DPF_API_CAPABILITIES_UNKNOWN", "Required DPF CRD schemas could not be verified."}
	byName := make(map[string]managementprobe.CRD, len(s.CRDs))
	for _, crd := range s.CRDs {
		byName[crd.Name] = crd
	}
	allAbsent := true
	for _, contract := range hbnManagementCRDs {
		crd, found := byName[contract.name]
		if !found || crd.State == "unavailable" {
			check.subject = contract.name
			check.message = fmt.Sprintf("Required CRD %s could not be read; verify API access and retry discovery.", contract.name)
			return check
		}
		allAbsent = allAbsent && crd.State == "absent"
	}
	if allAbsent {
		if selectedChartDigest(inv) {
			check.state, check.reason, check.message = "passed", "DPF_APIS_PLANNED_FOR_INSTALL", "Required DPF APIs are wholly absent and can be installed from the selected catalog chart with a recorded digest."
		} else {
			check.reason, check.message = "SELECTED_CHART_DIGEST_REQUIRED", "Fresh API installation is available only when the selected catalog chart has an immutable digest."
		}
		return check
	}
	for _, contract := range hbnManagementCRDs {
		crd := byName[contract.name]
		check.subject = contract.name
		if crd.State == "absent" {
			check.state, check.reason, check.message = "failed", "DPF_API_SET_PARTIAL", "Some DPF CRDs exist while required CRDs are absent; resolve partial installation or cleanup before generation."
			return check
		}
		if crd.State != "observed" || crd.Group != contract.group || crd.Plural != contract.plural || crd.Kind != contract.kind || crd.Scope != "Namespaced" {
			check.state, check.reason, check.message = "failed", "DPF_API_IDENTITY_MISMATCH", "An existing DPF CRD has an unexpected identity or scope and cannot be safely adopted."
			return check
		}
		if crd.StatusFreshness == "stale" || !crdConditionTrue(crd, "Established") || !crdConditionTrue(crd, "NamesAccepted") || !served(crd, "v1alpha1") {
			check.state, check.reason, check.message = "failed", "DPF_API_NOT_ESTABLISHED", "An existing DPF CRD lacks established names, a served required version, or current observed status."
			return check
		}
		for _, required := range contract.fields {
			present := false
			for _, field := range crd.Fields {
				if field.Path == required && slices.Contains(field.PresentIn, "v1alpha1") {
					present = true
					break
				}
			}
			if !present {
				check.state, check.reason, check.message = "failed", "DPF_API_SCHEMA_MISMATCH", fmt.Sprintf("CRD %s is missing required v1alpha1 field %s; install the selected release schema before deployment.", contract.name, required)
				return check
			}
		}
	}
	check.subject = s.Namespace
	check.state, check.reason, check.message = "passed", "DPF_APIS_COMPATIBLE", "Required DPF CRD identities, served versions and schema fields are present."
	return check
}

func operatorCheck(s *managementprobe.Snapshot, inv *inventory.Inventory, namespace string) managementCheck {
	check := managementCheck{"operator", namespace, "unknown", "DPF_OPERATOR_UNOBSERVED", "The selected DPF operator deployment could not be identified or its readiness is unknown."}
	if s.Deployments.State == managementprobe.CollectionUnavailable || s.Deployments.State == managementprobe.CollectionIncomplete {
		return check
	}
	expectedImage := ""
	if inv != nil && inv.Software != nil {
		expectedImage = inv.Software.Operator.Image
	}
	var matches []managementprobe.Deployment
	for _, deployment := range s.Deployments.Items {
		if deployment.Name == "dpf-operator-controller-manager" || imageRepositoryMatch(deployment.Images, expectedImage) {
			matches = append(matches, deployment)
		}
	}
	if len(matches) > 1 {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_AMBIGUOUS", "Multiple deployments match the selected DPF operator identity."
		return check
	}
	if len(matches) == 0 {
		if freshUninstalled(s, inv) && selectedChartDigest(inv) {
			check.state, check.reason, check.message = "passed", "DPF_OPERATOR_PLANNED_FOR_INSTALL", "No existing DPF operator or DPF state was found; the selected immutable chart can install it."
		} else if freshUninstalled(s, inv) {
			check.reason, check.message = "SELECTED_CHART_DIGEST_REQUIRED", "Fresh DPF installation is available only when the selected catalog chart has an immutable digest."
		} else if s.Deployments.State == managementprobe.CollectionComplete {
			check.state, check.reason, check.message = "failed", "DPF_OPERATOR_ADOPTION_UNRESOLVED", "Existing DPF APIs or resources remain without an identifiable operator; reconcile ownership before deployment."
		}
		return check
	}
	deployment := matches[0]
	image := selectedOperatorImage(deployment.Images, expectedImage)
	actualVersion := imageVersion(image)
	if expectedImage == "" || imageRepository(expectedImage) == "" {
		return check
	}
	if imageRepository(image) != imageRepository(expectedImage) || actualVersion != inv.Software.Operator.Version {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_UPGRADE_REQUIRED", "The existing DPF operator image does not match the selected release; upgrade it before HBN deployment."
		return check
	}
	if deployment.StatusFreshness == "stale" {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_STATUS_STALE", "The DPF operator Deployment status does not reflect its current generation."
		return check
	}
	if deployment.StatusFreshness != "current" || deployment.Replicas == nil {
		return check
	}
	available, hasAvailable := conditionValue(deployment.Conditions, "Available")
	if !hasAvailable {
		return check
	}
	if *deployment.Replicas <= 0 || !available || deployment.ReadyReplicas < *deployment.Replicas || deployment.AvailableReplicas < *deployment.Replicas {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_NOT_READY", "The selected DPF operator Deployment is not Available at its desired replica count."
		return check
	}
	check.state, check.reason, check.message = "passed", "DPF_OPERATOR_READY", "The selected DPF operator image is current and its Deployment is Available at the desired replica count."
	return check
}

func operatorConfigCheck(s *managementprobe.Snapshot, inv *inventory.Inventory, namespace string) managementCheck {
	check := managementCheck{"operator-config", namespace, "unknown", "DPF_OPERATOR_CONFIG_UNKNOWN", "DPFOperatorConfig mode or Ready status could not be verified."}
	if s.OperatorConfigs.State == managementprobe.CollectionUnavailable || s.OperatorConfigs.State == managementprobe.CollectionIncomplete {
		return check
	}
	if len(s.OperatorConfigs.Items) == 0 {
		if freshUninstalled(s, inv) && selectedChartDigest(inv) {
			check.state, check.reason, check.message = "passed", "DPF_OPERATOR_CONFIG_PLANNED", "No existing DPFOperatorConfig was found in the fresh namespace; the selected chart can establish the singleton."
		} else if freshUninstalled(s, inv) {
			check.reason, check.message = "SELECTED_CHART_DIGEST_REQUIRED", "Fresh DPFOperatorConfig installation is available only when the selected catalog chart has an immutable digest."
		} else {
			check.state, check.reason, check.message = "failed", "DPF_OPERATOR_CONFIG_MISSING", "Existing DPF installation state has no DPFOperatorConfig; resolve manager ownership before deployment."
		}
		return check
	}
	if len(s.OperatorConfigs.Items) != 1 {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_CONFIG_NOT_SINGLETON", "The namespace contains multiple DPFOperatorConfig objects; resolve the singleton conflict before deployment."
		return check
	}
	config := s.OperatorConfigs.Items[0]
	if config.StatusFreshness == "stale" {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_CONFIG_STATUS_STALE", "DPFOperatorConfig status does not reflect its current generation."
		return check
	}
	if !zeroTrust(config.DeploymentMode) {
		if config.DeploymentMode == nil {
			return check
		}
		check.state, check.reason, check.message = "failed", "DPF_DEPLOYMENT_MODE_MISMATCH", "Existing DPFOperatorConfig uses a deployment mode other than ZeroTrust."
		return check
	}
	ready := currentReadyCondition(config.Generation, config.Conditions)
	if ready == nil {
		return check
	}
	if !*ready || config.StatusFreshness != "current" {
		check.state, check.reason, check.message = "failed", "DPF_OPERATOR_CONFIG_NOT_READY", "DPFOperatorConfig is not Ready for its current generation."
		return check
	}
	check.state, check.reason, check.message = "passed", "DPF_ZERO_TRUST_CONFIG_READY", "The singleton DPFOperatorConfig selects ZeroTrust and is Ready for its current generation."
	return check
}

func freshUninstalled(s *managementprobe.Snapshot, inv *inventory.Inventory) bool {
	if s.Deployments.State != managementprobe.CollectionComplete || len(s.OperatorConfigs.Items) != 0 || len(s.DPUClusters.Items) != 0 || len(s.DPUDevices.Items) != 0 || len(s.DPUs.Items) != 0 ||
		s.OperatorConfigs.State != managementprobe.CollectionAbsent || s.DPUClusters.State != managementprobe.CollectionAbsent || s.DPUDevices.State != managementprobe.CollectionAbsent || s.DPUs.State != managementprobe.CollectionAbsent {
		return false
	}
	selectedImage := ""
	if inv != nil && inv.Software != nil {
		selectedImage = inv.Software.Operator.Image
	}
	for _, deployment := range s.Deployments.Items {
		if deployment.Name == "dpf-operator-controller-manager" || imageRepositoryMatch(deployment.Images, selectedImage) {
			return false
		}
	}
	for _, crd := range s.CRDs {
		if crd.State != "absent" {
			return false
		}
	}
	return len(s.CRDs) == len(hbnManagementCRDs)
}

func selectedChartDigest(inv *inventory.Inventory) bool {
	if inv == nil || inv.Software == nil || inv.Software.Operator.Chart != "dpf-operator" || !strings.HasPrefix(inv.Software.Operator.ChartDigest, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(inv.Software.Operator.ChartDigest, "sha256:")
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32 && digest == strings.ToLower(digest)
}

func crdConditionTrue(crd managementprobe.CRD, conditionType string) bool {
	for _, condition := range crd.Conditions {
		if condition.Type == conditionType {
			return condition.Status == "True"
		}
	}
	return false
}

func served(crd managementprobe.CRD, versionName string) bool {
	for _, version := range crd.Versions {
		if version.Name == versionName && version.Served {
			return true
		}
	}
	return false
}

func zeroTrust(mode *string) bool {
	return mode != nil && *mode == "zero-trust"
}

func readyCondition(generation int64, conditions []managementprobe.Condition) *bool {
	for _, condition := range conditions {
		if condition.Type != "Ready" {
			continue
		}
		if condition.ObservedGeneration == nil || *condition.ObservedGeneration < generation {
			return nil
		}
		ready := condition.Status == "True"
		return &ready
	}
	return nil
}

func currentReadyCondition(generation int64, conditions []managementprobe.Condition) *bool {
	return readyCondition(generation, conditions)
}

func conditionValue(conditions []managementprobe.Condition, kind string) (bool, bool) {
	for _, condition := range conditions {
		if condition.Type == kind {
			return condition.Status == "True", true
		}
	}
	return false, false
}

func deploymentReady(deployment managementprobe.Deployment) *bool {
	if deployment.StatusFreshness != "current" || deployment.Replicas == nil {
		return nil
	}
	available, found := conditionValue(deployment.Conditions, "Available")
	if !found {
		return nil
	}
	ready := *deployment.Replicas > 0 && available && deployment.ReadyReplicas >= *deployment.Replicas && deployment.AvailableReplicas >= *deployment.Replicas
	return &ready
}

func ownerSummaries(owners []managementprobe.OwnerReference) []string {
	result := make([]string, 0, len(owners))
	for _, owner := range owners {
		result = append(result, owner.Kind+"/"+owner.Name)
	}
	slices.Sort(result)
	return result
}

func selectedOperatorImage(images []string, selected string) string {
	selectedRepo := imageRepository(selected)
	for _, image := range images {
		if selectedRepo != "" && imageRepository(image) == selectedRepo {
			return image
		}
	}
	if len(images) == 1 {
		return images[0]
	}
	return ""
}

func imageRepositoryMatch(images []string, selected string) bool {
	repository := imageRepository(selected)
	if repository == "" {
		return false
	}
	for _, image := range images {
		if imageRepository(image) == repository {
			return true
		}
	}
	return false
}

func imageRepository(image string) string {
	image = strings.SplitN(image, "@", 2)[0]
	lastSlash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > lastSlash {
		image = image[:colon]
	}
	return image
}

func imageVersion(image string) string {
	image = strings.SplitN(image, "@", 2)[0]
	lastSlash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > lastSlash {
		return image[colon+1:]
	}
	return ""
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
