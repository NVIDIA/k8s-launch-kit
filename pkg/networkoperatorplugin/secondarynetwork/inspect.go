// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package secondarynetwork inspects known API-visible networking installers.
// It neither chooses desired ownership nor mutates Kubernetes resources.
package secondarynetwork

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var PolicyGVK = schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicy"}

const (
	Multus     = "multus"
	CNIPlugins = "cniPlugins"
)

// Inspection retains observed DaemonSets only for the current operation.
type Inspection struct {
	Observation config.SecondaryNetworkObservation
	Installers  map[string][]appsv1.DaemonSet
}

// Inspect recognizes upstream/Network Operator Multus and reference plugin
// installers from executable/image AND a CNI host-directory mount. Names and
// NAD CRDs alone are not evidence. Host-only installations remain unverified.
func Inspect(ctx context.Context, c client.Reader) (*Inspection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("secondary-network inspection requires a Kubernetes client")
	}
	ds := &appsv1.DaemonSetList{}
	if err := c.List(ctx, ds); err != nil {
		return nil, fmt.Errorf("list secondary-network installers: %w", err)
	}
	result := &Inspection{Installers: map[string][]appsv1.DaemonSet{}}
	for _, workload := range ds.Items {
		multus, plugins := recognizedInstallers(&workload)
		for _, component := range []struct {
			name       string
			recognized bool
		}{{Multus, multus}, {CNIPlugins, plugins}} {
			if component.recognized {
				result.Installers[component.name] = append(result.Installers[component.name], workload)
			}
		}
	}
	for _, component := range []string{Multus, CNIPlugins} {
		observation := config.ComponentObservation{Status: config.InstallerNotDetected, Reason: "no recognized API installer; host/platform installation may exist"}
		for _, workload := range result.Installers[component] {
			ref := config.InstallerReference{Namespace: workload.Namespace, Name: workload.Name, UID: string(workload.UID)}
			controller := metav1.GetControllerOf(&workload)
			if controller != nil && controller.Kind == PolicyGVK.Kind && strings.HasPrefix(controller.APIVersion, PolicyGVK.Group+"/") {
				policy, err := GetPolicy(ctx, c, controller.Name)
				if err != nil {
					return nil, err
				}
				if policy != nil && policy.GetUID() == controller.UID && controller.UID != "" {
					ref.PolicyName = policy.GetName()
					ref.PolicyUID = string(controller.UID)
				} else {
					observation.Status = config.InstallerAmbiguous
					observation.Reason = "installer's NicClusterPolicy controller UID does not match a live policy"
				}
			}
			observation.Installers = append(observation.Installers, ref)
		}
		sort.Slice(observation.Installers, func(i, j int) bool {
			a, b := observation.Installers[i], observation.Installers[j]
			return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
		})
		if len(observation.Installers) > 1 {
			observation.Status = config.InstallerAmbiguous
			observation.Reason = "multiple recognized installers"
		} else if len(observation.Installers) == 1 && observation.Status != config.InstallerAmbiguous {
			observation.Status = config.InstallerExternal
			observation.Reason = "recognized installer is externally managed"
			if observation.Installers[0].PolicyUID != "" {
				observation.Status = config.InstallerOperatorManaged
				observation.Reason = "installer controller matches live NicClusterPolicy UID"
			}
		}
		if component == Multus {
			result.Observation.Multus = observation
		} else {
			result.Observation.CNIPlugins = observation
		}
	}
	return result, ctx.Err()
}

// GetPolicy distinguishes verified absence (including an uninstalled CRD) from
// denied/unavailable inspection. Callers must not downgrade the latter.
func GetPolicy(ctx context.Context, c client.Reader, name string) (*unstructured.Unstructured, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("secondary-network ownership requires a Kubernetes client")
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(PolicyGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: name}, policy); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect NicClusterPolicy %s ownership: %w", name, err)
	}
	return policy, nil
}

// DiscoverEvidence saves optional inspection failures as uncertainty, allowing
// hardware discovery to complete. Cancellation is always an operation error.
func DiscoverEvidence(ctx context.Context, c client.Reader) (*config.SecondaryNetworkObservation, error) {
	inspection, err := Inspect(ctx, c)
	if err == nil {
		return &inspection.Observation, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	unavailable := config.ComponentObservation{Status: config.InstallerUnavailable, Reason: err.Error()}
	return &config.SecondaryNetworkObservation{Multus: unavailable, CNIPlugins: unavailable}, nil
}

func recognizedInstallers(ds *appsv1.DaemonSet) (bool, bool) {
	multus, plugins := false, false
	containers := append([]corev1.Container(nil), ds.Spec.Template.Spec.InitContainers...)
	containers = append(containers, ds.Spec.Template.Spec.Containers...)
	for _, container := range containers {
		if !hasCNIHostMount(container, ds.Spec.Template.Spec.Volumes) {
			continue
		}
		image := strings.Split(path.Base(strings.Split(container.Image, "@")[0]), ":")[0]
		commands := append(append([]string(nil), container.Command...), container.Args...)
		multusExecutable := false
		for _, command := range commands {
			if command == "--multus-conf-file" || strings.HasPrefix(command, "--multus-conf-file=") || command == "--multus-autoconfig-dir" || strings.HasPrefix(command, "--multus-autoconfig-dir=") {
				multusExecutable = true
			}
			switch path.Base(command) {
			case "multus", "multus-daemon", "thin_entrypoint", "thick_entrypoint", "install_multus":
				multusExecutable = true
			}
		}
		if multusExecutable || image == "multus-cni" {
			multus = true
		}
		// The plugins image's default entrypoint installs the reference suite.
		// A Multus init container copies only Multus and is not CNI-suite proof.
		if image == "plugins" || image == "cni-plugins" {
			plugins = true
		}
	}
	return multus, plugins
}

func hasCNIHostMount(container corev1.Container, volumes []corev1.Volume) bool {
	for _, mount := range container.VolumeMounts {
		if mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
			continue
		}
		for _, volume := range volumes {
			if volume.Name != mount.Name || volume.HostPath == nil {
				continue
			}
			host := path.Clean(volume.HostPath.Path)
			if strings.HasSuffix(host, "/cni/bin") || strings.HasSuffix(host, "/cni/net.d") {
				return true
			}
		}
	}
	return false
}
