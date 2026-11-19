// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/mitchellh/copystructure"
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
// Discovery and deployment continue to use the fail-fast Resolve entry point.
func ResolveForValidation(in Input) (Request, []AccessIssue, error) {
	frozen, err := validationInputSnapshot(in)
	if err != nil {
		return Request{}, nil, err
	}
	availability := &accessAvailability{}
	request, err := resolve(frozen, availability)
	if err != nil {
		return Request{}, nil, err
	}
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
	issues []AccessIssue
}

func (a *accessAvailability) record(inputRef, role string, err error) bool {
	var material *accessMaterialError
	if a == nil || !errors.As(err, &material) {
		return false
	}
	a.issues = append(a.issues, AccessIssue{InputRef: inputRef, Role: role, Message: material.message})
	return true
}
