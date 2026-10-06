// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const ra24NCTCRDName = "nicconfigurationtemplates.configuration.net.nvidia.com"

func ra24DeploymentNCT() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "configuration.net.nvidia.com/v1alpha1", "kind": "NicConfigurationTemplate",
		"metadata": map[string]any{"name": "spectrum-x", "namespace": "network-operator"},
		"spec":     map[string]any{"template": map[string]any{"spectrumXOptimized": map[string]any{"version": "RA2.4", "platformType": "gb300"}}},
	}}
}

func ra24DeploymentCRD(version, fieldType string, served bool) *apiextv1.CustomResourceDefinition {
	leaf := apiextv1.JSONSchemaProps{Type: fieldType}
	for _, name := range []string{"platformType", "spectrumXOptimized", "template", "spec"} {
		leaf = apiextv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{name: leaf}}
	}
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: ra24NCTCRDName},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "configuration.net.nvidia.com", Names: apiextv1.CustomResourceDefinitionNames{Kind: "NicConfigurationTemplate", Plural: "nicconfigurationtemplates"},
			Scope:    apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: version, Served: served, Storage: true, Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &leaf}}},
		},
	}
}

func ra24DeploymentClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	gomega.Expect(apiextv1.AddToScheme(scheme)).To(gomega.Succeed())
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func ra24DeploymentBundle(includeNCP bool) *bundle.Bundle {
	raw, err := ra24DeploymentNCT().MarshalJSON()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	files := []bundle.File{
		{Name: "25-nit.yaml", Content: "apiVersion: configuration.net.nvidia.com/v1alpha1\nkind: NicInterfaceNameTemplate\nmetadata:\n  name: rename\n  namespace: network-operator\n"},
		{Name: "28-cm.yaml", Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dospcx\n  namespace: network-operator\n"},
		{Name: "30-nct.yaml", Content: string(raw)},
	}
	if includeNCP {
		files = append(files, bundle.File{Name: "10-ncp.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\n"})
	}
	artifacts, err := bundle.FromFiles(files)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return artifacts
}

var _ = ginkgo.Describe("Spectrum-X deployment API compatibility", func() {
	for _, test := range []struct {
		name       string
		crd        *apiextv1.CustomResourceDefinition
		compatible bool
	}{
		{name: "accepts the matching served string field", crd: ra24DeploymentCRD("v1alpha1", "string", true), compatible: true},
		{name: "rejects a missing CRD", compatible: false},
		{name: "rejects an unserved version", crd: ra24DeploymentCRD("v1alpha1", "string", false), compatible: false},
		{name: "rejects another API version", crd: ra24DeploymentCRD("v1alpha2", "string", true), compatible: false},
		{name: "rejects an incompatible field type", crd: ra24DeploymentCRD("v1alpha1", "integer", true), compatible: false},
	} {
		ginkgo.It(test.name, func() {
			var objects []client.Object
			if test.crd != nil {
				objects = append(objects, test.crd)
			}
			err := validateSpectrumXDeploymentAPI(context.Background(), ra24DeploymentClient(objects...), []*unstructured.Unstructured{ra24DeploymentNCT()})
			if test.compatible {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			} else {
				gomega.Expect(err).To(gomega.HaveOccurred())
			}
		})
	}

	ginkgo.It("rejects a schema that prunes platformType", func() {
		crd := ra24DeploymentCRD("v1alpha1", "string", true)
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties = nil
		err := validateSpectrumXDeploymentAPI(context.Background(), ra24DeploymentClient(crd), []*unstructured.Unstructured{ra24DeploymentNCT()})
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("platformType"))
	})

	ginkgo.It("requires a client for RA2.4 but skips legacy and unrelated templates", func() {
		nct := ra24DeploymentNCT()
		gomega.Expect(validateSpectrumXDeploymentAPI(context.Background(), nil, []*unstructured.Unstructured{nct})).To(gomega.HaveOccurred())
		unstructured.RemoveNestedField(nct.Object, "spec", "template", "spectrumXOptimized", "platformType")
		gomega.Expect(unstructured.SetNestedField(nct.Object, "RA2.3", "spec", "template", "spectrumXOptimized", "version")).To(gomega.Succeed())
		gomega.Expect(validateSpectrumXDeploymentAPI(context.Background(), nil, []*unstructured.Unstructured{nct})).To(gomega.Succeed())
		nct.SetAPIVersion("example.invalid/v1alpha1")
		gomega.Expect(unstructured.SetNestedField(nct.Object, "RA2.4", "spec", "template", "spectrumXOptimized", "version")).To(gomega.Succeed())
		gomega.Expect(validateSpectrumXDeploymentAPI(context.Background(), nil, []*unstructured.Unstructured{nct})).To(gomega.Succeed())
	})

	ginkgo.It("detects RA2.4 even before a platformType is supplied", func() {
		nct := ra24DeploymentNCT()
		unstructured.RemoveNestedField(nct.Object, "spec", "template", "spectrumXOptimized", "platformType")
		gomega.Expect(validateSpectrumXDeploymentAPI(context.Background(), nil, []*unstructured.Unstructured{nct})).To(gomega.HaveOccurred())
	})

	ginkgo.It("also detects platformType without a version", func() {
		nct := ra24DeploymentNCT()
		unstructured.RemoveNestedField(nct.Object, "spec", "template", "spectrumXOptimized", "version")
		gomega.Expect(validateSpectrumXDeploymentAPI(context.Background(), nil, []*unstructured.Unstructured{nct})).To(gomega.HaveOccurred())
	})

	ginkgo.It("preserves forbidden and transport errors", func() {
		forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, ra24NCTCRDName, errors.New("denied"))
		for _, cause := range []error{forbidden, errors.New("connection reset")} {
			c := interceptor.NewClient(ra24DeploymentClient(), interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return cause
				},
			})
			err := validateSpectrumXDeploymentAPI(context.Background(), c, []*unstructured.Unstructured{ra24DeploymentNCT()})
			gomega.Expect(errors.Is(err, cause)).To(gomega.BeTrue())
		}
	})

	for _, opts := range []DeployOptions{
		{}, {DryRun: true}, {SkipHelmChart: true}, {OverwriteExisting: true},
		{DryRun: true, SkipHelmChart: true, OverwriteExisting: true},
	} {
		ginkgo.It(fmt.Sprintf("blocks every additional apply for incompatible standalone deployment (dry-run=%t skip-helm=%t overwrite=%t)", opts.DryRun, opts.SkipHelmChart, opts.OverwriteExisting), func() {
			applies := 0
			c := interceptor.NewClient(ra24DeploymentClient(ra24DeploymentCRD("v1alpha1", "integer", true)), interceptor.Funcs{
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					applies++
					return nil
				},
			})
			err := ApplyBundle(context.Background(), c, ra24DeploymentBundle(false), opts)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("platformType"))
			gomega.Expect(applies).To(gomega.BeZero())
		})
	}

	ginkgo.It("permits server dry-run against an already compatible API", func() {
		reachedApply := errors.New("reached dry-run apply")
		c := interceptor.NewClient(ra24DeploymentClient(ra24DeploymentCRD("v1alpha1", "string", true)), interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				appliedOptions := (&client.ApplyOptions{}).ApplyOptions(opts)
				gomega.Expect(appliedOptions.DryRun).To(gomega.Equal([]string{metav1.DryRunAll}))
				return reachedApply
			},
		})
		err := ApplyBundle(context.Background(), c, ra24DeploymentBundle(false), DeployOptions{DryRun: true})
		gomega.Expect(errors.Is(err, reachedApply)).To(gomega.BeTrue())
	})

	ginkgo.It("waits for NCP reconciliation before checking freshly installed APIs", func() {
		ncpApplied := false
		ncpReadyObserved := false
		schemaChecked := false
		reachedAdditionalApply := errors.New("reached first additional manifest")
		c := interceptor.NewClient(ra24DeploymentClient(), interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if crd, ok := obj.(*apiextv1.CustomResourceDefinition); ok {
					gomega.Expect(ncpApplied).To(gomega.BeTrue())
					gomega.Expect(ncpReadyObserved).To(gomega.BeTrue())
					gomega.Expect(key.Name).To(gomega.Equal(ra24NCTCRDName))
					*crd = *ra24DeploymentCRD("v1alpha1", "string", true)
					schemaChecked = true
					return nil
				}
				if obj.GetObjectKind().GroupVersionKind().Kind == "NicClusterPolicy" && ncpApplied {
					ncp := obj.(*unstructured.Unstructured)
					ncp.SetName(key.Name)
					ncp.SetResourceVersion("2")
					gomega.Expect(unstructured.SetNestedField(ncp.Object, "ready", "status", "state")).To(gomega.Succeed())
					ncpReadyObserved = true
					return nil
				}
				return apierrors.NewNotFound(schema.GroupResource{Resource: "test"}, key.Name)
			},
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applied, ok := obj.(client.Object)
				gomega.Expect(ok).To(gomega.BeTrue())
				if applied.GetObjectKind().GroupVersionKind().Kind == "NicClusterPolicy" {
					ncpApplied = true
					applied.SetGeneration(1)
					applied.SetResourceVersion("1")
					return nil
				}
				gomega.Expect(schemaChecked).To(gomega.BeTrue())
				return reachedAdditionalApply
			},
		})
		err := ApplyBundle(context.Background(), c, ra24DeploymentBundle(true), DeployOptions{})
		gomega.Expect(errors.Is(err, reachedAdditionalApply)).To(gomega.BeTrue())
		gomega.Expect(schemaChecked).To(gomega.BeTrue())
	})
})
