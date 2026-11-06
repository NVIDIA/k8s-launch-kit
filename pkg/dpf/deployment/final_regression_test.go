// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"text/template"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Use the actual profile RBAC document so this end-to-end fixture cannot omit
// the top-level rules/subjects/roleRef fields which differ from DPF spec APIs.
func policyRBACBundle(b *bundle.Bundle) *bundle.Bundle {
	data, err := os.ReadFile("../../../profiles/sentry-bf4/60-policy-rbac.yaml")
	Expect(err).NotTo(HaveOccurred())
	parsed, err := template.New("policy-rbac").Parse(string(data))
	Expect(err).NotTo(HaveOccurred())
	var output bytes.Buffer
	Expect(parsed.Execute(&output, map[string]any{"Names": map[string]string{"Server": "policy-server"}, "Namespace": "sentry-system"})).To(Succeed())
	account := []byte("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: policy-server\n  namespace: sentry-system\nimagePullSecrets:\n  - name: pull\nautomountServiceAccountToken: false\n")
	index := b.Index()
	index.BundleDigest = ""
	index.Files = append(index.Files, bundle.FileRecord{Path: "objects/policy-account.yaml", Role: bundle.Manifest}, bundle.FileRecord{Path: "objects/policy-rbac.yaml", Role: bundle.Manifest})
	index.Stages = append(index.Stages, bundle.Stage{ID: "policy-rbac", Destination: bundle.DPU, Namespace: "sentry-system", Files: []string{"objects/policy-account.yaml", "objects/policy-rbac.yaml"}, DependsOn: []string{"provision"}})
	index.References = append(index.References, bundle.Reference{From: ref(bundle.DPU, "rbac.authorization.k8s.io", "RoleBinding", "sentry-system", "policy-server"), To: ref(bundle.DPU, "rbac.authorization.k8s.io", "Role", "sentry-system", "policy-server")})
	files := append(b.Files(), bundle.File{Path: "objects/policy-account.yaml", Data: account}, bundle.File{Path: "objects/policy-rbac.yaml", Data: output.Bytes()})
	result, err := bundle.New(index, files)
	Expect(err).NotTo(HaveOccurred())
	return result
}

var _ = Describe("full native lifecycle convergence regressions", func() {
	runnerFor := func(f *fakeFactory, v *fakeArtifacts) Runner {
		dir, err := os.MkdirTemp("", "dpf-final-regression-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		return Runner{Clients: f, Artifacts: v, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
	}
	It("creates and resumes actual native policy RBAC and explicit ServiceAccount intent", func() {
		b, g, f, v := lifecycleFixture()
		b = policyRBACBundle(b)
		r := runnerFor(f, v)
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.tenant.creates).To(Equal([]string{"ServiceAccount/policy-server", "Role/policy-server", "RoleBinding/policy-server"}))
		role := f.tenant.objects[objectKey(ref("", "rbac.authorization.k8s.io", "Role", "sentry-system", "policy-server"))]
		rules, _, err := unstructured.NestedSlice(role.Object, "rules")
		Expect(err).NotTo(HaveOccurred())
		Expect(rules).To(HaveLen(2))
		Expect(rules[0].(map[string]any)["resources"]).To(Equal([]any{"docatowerpolicies"}))
		result, err = r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.tenant.creates).To(HaveLen(3))
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.tenant.creates).To(HaveLen(3))
	})
	It("accepts only documented native RBAC defaults and rejects privilege or identity changes", func() {
		b, _, _, _ := lifecycleFixture()
		b = policyRBACBundle(b)
		for _, kind := range []string{"ServiceAccount", "Role", "RoleBinding"} {
			desired := resourcesInBundle(b, kind)[0].ObjectCopy()
			actual := desired.DeepCopy()
			if kind == "RoleBinding" {
				unstructured.RemoveNestedField(desired.Object, "roleRef", "apiGroup")
				subjects, _, _ := unstructured.NestedSlice(actual.Object, "subjects")
				subjects[0].(map[string]any)["apiGroup"] = ""
				Expect(unstructured.SetNestedSlice(actual.Object, subjects, "subjects")).To(Succeed())
			}
			Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeTrue(), kind)
			switch kind {
			case "Role":
				rules, _, _ := unstructured.NestedSlice(actual.Object, "rules")
				rules[0].(map[string]any)["verbs"] = []any{"*"}
				Expect(unstructured.SetNestedSlice(actual.Object, rules, "rules")).To(Succeed())
			case "RoleBinding":
				Expect(unstructured.SetNestedField(actual.Object, "foreign-role", "roleRef", "name")).To(Succeed())
			case "ServiceAccount":
				Expect(unstructured.SetNestedField(actual.Object, true, "automountServiceAccountToken")).To(Succeed())
			}
			Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse(), kind)
		}
	})
	It("rejects additional RoleBinding subjects and unsupported native kinds", func() {
		b, _, _, _ := lifecycleFixture()
		b = policyRBACBundle(b)
		desired := resourcesInBundle(b, "RoleBinding")[0].ObjectCopy()
		actual := desired.DeepCopy()
		subjects, _, _ := unstructured.NestedSlice(actual.Object, "subjects")
		subjects = append(subjects, map[string]any{"kind": "ServiceAccount", "name": "other", "namespace": "sentry-system"})
		Expect(unstructured.SetNestedSlice(actual.Object, subjects, "subjects")).To(Succeed())
		Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse())
		arbitrary := testObject("v1", "ConfigMap", "sentry-system", "unsupported", nil)
		Expect(nativeMatches("26.8.0-rc.1", arbitrary, arbitrary.DeepCopy())).To(BeFalse())
	})
	DescribeTable("blocks late DPUNode transitions after observing an otherwise Ready child", func(change string) {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		dpuNode := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", "dpf-system", "node-a"))]
		f.tenant.onGet = func(reference bundle.ResourceRef) {
			if reference.Kind != "Node" {
				return
			}
			switch change {
			case "annotation":
				dpuNode.SetAnnotations(map[string]string{"provisioning.dpu.nvidia.com/dpunode-external-reboot-required": "SystemLevelReset"})
			case "flag":
				Expect(unstructured.SetNestedField(dpuNode.Object, true, "status", "rebootInProgress")).To(Succeed())
			case "method":
				Expect(unstructured.SetNestedMap(dpuNode.Object, map[string]any{"script": map[string]any{"name": "late-executor"}}, "spec", "nodeRebootMethod")).To(Succeed())
			}
		}
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).To(HaveOccurred())
		Expect(result.Outcome).NotTo(Equal("complete"))
		if change == "method" {
			Expect(err).To(MatchError(ContainSubstring("NODE_REBOOT_METHOD_UNSUPPORTED")))
		} else {
			Expect(err).To(MatchError(ContainSubstring("NODE_TRANSITION_PENDING")))
		}
	}, Entry("external marker", "annotation"), Entry("active reboot flag", "flag"), Entry("executor changed", "method"))
	It("provisions first tenant workers before requiring DNS and polls DNS before completion", func() {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		dns := f.tenant.objects[objectKey(ref("", "apps", "Deployment", "kube-system", "coredns"))]
		Expect(unstructured.SetNestedField(dns.Object, int64(0), "status", "availableReplicas")).To(Succeed())
		nodes, err := f.tenant.List(context.Background(), ref("", "", "Node", "", ""), "")
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).To(BeEmpty())
		reads := 0
		f.tenant.onList = func(reference bundle.ResourceRef) {
			if reference.Kind != "Deployment" || reference.Namespace != "kube-system" {
				return
			}
			reads++
			Expect(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", "dpf-system", "selected-set"))]).NotTo(BeNil())
			Expect(f.tenant.objects[objectKey(ref("", "", "Node", "", "dpu-child"))]).NotTo(BeNil())
			if reads >= 2 {
				Expect(unstructured.SetNestedField(dns.Object, int64(1), "status", "availableReplicas")).To(Succeed())
			}
		}
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(reads).To(BeNumerically(">=", 2))
		Expect(result.Checks).To(ContainElement(HaveField("ReasonCode", "TENANT_DNS_NOT_READY")))
	})
	It("does not complete validation while DNS remains unavailable after provisioning", func() {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		dns := f.tenant.objects[objectKey(ref("", "apps", "Deployment", "kube-system", "coredns"))]
		Expect(unstructured.SetNestedField(dns.Object, int64(0), "status", "availableReplicas")).To(Succeed())
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("TENANT_DNS_NOT_READY")))
		Expect(result.Outcome).To(Equal("failed"))
	})
	It("keeps dry-run admission independent of runtime DNS availability with an explicit non-evaluation", func() {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		delete(f.tenant.objects, objectKey(ref("", "apps", "Deployment", "kube-system", "coredns")))
		result, err := r.Run(context.Background(), b, g, DryRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(result.Checks).To(ContainElement(And(HaveField("State", "not-evaluated"), HaveField("ReasonCode", "RUNTIME_READINESS_NOT_EVALUATED"))))
	})
})
