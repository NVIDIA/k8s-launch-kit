// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sentry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yaml "sigs.k8s.io/yaml"
)

func TestSentry(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Sentry offline rendering") }

var _ = Describe("the BF4 Sentry renderer", func() {
	var g config.Generation
	var profile *profiles.ProfileMetadata
	var archives map[string][]byte
	BeforeEach(func() {
		g = generationFixture()
		profile = profileFixture()
		archives = archiveFixture()
	})
	It("freezes source-pinned charts, images and exact selected scope into a valid bundle", func() {
		r, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.SelectedDevices).To(Equal([]string{"device-a", "device-b"}))
		b, err := freezeFixture(r, g)
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Index().BundleDigest).NotTo(BeEmpty())
		for _, id := range []string{"tower-workloads", "policy-controller-workloads", "argus-workloads"} {
			s, ok := b.Stage(id)
			Expect(ok).To(BeTrue())
			Expect(s.Stage().Destination).To(Equal(bundle.DPU))
			Expect(s.Documents()).To(BeEmpty())
			Expect(s.Projections()).NotTo(BeEmpty())
		}
		sets := objectsOf(r, bundle.Manifest, "DPUSet")
		Expect(sets).To(HaveLen(2))
		for _, set := range sets {
			selector, _, err := unstructured.NestedStringMap(set.Object, "spec", "dpuDeviceSelector", "matchLabels")
			Expect(err).NotTo(HaveOccurred())
			Expect(selector).To(HaveLen(2))
			Expect(selector).To(HaveKeyWithValue(DeviceNodeLabel, "worker-a"))
			Expect(selector[DeviceBMCLabel]).To(BeElementOf("192.0.2.10", "192.0.2.11"))
			clusterSelector, found, err := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "selector", "matchLabels")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(clusterSelector).To(Equal(g.Provisioning.DPUCluster.Selector))
			_, hasName, _ := unstructured.NestedFieldNoCopy(set.Object, "spec", "dpuTemplate", "spec", "cluster", "name")
			Expect(hasName).To(BeFalse())
			_, hasNamespace, _ := unstructured.NestedFieldNoCopy(set.Object, "spec", "dpuTemplate", "spec", "cluster", "namespace")
			Expect(hasNamespace).To(BeFalse())
		}
		for _, kind := range []string{"DPUDevice", "DPUNode", "DPFOperatorConfig", "DPUCluster", "Secret"} {
			Expect(objectsOf(r, bundle.Manifest, kind)).To(BeEmpty())
		}
		Expect(objectsOf(r, bundle.Manifest, "Namespace")).To(BeEmpty())
		for _, kind := range []string{"DPUService", "DPUServiceInterface", "DPUServiceChain", "DPUServiceNAD", "DPUServiceCredentialRequest", "Role", "RoleBinding"} {
			for _, obj := range objectsOf(r, bundle.Manifest, kind) {
				Expect(obj.GetNamespace()).To(Equal(*g.Sentry.Namespace))
			}
		}
		for _, kind := range []string{"DPUService", "DPUServiceInterface", "DPUServiceChain", "DPUServiceNAD"} {
			for _, obj := range objectsOf(r, bundle.Manifest, kind) {
				inCluster, _, _ := unstructured.NestedBool(obj.Object, "spec", "deployInCluster")
				if inCluster {
					continue
				}
				selector, found, err := unstructured.NestedStringMap(obj.Object, "spec", "dpuClusterSelector", "matchLabels")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(selector).To(Equal(g.Provisioning.DPUCluster.Selector))
			}
		}
		flavor := objectsOf(r, bundle.Manifest, "DPUFlavor")[0]
		_, hasBFCfg, _ := unstructured.NestedSlice(flavor.Object, "spec", "bfcfgParameters")
		Expect(hasBFCfg).To(BeFalse())
		body, _ := json.Marshal(flavor.Object)
		Expect(string(body)).NotTo(ContainSubstring("PermitRootLogin"))
		Expect(string(body)).NotTo(ContainSubstring("usermod --password"))
		Expect(string(body)).NotTo(ContainSubstring("docatower$"))
		Expect(string(body)).To(ContainSubstring("br-envoy-vfs-monitor"))
	})
	It("projects the actual five-container Tower and the controller CRD without applying them", func() {
		r, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		var tower *unstructured.Unstructured
		for _, obj := range objectsOf(r, bundle.Projection, "DaemonSet") {
			if obj.GetName() == "site-a-tower" {
				tower = obj
			}
		}
		Expect(tower).NotTo(BeNil())
		containers, _, _ := unstructured.NestedSlice(tower.Object, "spec", "template", "spec", "containers")
		Expect(containers).To(HaveLen(5))
		for _, raw := range containers {
			c := raw.(map[string]interface{})
			Expect(c["image"]).To(ContainSubstring("@sha256:"))
		}
		selector, _, _ := unstructured.NestedMap(tower.Object, "spec", "template", "spec", "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution")
		Expect(selector).To(HaveKey("nodeSelectorTerms"))
		crds := objectsOf(r, bundle.Projection, "CustomResourceDefinition")
		Expect(crds).To(HaveLen(1))
		Expect(crds[0].GetName()).To(Equal("docatowerpolicies.tower.dpu.nvidia.com"))
		Expect(crds[0].GetNamespace()).To(BeEmpty())
		Expect(objectsOf(r, bundle.Projection, "Secret")).To(BeEmpty())
		for _, svc := range objectsOf(r, bundle.Manifest, "DPUService") {
			name, _, _ := unstructured.NestedString(svc.Object, "spec", "helmChart", "source", "releaseName")
			Expect(name).To(Equal(svc.GetName()))
			fullName, _, _ := unstructured.NestedString(svc.Object, "spec", "helmChart", "values", "fullnameOverride")
			Expect(fullName).To(Equal(svc.GetName()))
			if svc.GetName() == "site-a-policy-server" {
				policyNS, _, _ := unstructured.NestedString(svc.Object, "spec", "helmChart", "values", "policyNamespace")
				Expect(policyNS).To(Equal("site-a-services"))
				dpuNS, _, _ := unstructured.NestedString(svc.Object, "spec", "helmChart", "values", "dpuNamespace")
				Expect(dpuNS).To(Equal(g.Namespace))
				secret, _, _ := unstructured.NestedString(svc.Object, "spec", "helmChart", "values", "mitmCA", "secretName")
				Expect(secret).To(Equal("site-ca"))
			}
		}
	})
	It("creates the coupled services before any workload barrier and retains every downstream readiness stage", func() {
		r, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		b, err := freezeFixture(r, g)
		Expect(err).NotTo(HaveOccurred())
		stageIDs := []string{}
		for _, stage := range b.Index().Stages {
			stageIDs = append(stageIDs, stage.ID)
		}
		Expect(stageIDs).To(Equal([]string{"provisioning-artifacts", "provisioning", "passthrough", "services", "tower-workloads", "policy-controller-workloads", "argus-workloads", "policy-rbac", "policy-credentials", "policy-server"}))
		services, ok := b.Stage("services")
		Expect(ok).To(BeTrue())
		Expect(services.Stage().Destination).To(Equal(bundle.Management))
		Expect(services.Projections()).To(BeEmpty())
		createdServices := []string{}
		createdKinds := []string{}
		for _, doc := range services.Documents() {
			createdKinds = append(createdKinds, doc.Ref().Kind)
			if doc.Ref().Kind == "DPUService" {
				createdServices = append(createdServices, doc.Ref().Name)
			}
		}
		Expect(createdServices).To(Equal([]string{"site-a-tower", "site-a-controller", "site-a-argus"}))
		Expect(createdKinds).To(ContainElements("DPUServiceNAD", "DPUServiceInterface", "DPUServiceChain"))
		for index, stage := range b.Index().Stages {
			if index > 0 {
				Expect(stage.DependsOn).To(Equal([]string{stageIDs[index-1]}))
			}
			if strings.HasSuffix(stage.ID, "-workloads") {
				view, ok := b.Stage(stage.ID)
				Expect(ok).To(BeTrue())
				Expect(index).To(BeNumerically(">", 3))
				Expect(view.Documents()).To(BeEmpty())
				Expect(view.Projections()).NotTo(BeEmpty())
			}
		}
		controller, ok := b.Stage("policy-controller-workloads")
		Expect(ok).To(BeTrue())
		projectionKinds := []string{}
		for _, doc := range controller.Projections() {
			projectionKinds = append(projectionKinds, doc.Ref().Kind)
		}
		Expect(projectionKinds).To(ContainElement("CustomResourceDefinition"))
	})
	It("binds the egress patch lookup to the actual generated interface for each namespace, instance and release", func() {
		for _, release := range []string{"26.8.0-rc.1", "26.8.0"} {
			for _, site := range []struct{ name, namespace string }{{"site-a", "site-a-services"}, {"other-site", "isolated-services"}} {
				g.Release, g.Name, g.Sentry.Namespace = release, site.name, &site.namespace
				r, err := Render(g, profile, archives)
				Expect(err).NotTo(HaveOccurred())
				patchID := ""
				for _, iface := range objectsOf(r, bundle.Manifest, "DPUServiceInterface") {
					kind, _, _ := unstructured.NestedString(iface.Object, "spec", "template", "spec", "template", "spec", "interfaceType")
					if kind == "patch" {
						patchID = iface.GetNamespace() + "_" + iface.GetName()
					}
				}
				Expect(patchID).To(Equal(site.namespace + "_" + site.name + "-patch"))
				for _, service := range objectsOf(r, bundle.Manifest, "DPUService") {
					if service.GetName() == site.name+"-tower" {
						actual, found, err := unstructured.NestedString(service.Object, "spec", "helmChart", "values", "egress", "patchDPFID")
						Expect(err).NotTo(HaveOccurred())
						Expect(found).To(BeTrue())
						Expect(actual).To(Equal(patchID))
					}
				}
				for _, file := range r.Files {
					if file.Path == "values/"+site.name+"-tower.yaml" {
						var values map[string]interface{}
						Expect(yaml.Unmarshal(file.Data, &values)).To(Succeed())
						actual, _, _ := unstructured.NestedString(values, "egress", "patchDPFID")
						Expect(actual).To(Equal(patchID))
					}
				}
				for _, ds := range objectsOf(r, bundle.Projection, "DaemonSet") {
					if ds.GetName() != site.name+"-tower" {
						continue
					}
					containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
					for _, raw := range containers {
						container := raw.(map[string]interface{})
						if container["name"] == "tower-egress" {
							env, _, _ := unstructured.NestedSlice(container, "env")
							Expect(env).To(ContainElement(map[string]interface{}{"name": "PATCH_DPF_ID", "value": patchID}))
						}
					}
				}
			}
		}
	})
	It("has deterministic concurrent output and never mutates caller-owned inputs", func() {
		before, _ := json.Marshal(g)
		one, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		type result struct {
			r   Rendered
			err error
		}
		results := make(chan result, 3)
		for range 3 {
			go func() { r, err := Render(g, profile, archives); results <- result{r, err} }()
		}
		for range 3 {
			other := <-results
			Expect(other.err).NotTo(HaveOccurred())
			Expect(other.r).To(Equal(one))
		}
		after, _ := json.Marshal(g)
		Expect(after).To(Equal(before))
		for _, f := range one.Files {
			if strings.HasSuffix(f.Path, ".yaml") {
				Expect(safeTemplateOutput(string(f.Data))).To(BeTrue())
			}
		}
	})
	It("keeps a strict subset exact and rejects duplicate physical selectors", func() {
		g.Provisioning.Devices = g.Provisioning.Devices[:1]
		g.Devices = g.Devices[:1]
		r, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.SelectedDevices).To(Equal([]string{"device-b"}))
		Expect(objectsOf(r, bundle.Manifest, "DPUSet")).To(HaveLen(1))
		g = generationFixture()
		g.Devices[1].BMCEndpoint = g.Devices[0].BMCEndpoint
		r, err = Render(g, profile, archives)
		Expect(err).To(MatchError(ContainSubstring("duplicate node/BMC selector")))
		Expect(r.Files).To(BeEmpty())
	})
	It("rejects missing or tampered chart bytes before returning any output", func() {
		delete(archives, "doca-argus")
		r, err := Render(g, profile, archives)
		Expect(err).To(HaveOccurred())
		Expect(r.Files).To(BeEmpty())
		archives = archiveFixture()
		archives["doca-tower"][20] ^= 0xff
		r, err = Render(g, profile, archives)
		Expect(err).To(MatchError(ContainSubstring("SHA-256 mismatch")))
		Expect(r.Files).To(BeEmpty())
	})
	It("rejects unresolved substitutions, lab credentials and dependency identity changes", func() {
		for _, injection := range []string{"${TOWER_INGRESS_IP}", "{{ .MissingRequiredValue }}", "usermod --password accidental"} {
			p := copyProfile(profile)
			body, err := os.ReadFile(p.TemplatePaths[0])
			Expect(err).NotTo(HaveOccurred())
			path := filepath.Join(GinkgoT().TempDir(), "10-flavor.yaml")
			Expect(os.WriteFile(path, append(body, []byte("\n# "+injection+"\n")...), 0600)).To(Succeed())
			p.TemplatePaths[0] = path
			r, err := Render(g, p, archives)
			Expect(err).To(HaveOccurred())
			Expect(r.Files).To(BeEmpty())
		}
		p := copyProfile(profile)
		for i, path := range p.TemplatePaths {
			if filepath.Base(path) == "32-tower.yaml" {
				body, err := os.ReadFile(path)
				Expect(err).NotTo(HaveOccurred())
				body = bytes.ReplaceAll(body, []byte("version: \"0.2.16\""), []byte("version: \"0.0.0\""))
				path = filepath.Join(GinkgoT().TempDir(), "32-tower.yaml")
				Expect(os.WriteFile(path, body, 0600)).To(Succeed())
				p.TemplatePaths[i] = path
			}
		}
		r, err := Render(g, p, archives)
		Expect(err).To(MatchError(ContainSubstring("unlocked chart")))
		Expect(r.Files).To(BeEmpty())
	})
	It("does not open deployment credentials or require a live API", func() {
		// Deliberately unreadable/nonexistent references cannot affect offline rendering.
		g.KubeconfigRef = config.FileRef{File: "/nonexistent/never-read/kubeconfig"}
		g.Context = "no-live-context"
		g.Sentry.ImagePullSecret = "not-readable-at-generation"
		g.Sentry.MITMSecret = "not-readable-at-generation"
		g.Sentry.InferenceSecret = "not-readable-at-generation"
		r, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Files).NotTo(BeEmpty())
	})
	It("checks exact release OpenAPI unknown fields and CEL invariants before returning a bundle", func() {
		for _, release := range []string{"26.8.0-rc.1", "26.8.0"} {
			g.Release = release
			r, err := Render(g, profile, archives)
			Expect(err).NotTo(HaveOccurred())
			set := objectsOf(r, bundle.Manifest, "DPUSet")[0]
			Expect(unstructured.SetNestedField(set.Object, "unsupported-tenant-name", "spec", "dpuTemplate", "spec", "cluster", "name")).To(Succeed())
			schema, err := releaseSchema(release, "provisioning.dpu.nvidia.com", "DPUSet", "v1alpha1")
			Expect(err).NotTo(HaveOccurred())
			Expect(schema.validate(set)).To(MatchError(ContainSubstring("unknown schema fields: spec.dpuTemplate.spec.cluster.name")))
			for _, server := range objectsOf(r, bundle.Manifest, "DPUService") {
				if server.GetName() == "site-a-policy-server" {
					Expect(unstructured.SetNestedMap(server.Object, map[string]interface{}{"matchLabels": map[string]interface{}{"site.example/tenant": "tenant-a"}}, "spec", "dpuClusterSelector")).To(Succeed())
					schema, err = releaseSchema(release, "svc.dpu.nvidia.com", "DPUService", "v1alpha1")
					Expect(err).NotTo(HaveOccurred())
					Expect(schema.validate(server)).To(MatchError(ContainSubstring("CEL validation")))
				}
			}
			_, err = releaseSchema(release, "svc.dpu.nvidia.com", "DPUService", "v9")
			Expect(err).To(MatchError(ContainSubstring("schema identity mismatch")))
		}
	})
	It("uses the captured API version and a frozen capability list for offline Helm projection", func() {
		c := &chart.Chart{Metadata: &chart.Metadata{Name: "capability-check", Version: "1.0.0"}, Templates: []*chart.File{{Name: "templates/version.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: captured-version\ndata:\n  version: {{ .Capabilities.KubeVersion.Version | quote }}\n")}}}
		for _, version := range []string{g.ManagementKubernetesVersion, g.Provisioning.DPUCluster.KubernetesVersion} {
			objects, err := projectChart(c, map[string]interface{}{}, "capability-check", *g.Sentry.Namespace, version)
			Expect(err).NotTo(HaveOccurred())
			actual, _, _ := unstructured.NestedString(objects[0].Object, "data", "version")
			Expect(actual).To(Equal(version))
		}
		_, err := projectChart(c, map[string]interface{}{}, "capability-check", *g.Sentry.Namespace, "")
		Expect(err).To(HaveOccurred())
		r, err := Render(g, profile, archives)
		Expect(err).NotTo(HaveOccurred())
		var capabilities []byte
		for _, f := range r.Files {
			if f.Path == "metadata/helm-capabilities.yaml" {
				capabilities = f.Data
			}
		}
		Expect(string(capabilities)).To(ContainSubstring(g.ManagementKubernetesVersion))
		Expect(string(capabilities)).To(ContainSubstring(g.Provisioning.DPUCluster.KubernetesVersion))
		Expect(string(capabilities)).To(ContainSubstring("apiextensions.k8s.io/v1"))
	})
	It("renders core policy without inference credentials and keeps ext_authz fail-closed in both modes", func() {
		for _, enabled := range []bool{false, true} {
			g = generationFixture()
			g.Sentry.LLMInspection = &enabled
			if !enabled {
				g.Sentry.InferenceSecret = ""
			}
			r, err := Render(g, profile, archives)
			Expect(err).NotTo(HaveOccurred())
			for _, tower := range objectsOf(r, bundle.Projection, "DaemonSet") {
				if tower.GetName() == "site-a-tower" {
					containers, _, _ := unstructured.NestedSlice(tower.Object, "spec", "template", "spec", "containers")
					expected := 4
					if enabled {
						expected = 5
					}
					Expect(containers).To(HaveLen(expected))
					var tap map[string]interface{}
					for _, raw := range containers {
						container := raw.(map[string]interface{})
						if container["name"] == "tower-tap" {
							tap = container
						}
					}
					if enabled {
						Expect(tap).NotTo(BeNil())
						env, _, _ := unstructured.NestedSlice(tap, "env")
						Expect(env).To(ContainElement(HaveKeyWithValue("name", "INFERENCE_API_KEY")))
					} else {
						Expect(tap).To(BeNil())
					}
				}
			}
			for _, tower := range objectsOf(r, bundle.Manifest, "DPUService") {
				if tower.GetName() == "site-a-tower" {
					tapEnabled, _, _ := unstructured.NestedBool(tower.Object, "spec", "helmChart", "values", "tap", "enabled")
					Expect(tapEnabled).To(Equal(enabled))
					_, hasInference, _ := unstructured.NestedFieldNoCopy(tower.Object, "spec", "helmChart", "values", "tap", "inferenceSecret")
					Expect(hasInference).To(Equal(enabled))
				}
			}
			for _, cm := range objectsOf(r, bundle.Projection, "ConfigMap") {
				if cm.GetName() == "site-a-tower" {
					configBody, _, _ := unstructured.NestedString(cm.Object, "data", "envoy.yaml")
					var envoy map[string]interface{}
					Expect(yaml.Unmarshal([]byte(configBody), &envoy)).To(Succeed())
					listeners, _, _ := unstructured.NestedSlice(envoy, "static_resources", "listeners")
					authzCount, optionalHookCount := 0, 0
					for _, listener := range listeners {
						chains, _, _ := unstructured.NestedSlice(listener.(map[string]interface{}), "filter_chains")
						for _, chain := range chains {
							filters, _, _ := unstructured.NestedSlice(chain.(map[string]interface{}), "filters")
							for _, filter := range filters {
								httpFilters, _, _ := unstructured.NestedSlice(filter.(map[string]interface{}), "typed_config", "http_filters")
								for _, raw := range httpFilters {
									f := raw.(map[string]interface{})
									allow, _, _ := unstructured.NestedBool(f, "typed_config", "failure_mode_allow")
									if f["name"] == "envoy.filters.http.ext_authz" {
										authzCount++
										Expect(allow).To(BeFalse())
									}
									if f["name"] == "envoy.filters.http.ext_proc" {
										optionalHookCount++
										Expect(allow).To(BeTrue())
									}
								}
							}
						}
					}
					Expect(authzCount).To(Equal(2))
					Expect(optionalHookCount).To(Equal(2))
				}
			}
		}
		g = generationFixture()
		g.Sentry.InferenceSecret = ""
		r, err := Render(g, profile, archives)
		Expect(err).To(MatchError(ContainSubstring("LLM inspection requires")))
		Expect(r.Files).To(BeEmpty())
	})
})

func generationFixture() config.Generation {
	port := 30080
	namespace := "site-a-services"
	inspection := true
	return config.Generation{Name: "site-a", Profile: "sentry-bf4", Release: "26.8.0-rc.1", Namespace: "sentry-system", ManagementKubernetesVersion: "v1.35.4", ManagementUID: "management-uid", Context: "management", KubeconfigRef: config.FileRef{File: "/never-read/kubeconfig"},
		Provisioning: config.Provisioning{DPUCluster: config.ClusterBinding{Name: "tenant-a", Namespace: "tenant-system", KubernetesVersion: "v1.35.6", ClusterUID: "tenant-uid", Selector: map[string]string{"site.example/tenant": "tenant-a"}}, Devices: []config.DeviceBinding{{ID: "device-b", DPUDevice: "serial-b", DPUNode: "worker-a"}, {ID: "device-a", DPUDevice: "serial-a", DPUNode: "worker-a"}}, Software: config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("1", 64)}, PLDM: map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/firmware.fwpkg", SHA256: strings.Repeat("2", 64)}}}},
		Sentry:       config.Sentry{LLMInspection: &inspection, Namespace: &namespace, IngressCIDR: "192.168.100.0/24", IngressIP: "192.168.100.1", ImagePullSecret: "registry-pull", MITMSecret: "site-ca", InferenceSecret: "inference-api", PolicyServerAddress: "192.0.2.100", PolicyServerNodePort: &port},
		Devices:      []inventory.Device{{ID: "device-b", BMCEndpoint: "https://192.0.2.11", SerialNumber: "SERIAL-B"}, {ID: "device-a", BMCEndpoint: "https://192.0.2.10", SerialNumber: "SERIAL-A"}},
	}
}

func profileFixture() *profiles.ProfileMetadata {
	root, err := filepath.Abs("../../../profiles/sentry-bf4")
	Expect(err).NotTo(HaveOccurred())
	p := &profiles.ProfileMetadata{Target: target.DPF, Directory: root, Name: "BlueField-4 Sentry"}
	for _, s := range templateSpecs {
		p.TemplatePaths = append(p.TemplatePaths, filepath.Join(root, s.filename))
	}
	return p
}

func archiveFixture() map[string][]byte {
	out := map[string][]byte{}
	for _, lock := range Catalog() {
		data, err := os.ReadFile(filepath.Join("testdata", "charts", lock.Filename))
		Expect(err).NotTo(HaveOccurred())
		out[lock.Name] = data
	}
	return out
}

func objectsOf(r Rendered, role bundle.Role, kind string) []*unstructured.Unstructured {
	roles := map[string]bundle.Role{}
	for _, rec := range r.Records {
		roles[rec.Path] = rec.Role
	}
	var out []*unstructured.Unstructured
	for _, f := range r.Files {
		if roles[f.Path] != role {
			continue
		}
		objects, err := decodeDocuments(f.Data)
		Expect(err).NotTo(HaveOccurred())
		for _, obj := range objects {
			if obj.GetKind() == kind {
				out = append(out, obj)
			}
		}
	}
	return out
}

func freezeFixture(r Rendered, g config.Generation) (*bundle.Bundle, error) {
	r.Files = append(r.Files, bundle.File{Path: bundle.ResolvedConfigPath, Data: []byte("formatVersion: v1alpha1\ntarget: dpf\nconfig: {}\n")})
	r.Records = append(r.Records, bundle.FileRecord{Path: bundle.ResolvedConfigPath, Role: bundle.ResolvedConfig})
	return bundle.New(bundle.Index{FormatVersion: bundle.FormatVersion, Target: bundle.Target, Phase: bundle.Phase, Profile: g.Profile, Release: g.Release, Renderer: RendererVersion, SelectedDevices: r.SelectedDevices, Files: r.Records, Stages: r.Stages, References: r.References, ExternalPrerequisites: r.ExternalPrerequisites}, r.Files)
}

func copyProfile(p *profiles.ProfileMetadata) *profiles.ProfileMetadata {
	c := *p
	c.TemplatePaths = append([]string(nil), p.TemplatePaths...)
	return &c
}
