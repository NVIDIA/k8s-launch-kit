// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import "github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

// resolvedOutput versions the selected-device wire shape independently of the
// bundle index. Private collector observations never enter new sidecars.
type resolvedOutput struct {
	FormatVersion string           `yaml:"formatVersion"`
	Target        string           `yaml:"target"`
	Config        generationOutput `yaml:"config"`
}

type generationOutput struct {
	ManagementKubernetesVersion string                   `yaml:"managementKubernetesVersion"`
	Name                        string                   `yaml:"name"`
	Profile                     string                   `yaml:"profile"`
	Release                     string                   `yaml:"release"`
	Namespace                   string                   `yaml:"namespace"`
	ManagementUID               string                   `yaml:"managementUID"`
	KubeconfigRef               FileRef                  `yaml:"kubeconfigRef"`
	Context                     string                   `yaml:"context"`
	Provisioning                Provisioning             `yaml:"provisioning"`
	Sentry                      Sentry                   `yaml:"sentry"`
	Devices                     []inventory.DeviceOutput `yaml:"devices"`
	DevicePSIDs                 map[string]string        `yaml:"devicePSIDs"`
}

func generationToOutput(g Generation) (generationOutput, error) {
	w := generationOutput{
		ManagementKubernetesVersion: g.ManagementKubernetesVersion,
		Name:                        g.Name,
		Profile:                     g.Profile,
		Release:                     g.Release,
		Namespace:                   g.Namespace,
		ManagementUID:               g.ManagementUID,
		KubeconfigRef:               g.KubeconfigRef,
		Context:                     g.Context,
		Provisioning:                g.Provisioning,
		Sentry:                      g.Sentry,
		DevicePSIDs:                 g.DevicePSIDs,
	}
	for _, device := range g.Devices {
		out, err := inventory.DeviceToOutput(device, "dpus")
		if err != nil {
			return generationOutput{}, err
		}
		w.Devices = append(w.Devices, out)
	}
	return w, nil
}

func (w generationOutput) generation() (Generation, error) {
	g := Generation{
		ManagementKubernetesVersion: w.ManagementKubernetesVersion,
		Name:                        w.Name,
		Profile:                     w.Profile,
		Release:                     w.Release,
		Namespace:                   w.Namespace,
		ManagementUID:               w.ManagementUID,
		KubeconfigRef:               w.KubeconfigRef,
		Context:                     w.Context,
		Provisioning:                w.Provisioning,
		Sentry:                      w.Sentry,
		DevicePSIDs:                 w.DevicePSIDs,
	}
	for _, device := range w.Devices {
		out, err := inventory.DeviceFromOutput(device, "dpus")
		if err != nil {
			return Generation{}, err
		}
		g.Devices = append(g.Devices, out)
	}
	return g, nil
}
