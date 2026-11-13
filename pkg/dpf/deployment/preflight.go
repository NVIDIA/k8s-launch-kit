// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

func (s *run) management(ctx context.Context) error {
	c := s.clients[bundle.Management]
	objects, err := c.List(ctx, ref(bundle.Management, "operator.dpu.nvidia.com", "DPFOperatorConfig", s.g.Namespace, ""), "")
	if err != nil {
		return fail("DPF_CONFIG_UNAVAILABLE", "cannot read effective DPFOperatorConfig")
	}
	if len(objects) != 1 {
		return fail("DPF_CONFIG_AMBIGUOUS", "require exactly one effective DPFOperatorConfig")
	}
	object := objects[0]
	if strings.TrimPrefix(text(object, "status", "version"), "v") != s.g.Release {
		return fail("DPF_VERSION_MISMATCH", "effective DPF version differs from bundle release")
	}
	target := text(object, "status", "targetVersion")
	if target != "" && strings.TrimPrefix(target, "v") != s.g.Release {
		return fail("DPF_UPGRADE_IN_PROGRESS", "DPF target version differs from active version")
	}
	_, redfish, _ := unstructured.NestedMap(object.Object, "spec", "provisioningController", "installInterface", "installViaRedfish")
	if text(object, "spec", "deploymentMode") != "zero-trust" || !redfish {
		return fail("INSTALL_INTERFACE_MISMATCH", "DPF must already use zero-trust installViaRedfish")
	}
	if !condition(object, "Ready") {
		if err := s.check("global-operator-readiness", object.GetName(), "management:DPFOperatorConfig", "diagnostic", "GLOBAL_OPERATOR_NOT_READY", conditionMessage(object)+"; selected controller and tenant checks remain independently required"); err != nil {
			return err
		}
	}
	deployments, err := c.List(ctx, ref(bundle.Management, "apps", "Deployment", s.g.Namespace, ""), "")
	if err != nil {
		return fail("CONTROLLER_READ_FAILED", "cannot inspect DPF management controllers")
	}
	roles := map[string][]*unstructured.Unstructured{}
	for _, deployment := range deployments {
		role := deployment.GetLabels()["dpu.nvidia.com/component"]
		roles[role] = append(roles[role], deployment)
	}
	for _, role := range []string{"dpf-operator-controller-manager", "dpf-provisioning-controller-manager", "dpuservice-controller-manager"} {
		if len(roles[role]) != 1 {
			return fail("MANAGEMENT_CONTROLLER_MISSING", role+" must resolve to exactly one Deployment by component label")
		}
		ready, _, message := Readiness(roles[role][0])
		if !ready {
			return fail("MANAGEMENT_CONTROLLER_NOT_READY", role+": "+message)
		}
	}
	return s.check("existing-management", s.g.Namespace, "management:DPFOperatorConfig", "passed", "MANAGEMENT_READY", "")
}

func (s *run) cluster(ctx context.Context) error {
	binding := s.g.Provisioning.DPUCluster
	objects, err := s.clients[bundle.Management].List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUCluster", "", ""), labels.SelectorFromSet(binding.Selector).String())
	if err != nil {
		return fail("DPU_CLUSTER_SELECTION_UNAVAILABLE", "cannot enumerate DPUCluster selector domain across all namespaces")
	}
	if len(objects) != 1 {
		return fail("DPU_CLUSTER_SELECTION_MISMATCH", "DPUCluster selector must match exactly one existing tenant")
	}
	object := objects[0]
	if object.GetName() != binding.Name || object.GetNamespace() != binding.Namespace || string(object.GetUID()) != binding.ResourceUID {
		return fail("DPU_CLUSTER_IDENTITY_MISMATCH", "selected DPUCluster identity differs")
	}
	if object.GetGeneration() < 1 {
		return fail("DPU_CLUSTER_GENERATION_MISSING", "selected DPUCluster generation is unavailable")
	}
	if s.clusterGeneration != 0 && object.GetGeneration() != s.clusterGeneration {
		return fail("DPU_CLUSTER_GENERATION_DRIFT", "selected DPUCluster spec changed during this run")
	}
	ready, _, _ := Readiness(object)
	if !ready {
		return fail("DPU_CLUSTER_NOT_READY", "existing DPUCluster is not Ready")
	}
	if s.clusterGeneration == 0 {
		s.clusterGeneration = object.GetGeneration()
		if err := s.check("dpucluster-freshness", binding.Namespace+"/"+binding.Name, "DPF DPUCluster controller", "diagnostic", "FRESHNESS_NOT_EXPOSED", fmt.Sprintf("rc.1/GA publish no observedGeneration; phase/Ready and unchanged generation=%d are required, with tenant UID/API version/access checked separately", s.clusterGeneration)); err != nil {
			return err
		}
	}
	if text(object, "spec", "type") == "kamaji" {
		controllers, err := s.clients[bundle.Management].List(ctx, ref(bundle.Management, "apps", "Deployment", s.g.Namespace, ""), "dpu.nvidia.com/component=kamaji-cluster-manager")
		if err != nil || len(controllers) != 1 {
			return fail("KAMAJI_CONTROLLER_MISSING", "selected Kamaji tenant requires exactly one ready Kamaji cluster manager")
		}
		ready, _, message := Readiness(controllers[0])
		if !ready {
			return fail("KAMAJI_CONTROLLER_NOT_READY", message)
		}
	}
	return s.check("existing-tenant", binding.Namespace+"/"+binding.Name, "management:DPUCluster", "passed", "TENANT_BINDING_MATCH", "")
}

func (s *run) selection(ctx context.Context) error {
	c := s.clients[bundle.Management]
	expected := map[string]config.DeviceBinding{}
	selectedNodes := map[string]map[string]bool{}
	for _, binding := range s.g.Provisioning.Devices {
		expected[binding.DPUDevice] = binding
		if selectedNodes[binding.DPUNode] == nil {
			selectedNodes[binding.DPUNode] = map[string]bool{}
		}
		selectedNodes[binding.DPUNode][binding.DPUDevice] = true
		var inventoryDevice inventory.Device
		for _, d := range s.g.Devices {
			if d.ID == binding.ID {
				inventoryDevice = d
			}
		}
		device, err := c.Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUDevice", s.g.Namespace, binding.DPUDevice))
		if err != nil {
			return fail("DEVICE_ENROLLMENT_MISSING", binding.DPUDevice+" is not enrolled")
		}
		endpoint, _ := url.Parse(inventoryDevice.BMCEndpoint)
		if inventoryDevice.SerialNumber == "" || text(device, "spec", "serialNumber") != inventoryDevice.SerialNumber || text(device, "status", "psid") != s.g.DevicePSIDs[binding.ID] || text(device, "spec", "bmcIp") != endpoint.Hostname() {
			return fail("DEVICE_IDENTITY_MISMATCH", binding.DPUDevice+" serial/PSID/BMC differs from selected inventory")
		}
		if device.GetLabels()["provisioning.dpu.nvidia.com/dpunode-name"] != binding.DPUNode {
			return fail("DEVICE_NODE_MISMATCH", binding.DPUDevice+" is attached elsewhere")
		}
		for _, name := range []string{"NodeAttached", "Initialized", "Discovered", "Ready"} {
			if !condition(device, name) {
				return fail("DEVICE_NOT_READY", binding.DPUDevice+" condition "+name+" is not True")
			}
		}
		for _, name := range []string{"BMCCredentialsReady", "BMCServerCertificateReady", "CATrustBundleReady"} {
			if hasCondition(device, name) && !condition(device, name) {
				return fail("DEVICE_SECURITY_NOT_READY", binding.DPUDevice+" security condition "+name+" is not True")
			}
		}
		if err = c.VerifyBMC(ctx, s.g, binding, inventoryDevice, s.g.DevicePSIDs[binding.ID]); err != nil {
			return fail("BMC_IDENTITY_UNVERIFIED", binding.DPUDevice+": "+err.Error())
		}
	}
	for name, members := range selectedNodes {
		node, err := c.Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUNode", s.g.Namespace, name))
		if err != nil {
			return fail("DEVICE_NODE_MISSING", name+" is absent")
		}
		if text(node, "status", "dpuInstallInterface") != "redfish" {
			return fail("DEVICE_NODE_INTERFACE_MISMATCH", name+" did not snapshot redfish")
		}
		if err := reviewedRebootMethod(node); err != nil {
			return err
		}
		if message := pendingNodeTransition(node); message != "" {
			return fail("NODE_TRANSITION_PENDING", name+": "+message)
		}
		refs, _, _ := unstructured.NestedSlice(node.Object, "spec", "dpus")
		if len(refs) != len(members) {
			return fail("EFFECT_DOMAIN_MISMATCH", name+" contains an unselected or missing device")
		}
		for _, item := range refs {
			entry, ok := item.(map[string]any)
			if !ok || !members[fmt.Sprint(entry["name"])] {
				return fail("EFFECT_DOMAIN_MISMATCH", name+" membership differs")
			}
		}
	}
	dpus, err := c.List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPU", s.g.Namespace, ""), "")
	if err != nil {
		return fail("OWNERSHIP_READ_FAILED", "cannot inspect selected DPU ownership")
	}
	ownSets := map[string]bool{}
	for _, doc := range resourcesInBundle(s.bundle, "DPUSet") {
		ownSets[doc.Ref().Name] = true
	}
	for _, dpu := range dpus {
		if _, selected := expected[text(dpu, "spec", "dpuDeviceName")]; !selected {
			continue
		}
		owned := false
		for _, owner := range dpu.GetOwnerReferences() {
			if owner.Kind == "DPUSet" && ownSets[owner.Name] {
				set, err := c.Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUSet", s.g.Namespace, owner.Name))
				if err != nil || set.GetUID() != owner.UID || set.GetAnnotations()[bundleAnnotation] != s.result.BundleDigest {
					return fail("STALE_DPU_OWNER", "selected DPU owner UID/bundle is missing or changed; explicit recovery is required")
				}
				owned = true
			}
		}
		if !owned {
			return fail("FOREIGN_DPU_OWNER", "selected DPU remains owned outside this bundle; explicit external transition is required")
		}
	}
	sets, err := c.List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUSet", s.g.Namespace, ""), "")
	if err != nil {
		return fail("OWNERSHIP_READ_FAILED", "cannot inspect DPUSet selector overlap")
	}
	devices, err := c.List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUDevice", s.g.Namespace, ""), "")
	if err != nil {
		return fail("SELECTION_READ_FAILED", "cannot inspect exact live DPUDevice membership")
	}
	for _, set := range sets {
		if ownSets[set.GetName()] && set.GetAnnotations()[bundleAnnotation] == s.result.BundleDigest {
			continue
		}
		for _, device := range devices {
			if _, selected := expected[device.GetName()]; selected && matchesDeviceSet(set, device) {
				return fail("FOREIGN_DPUSET_SELECTOR", "foreign DPUSet still selects a requested device")
			}
		}
	}
	return s.check("selected-scope", fmt.Sprint(len(expected)), "management:DPUDevice/DPUNode/DPUSet", "passed", "EXACT_SCOPE_MATCH", "")
}

func matchesDeviceSet(set, device *unstructured.Unstructured) bool {
	raw, exists, _ := unstructured.NestedMap(set.Object, "spec", "dpuDeviceSelector")
	if !exists {
		raw, _, _ = unstructured.NestedMap(set.Object, "spec", "dpuSelector")
		if len(raw) == 0 {
			return true
		}
		for key, value := range raw {
			if device.GetLabels()[key] != value {
				return false
			}
		}
		return true
	}
	data, _ := json.Marshal(raw)
	var selector metav1.LabelSelector
	if json.Unmarshal(data, &selector) != nil {
		return true
	}
	parsed, err := metav1.LabelSelectorAsSelector(&selector)
	return err != nil || parsed.Matches(labels.Set(device.GetLabels()))
}

func (s *run) externalSecrets(ctx context.Context) error {
	namespace := s.g.Namespace
	if s.g.Sentry.Namespace != nil {
		namespace = *s.g.Sentry.Namespace
	}
	for _, destination := range []bundle.Destination{bundle.Management, bundle.DPU} {
		object, err := s.clients[destination].Get(ctx, ref(destination, "", "Namespace", "", namespace))
		if err != nil || text(object, "status", "phase") != "Active" || object.GetUID() == "" {
			return fail("EXTERNAL_NAMESPACE_UNAVAILABLE", string(destination)+" service namespace must already exist and be Active")
		}
		if s.namespaceUIDs[destination] == nil {
			s.namespaceUIDs[destination] = map[string]string{}
		}
		s.namespaceUIDs[destination][namespace] = string(object.GetUID())
		if err = s.check("external-namespace", string(destination)+"/"+namespace, "kubernetes:Namespace", "passed", "NAMESPACE_IDENTITY_OBSERVED", string(object.GetUID())); err != nil {
			return err
		}
	}
	for _, destination := range []bundle.Destination{bundle.Management, bundle.DPU} {
		c := s.clients[destination]
		if err := c.SecretKeys(ctx, namespace, s.g.Sentry.ImagePullSecret, []string{".dockerconfigjson"}); err != nil {
			return fail("EXTERNAL_SECRET_MISSING", err.Error())
		}
	}
	for _, destination := range []bundle.Destination{bundle.Management, bundle.DPU} {
		if err := s.clients[destination].SecretKeys(ctx, namespace, s.g.Sentry.MITMSecret, []string{"ca.crt", "tls.crt", "tls.key"}); err != nil {
			return fail("EXTERNAL_SECRET_MISSING", err.Error())
		}
	}
	if s.g.Sentry.LLMInspection != nil && *s.g.Sentry.LLMInspection {
		if err := s.clients[bundle.DPU].SecretKeys(ctx, namespace, s.g.Sentry.InferenceSecret, []string{"INFERENCE_API_KEY"}); err != nil {
			return fail("EXTERNAL_SECRET_MISSING", err.Error())
		}
	} else {
		if err := s.check("llm-inspection", namespace, "resolved:Sentry.llmInspection", "disabled", "LLM_INSPECTION_DISABLED", "optional external inference path is disabled; no LLM inspection result is claimed"); err != nil {
			return err
		}
	}
	if err := s.serviceOwnership(ctx, namespace); err != nil {
		return err
	}
	return s.check("existing-secrets", namespace, "bound Kubernetes Secret keys", "passed", "SECRET_BINDINGS_PRESENT", "")
}

func (s *run) serviceOwnership(ctx context.Context, namespace string) error {
	expected := map[string]*unstructured.Unstructured{}
	for _, doc := range resourcesInBundle(s.bundle, "DPUService") {
		expected[doc.Ref().Name] = doc.ObjectCopy()
	}
	services, err := s.clients[bundle.Management].List(ctx, ref(bundle.Management, "svc.dpu.nvidia.com", "DPUService", namespace, ""), "")
	if err != nil {
		return fail("SERVICE_OWNERSHIP_UNAVAILABLE", "cannot inspect DPUService ownership in service namespace")
	}
	instanceExists := false
	for _, service := range services {
		desired := expected[service.GetName()]
		if desired == nil || !owns(service, desired, s.result.BundleDigest, s.g.Release) {
			return fail("FOREIGN_SERVICE_OWNER", "service namespace contains a foreign DPUService")
		}
		instanceExists = true
	}
	projection := map[bundle.ResourceRef]*unstructured.Unstructured{}
	for _, stage := range s.bundle.Index().Stages {
		view, _ := s.bundle.Stage(stage.ID)
		for _, doc := range view.Projections() {
			projection[doc.Ref()] = doc.ObjectCopy()
		}
	}
	for _, kind := range []string{"Deployment", "DaemonSet", "StatefulSet"} {
		objects, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "apps", kind, namespace, ""), "")
		if err != nil {
			return fail("SERVICE_OWNERSHIP_UNAVAILABLE", "cannot inspect existing tenant service workloads")
		}
		for _, object := range objects {
			reference := ref(bundle.DPU, "apps", kind, namespace, object.GetName())
			if !instanceExists || projection[reference] == nil || !desiredMatches(projection[reference], object) {
				return fail("FOREIGN_SERVICE_WORKLOAD", "service namespace contains an unrelated tenant workload")
			}
		}
	}
	policies, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "tower.dpu.nvidia.com", "DocaTowerPolicy", namespace, ""), "")
	if err != nil && !meta.IsNoMatchError(err) {
		return fail("POLICY_OWNERSHIP_UNAVAILABLE", "cannot inspect existing Sentry policies")
	}
	if len(policies) > 0 && !instanceExists {
		return fail("FOREIGN_POLICY_OWNER", "service namespace contains policies before this bundle's service exists")
	}
	return nil
}

// A fresh tenant can have no workers until DPU provisioning completes. DNS is
// therefore a post-provisioning prerequisite for Sentry, not a preflight gate.
func (s *run) baseWorkloads(ctx context.Context) error {
	for {
		observation, err := s.observeDNS(ctx)
		if err != nil {
			return err
		}
		if observation.ready {
			return s.check("tenant-dns", "bound tenant", observation.source, "passed", observation.code, observation.message)
		}
		if s.result.Mode == Validate && !s.runner.Wait {
			return fail(observation.code, observation.message)
		}
		if err := s.check("tenant-dns", "bound tenant", observation.source, "pending", observation.code, observation.message); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.runner.PollInterval):
		}
	}
}

func (s *run) provisioningGate(ctx context.Context, set *unstructured.Unstructured) error {
	if err := s.management(ctx); err != nil {
		return err
	}
	if err := s.cluster(ctx); err != nil {
		return err
	}
	if err := s.identity(ctx, s.clients[bundle.DPU], s.g.Provisioning.DPUCluster.ClusterUID, "tenant"); err != nil {
		return err
	}
	devices, err := s.clients[bundle.Management].List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUDevice", s.g.Namespace, ""), "")
	if err != nil {
		return fail("SELECTION_READ_FAILED", "cannot check generated DPUSet selector")
	}
	expected := map[string]bool{}
	for _, binding := range s.g.Provisioning.Devices {
		expected[binding.DPUDevice] = true
	}
	matches := 0
	for _, device := range devices {
		if matchesDeviceSet(set, device) {
			if !expected[device.GetName()] {
				return fail("DPUSET_SCOPE_MISMATCH", "generated DPUSet selects an unselected device")
			}
			matches++
		}
	}
	if matches != 1 {
		return fail("DPUSET_SCOPE_MISMATCH", "per-device DPUSet must select exactly one enrolled device")
	}
	name := text(set, "spec", "dpuTemplate", "spec", "blueFieldSoftware", "name")
	software, err := s.clients[bundle.Management].Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "BlueFieldSoftware", s.g.Namespace, name))
	if err != nil {
		return fail("SOFTWARE_NOT_READY", "DPUSet software is absent")
	}
	ready, _, _ := Readiness(software)
	if !ready {
		return fail("SOFTWARE_NOT_READY", "DPUSet software is not Ready")
	}
	if err = s.runner.Artifacts.Cached(ctx, s.clients[bundle.Management], s.g, software); err != nil {
		return fail("SOFTWARE_INTEGRITY_FAILED", err.Error())
	}
	// DPUCluster has no controller observation generation. Recheck the exact
	// tenant object generation after potentially long artifact hashing, before
	// any DPUSet may trigger physical provisioning.
	if err := s.cluster(ctx); err != nil {
		return err
	}
	// Large cached ISO hashes can take time. Reconfirm selected physical identity
	// and owner scope after streaming, then re-read the immutable software binding.
	if err := s.selection(ctx); err != nil {
		return err
	}
	after, err := s.clients[bundle.Management].Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "BlueFieldSoftware", s.g.Namespace, name))
	if err != nil {
		return fail("SOFTWARE_BINDING_DRIFT", "cannot re-read software before provisioning")
	}
	beforePaths, _, _ := unstructured.NestedMap(software.Object, "status", "downloadedComponents")
	afterPaths, _, _ := unstructured.NestedMap(after.Object, "status", "downloadedComponents")
	ready, _, _ = Readiness(after)
	if !ready || after.GetUID() != software.GetUID() || after.GetGeneration() != software.GetGeneration() || !reflect.DeepEqual(beforePaths, afterPaths) {
		return fail("SOFTWARE_BINDING_DRIFT", "software changed after consumed-byte/identity verification")
	}
	return s.check("software-integrity", name, "management:BlueFieldSoftware cached consumed files", "passed", "CACHED_BYTES_MATCH", "external invariant: cache payloads must not be replaced or removed during provisioning")
}

func (s *run) selectedReady(ctx context.Context) error {
	for {
		dpus, err := s.clients[bundle.Management].List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPU", s.g.Namespace, ""), "")
		if err != nil {
			return fail("DPU_READINESS_UNAVAILABLE", "cannot list selected DPU children")
		}
		readyCount := 0
		selected := map[string]map[string]string{}
		pending := []string{}
		for _, binding := range s.g.Provisioning.Devices {
			dpuNode, err := s.clients[bundle.Management].Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUNode", s.g.Namespace, binding.DPUNode))
			if err != nil {
				return fail("DPU_NODE_READINESS_UNAVAILABLE", "cannot inspect selected DPUNode transition")
			}
			if err := reviewedRebootMethod(dpuNode); err != nil {
				return err
			}
			transition := pendingNodeTransition(dpuNode)
			if transition != "" {
				pending = append(pending, binding.DPUNode+": "+transition)
			}
			var children []*unstructured.Unstructured
			for _, dpu := range dpus {
				if text(dpu, "spec", "dpuDeviceName") == binding.DPUDevice {
					children = append(children, dpu)
				}
			}
			if len(children) > 1 {
				return fail("DPU_CHILD_MEMBERSHIP_MISMATCH", "selected device must have exactly one DPU child")
			}
			if len(children) == 0 {
				pending = append(pending, binding.DPUDevice+": no DPU child")
				continue
			}
			child := children[0]
			if text(child, "spec", "cluster", "name") != s.g.Provisioning.DPUCluster.Name || text(child, "spec", "cluster", "namespace") != s.g.Provisioning.DPUCluster.Namespace {
				return fail("DPU_TENANT_MISMATCH", "selected DPU is assigned to a different tenant")
			}
			ownerVerified := false
			nodeLabels := map[string]string{}
			for _, owner := range child.GetOwnerReferences() {
				if owner.Kind != "DPUSet" {
					continue
				}
				for _, doc := range resourcesInBundle(s.bundle, "DPUSet") {
					if owner.Name != doc.Ref().Name {
						continue
					}
					set, err := s.clients[bundle.Management].Get(ctx, doc.Ref())
					if err != nil {
						return fail("DPU_OWNER_UNAVAILABLE", "cannot recheck selected DPUSet owner")
					}
					if set.GetUID() != owner.UID || !owns(set, doc.ObjectCopy(), s.result.BundleDigest, s.g.Release) {
						return fail("DPU_OWNER_MISMATCH", "DPU child owner identity differs from the retained bundle")
					}
					if text(child, "spec", "blueFieldSoftware") != text(set, "spec", "dpuTemplate", "spec", "blueFieldSoftware", "name") || text(child, "spec", "dpuFlavor") != text(set, "spec", "dpuTemplate", "spec", "dpuFlavor") {
						return fail("DPU_TEMPLATE_MISMATCH", "selected DPU child software/flavor differs from generated intent")
					}
					nodeLabels, err = expectedDPUNodeLabels(set, binding.DPUNode, s.g.Namespace, s.g.Release)
					if err != nil {
						return err
					}
					childLabels, _, err := unstructured.NestedStringMap(child.Object, "spec", "cluster", "nodeLabels")
					if err != nil || !jsonEqual(nodeLabels, childLabels) {
						return fail("DPU_NODE_SCOPE_MISMATCH", "DPU child node labels differ from its bundle DPUSet")
					}
					ownerVerified = true
				}
			}
			if !ownerVerified {
				return fail("DPU_OWNER_MISMATCH", "selected DPU lacks the expected bundle DPUSet owner")
			}
			ready, terminal, message := Readiness(children[0])
			if terminal {
				return fail("DPU_NOT_READY", message)
			}
			if !ready {
				pending = append(pending, binding.DPUDevice+": DPU phase="+text(child, "status", "phase")+" "+message)
				continue
			}
			node, err := s.clients[bundle.DPU].Get(ctx, ref(bundle.DPU, "", "Node", "", nodeForDPU(children[0])))
			if err != nil {
				pending = append(pending, binding.DPUDevice+": tenant Node absent")
				continue
			}
			selected[node.GetName()] = nodeLabels
			ready, _, _ = Readiness(node)
			labelsMatch := true
			for key, value := range nodeLabels {
				if node.GetLabels()[key] != value {
					labelsMatch = false
				}
			}
			if !labelsMatch {
				pending = append(pending, node.GetName()+": tenant Node scope labels do not match")
			}
			if ready && labelsMatch && transition == "" {
				baseReady, err := s.selectedNodeBase(ctx, node.GetName())
				if err != nil {
					return err
				}
				if baseReady {
					readyCount++
				}
			}
		}
		if readyCount == len(s.g.Provisioning.Devices) {
			if _, err := s.tenantScope(ctx, selected); err != nil {
				return err
			}
			// Re-read after tenant observations: a reset marker or executor
			// change that appeared during this sampling round blocks success.
			clear, diagnostic, err := s.selectedTransitionsClear(ctx)
			if err != nil {
				return err
			}
			if !clear {
				if s.result.Mode == Validate && !s.runner.Wait {
					return fail("NODE_TRANSITION_PENDING", diagnostic)
				}
				if err := s.check("selected-dpu-readiness", "DPUNode", "management:DPUNode", "pending", "NODE_TRANSITION_PENDING", diagnostic); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(s.runner.PollInterval):
					continue
				}
			}
			s.selectedNodes = selected
			return s.check("selected-dpu-readiness", fmt.Sprint(readyCount), "management:DPU + tenant:Node", "passed", "SELECTED_CHILDREN_READY", "")
		}
		if s.result.Mode == Validate && !s.runner.Wait {
			return fail("SELECTED_DPUS_NOT_READY", "selected DPU children or tenant Nodes are not Ready: "+strings.Join(pending, "; "))
		}
		if err := s.check("selected-dpu-readiness", fmt.Sprint(readyCount), "management:DPU + tenant:Node/base Pods", "pending", "SELECTED_CHILDREN_NOT_READY", "waiting for exact selected children, tenant Nodes and selected-node networking components: "+strings.Join(pending, "; ")); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.runner.PollInterval):
		}
	}
}

func (s *run) selectedNodeBase(ctx context.Context, node string) (bool, error) {
	checks := []struct{ namespace, selector, name string }{
		{s.g.Namespace, "dpu.nvidia.com/component=cni-installer", "cni-installer"},
		{s.g.Namespace, "dpu.nvidia.com/component=multus", "multus"},
		{s.g.Namespace, "app=flannel,tier=node", "flannel"},
		{s.g.Namespace, "dpu.nvidia.com/component=sriov-device-plugin", "sriov-device-plugin"},
		{s.g.Namespace, "dpu.nvidia.com/component=sfc-controller", "sfc-controller"},
		{s.g.Namespace, "dpu.nvidia.com/component=nvidia-k8s-ipam", "nvidia-k8s-ipam"},
		{"kube-system", "k8s-app=kube-proxy", "kube-proxy"},
	}
	allReady := true
	for _, check := range checks {
		pods, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "", "Pod", check.namespace, ""), check.selector)
		if err != nil {
			return false, fail("TENANT_BASE_READ_FAILED", "cannot inspect selected-node "+check.name)
		}
		readyCount := 0
		for _, pod := range pods {
			if text(pod, "spec", "nodeName") != node || !pod.GetDeletionTimestamp().IsZero() {
				continue
			}
			ready, _, _ := Readiness(pod)
			if ready {
				readyCount++
			}
		}
		if readyCount != 1 {
			allReady = false
		}
	}
	if allReady {
		if err := s.check("selected-node-base", node, "tenant:Pod networking component selectors", "passed", "SELECTED_NODE_BASE_READY", ""); err != nil {
			return false, err
		}
	}
	npd, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "", "Pod", s.g.Namespace, ""), "dpu.nvidia.com/component=node-problem-detector")
	if err != nil {
		return false, fail("TENANT_BASE_READ_FAILED", "cannot inspect selected-node problem detector diagnostic")
	}
	npdReady := false
	for _, pod := range npd {
		if text(pod, "spec", "nodeName") == node {
			ready, _, _ := Readiness(pod)
			npdReady = npdReady || ready
		}
	}
	if !npdReady {
		if err := s.check("selected-node-problem-detector", node, "tenant:Pod node-problem-detector", "diagnostic", "NODE_PROBLEM_DETECTOR_NOT_READY", "node problem detector is missing/not Ready on the selected node; no healthy detector claim"); err != nil {
			return false, err
		}
	}
	return allReady, nil
}

// A previous operation must finish externally before a new physical operation.
// rebootMethod alone records the most recent reset type and is not an active flag.
func pendingNodeTransition(node *unstructured.Unstructured) string {
	active, _, _ := unstructured.NestedBool(node.Object, "status", "rebootInProgress")
	if active {
		return "rebootInProgress=true (" + text(node, "status", "rebootMethod") + ")"
	}
	const annotation = "provisioning.dpu.nvidia.com/dpunode-external-reboot-required"
	if value, exists := node.GetAnnotations()[annotation]; exists {
		return "external reboot annotation remains: " + value
	}
	for _, name := range []string{"DPUNodeRebootInProgress", "DPUUpdateInProgress", "DPUNodeNodeEffectInProgress"} {
		if condition(node, name) {
			return name + "=True"
		}
	}
	return ""
}

// DPUSet readiness may stay False throughout firmware, ISO installation and
// external host reset. Preserve selected-child phase and node reset evidence
// during that wait instead of hiding the physical transition behind a counter.
func (s *run) selectedTransitionDiagnostics(ctx context.Context) (string, error) {
	children, err := s.clients[bundle.Management].List(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPU", s.g.Namespace, ""), "")
	if err != nil {
		return "", fail("DPU_READINESS_UNAVAILABLE", "cannot inspect selected DPU transition diagnostics")
	}
	out := []string{}
	for _, binding := range s.g.Provisioning.Devices {
		for _, child := range children {
			if text(child, "spec", "dpuDeviceName") == binding.DPUDevice {
				out = append(out, binding.DPUDevice+": phase="+text(child, "status", "phase")+" "+conditionMessage(child))
			}
		}
		node, err := s.clients[bundle.Management].Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUNode", s.g.Namespace, binding.DPUNode))
		if err != nil {
			return "", fail("DPU_NODE_READINESS_UNAVAILABLE", "cannot inspect selected DPUNode reset evidence")
		}
		if transition := pendingNodeTransition(node); transition != "" {
			out = append(out, binding.DPUNode+": "+transition)
		}
	}
	return strings.Join(out, "; "), nil
}

// The first slice permits only a reviewed external executor. Reuse this exact
// contract before writes and at convergence so late changes cannot be ignored.
func reviewedRebootMethod(node *unstructured.Unstructured) error {
	if node == nil {
		return fail("DPU_NODE_READINESS_UNAVAILABLE", "selected DPUNode is absent")
	}
	method, exists, err := unstructured.NestedMap(node.Object, "spec", "nodeRebootMethod")
	external, ok := method["external"].(map[string]any)
	if err != nil || !exists || !ok || len(method) != 1 || len(external) != 0 {
		return fail("NODE_REBOOT_METHOD_UNSUPPORTED", node.GetName()+" requires the explicitly reviewed external:{} reboot method")
	}
	return nil
}
func (s *run) selectedTransitionsClear(ctx context.Context) (bool, string, error) {
	pending := []string{}
	for _, binding := range s.g.Provisioning.Devices {
		node, err := s.clients[bundle.Management].Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUNode", s.g.Namespace, binding.DPUNode))
		if err != nil {
			return false, "", fail("DPU_NODE_READINESS_UNAVAILABLE", "cannot recheck selected DPUNode at convergence")
		}
		if err := reviewedRebootMethod(node); err != nil {
			return false, "", err
		}
		if transition := pendingNodeTransition(node); transition != "" {
			pending = append(pending, binding.DPUNode+": "+transition)
		}
	}
	return len(pending) == 0, strings.Join(pending, "; "), nil
}

// rc.1 dpuset_controller.go buildDPUClusterNodeLabels: the controller appends
// these three reserved labels to frozen user labels. Do not accept arbitrary
// additional labels: they can change service/workload selection.
func expectedDPUNodeLabels(set *unstructured.Unstructured, node, namespace, release string) (map[string]string, error) {
	expected, _, err := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
	if err != nil || expected[sentry.ScopeLabel] == "" {
		return nil, fail("DPU_NODE_SCOPE_MISSING", "DPUSet must bind the Sentry node scope label")
	}
	reserved := map[string]string{
		"provisioning.dpu.nvidia.com/dpunode-name":      node,
		"provisioning.dpu.nvidia.com/dpunode-namespace": namespace,
		"operator.dpu.nvidia.com/dpf-version":           "v" + strings.TrimPrefix(release, "v"),
	}
	for key, value := range reserved {
		if supplied, exists := expected[key]; exists && supplied != value {
			return nil, fail("DPU_NODE_LABEL_INTENT_CONFLICT", "DPUSet reserved node label conflicts with its selected enrollment/release")
		}
		expected[key] = value
	}
	return expected, nil
}
