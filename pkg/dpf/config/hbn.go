// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
	yaml "gopkg.in/yaml.v3"
)

// Switch selects a read-only fabric source. Port bindings express the intended
// cabling; discovery must corroborate them against physical observations.
type Switch struct {
	Name          string       `yaml:"name"`
	Platform      *string      `yaml:"platform,omitempty"`
	SSH           SSH          `yaml:"ssh"`
	CredentialRef string       `yaml:"credentialRef"`
	Ports         []SwitchPort `yaml:"ports"`
}

type SwitchPort = inventory.SwitchPort

// SwitchTarget contains snapshotted trust and authentication, never serialized.
type SwitchTarget struct {
	Name     string
	Platform string
	Endpoint string
	Access   *SSHAccess
	Ports    []SwitchPort
}

// WithUseCase applies only an explicitly supplied CLI selection to a copy of the
// YAML document. Saved discovery therefore retains the effective selection.
func WithUseCase(in Input, selection *string) (Input, error) {
	if selection == nil {
		return in, nil
	}
	if *selection != "hbn-bgp" {
		return Input{}, fmt.Errorf("--use-case must be hbn-bgp")
	}
	doc, err := yamlinput.Expand(in.Document)
	if err != nil {
		return Input{}, err
	}
	root := doc.Content[0]
	var dpf *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "dpf" {
			dpf = root.Content[i+1]
			break
		}
	}
	if dpf == nil || dpf.Tag == "!!null" {
		dpf = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		yamlinput.Set(root, "dpf", dpf)
	}
	yamlinput.Set(dpf, "useCase", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: *selection})
	b, err := yamlinput.Encode(doc)
	if err != nil {
		return Input{}, err
	}
	result, err := Decode(b, in.Path)
	if err != nil {
		return Input{}, err
	}
	// Source guards concurrent edits to the file; Document is the effective
	// configuration to save. CLI overrides must not change that source guard.
	result.Source = bytes.Clone(in.Source)
	return result, nil
}

var switchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
var switchPortName = regexp.MustCompile(`^swp[0-9]+(s[0-9]+)?$`)

func resolveUseCase(r *Request) error {
	s := r.Input.Site
	intent, err := ResolveIntent(s)
	if err != nil {
		return err
	}
	r.UseCase, r.DPFNamespace = intent.UseCase, intent.Namespace
	r.Software = &intent.Release
	r.HBN, r.HBNGeneration = intent.Networking, intent.Generation
	if r.HBN == nil {
		return nil
	}
	// HBN readiness adds direct read-only fabric inspection. It does not need
	// the optional management-network probe Job to collect physical inventory.
	r.Mode = "inventory"
	if len(s.Discovery.Switches) > 32 {
		return fmt.Errorf("at most 32 discovery switches are supported")
	}
	names, endpoints, attachments := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, sw := range s.Discovery.Switches {
		where := fmt.Sprintf("/discovery/switches/%d", i)
		if !switchName.MatchString(sw.Name) || names[sw.Name] {
			return fmt.Errorf("%s/name must be a unique switch name", where)
		}
		names[sw.Name] = true
		platform := "cumulus-nvue"
		if sw.Platform != nil {
			platform = *sw.Platform
		}
		if platform != "cumulus-nvue" {
			return fmt.Errorf("%s/platform must be cumulus-nvue", where)
		}
		endpoint, err := SSHOrigin(sw.SSH.Address)
		if err != nil || endpoints[endpoint] {
			return fmt.Errorf("%s/ssh/address must be a valid, unique SSH endpoint", where)
		}
		endpoints[endpoint] = true
		credential, ok := s.Credentials[sw.CredentialRef]
		if !ok {
			return fmt.Errorf("%s/credentialRef is unknown", where)
		}
		access, err := resolveSSHAccess(credential, sw.SSH, filepath.Dir(r.Input.Path))
		if err != nil {
			return fmt.Errorf("%s/ssh: %w", where, err)
		}
		ports := slices.Clone(sw.Ports)
		if len(ports) == 0 || len(ports) > 256 {
			return fmt.Errorf("%s/ports requires 1 to 256 explicit DPU uplink bindings", where)
		}
		seen := map[string]bool{}
		for _, port := range ports {
			if !switchPortName.MatchString(port.Name) || seen[port.Name] {
				return fmt.Errorf("%s/ports requires unique physical swp port names", where)
			}
			seen[port.Name] = true
			if port.DPUSerial == "" || strings.TrimSpace(port.DPUSerial) != port.DPUSerial || strings.ContainsAny(port.DPUSerial, "\r\n\x00") {
				return fmt.Errorf("%s/ports requires nonempty DPU serial numbers", where)
			}
			if port.DPUInterface != "p0" && port.DPUInterface != "p1" {
				return fmt.Errorf("%s/ports dpuInterface must be p0 or p1", where)
			}
			key := port.DPUSerial + "/" + port.DPUInterface
			if attachments[key] {
				return fmt.Errorf("%s/ports repeats a DPU uplink binding", where)
			}
			attachments[key] = true
		}
		slices.SortFunc(ports, func(a, b SwitchPort) int { return strings.Compare(a.Name, b.Name) })
		r.Switches = append(r.Switches, SwitchTarget{Name: sw.Name, Platform: platform, Endpoint: endpoint, Access: access, Ports: ports})
	}
	slices.SortFunc(r.Switches, func(a, b SwitchTarget) int { return strings.Compare(a.Name, b.Name) })
	return nil
}

func hbnIntegerPath(p string) bool {
	switch p {
	case "/dpf/hbn/fabricASN", "/dpf/hbn/dpuASNBase", "/dpf/hbn/dpuASNStride", "/dpf/hbn/uplinkMTU", "/dpf/hbn/workloadMTU", "/dpf/hbn/workloadPrefix":
		return true
	}
	return false
}
