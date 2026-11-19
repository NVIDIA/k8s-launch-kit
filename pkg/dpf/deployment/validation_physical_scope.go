// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// A source descriptor is declared authority, not successfully resolved access.
// In particular missing material cannot hide an endpoint or policy change.
type validationSource struct {
	ref, role, endpoint, system string
	insecure, customCA, sudo    bool
}

func validationSources(site config.Site) ([]validationSource, error) {
	var sources []validationSource
	for _, group := range []struct {
		role      string
		endpoints []config.Endpoint
	}{{"dpus", site.Discovery.DPUs}, {"hosts", site.Discovery.Hosts}} {
		for i, endpoint := range group.endpoints {
			s := validationSource{ref: fmt.Sprintf("/discovery/%s/%d", group.role, i), role: group.role}
			var err error
			if endpoint.SSH != nil {
				s.endpoint, err = config.SSHOrigin(endpoint.SSH.Address)
				s.sudo = endpoint.SSH.Sudo != nil && *endpoint.SSH.Sudo
			} else {
				s.endpoint, err = config.Origin(endpoint.BMCEndpoint)
				if endpoint.SystemURI != nil {
					s.system = *endpoint.SystemURI
				}
				policy := site.Discovery.TLS
				if endpoint.TLS != nil {
					policy = endpoint.TLS
				}
				if policy != nil {
					s.insecure = policy.InsecureSkipVerify != nil && *policy.InsecureSkipVerify
					s.customCA = policy.CAFile != nil && *policy.CAFile != ""
				}
			}
			if err != nil {
				return nil, fmt.Errorf("declared physical source is invalid")
			}
			sources = append(sources, s)
		}
	}
	for i, sw := range site.Discovery.Switches {
		endpoint, err := config.SSHOrigin(sw.SSH.Address)
		if err != nil {
			return nil, fmt.Errorf("declared switch source is invalid")
		}
		sources = append(sources, validationSource{ref: fmt.Sprintf("/discovery/switches/%d", i), role: "switches", endpoint: endpoint, sudo: sw.SSH.Sudo != nil && *sw.SSH.Sudo})
	}
	return sources, nil
}

func validateValidationPhysicalScope(desired *managementDesired, request config.Request) error {
	if desired == nil || desired.retained == nil || request.Input.Site.Inventory == nil {
		return fmt.Errorf("validation requires retained physical inventory")
	}
	var original config.Input
	for _, file := range desired.retained.Files() {
		if file.Path == ".l8k/source-config.yaml" {
			var err error
			original, err = config.Decode(file.Data, file.Path)
			if err != nil {
				return fmt.Errorf("retained physical source configuration is invalid")
			}
		}
	}
	if err := config.VerifyRetainedPhysicalBindings(original.Site.Inventory, request.Input.Site.Inventory); err != nil {
		return err
	}
	old, err := validationSources(original.Site)
	if err != nil {
		return err
	}
	current, err := validationSources(request.Input.Site)
	if err != nil {
		return err
	}
	if len(old) == 0 || !reflect.DeepEqual(old, current) {
		return fmt.Errorf("declared physical endpoints or security policy differ from the retained deployment; repeat discovery and generation")
	}
	if err := validateResolvedPhysicalSources(current, request); err != nil {
		return err
	}
	// Port intent is independent of SSH credential availability, too.
	oldSwitches, newSwitches := original.Site.Discovery.Switches, request.Input.Site.Discovery.Switches
	for i, sw := range oldSwitches {
		if sw.Name != newSwitches[i].Name || !reflect.DeepEqual(sw.Platform, newSwitches[i].Platform) || !reflect.DeepEqual(sw.Ports, newSwitches[i].Ports) {
			return fmt.Errorf("declared switch port bindings differ from the retained deployment")
		}
	}
	// Hash resolved trust only when every declared source is available. A
	// missing source is separately unverified; no partial hash can prove equality.
	if len(request.Targets)+len(request.Switches) != len(current) {
		return nil
	}
	inv := request.Input.Site.Inventory
	scopes := make([]inventory.ScopeEndpoint, 0, len(current))
	for _, target := range request.Targets {
		scopes = append(scopes, config.Scope(target))
	}
	for _, sw := range request.Switches {
		scopes = append(scopes, config.Scope(config.Target{Role: "switches", Endpoint: sw.Endpoint, SSH: sw.Access}))
	}
	slices.SortFunc(scopes, func(a, b inventory.ScopeEndpoint) int {
		return strings.Compare(a.Role+a.Endpoint+a.SystemURI, b.Role+b.Endpoint+b.SystemURI)
	})
	observed := &inventory.Inventory{Collection: inv.Collection, RequestedScope: scopes, Management: inv.Management, HBN: inv.HBN}
	if inv.ScopeDigest == "" || discovery.ScopeDigest(observed) != inv.ScopeDigest {
		return fmt.Errorf("resolved physical access policy differs from the discovered scope; repeat discovery and generation")
	}
	return nil
}

// Even a partial resolved request must not introduce an undeclared source or
// change its access policy. Credential availability affects coverage, not scope.
func validateResolvedPhysicalSources(declared []validationSource, request config.Request) error {
	seen := map[string]bool{}
	check := func(target config.Target) error {
		key := target.Role + " " + target.Endpoint
		if seen[key] {
			return fmt.Errorf("resolved physical source is duplicated")
		}
		seen[key] = true
		for _, source := range declared {
			if source.role != target.Role || source.endpoint != target.Endpoint {
				continue
			}
			if target.SystemURI != source.system || (target.InputRef != "" && target.InputRef != source.ref) {
				break
			}
			if target.SSH != nil {
				if strings.HasPrefix(source.endpoint, "ssh://") && target.SSH.Sudo == source.sudo {
					return nil
				}
			} else if !strings.HasPrefix(source.endpoint, "ssh://") && target.Access.InsecureSkipVerify() == source.insecure && (len(target.Access.PublicCA()) > 0) == source.customCA {
				return nil
			}
			break
		}
		return fmt.Errorf("resolved physical source does not match its declared endpoint and security policy")
	}
	for _, target := range request.Targets {
		if err := check(target); err != nil {
			return err
		}
	}
	for _, sw := range request.Switches {
		if err := check(config.Target{Role: "switches", Endpoint: sw.Endpoint, SSH: sw.Access}); err != nil {
			return err
		}
		matched := false
		for _, source := range request.Input.Site.Discovery.Switches {
			endpoint, _ := config.SSHOrigin(source.SSH.Address)
			if endpoint != sw.Endpoint {
				continue
			}
			platform := "cumulus-nvue"
			if source.Platform != nil {
				platform = *source.Platform
			}
			ports := slices.Clone(source.Ports)
			slices.SortFunc(ports, func(a, b config.SwitchPort) int { return strings.Compare(a.Name, b.Name) })
			matched = sw.Name == source.Name && sw.Platform == platform && slices.Equal(sw.Ports, ports)
		}
		if !matched {
			return fmt.Errorf("resolved switch identity, platform or port bindings differ from declared scope")
		}
	}
	return nil
}
