// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sentry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kjson "k8s.io/apimachinery/pkg/util/json"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	yaml "sigs.k8s.io/yaml"
)

const (
	RendererVersion = "sentry-bf4/v1alpha1"
	ScopeLabel      = "nvidia.kubernetes-launch-kit.dpf-instance"
	DeviceBMCLabel  = "provisioning.dpu.nvidia.com/dpudevice-bmc-ip"
	DeviceNodeLabel = "provisioning.dpu.nvidia.com/dpunode-name"
)

// Rendered is one complete snapshot, ready for the DPF bundle constructor. Chart
// projections participate in validation only; deploy applies the DPUService CRs.
type Rendered struct {
	Files                 []bundle.File
	Records               []bundle.FileRecord
	Stages                []bundle.Stage
	References            []bundle.Reference
	ExternalPrerequisites []string
	SelectedDevices       []string
}

type names struct {
	Flavor, Software, Tower, Controller, Argus, Server, CredentialSecret string
	Physical, PF, Patch, Passthrough, IngressNAD, EgressNAD              string
	IngressInterface, EgressInterface                                    string
}

type templateData struct {
	LLMInspection                                                                  bool
	ProvisioningNamespace                                                          string
	Names                                                                          names
	Namespace, TenantName, TenantNamespace, ScopeKey, ScopeValue                   string
	PassthroughLabel, TowerLabel, PullSecret, MITMSecret, InferenceSecret          string
	IngressCIDR, IngressIP                                                         string
	IngressPrefix, NodePort                                                        int
	EnvoyTag, OpenFlowTag, PluginTag, TapTag, ControllerTag, ServerTag, ArgusImage string
}

type templateSpec struct {
	filename, stage, workloadStage string
	destination                    bundle.Destination
}

var templateSpecs = []templateSpec{
	{"10-flavor.yaml", "provisioning-artifacts", "", bundle.Management},
	{"20-passthrough-interfaces.yaml", "passthrough", "", bundle.Management},
	{"21-passthrough-chain.yaml", "passthrough", "", bundle.Management},
	{"30-tower-nads.yaml", "services", "", bundle.Management},
	{"31-tower-interfaces.yaml", "services", "", bundle.Management},
	{"32-tower.yaml", "services", "tower-workloads", bundle.Management},
	{"33-tower-chain.yaml", "services", "", bundle.Management},
	{"40-controller.yaml", "services", "policy-controller-workloads", bundle.Management},
	{"50-argus.yaml", "services", "argus-workloads", bundle.Management},
	{"60-policy-rbac.yaml", "policy-rbac", "", bundle.DPU},
	{"61-policy-credential.yaml", "policy-credentials", "", bundle.Management},
	{"62-policy-server.yaml", "policy-server", "", bundle.Management},
}

// Render uses the common profile's local templates and supplied archive bytes.
// It never resolves kubeconfigs, reads Secrets, discovers hardware, or downloads
// charts. All archives are checked before any output is returned.
func Render(g config.Generation, profile *profiles.ProfileMetadata, archives map[string][]byte) (Rendered, error) {
	if err := validateInputs(g, profile); err != nil {
		return Rendered{}, err
	}
	charts, err := loadCharts(archives)
	if err != nil {
		return Rendered{}, err
	}
	templates, err := readTemplates(profile)
	if err != nil {
		return Rendered{}, err
	}
	d := newTemplateData(g)
	flavorObjects, err := renderTemplate("10-flavor.yaml", templates["10-flavor.yaml"], d)
	if err != nil {
		return Rendered{}, err
	}
	if len(flavorObjects) != 1 || flavorObjects[0].GetKind() != "DPUFlavor" {
		return Rendered{}, fmt.Errorf("sentry flavor template must render exactly one DPUFlavor")
	}
	flavorSpec, err := json.Marshal(flavorObjects[0].Object["spec"])
	if err != nil {
		return Rendered{}, err
	}
	d.Names.Flavor += "-" + shortHash(string(flavorSpec))
	flavorObjects[0].SetName(d.Names.Flavor)
	r := Rendered{ExternalPrerequisites: []string{"existing-management", "existing-tenant", "enrolled-devices", "selected-scope", "external-transition-complete", "software-integrity", "existing-secrets"}}
	r.Stages = stages(g.Namespace, *g.Sentry.Namespace)
	stageByID := map[string]int{}
	for i := range r.Stages {
		stageByID[r.Stages[i].ID] = i
	}
	add := func(path string, data []byte, role bundle.Role, stage string) {
		r.Files = append(r.Files, bundle.File{Path: path, Data: append([]byte(nil), data...)})
		r.Records = append(r.Records, bundle.FileRecord{Path: path, Role: role})
		if stage != "" {
			s := &r.Stages[stageByID[stage]]
			if role == bundle.Projection {
				s.ProjectionFiles = append(s.ProjectionFiles, path)
			} else {
				s.Files = append(s.Files, path)
			}
		}
	}
	capabilities, err := yaml.Marshal(map[string]interface{}{"managementKubernetesVersion": g.ManagementKubernetesVersion, "dpuKubernetesVersion": g.Provisioning.DPUCluster.KubernetesVersion, "apiVersions": projectionAPIVersions()})
	if err != nil {
		return Rendered{}, err
	}
	add("metadata/helm-capabilities.yaml", capabilities, bundle.Metadata, "")
	chartStages := map[string]string{"doca-tower": "services", "doca-sentry-policy-controller": "services", "doca-argus": "services", "doca-sentry-policy-server": "policy-server"}
	for _, lock := range Catalog() {
		add("charts/"+lock.Filename, archives[lock.Name], bundle.Chart, chartStages[lock.Name])
	}
	software := softwareObject(g, d.Names.Software)
	b, err := marshalDocuments([]*unstructured.Unstructured{software})
	if err != nil {
		return Rendered{}, err
	}
	add("management/00-software.yaml", b, bundle.Manifest, "provisioning-artifacts")
	bindings := append([]config.DeviceBinding(nil), g.Provisioning.Devices...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	for _, binding := range bindings {
		var endpoint string
		for _, device := range g.Devices {
			if device.ID == binding.ID {
				endpoint = device.BMCEndpoint
			}
		}
		u, _ := url.Parse(endpoint)
		set := deviceSet(g, d, binding, u.Hostname())
		b, err = marshalDocuments([]*unstructured.Unstructured{set})
		if err != nil {
			return Rendered{}, err
		}
		add("management/15-dpuset-"+shortHash(binding.ID)+".yaml", b, bundle.Manifest, "provisioning")
		r.SelectedDevices = append(r.SelectedDevices, binding.ID)
		for _, to := range []string{d.Names.Flavor, d.Names.Software} {
			kind := "DPUFlavor"
			if to == d.Names.Software {
				kind = "BlueFieldSoftware"
			}
			r.References = append(r.References, bundle.Reference{From: resource(bundle.Management, set), To: bundle.ResourceRef{Destination: bundle.Management, Group: "provisioning.dpu.nvidia.com", Kind: kind, Namespace: g.Namespace, Name: to}})
		}
	}
	projectionDependencies := projectionInputs{interfaces: map[string]*unstructured.Unstructured{}, nads: map[string]*unstructured.Unstructured{}}
	for _, spec := range templateSpecs {
		objects := flavorObjects
		if spec.filename != "10-flavor.yaml" {
			objects, err = renderTemplate(spec.filename, templates[spec.filename], d)
			if err != nil {
				return Rendered{}, err
			}
		}
		for _, obj := range objects {
			if err = bindObject(g, d, obj); err != nil {
				return Rendered{}, fmt.Errorf("bind %s/%s: %w", obj.GetKind(), obj.GetName(), err)
			}
			if err = projectionDependencies.record(obj); err != nil {
				return Rendered{}, err
			}
			r.References = append(r.References, objectReferences(spec.destination, obj)...)
			if obj.GetKind() == "DPUService" {
				chartName, _, _ := unstructured.NestedString(obj.Object, "spec", "helmChart", "source", "chart")
				version, _, _ := unstructured.NestedString(obj.Object, "spec", "helmChart", "source", "version")
				repository, _, _ := unstructured.NestedString(obj.Object, "spec", "helmChart", "source", "repoURL")
				chart, ok := charts[chartName]
				if !ok || chart.Metadata.Version != version {
					return Rendered{}, fmt.Errorf("DPUService %s refers to an unlocked chart %s:%s", obj.GetName(), chartName, version)
				}
				for _, lock := range Catalog() {
					if lock.Name == chartName && repository != lock.Repository {
						return Rendered{}, fmt.Errorf("DPUService %s refers to an unlocked chart repository", obj.GetName())
					}
				}
				values, err := effectiveHelmValues(g.Release, obj, projectionDependencies)
				if err != nil {
					return Rendered{}, fmt.Errorf("effective Helm inputs for %s: %w", obj.GetName(), err)
				}
				kubernetesVersion := g.Provisioning.DPUCluster.KubernetesVersion
				if obj.GetName() == d.Names.Server {
					kubernetesVersion = g.ManagementKubernetesVersion
				}
				projected, err := projectChart(chart, values, obj.GetName(), *g.Sentry.Namespace, kubernetesVersion)
				if err != nil {
					return Rendered{}, fmt.Errorf("project DPUService %s: %w", obj.GetName(), err)
				}
				projection, err := marshalDocuments(projected)
				if err != nil {
					return Rendered{}, err
				}
				vb, err := yaml.Marshal(values)
				if err != nil {
					return Rendered{}, err
				}
				add("values/"+obj.GetName()+".yaml", vb, bundle.HelmValues, spec.stage)
				projectionStage := spec.stage
				if spec.workloadStage != "" {
					projectionStage = spec.workloadStage
				}
				add("projections/"+obj.GetName()+".yaml", projection, bundle.Projection, projectionStage)
			}
		}
		b, err = marshalDocuments(objects)
		if err != nil {
			return Rendered{}, err
		}
		add(string(spec.destination)+"/"+spec.filename, b, bundle.Manifest, spec.stage)
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	sort.Slice(r.Records, func(i, j int) bool { return r.Records[i].Path < r.Records[j].Path })
	if err := validateRendered(r, g.Release); err != nil {
		return Rendered{}, err
	}
	return r, nil
}

func renderTemplate(name string, body []byte, data templateData) ([]*unstructured.Unstructured, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse profile template %s: %w", name, err)
	}
	var out bytes.Buffer
	if err = t.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render profile template %s: %w", name, err)
	}
	if !safeTemplateOutput(out.String()) {
		return nil, fmt.Errorf("profile template %s contains unresolved site parameters or forbidden lab credentials", name)
	}
	objects, err := decodeDocuments(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("profile template %s: %w", name, err)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("profile template %s contains no Kubernetes documents", name)
	}
	return objects, nil
}

func readTemplates(p *profiles.ProfileMetadata) (map[string][]byte, error) {
	paths := map[string]string{}
	for _, path := range p.TemplatePaths {
		name := filepath.Base(path)
		if _, exists := paths[name]; exists {
			return nil, fmt.Errorf("duplicate Sentry template %s", name)
		}
		paths[name] = path
	}
	out := map[string][]byte{}
	for _, spec := range templateSpecs {
		path, ok := paths[spec.filename]
		if !ok {
			return nil, fmt.Errorf("sentry profile requires template %s", spec.filename)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read profile template %s: %w", spec.filename, err)
		}
		if len(body) > 1<<20 {
			return nil, fmt.Errorf("sentry template %s exceeds size limit", spec.filename)
		}
		out[spec.filename] = body
	}
	return out, nil
}

func validateInputs(g config.Generation, p *profiles.ProfileMetadata) error {
	if p == nil || p.Target != target.DPF || g.Profile != "sentry-bf4" {
		return fmt.Errorf("sentry requires the DPF sentry-bf4 profile")
	}
	if g.Release != "26.8.0" && g.Release != "26.8.0-rc.1" {
		return fmt.Errorf("unsupported Sentry DPF release %q", g.Release)
	}
	if g.Name == "" || g.Namespace == "" || g.Provisioning.DPUCluster.Name == "" || g.Provisioning.DPUCluster.Namespace == "" || len(g.Provisioning.DPUCluster.Selector) == 0 {
		return fmt.Errorf("sentry requires explicit instance, namespace and selected tenant identity/selector")
	}
	if len(g.Provisioning.Devices) == 0 || len(g.Devices) != len(g.Provisioning.Devices) {
		return fmt.Errorf("sentry requires exactly the selected resolved devices")
	}
	if len(p.TemplatePaths) != len(templateSpecs) {
		return fmt.Errorf("sentry profile has unexpected template membership")
	}
	seen := map[string]bool{}
	bmcs := map[string]bool{}
	for _, b := range g.Provisioning.Devices {
		if b.ID == "" || b.DPUDevice == "" || b.DPUNode == "" || seen[b.ID] {
			return fmt.Errorf("sentry device bindings require unique IDs and existing enrollment names")
		}
		seen[b.ID] = true
		matches := 0
		for _, device := range g.Devices {
			if device.ID == b.ID {
				matches++
				u, err := url.Parse(device.BMCEndpoint)
				if err != nil || u.Hostname() == "" {
					return fmt.Errorf("device %s has no BMC selector identity", b.ID)
				}
				addr, err := netip.ParseAddr(u.Hostname())
				if err != nil || !addr.Is4() {
					return fmt.Errorf("device %s requires an observed BMC IPv4 selector", b.ID)
				}
				key := b.DPUNode + "/" + addr.String()
				if bmcs[key] {
					return fmt.Errorf("duplicate node/BMC selector for %s", b.ID)
				}
				bmcs[key] = true
			}
		}
		if matches != 1 {
			return fmt.Errorf("device %s is absent or ambiguous in resolved inventory", b.ID)
		}
	}
	if _, err := netip.ParsePrefix(g.Sentry.IngressCIDR); err != nil {
		return fmt.Errorf("invalid Sentry ingress CIDR: %w", err)
	}
	if g.Sentry.PolicyServerNodePort == nil || g.Sentry.ImagePullSecret == "" || g.Sentry.MITMSecret == "" || g.Sentry.LLMInspection == nil {
		return fmt.Errorf("sentry requires resolved NodePort and existing Secret references")
	}
	if *g.Sentry.LLMInspection && g.Sentry.InferenceSecret == "" {
		return fmt.Errorf("sentry LLM inspection requires an existing inference Secret reference")
	}
	if g.Sentry.Namespace == nil || *g.Sentry.Namespace == "" || *g.Sentry.Namespace == g.Namespace {
		return fmt.Errorf("sentry requires a dedicated resolved service namespace")
	}
	return nil
}

func newTemplateData(g config.Generation) templateData {
	prefix := g.Name
	if len(prefix) > 24 {
		prefix = prefix[:15] + "-" + shortHash(prefix)
	}
	n := names{Flavor: prefix + "-flavor", Software: prefix + "-software", Tower: prefix + "-tower", Controller: prefix + "-controller", Argus: prefix + "-argus", Server: prefix + "-policy-server", CredentialSecret: prefix + "-policy-kubeconfig", Physical: prefix + "-p0", PF: prefix + "-pf0hpf", Patch: prefix + "-patch", Passthrough: prefix + "-passthrough", IngressNAD: prefix + "-ingress", EgressNAD: prefix + "-egress", IngressInterface: prefix + "-ingress-interface", EgressInterface: prefix + "-egress-interface"}
	// Immutable provisioning resources receive a content-derived suffix, so a
	// changed flavor/software does not attempt an immutable spec update.
	software, _ := json.Marshal(g.Provisioning.Software)
	n.Software += "-" + shortHash(string(software))
	images := map[string]ImageLock{}
	for _, i := range Images() {
		images[i.Name] = i
	}
	p, _ := netip.ParsePrefix(g.Sentry.IngressCIDR)
	return templateData{LLMInspection: *g.Sentry.LLMInspection, Names: n, ProvisioningNamespace: g.Namespace, Namespace: *g.Sentry.Namespace, TenantName: g.Provisioning.DPUCluster.Name, TenantNamespace: g.Provisioning.DPUCluster.Namespace, ScopeKey: ScopeLabel, ScopeValue: prefix, PassthroughLabel: "sentry.l8k.nvidia.com/" + prefix + "-passthrough", TowerLabel: "sentry.l8k.nvidia.com/" + prefix + "-tower", PullSecret: g.Sentry.ImagePullSecret, MITMSecret: g.Sentry.MITMSecret, InferenceSecret: g.Sentry.InferenceSecret, IngressCIDR: g.Sentry.IngressCIDR, IngressIP: g.Sentry.IngressIP, IngressPrefix: p.Bits(), NodePort: *g.Sentry.PolicyServerNodePort, EnvoyTag: images["envoy"].TagDigest(), OpenFlowTag: images["openflow"].TagDigest(), PluginTag: images["plugin"].TagDigest(), TapTag: images["tap"].TagDigest(), ControllerTag: images["controller"].TagDigest(), ServerTag: images["server"].TagDigest(), ArgusImage: images["argus"].Reference()}
}

func softwareObject(g config.Generation, name string) *unstructured.Unstructured {
	pldm := map[string]interface{}{}
	for psid, a := range g.Provisioning.Software.PLDM {
		pldm[psid] = a.URL
	}
	spec := map[string]interface{}{"osIso": g.Provisioning.Software.OSISO.URL, "pldmFwBundle": pldm}
	if g.Provisioning.Software.NICFirmware != nil {
		spec["nicFw"] = g.Provisioning.Software.NICFirmware.URL
	}
	return object("provisioning.dpu.nvidia.com/v1alpha1", "BlueFieldSoftware", g.Namespace, name, spec)
}

func deviceSet(g config.Generation, d templateData, b config.DeviceBinding, bmc string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"strategy":          map[string]interface{}{"type": "OnDelete"},
		"dpuDeviceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{DeviceNodeLabel: b.DPUNode, DeviceBMCLabel: bmc}},
		"dpuTemplate": map[string]interface{}{"spec": map[string]interface{}{
			"blueFieldSoftware": map[string]interface{}{"name": d.Names.Software}, "dpuFlavor": d.Names.Flavor,
			"nodeEffect": map[string]interface{}{"noEffect": true},
			"cluster":    map[string]interface{}{"selector": map[string]interface{}{"matchLabels": stringMap(g.Provisioning.DPUCluster.Selector)}, "nodeLabels": map[string]interface{}{ScopeLabel: d.ScopeValue}},
		}},
	}
	return object("provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", g.Namespace, d.ScopeValue+"-"+shortHash(b.ID), spec)
}

func object(api, kind, namespace, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": api, "kind": kind, "metadata": map[string]interface{}{"name": name, "namespace": namespace}, "spec": spec}}
}

// bindObject completes explicit native tenant and chart inputs once. The DPF
// controller transformation for offline projections remains in effectiveHelmValues.
func bindObject(g config.Generation, d templateData, obj *unstructured.Unstructured) error {
	expectedNamespace := *g.Sentry.Namespace
	if obj.GetKind() == "DPUFlavor" {
		expectedNamespace = g.Namespace
	}
	if obj.GetNamespace() != expectedNamespace {
		return fmt.Errorf("resource requires explicit configured namespace")
	}
	if obj.GetKind() == "Secret" || obj.GetKind() == "DPUDevice" || obj.GetKind() == "DPUNode" {
		return fmt.Errorf("profile cannot create credentials or enrollment objects")
	}
	kind := obj.GetKind()
	if kind == "DPUServiceInterface" || kind == "DPUServiceNAD" || kind == "DPUServiceChain" || (kind == "DPUService" && obj.GetName() != d.Names.Server) {
		selector := map[string]interface{}{}
		for k, v := range g.Provisioning.DPUCluster.Selector {
			selector[k] = v
		}
		if err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{"matchLabels": selector}, "spec", "dpuClusterSelector"); err != nil {
			return err
		}
	}
	if kind != "DPUService" {
		return nil
	}
	if err := unstructured.SetNestedField(obj.Object, obj.GetName(), "spec", "helmChart", "source", "releaseName"); err != nil {
		return err
	}
	values, _, err := unstructured.NestedMap(obj.Object, "spec", "helmChart", "values")
	if err != nil {
		return err
	}
	values["fullnameOverride"] = obj.GetName()
	if obj.GetName() == d.Names.Tower {
		// DPF's ServiceInterfaceSet entry name is namespace_name; the SFC
		// controller uses that entry name as dpf-id on both patch peers.
		if err := unstructured.SetNestedField(values, obj.GetNamespace()+"_"+d.Names.Patch, "egress", "patchDPFID"); err != nil {
			return err
		}
	}
	if obj.GetName() == d.Names.Server {
		values["policyNamespace"] = *g.Sentry.Namespace
		values["dpuNamespace"] = g.Namespace
		values["mitmCA"] = map[string]interface{}{"secretName": g.Sentry.MITMSecret, "namespace": *g.Sentry.Namespace, "key": "ca.crt"}
	}
	return unstructured.SetNestedMap(obj.Object, values, "spec", "helmChart", "values")
}

func stages(provisioningNamespace, serviceNamespace string) []bundle.Stage {
	// Apply all three services before the first readiness barrier: Tower's
	// openflow watches the policy CRD installed by the controller chart, while
	// the controller programs Tower through its hostPath socket.
	ids := []string{"provisioning-artifacts", "provisioning", "passthrough", "services", "tower-workloads", "policy-controller-workloads", "argus-workloads", "policy-rbac", "policy-credentials", "policy-server"}
	out := make([]bundle.Stage, 0, len(ids))
	for i, id := range ids {
		s := bundle.Stage{ID: id, Destination: bundle.Management, Namespace: serviceNamespace}
		if id == "provisioning-artifacts" || id == "provisioning" {
			s.Namespace = provisioningNamespace
		}
		if id == "policy-rbac" || strings.HasSuffix(id, "-workloads") {
			s.Destination = bundle.DPU
		}
		if i > 0 {
			s.DependsOn = []string{ids[i-1]}
		}
		if id == "provisioning-artifacts" {
			s.Requires = []string{"existing-management", "existing-tenant", "enrolled-devices", "selected-scope", "external-transition-complete", "existing-secrets"}
		}
		if id == "provisioning" {
			s.Requires = []string{"software-integrity"}
		}
		out = append(out, s)
	}
	return out
}

func decodeDocuments(data []byte) ([]*unstructured.Unstructured, error) {
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		var obj map[string]interface{}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		if err := kjson.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		if len(obj) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		if u.GetAPIVersion() == "" || u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("document requires apiVersion, kind and metadata.name")
		}
		out = append(out, u)
	}
	return out, nil
}

func marshalDocuments(objects []*unstructured.Unstructured) ([]byte, error) {
	var out bytes.Buffer
	for i, obj := range objects {
		if i > 0 {
			out.WriteString("---\n")
		}
		b, err := yaml.Marshal(obj.Object)
		if err != nil {
			return nil, err
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}

func resource(destination bundle.Destination, obj *unstructured.Unstructured) bundle.ResourceRef {
	group := obj.GroupVersionKind().Group
	return bundle.ResourceRef{Destination: destination, Group: group, Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

func objectReferences(destination bundle.Destination, obj *unstructured.Unstructured) []bundle.Reference {
	var out []bundle.Reference
	add := func(group, kind, name string) {
		if name != "" {
			out = append(out, bundle.Reference{From: resource(destination, obj), To: bundle.ResourceRef{Destination: destination, Group: group, Kind: kind, Namespace: obj.GetNamespace(), Name: name}})
		}
	}
	switch obj.GetKind() {
	case "DPUService":
		interfaces, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "interfaces")
		for _, name := range interfaces {
			add("svc.dpu.nvidia.com", "DPUServiceInterface", name)
		}
	case "DPUServiceInterface":
		service, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "spec", "template", "spec", "service", "serviceID")
		network, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "spec", "template", "spec", "service", "network")
		add("svc.dpu.nvidia.com", "DPUService", service)
		add("svc.dpu.nvidia.com", "DPUServiceNAD", network)
	case "RoleBinding":
		kind, _, _ := unstructured.NestedString(obj.Object, "roleRef", "kind")
		name, _, _ := unstructured.NestedString(obj.Object, "roleRef", "name")
		if kind == "Role" {
			add("rbac.authorization.k8s.io", kind, name)
		}
	}
	return out
}

func shortHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:4]) }

func safeTemplateOutput(s string) bool {
	for _, forbidden := range []string{"${BFS_", "${DPU_", "${TOWER_", "${BMC_", "{{ .", "usermod --password", "ubuntu_PASSWORD=", "PermitRootLogin yes"} {
		if strings.Contains(s, forbidden) {
			return false
		}
	}
	return true
}

func stringMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
