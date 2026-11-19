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

// Physical expectations belong to the frozen bundle. Caller inventory supplies
// access declarations; it cannot redefine the identities being validated.
type validationPhysicalScope struct {
	expected       *inventory.Inventory
	accessVerified bool
	trustProblem   string
}

func compileValidationPhysicalScope(desired *managementDesired, request config.Request) (validationPhysicalScope, error) {
	var scope validationPhysicalScope
	if desired == nil || desired.retained == nil || request.Input.Site.Inventory == nil {
		return scope, fmt.Errorf("validation requires retained physical inventory")
	}
	var original config.Input
	for _, file := range desired.retained.Files() {
		if file.Path == ".l8k/source-config.yaml" {
			var err error
			original, err = config.Decode(file.Data, file.Path)
			if err != nil {
				return scope, fmt.Errorf("retained physical source configuration is invalid")
			}
		}
	}
	if err := config.VerifyRetainedPhysicalBindings(original.Site.Inventory, request.Input.Site.Inventory); err != nil {
		return scope, err
	}
	old, err := validationSources(original.Site)
	if err != nil {
		return scope, err
	}
	current, err := validationSources(request.Input.Site)
	if err != nil {
		return scope, err
	}
	if len(old) == 0 || !reflect.DeepEqual(old, current) {
		return scope, fmt.Errorf("declared physical endpoints or security policy differ from the retained deployment; repeat discovery and generation")
	}
	if err := validateResolvedPhysicalSources(current, request); err != nil {
		return scope, err
	}
	// Port intent is independent of SSH credential availability, too.
	oldSwitches, newSwitches := original.Site.Discovery.Switches, request.Input.Site.Discovery.Switches
	for i, sw := range oldSwitches {
		if sw.Name != newSwitches[i].Name || !reflect.DeepEqual(sw.Platform, newSwitches[i].Platform) || !reflect.DeepEqual(sw.Ports, newSwitches[i].Ports) {
			return scope, fmt.Errorf("declared switch port bindings differ from the retained deployment")
		}
	}
	scope.expected = original.Site.Inventory
	// Resolve trust independently of credential availability. The proof and
	// successful transports retain the same immutable trust bytes.
	scopes, complete := request.ValidationScope()
	available := make([]inventory.ScopeEndpoint, 0, len(request.Targets)+len(request.Switches))
	for _, target := range request.Targets {
		available = append(available, config.Scope(target))
	}
	for _, sw := range request.Switches {
		available = append(available, config.Scope(config.Target{Role: "switches", Endpoint: sw.Endpoint, SSH: sw.Access}))
	}
	if !complete && len(available) == len(current) {
		// Ordinary fully resolved library requests already contain every trust
		// snapshot. Partial requests without a validation proof are never sufficient.
		scopes, complete = available, true
	}
	if !complete {
		var missing []string
		for _, source := range current {
			if !slices.ContainsFunc(scopes, func(s inventory.ScopeEndpoint) bool {
				return s.Role == source.role && s.Endpoint == source.endpoint && s.SystemURI == source.system
			}) {
				missing = append(missing, source.role+" "+source.endpoint)
			}
		}
		scope.trustProblem = "The complete physical trust policy could not be verified; restore the trust files for " + strings.Join(missing, ", ") + " and repeat validation."
		return scope, nil
	}
	if len(scopes) != len(current) {
		return scope, fmt.Errorf("complete validation trust scope does not cover every declared source")
	}
	for _, observed := range available {
		if !slices.Contains(scopes, observed) {
			return scope, fmt.Errorf("resolved physical access policy differs from its frozen validation trust scope")
		}
	}
	slices.SortFunc(scopes, func(a, b inventory.ScopeEndpoint) int {
		return strings.Compare(a.Role+a.Endpoint+a.SystemURI, b.Role+b.Endpoint+b.SystemURI)
	})
	// Use the retained context as well as retained digest. Fresh caller facts must
	// not alter the meaning of the original access and intent binding.
	inv := scope.expected
	observed := &inventory.Inventory{Collection: inv.Collection, RequestedScope: scopes, Management: inv.Management, HBN: inv.HBN}
	if inv.ScopeDigest == "" || discovery.ScopeDigest(observed) != inv.ScopeDigest {
		return scope, fmt.Errorf("resolved physical access policy differs from the discovered scope; repeat discovery and generation")
	}
	scope.accessVerified = true
	return scope, nil
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
