// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
)

// GeneratedManifestRefs builds the expected inventory only from a complete
// validated snapshot. A nil snapshot must never become an empty inventory.
func GeneratedManifestRefs(artifacts *bundle.Bundle) ([]ObjectRef, error) {
	if artifacts == nil {
		return nil, fmt.Errorf("generated artifact bundle is nil")
	}
	var refs []ObjectRef
	for _, doc := range artifacts.Documents() {
		if doc.Role() != bundle.Deployment {
			continue
		}
		obj := doc.ObjectCopy()
		refs = append(refs, ObjectRef{GVK: obj.GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()})
	}
	return refs, nil
}
