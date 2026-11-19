// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/mitchellh/copystructure"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
)

// AccessIssue identifies an unavailable selected source. It contains no access
// material or transport error; Message comes only from local access parsers.
type AccessIssue struct {
	InputRef string
	Role     string
	Message  string
}

// ResolveForValidation preserves independent management/tenant observations
// when one physical source's credential or trust reference is unavailable.
// Structural configuration errors still reject the entire request. Targets
// contains only resolved access; callers must retain every returned issue and
// derive expected coverage from Input, never from the available target count.
// ValidationScope records every readable source trust policy independently of
// authentication. Its proof must be matched to each transport before use; it
// cannot authorize public targets changed after resolution.
// Discovery and deployment continue to use the fail-fast Resolve entry point.
func ResolveForValidation(in Input) (Request, []AccessIssue, error) {
	frozen, err := validationInputSnapshot(in)
	if err != nil {
		return Request{}, nil, err
	}
	availability := &accessAvailability{scopeComplete: true}
	request, err := resolve(frozen, availability)
	if err != nil {
		return Request{}, nil, err
	}
	request.validationScope = availability.scope
	request.validationScopeComplete = availability.scopeComplete
	return request, availability.issues, nil
}

// Snapshot the scope and source guards together. YAML nodes are expanded with
// the existing bounded clone so aliases cannot retain caller-owned nodes.
func validationInputSnapshot(in Input) (Input, error) {
	copied, err := copystructure.Copy(in.Site)
	if err != nil {
		return Input{}, fmt.Errorf("cannot snapshot validation input")
	}
	in.Site = copied.(Site)
	in.Source = bytes.Clone(in.Source)
	in.OverridesSource = bytes.Clone(in.OverridesSource)
	if in.Document != nil {
		in.Document, err = yamlinput.Expand(in.Document)
		if err != nil {
			return Input{}, fmt.Errorf("cannot snapshot validation input document")
		}
	}
	if in.InventoryDocument != nil {
		in.InventoryDocument, err = yamlinput.Expand(in.InventoryDocument)
		if err != nil {
			return Input{}, fmt.Errorf("cannot snapshot validation inventory document")
		}
	}
	return in, nil
}

// Only a selected source's unreadable or malformed material is recoverable.
// An invalid reference shape remains a configuration error for every caller.
type accessMaterialError struct{ message string }

func (e *accessMaterialError) Error() string { return e.message }

// A nil collector selects the ordinary fail-fast resolution policy. The
// validation caller owns this collector for one synchronous resolution only.
type accessAvailability struct {
	issues        []AccessIssue
	scope         []inventory.ScopeEndpoint
	scopeComplete bool
}

func (a *accessAvailability) record(inputRef, role string, err error) bool {
	var material *accessMaterialError
	if a == nil || !errors.As(err, &material) {
		return false
	}
	a.issues = append(a.issues, AccessIssue{InputRef: inputRef, Role: role, Message: material.message})
	return true
}

// ValidationScope returns a defensive copy of the selected access-policy snapshot
// captured by ResolveForValidation. Completeness requires readable trust
// for every source, independently of available authentication. This runtime-only
// proof does not authorize mutated public targets: consumers must also match each
// transport's Scope and declared source selection against the snapshot.
func (r Request) ValidationScope() ([]inventory.ScopeEndpoint, bool) {
	return slices.Clone(r.validationScope), r.validationScopeComplete
}

func (a *accessAvailability) captureScope(target Target, trustErr error) {
	if a == nil {
		return
	}
	if trustErr != nil {
		a.scopeComplete = false
		return
	}
	a.scope = append(a.scope, Scope(target))
}

// The validation policy resolves trust independently of authentication. Only a
// fully resolved access value escapes to Targets; the private proof records the
// same trust bytes even when authentication is unavailable. Ordinary resolution
// keeps its existing authentication-first, fail-fast behavior.
func (a *accessAvailability) resolveSSHAccess(c Credential, endpoint SSH, base string, target Target) (*SSHAccess, error) {
	if a == nil {
		return resolveSSHAccess(c, endpoint, base)
	}
	if err := validateSSHAccess(c, endpoint); err != nil {
		return nil, err
	}
	known, trustErr := resolveSSHTrust(endpoint, base)
	target.SSH = &SSHAccess{knownHosts: known, Sudo: endpoint.Sudo != nil && *endpoint.Sudo}
	a.captureScope(target, trustErr)
	access, authErr := resolveSSHAuthentication(c, base)
	if authErr != nil {
		return nil, authErr
	}
	if trustErr != nil {
		return nil, trustErr
	}
	access.knownHosts, access.Sudo = known, target.SSH.Sudo
	return access, nil
}

func (a *accessAvailability) resolveAccess(c Credential, caPath, base string, insecure bool, target Target) (Access, error) {
	if a == nil {
		return resolveAccess(c, caPath, base, insecure)
	}
	if err := validateRedfishCredential(c); err != nil {
		return Access{}, err
	}
	access, trustErr := resolveAccessTrust(caPath, insecure)
	target.Access = access
	a.captureScope(target, trustErr)
	password, authErr := resolvePassword(c.PasswordRef, base)
	if authErr != nil {
		return Access{}, authErr
	}
	if trustErr != nil {
		return Access{}, trustErr
	}
	access.username, access.password = c.Username, password
	return access, nil
}

// SnapshotRequestForValidation isolates resolved scope without resolving access
// again. Private access bytes have no mutation API; SSH's public sudo flag and
// every caller-owned public tree are copied before observers retain them. The
// private proof is preserved, but cannot authorize mutated public targets by itself.
func SnapshotRequestForValidation(request Request) (Request, error) {
	input, err := validationInputSnapshot(request.Input)
	if err != nil {
		return Request{}, err
	}
	// copystructure cannot preserve private resolved access fields. Exclude the
	// access containers, clone the public configuration, then restore value copies.
	targets, switches := request.Targets, request.Switches
	scope, complete := request.ValidationScope()
	request.validationScope = nil
	request.Input = Input{}
	request.Targets = nil
	request.Switches = nil
	cloned, err := copystructure.Copy(request)
	if err != nil {
		return Request{}, fmt.Errorf("cannot snapshot validation request")
	}
	result := cloned.(Request)
	result.Input = input
	result.validationScope, result.validationScopeComplete = scope, complete
	result.Targets = make([]Target, len(targets))
	for i, target := range targets {
		result.Targets[i] = target
		if target.SSH != nil {
			ssh := *target.SSH
			result.Targets[i].SSH = &ssh
		}
	}
	result.Switches = make([]SwitchTarget, len(switches))
	for i, sw := range switches {
		result.Switches[i] = sw
		result.Switches[i].Ports = append([]SwitchPort(nil), sw.Ports...)
		if sw.Access != nil {
			ssh := *sw.Access
			result.Switches[i].Access = &ssh
		}
	}
	return result, nil
}
