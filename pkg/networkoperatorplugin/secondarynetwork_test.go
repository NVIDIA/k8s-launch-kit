// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSecondaryNetworkAllKubernetesNCPTemplates(t *testing.T) {
	for _, name := range []string{"sriov-ib-rdma", "sriov-ethernet-rdma", "ipoib-rdma-shared", "macvlan-rdma-shared", "host-device-rdma", "spectrum-x", "spectrum-x-ra2.1", "spectrum-x-ra2.2", "spectrum-x-ra2.4", "spectrum-x-ra3.1"} {
		for _, mode := range []struct{ multus, plugins config.ComponentManagement }{{config.ManagementManaged, config.ManagementManaged}, {config.ManagementExternal, config.ManagementManaged}, {config.ManagementManaged, config.ManagementExternal}, {config.ManagementExternal, config.ManagementExternal}} {
			t.Run(name+"/"+string(mode.multus)+"/"+string(mode.plugins), func(t *testing.T) {
				cfg, err := config.DefaultLaunchKitConfig()
				require.NoError(t, err)
				cfg.NetworkOperator.SecondaryNetwork = &config.SecondaryNetworkPolicy{Multus: mode.multus, CNIPlugins: mode.plugins}
				cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", SpectrumX: &config.ProfileSpectrumX{}}
				cfg.ClusterConfig = []config.ClusterConfig{{Identifier: "source-a", WorkerNodes: []string{"worker-a"}, NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-a"}, PFs: []config.PFConfig{{PciAddress: "0000:01:00.0", Traffic: "east-west", DeviceID: "1023"}}}}
				profile := &profiles.Profile{Templates: []string{filepath.Join("..", "..", "profiles", name, "10-nicclusterpolicy.yaml")}}
				rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
				require.NoError(t, err)
				files := []bundle.File{}
				for file, content := range rendered {
					files = append(files, bundle.File{Name: file, Content: content})
				}
				artifacts, err := bundle.FromFiles(files)
				require.NoError(t, err)
				require.Len(t, artifacts.Documents(), 1)
				ncp := artifacts.Documents()[0].ObjectCopy()
				nvIPAM, found, err := unstructured.NestedMap(ncp.Object, "spec", "nvIpam")
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, "nvidia-k8s-ipam", nvIPAM["image"])
				require.Equal(t, cfg.NetworkOperator.Repository, nvIPAM["repository"])
				require.Equal(t, cfg.NetworkOperator.ComponentVersion, nvIPAM["version"])
				if name == "ipoib-rdma-shared" {
					ipoib, present, readErr := unstructured.NestedMap(ncp.Object, "spec", "secondaryNetwork", "ipoib")
					require.NoError(t, readErr)
					require.True(t, present, "IPoIB CNI is independent of Multus/reference plugin ownership")
					require.Equal(t, map[string]interface{}{"image": "ipoib-cni", "repository": cfg.NetworkOperator.Repository, "version": cfg.NetworkOperator.ComponentVersion}, ipoib)
				}
				for _, path := range [][]string{{"spec", "nvIpam", "ipoib"}, {"spec", "ipoib"}} {
					_, present, readErr := unstructured.NestedMap(ncp.Object, path...)
					require.NoError(t, readErr)
					require.False(t, present, "IPoIB must remain under spec.secondaryNetwork.ipoib")
				}
				for _, component := range []struct {
					name string
					mode config.ComponentManagement
				}{{"multus", mode.multus}, {"cniPlugins", mode.plugins}} {
					_, found, err = unstructured.NestedMap(ncp.Object, "spec", "secondaryNetwork", component.name)
					require.NoError(t, err)
					require.Equal(t, component.mode == config.ManagementManaged, found)
				}
				if name != "ipoib-rdma-shared" && mode.multus == config.ManagementExternal && mode.plugins == config.ManagementExternal {
					_, found, err = unstructured.NestedMap(ncp.Object, "spec", "secondaryNetwork")
					require.NoError(t, err)
					require.False(t, found, "do not emit an empty secondaryNetwork mapping")
				}
				// Component ownership may change only those two sections. Compare
				// every other field with the same profile's managed rendering.
				baseline, cloneErr := config.CloneConfig(cfg)
				require.NoError(t, cloneErr)
				baseline.NetworkOperator.SecondaryNetwork = &config.SecondaryNetworkPolicy{Multus: config.ManagementManaged, CNIPlugins: config.ManagementManaged}
				baselineRendered, renderErr := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, baseline)
				require.NoError(t, renderErr)
				baselineFiles := []bundle.File{}
				for file, content := range baselineRendered {
					baselineFiles = append(baselineFiles, bundle.File{Name: file, Content: content})
				}
				baselineBundle, bundleErr := bundle.FromFiles(baselineFiles)
				require.NoError(t, bundleErr)
				require.Len(t, baselineBundle.Documents(), 1)
				withoutInstallers := func(obj *unstructured.Unstructured) map[string]interface{} {
					copy := obj.DeepCopy()
					unstructured.RemoveNestedField(copy.Object, "spec", "secondaryNetwork", "multus")
					unstructured.RemoveNestedField(copy.Object, "spec", "secondaryNetwork", "cniPlugins")
					secondary, _, nestedErr := unstructured.NestedMap(copy.Object, "spec", "secondaryNetwork")
					require.NoError(t, nestedErr)
					if len(secondary) == 0 {
						unstructured.RemoveNestedField(copy.Object, "spec", "secondaryNetwork")
					}
					return copy.Object
				}
				require.Equal(t, withoutInstallers(baselineBundle.Documents()[0].ObjectCopy()), withoutInstallers(ncp), "ownership policies must preserve nvIpam and every unrelated NCP field")
				require.Equal(t, mode.multus, cfg.NetworkOperator.SecondaryNetwork.Multus)
			})
		}
	}
}

func TestSecondaryNetworkAutoGenerationRequiresUsableEvidence(t *testing.T) {
	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	cfg.DiscoveredSecondaryNetwork = &config.SecondaryNetworkObservation{Multus: config.ComponentObservation{Status: config.InstallerUnavailable, Reason: "list denied"}, CNIPlugins: config.ComponentObservation{Status: config.InstallerNotDetected}}
	profile := &profiles.Profile{Templates: []string{filepath.Join("..", "..", "profiles", "sriov-ethernet-rdma", "10-nicclusterpolicy.yaml")}}
	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.ErrorContains(t, err, "cannot resolve automatic multus ownership")
	cfg.DiscoveredSecondaryNetwork.Multus.Status = config.InstallerExternal
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	for _, content := range rendered {
		require.NotContains(t, content, "    multus:")
		require.Contains(t, content, "    cniPlugins:")
	}
}

type secondaryMutationSpy struct {
	client.Client
	mutations int
}

func (c *secondaryMutationSpy) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	c.mutations++
	return errors.New("unexpected apply")
}
func (c *secondaryMutationSpy) Create(context.Context, client.Object, ...client.CreateOption) error {
	c.mutations++
	return nil
}
func (c *secondaryMutationSpy) Update(context.Context, client.Object, ...client.UpdateOption) error {
	c.mutations++
	return nil
}
func (c *secondaryMutationSpy) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	c.mutations++
	return nil
}
func (c *secondaryMutationSpy) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	c.mutations++
	return nil
}
func (c *secondaryMutationSpy) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error {
	c.mutations++
	return nil
}

func TestSecondaryNetworkConflictPrecedesHelmAndOverwriteRemediation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "platform-multus", Namespace: "kube-system", UID: "platform-uid"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "multus", Image: "registry.example:5000/platform/multus-cni:v1", Command: []string{"/thin_entrypoint"}, VolumeMounts: []corev1.VolumeMount{{Name: "cni", MountPath: "/host/opt/cni/bin"}}}}, Volumes: []corev1.Volume{{Name: "cni", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/cni/bin"}}}}}}}}
	artifacts, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "operator: {}\n"}, {Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  secondaryNetwork:\n    multus:\n      image: multus-cni\n"}})
	require.NoError(t, err)
	for _, dryRun := range []bool{false, true} {
		for _, overwrite := range []bool{false, true} {
			c := &secondaryMutationSpy{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds).Build()}
			helmCalls := 0
			install := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
				helmCalls++
				return nil
			}
			err = applyBundleWithInstaller(context.Background(), c, artifacts, DeployOptions{RestConfig: &rest.Config{}, NetworkOperator: &config.NetworkOperatorConfig{HelmRepoURL: "https://example.invalid", Version: "v1"}, DryRun: dryRun, OverwriteExisting: overwrite}, install)
			require.ErrorContains(t, err, "refusing to install multus")
			require.Zero(t, helmCalls)
			require.Zero(t, c.mutations)
		}
	}
}

func TestSecondaryNetworkCleanupPreservesExternalDaemonSet(t *testing.T) {
	c := newCleanTestClient(t)
	require.NoError(t, appsv1.AddToScheme(c.Scheme()))
	external := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "platform-multus", Namespace: "kube-system", UID: "external-uid"}}
	require.NoError(t, c.Create(context.Background(), external))
	deleted, err := deleteNetworkOperatorCustomResources(context.Background(), c, "operator-system", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	require.Zero(t, deleted)
	got := &appsv1.DaemonSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(external), got))
	require.Equal(t, external.UID, got.UID)
}

func TestSecondaryNetworkDirectOpenShiftGenerationRejectsManagedOwnership(t *testing.T) {
	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	cfg.Flavor = config.FlavorOCP
	cfg.NetworkOperator.SecondaryNetwork = &config.SecondaryNetworkPolicy{Multus: config.ManagementManaged}
	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.ErrorContains(t, err, "cannot be managed on OpenShift")
}

func TestSecondaryNetworkOverwriteRefusesLegacyPolicyTeardownBeforeHelm(t *testing.T) {
	for _, component := range []string{"multus", "cniPlugins"} {
		for _, dryRun := range []bool{false, true} {
			t.Run(component+fmt.Sprint(dryRun), func(t *testing.T) {
				scheme := runtime.NewScheme()
				require.NoError(t, appsv1.AddToScheme(scheme))
				legacy := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]interface{}{"name": "legacy-policy", "uid": "legacy-uid"}, "spec": map[string]interface{}{"secondaryNetwork": map[string]interface{}{component: map[string]interface{}{"image": "installer"}}}}}
				c := &secondaryMutationSpy{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()}
				artifacts, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "operator: {}\n"}, {Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  nvIpam: {}\n"}})
				require.NoError(t, err)
				helmCalls := 0
				install := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
					helmCalls++
					return nil
				}
				err = applyBundleWithInstaller(context.Background(), c, artifacts, DeployOptions{RestConfig: &rest.Config{}, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system", HelmRepoURL: "https://example.invalid", Version: "v1"}, DryRun: dryRun, OverwriteExisting: true}, install)
				require.ErrorContains(t, err, "refusing to remove NicClusterPolicy legacy-policy")
				require.ErrorContains(t, err, component)
				require.Zero(t, helmCalls)
				require.Zero(t, c.mutations)
			})
		}
	}
}

func TestSecondaryNetworkNonOverwriteExternalOnlyDoesNotRequireNCPList(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	applyReached := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errors.New("list denied")
		},
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			applyReached = true
			return errors.New("apply boundary reached")
		},
	})
	artifacts, err := bundle.FromFiles([]bundle.File{{Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  nvIpam: {}\n"}})
	require.NoError(t, err)
	err = applyBundleWithInstaller(context.Background(), c, artifacts, DeployOptions{SkipHelmChart: true, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}}, nil)
	require.ErrorContains(t, err, "apply boundary reached")
	require.True(t, applyReached, "denied optional reads and non-overwrite NCPList must not block omission")
}

func TestSecondaryNetworkOverwriteDeniedInspectionAndCancellationHaveNoEffects(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		scheme := runtime.NewScheme()
		require.NoError(t, appsv1.AddToScheme(scheme))
		base := fake.NewClientBuilder().WithScheme(scheme).Build()
		c := &secondaryMutationSpy{Client: interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "NicClusterPolicyList" {
				return errors.New("policy list denied")
			}
			return c.List(ctx, list, opts...)
		}})}
		artifacts, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "operator: {}\n"}, {Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  nvIpam: {}\n"}})
		require.NoError(t, err)
		helmCalls := 0
		install := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
			helmCalls++
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		err = applyBundleWithInstaller(ctx, c, artifacts, DeployOptions{RestConfig: &rest.Config{}, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system", HelmRepoURL: "https://example.invalid", Version: "v1"}, OverwriteExisting: true}, install)
		cancel()
		if canceled {
			require.ErrorIs(t, err, context.Canceled)
		} else {
			require.ErrorContains(t, err, "policy list denied")
		}
		require.Zero(t, helmCalls)
		require.Zero(t, c.mutations)
	}
}
