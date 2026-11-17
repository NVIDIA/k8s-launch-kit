// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"fmt"

	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ObjectSchemas holds immutable per-GVK schemas extracted from one retained
// operator manifest. Build it once and reuse it for resource verification.
type ObjectSchemas struct {
	byGVK map[resourceKey]dpfSchema
}

// NewObjectSchemas parses and compiles all served CRD schemas in manifest.
func NewObjectSchemas(manifest []byte) (*ObjectSchemas, error) {
	schemas, err := schemasFromOperatorManifest(manifest)
	if err != nil {
		return nil, err
	}
	return &ObjectSchemas{byGVK: schemas}, nil
}

// DefaultedObject applies the selected operator manifest's CRD schema to a
// defensive copy of object. It validates the object, applies structural CRD
// defaults, and rejects every field the selected API server would prune.
func DefaultedObject(operatorManifest []byte, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	schemas, err := NewObjectSchemas(operatorManifest)
	if err != nil {
		return nil, err
	}
	return schemas.DefaultedCopy(object)
}

// DefaultedCopy validates, defaults, and returns a defensive copy of object.
func (s *ObjectSchemas) DefaultedCopy(object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if s == nil || len(s.byGVK) == 0 {
		return nil, fmt.Errorf("selected operator schemas are required")
	}
	if object == nil {
		return nil, fmt.Errorf("custom resource is required")
	}
	group, version, err := splitAPIVersion(object.GetAPIVersion())
	if err != nil || group == "" || version == "" || object.GetKind() == "" {
		return nil, fmt.Errorf("custom resource requires a supported apiVersion and kind")
	}
	schema, ok := s.byGVK[resourceKey{group: group, kind: object.GetKind()}]
	if !ok {
		return nil, fmt.Errorf("custom resource %s %s is not served by selected operator manifest", object.GetKind(), group)
	}
	versionSchema, ok := schema.versions[version]
	if !ok {
		return nil, fmt.Errorf("custom resource %s %s/%s has no selected schema", object.GetKind(), group, version)
	}
	copy := object.DeepCopy()
	delete(copy.Object, "status")
	defaulting.Default(copy.Object, versionSchema.structural)
	delete(copy.Object, "status")
	if issues := validation.ValidateCustomResource(field.NewPath("resource"), copy.Object, versionSchema.validator); len(issues) > 0 {
		return nil, fmt.Errorf("custom resource %s %q fails selected schema: %s", copy.GetKind(), copy.GetName(), issues[0])
	}
	pruned := copy.DeepCopy().Object
	unknown := pruning.PruneWithOptions(pruned, versionSchema.structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	if len(unknown) > 0 {
		return nil, fmt.Errorf("custom resource %s %q contains fields the selected schema would prune: %s", copy.GetKind(), copy.GetName(), unknown[0])
	}
	return copy, nil
}
