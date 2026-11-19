// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

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
// Discovery and deployment continue to use the fail-fast Resolve entry point.
func ResolveForValidation(in Input) (Request, []AccessIssue, error) {
	availability := &accessAvailability{}
	request, err := resolve(in, availability)
	if err != nil {
		return Request{}, nil, err
	}
	return request, availability.issues, nil
}

// A nil collector selects the ordinary fail-fast resolution policy. The
// validation caller owns this collector for one synchronous resolution only.
type accessAvailability struct {
	issues []AccessIssue
}

func (a *accessAvailability) record(inputRef, role string, err error) bool {
	if a == nil {
		return false
	}
	a.issues = append(a.issues, AccessIssue{InputRef: inputRef, Role: role, Message: err.Error()})
	return true
}
