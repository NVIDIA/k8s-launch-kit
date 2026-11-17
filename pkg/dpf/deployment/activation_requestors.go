// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

type activationVersionGraph struct {
	requestors []string
	identities map[string]types.UID
}

const (
	activationParticipantList              = "ACTIVATION_PARTICIPANT_LIST_UNAVAILABLE"
	activationParticipantClusterLabels     = "ACTIVATION_DPU_CLUSTER_LABELS_CHANGED"
	activationParticipantGraph             = "ACTIVATION_PARTICIPANT_GRAPH_INVALID"
	activationParticipantOwner             = "ACTIVATION_PARTICIPANT_OWNER_CHANGED"
	activationParticipantSelector          = "ACTIVATION_PARTICIPANT_SELECTOR_CHANGED"
	activationParticipantServiceSpec       = "ACTIVATION_PARTICIPANT_SERVICE_SPEC_CHANGED"
	activationParticipantChainSpec         = "ACTIVATION_PARTICIPANT_CHAIN_SPEC_CHANGED"
	activationParticipantUnsupportedPolicy = "ACTIVATION_LABEL_CHANGE_POLICY_UNSUPPORTED"
	activationParticipantPolicy            = "ACTIVATION_PARTICIPANT_INLINE_POLICY_CHANGED"
	activationParticipantRequestors        = "ACTIVATION_MAINTENANCE_REQUESTORS_CHANGED"
	activationParticipantMaintenance       = "ACTIVATION_MAINTENANCE_IDENTITY_CHANGED"
)

// Both construction and public formatting use this fixed lookup. Error Message,
// wrapped text, unknown codes and remote field values never enter the journal.
func activationCoordinationText(code string) string {
	switch code {
	case activationParticipantClusterLabels:
		return "Selected DPU cluster labels must equal the retained controller labels plus the exact selected node, namespace and pinned DPF release."
	case activationParticipantList:
		return "A required complete controller Service or Chain list could not be read; verify management API access and list permissions."
	case activationParticipantGraph:
		return "The current controller participant graph cannot be bound to the retained service configuration."
	case activationParticipantOwner:
		return "A selected controller or child identity, owner, or retained device/software binding changed."
	case activationParticipantSelector:
		return "A controller version or tenant selector differs from the selected deployment cohort."
	case activationParticipantServiceSpec:
		return "The current service body differs from its retained template and configuration."
	case activationParticipantChainSpec:
		return "The current chain body differs from the retained switches and qualified service reference."
	case activationParticipantUnsupportedPolicy:
		return "The native immutable-plan path requires applyOnLabelChange=false and does not authorize label-triggered node effects or controller upgrade policy."
	case activationParticipantPolicy:
		return "The inline node-effect action or additional participant set differs between the selected deployment children."
	case activationParticipantRequestors:
		return "The maintenance participant set is malformed, duplicated, foreign, or incomplete for its release phase."
	case activationParticipantMaintenance:
		return "The selected maintenance UID or released hold changed without a new authorized operation."
	case "ACTIVATION_REBOOT_SCOPE_CHANGED":
		return "A released selected child identity changed before further host operations."
	default:
		return ""
	}
}

func activationCoordinationError(code string) error {
	text := activationCoordinationText(code)
	if text == "" {
		code = activationParticipantGraph
		text = activationCoordinationText(code)
	}
	return reconcileError(code, text, nil)
}

func activationCoordinationDiagnostic(err error) string {
	var diagnostic *ReconciliationError
	if !errors.As(err, &diagnostic) || diagnostic == nil {
		return ""
	}
	if text := activationCoordinationText(diagnostic.ReasonCode); text != "" {
		return " " + text + " (" + diagnostic.ReasonCode + ")"
	}
	return ""
}

const activationChainVersionKey = "svc.dpu.nvidia.com/dpuservicechain-version"

// activationVersionRequestors qualifies the finite current coordination graph.
// Interface names in a Service are opaque identity slots here; their complete
// objects remain independently qualified by the later HBN runtime boundary.
func activationVersionRequestors(ctx context.Context, client resourceLister, execution *managementVerificationInputs,
	deployment *unstructured.Unstructured, sets map[string]*unstructured.Unstructured,
) (activationVersionGraph, bool, error) {
	services, err := client.List(ctx, runtimeObject("DPUService", "", deployment.GetNamespace(), nil), "")
	if err != nil {
		return activationVersionGraph{}, false, reconcileError(activationParticipantList, activationCoordinationText(activationParticipantList), err)
	}
	chains, err := client.List(ctx, runtimeObject("DPUServiceChain", "", deployment.GetNamespace(), nil), "")
	if err != nil {
		return activationVersionGraph{}, false, reconcileError(activationParticipantList, activationCoordinationText(activationParticipantList), err)
	}
	return compileActivationVersionGraph(execution, deployment, sets, services, chains)
}

// compileActivationVersionGraph has explicit immutable source/snapshot inputs;
// it does not call APIs, change inputs or authorize an effect by itself.
func compileActivationVersionGraph(execution *managementVerificationInputs, deployment *unstructured.Unstructured, sets map[string]*unstructured.Unstructured, services, chains []unstructured.Unstructured) (activationVersionGraph, bool, error) {
	bad := func() (activationVersionGraph, bool, error) {
		return activationVersionGraph{}, false, activationCoordinationError(activationParticipantGraph)
	}
	if execution == nil || execution.plan == nil || execution.reconciler == nil || execution.rules == nil || deployment == nil {
		return bad()
	}

	if len(sets) == 0 {
		return activationVersionGraph{}, false, nil
	}
	parent := deployment.GetNamespace() + "_" + deployment.GetName()
	serviceKey := "svc.dpu.nvidia.com/dpuservice-" + hbnContainerName + "-version"
	var versionLabels map[string]string
	for _, set := range sets {
		if policyErr := activationNativeNodeEffectPolicy(set, "spec", "dpuTemplate", "spec", "nodeEffect"); policyErr != nil {
			return activationVersionGraph{}, false, policyErr
		}

		labels, found, e := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
		if e != nil || !found || len(labels) != 3 || labels[activationParentLabel] != parent || !activationVersionName(labels[serviceKey]) || !activationVersionName(labels[activationChainVersionKey]) {
			return activationVersionGraph{}, false, activationCoordinationError(activationParticipantSelector)
		}
		if versionLabels != nil && !reflect.DeepEqual(labels, versionLabels) {
			return activationVersionGraph{}, false, activationCoordinationError(activationParticipantSelector)
		}
		versionLabels = labels
	}
	if len(services) == 0 || len(chains) == 0 {
		return activationVersionGraph{}, false, nil
	}
	service := uniqueObjectByName(services, versionLabels[serviceKey])
	chain := uniqueObjectByName(chains, versionLabels[activationChainVersionKey])
	if service == nil || chain == nil {
		return activationVersionGraph{}, false, nil
	}
	if service.GetKind() != "DPUService" || chain.GetKind() != "DPUServiceChain" || !activationVersionOwner(service, deployment) || !activationVersionOwner(chain, deployment) || service.GetLabels()[hbnServiceReferenceLabel] != hbnContainerName {
		return activationVersionGraph{}, false, activationCoordinationError(activationParticipantOwner)
	}
	objects, e := retainedWorkloadObjects(execution)
	if e != nil {
		return bad()
	}
	var template, configuration *unstructured.Unstructured
	for _, object := range objects {
		switch object.GetKind() {
		case "DPUServiceTemplate":
			template = object
		case "DPUServiceConfiguration":
			configuration = object
		}
	}
	if template == nil || configuration == nil {
		return bad()
	}
	template, e = execution.rules.DefaultedCopy(template)
	if e != nil {
		return bad()
	}
	configuration, e = execution.rules.DefaultedCopy(configuration)
	if e != nil {
		return bad()
	}
	refs, found, e := unstructured.NestedMap(deployment.Object, "spec", "services")
	if e != nil || !found || len(refs) != 1 {
		return bad()
	}
	ref, ok := refs[hbnContainerName].(map[string]any)
	if !ok || len(ref) != 2 || ref["serviceTemplate"] != template.GetName() || ref["serviceConfiguration"] != configuration.GetName() {
		return bad()
	}
	id, found, e := unstructured.NestedString(service.Object, "spec", "serviceID")
	version := service.GetAnnotations()[hbnServiceVersionAnnotation]
	if e != nil || !found || version == "" || id != deployment.GetName()+"_"+hbnContainerName+"_"+version && id != "dpudeployment_"+deployment.GetName()+"_"+hbnContainerName {
		return bad()
	}
	names, found, e := unstructured.NestedStringSlice(service.Object, "spec", "interfaces")
	if e != nil || !found || len(names) != 3 {
		return bad()
	}
	unique := map[string]bool{}
	opaque := make([]any, len(names))
	for i, name := range names {
		if !activationVersionName(name) || unique[name] {
			return bad()
		}
		unique[name] = true
		opaque[i] = name
	}
	expressions := []any{map[string]any{"key": serviceKey, "operator": "In", "values": []any{versionLabels[serviceKey]}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	nodeSelector := map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": expressions}}}
	clusterExpressions := []any{}
	labels := execution.plan.Intent().Bootstrap.Tenant.Labels
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		clusterExpressions = append(clusterExpressions, map[string]any{"key": key, "operator": "In", "values": []any{labels[key]}})
	}
	clusterSelector := map[string]any{"matchExpressions": clusterExpressions}
	observedSelector, _, selectorErr := unstructured.NestedMap(service.Object, "spec", "serviceDaemonSet", "nodeSelector")
	observedCluster, _, clusterErr := unstructured.NestedMap(service.Object, "spec", "dpuClusterSelector")
	chainSelector, _, chainSelectorErr := unstructured.NestedMap(chain.Object, "spec", "template", "spec", "nodeSelector")
	chainCluster, _, chainClusterErr := unstructured.NestedMap(chain.Object, "spec", "dpuClusterSelector")
	chainExpressions := []any{map[string]any{"key": activationChainVersionKey, "operator": "In", "values": []any{versionLabels[activationChainVersionKey]}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	if selectorErr != nil || clusterErr != nil || chainSelectorErr != nil || chainClusterErr != nil || !jsonEqual(observedSelector, nodeSelector) || !jsonEqual(observedCluster, clusterSelector) || !jsonEqual(chainCluster, clusterSelector) || !jsonEqual(chainSelector, map[string]any{"matchExpressions": chainExpressions}) {
		return activationVersionGraph{}, false, activationCoordinationError(activationParticipantSelector)
	}
	expected, e := execution.rules.HBNServiceSpec(template, configuration, service.GetName(), service.GetNamespace(), hbnContainerName, id, nodeSelector, clusterSelector, opaque)
	if e != nil || !jsonEqual(service.Object["spec"], expected.Spec) {
		return activationVersionGraph{}, false, activationCoordinationError(activationParticipantServiceSpec)
	}
	expectedChain, e := activationChainSpec(deployment, id, clusterSelector, parent, versionLabels[activationChainVersionKey])
	if e != nil {
		return activationVersionGraph{}, false, activationCoordinationError(activationParticipantChainSpec)
	}
	normalized, e := execution.rules.DefaultedCopy(runtimeObject("DPUServiceChain", chain.GetName(), chain.GetNamespace(), expectedChain))
	if e != nil || !jsonEqual(chain.Object["spec"], normalized.Object["spec"]) || chain.GetAnnotations()[activationChainVersionKey] == "" {
		return activationVersionGraph{}, false, activationCoordinationError(activationParticipantChainSpec)
	}
	tokens := []string{parent + "_" + service.GetName(), parent + "_" + chain.GetName()}
	sort.Strings(tokens)
	if tokens[0] == tokens[1] {
		tokens = tokens[:1]
	}
	return activationVersionGraph{requestors: tokens, identities: map[string]types.UID{"DPUService/" + service.GetName(): service.GetUID(), "DPUServiceChain/" + chain.GetName(): chain.GetUID()}}, true, nil
}

func activationVersionName(name string) bool {
	return name != "" && len(validation.IsDNS1123Subdomain(name)) == 0
}

func activationVersionOwner(object, deployment *unstructured.Unstructured) bool {
	if object == nil || object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetNamespace() != deployment.GetNamespace() || object.GetUID() == types.UID("") || object.GetResourceVersion() == "" || object.GetGeneration() < 1 || object.GetDeletionTimestamp() != nil || object.GetLabels()[activationParentLabel] != deployment.GetNamespace()+"_"+deployment.GetName() {
		return false
	}
	owners := object.GetOwnerReferences()
	return len(owners) == 1 && owners[0].Controller != nil && *owners[0].Controller && activationOwnerMatches(object, deployment.GetAPIVersion(), deployment.GetKind(), deployment.GetName(), deployment.GetUID())
}

// activationChainSpec mirrors convertToSFCSwitch for the retained HBN subset.
// It reads only retained ports, replacing the declared service name/interface
// with its independently qualified current serviceID selector.
func activationChainSpec(deployment *unstructured.Unstructured, id string, clusterSelector map[string]any, parent, versionName string) (map[string]any, error) {
	switches, found, err := unstructured.NestedSlice(deployment.Object, "spec", "serviceChains", "switches")
	if err != nil || !found || len(switches) == 0 {
		return nil, fmt.Errorf("retained switches missing")
	}
	for _, raw := range switches {
		sw, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("retained switch malformed")
		}
		ports, ok := sw["ports"].([]any)
		if !ok {
			return nil, fmt.Errorf("retained ports malformed")
		}
		for _, rawPort := range ports {
			port, ok := rawPort.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("retained port malformed")
			}
			if rawService, exists := port["service"]; exists {
				service, ok := rawService.(map[string]any)
				if !ok || len(service) != 2 || service["name"] != hbnContainerName {
					return nil, fmt.Errorf("retained service port unsupported")
				}
				name, ok := service["interface"].(string)
				if !ok || name == "" {
					return nil, fmt.Errorf("retained interface missing")
				}
				delete(port, "service")
				port["serviceInterface"] = map[string]any{"matchLabels": map[string]any{hbnServiceIDLabel: id, "svc.dpu.nvidia.com/interface": name}}
			}
		}
	}
	expressions := []any{map[string]any{"key": activationChainVersionKey, "operator": "In", "values": []any{versionName}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	return map[string]any{"dpuClusterSelector": clusterSelector, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"nodeSelector": map[string]any{"matchExpressions": expressions}, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"switches": switches}}}}}, nil
}

// Additional requestors are an unordered set in the inline UpgradePolicy.
func activationAdditionalRequestorsMatch(object *unstructured.Unstructured, expected []string, path ...string) bool {
	rows, found, err := unstructured.NestedStringSlice(object.Object, append(path, "nodeMaintenanceAdditionalRequestors")...)
	if err != nil || !found || len(rows) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	allowed := map[string]bool{}
	for _, value := range expected {
		allowed[value] = true
	}
	for _, value := range rows {
		if seen[value] || !allowed[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

// The graph compiler already enforces the native Set's false upgrade policy;
// typed equality then requires the consumed DPU to preserve that same policy.
func activationNodeEffectMatches(dpu, set *unstructured.Unstructured) bool {
	for _, field := range []string{"hold", "force", "applyOnLabelChange"} {
		left, leftFound, leftErr := unstructured.NestedBool(dpu.Object, "spec", "nodeEffect", field)
		right, rightFound, rightErr := unstructured.NestedBool(set.Object, "spec", "dpuTemplate", "spec", "nodeEffect", field)
		if leftErr != nil || rightErr != nil || left != right || field == "hold" && (!leftFound || !rightFound || !left) {
			return false
		}
	}
	return true
}

// Only the native immutable-plan subset is supported here. Missing means the
// pinned API/Go false default; null/string/numeric values are not false.
func activationNativeNodeEffectPolicy(object *unstructured.Unstructured, path ...string) error {
	if object == nil {
		return activationCoordinationError(activationParticipantUnsupportedPolicy)
	}
	apply, _, err := unstructured.NestedBool(object.Object, append(path, "applyOnLabelChange")...)
	if err != nil || apply {
		return activationCoordinationError(activationParticipantUnsupportedPolicy)
	}
	return nil
}

// DPF createDPU/buildDPUClusterNodeLabels adds these three system values after
// merging user metadata; later UpdateDPUs preserves them. The native graph
// admits only its retained three controller labels as user metadata. This is
// exact expected enrichment, not a subset or arbitrary Device-label adoption.
func activationDPUClusterLabelsMatch(setLabels, dpuLabels map[string]string, nodeName, namespace, version string) bool {
	if len(setLabels) == 0 || dpuLabels == nil || nodeName == "" || namespace == "" || version == "" {
		return false
	}
	expected := make(map[string]string, len(setLabels)+3)
	for key, value := range setLabels {
		expected[key] = value
	}
	expected[activationDPUNodeLabel] = nodeName
	expected[activationDPUNodeNamespaceLabel] = namespace
	expected[activationDPFVersionLabel] = version
	return reflect.DeepEqual(expected, dpuLabels)
}
