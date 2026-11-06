// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sentry

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// The exact upstream CRD schemas and their source hashes are retained together.
// These checks supplement, never replace, the installed cluster's admission.
//
//go:embed schemas/*.json
var schemas embed.FS

type schemaDocument struct {
	Group   string                          `json:"group"`
	Kind    string                          `json:"kind"`
	Version string                          `json:"version"`
	Schema  apiextensionsv1.JSONSchemaProps `json:"schema"`
}

type compiledSchema struct {
	mu         sync.Mutex
	structural *structuralschema.Structural
	openapi    schemavalidation.SchemaValidator
	cel        *schemacel.Validator
}

var schemaCache sync.Map

// DefaultedCopy applies only the selected release's pinned CRD defaults to an
// independent copy of a supported DPF object. It never prunes unknown fields or
// models controller/webhook changes, and performs no I/O.
func DefaultedCopy(release string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if obj == nil {
		return nil, fmt.Errorf("DPF object is required for schema defaulting")
	}
	gvk := obj.GroupVersionKind()
	if gvk.Group != "provisioning.dpu.nvidia.com" && gvk.Group != "svc.dpu.nvidia.com" {
		return nil, fmt.Errorf("unsupported schema-defaulting group %q", gvk.Group)
	}
	schema, err := releaseSchema(release, gvk.Group, gvk.Kind, gvk.Version)
	if err != nil {
		return nil, err
	}
	copy := obj.DeepCopy()
	defaulting.Default(copy.Object, schema.structural)
	return copy, nil
}

func releaseSchema(release, group, kind, version string) (*compiledSchema, error) {
	key := release + "/" + group + "/" + version + "/" + kind
	if cached, ok := schemaCache.Load(key); ok {
		return cached.(*compiledSchema), nil
	}
	data, err := schemas.ReadFile("schemas/" + release + "-" + kind + ".json")
	if err != nil {
		return nil, fmt.Errorf("no pinned %s schema for %s/%s", release, group, kind)
	}
	var doc schemaDocument
	if err = json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Group != group || doc.Kind != kind || doc.Version != version {
		return nil, fmt.Errorf("schema identity mismatch for %s/%s %s", group, kind, version)
	}
	compiled, err := compileSchema(doc.Schema)
	if err != nil {
		return nil, err
	}
	actual, _ := schemaCache.LoadOrStore(key, compiled)
	return actual.(*compiledSchema), nil
}

func compileSchema(external apiextensionsv1.JSONSchemaProps) (*compiledSchema, error) {
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&external, &internal, nil); err != nil {
		return nil, err
	}
	structural, err := structuralschema.NewStructural(&internal)
	if err != nil {
		return nil, err
	}
	openapi, _, err := schemavalidation.NewSchemaValidator(&internal)
	if err != nil {
		return nil, err
	}
	return &compiledSchema{structural: structural, openapi: openapi, cel: schemacel.NewValidator(structural, true, celconfig.PerCallLimit)}, nil
}

func (s *compiledSchema) validate(obj *unstructured.Unstructured) error {
	// The upstream OpenAPI validator has internal validation state. Keep one
	// compiled schema and serialize its use while callers render concurrently.
	s.mu.Lock()
	defer s.mu.Unlock()
	unknown := pruning.PruneWithOptions(obj.DeepCopy().Object, s.structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	if len(unknown) > 0 {
		return fmt.Errorf("unknown schema fields: %s", strings.Join(unknown, ", "))
	}
	if errs := schemavalidation.ValidateCustomResource(field.NewPath("resource"), obj.Object, s.openapi); len(errs) > 0 {
		return fmt.Errorf("OpenAPI validation: %s", errs.ToAggregate())
	}
	if s.cel != nil {
		errs, _ := s.cel.Validate(context.Background(), field.NewPath("resource"), s.structural, obj.Object, nil, celconfig.RuntimeCELCostBudget)
		if len(errs) > 0 {
			return fmt.Errorf("CEL validation: %s", errs.ToAggregate())
		}
	}
	return nil
}

func validateRendered(r Rendered, release string) error {
	roles := map[string]bundle.Role{}
	for _, record := range r.Records {
		roles[record.Path] = record.Role
	}
	type sourced struct {
		path string
		obj  *unstructured.Unstructured
	}
	var documents []sourced
	custom := map[string]*compiledSchema{}
	for _, file := range r.Files {
		if roles[file.Path] != bundle.Manifest && roles[file.Path] != bundle.Projection {
			continue
		}
		objects, err := decodeDocuments(file.Data)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Path, err)
		}
		for _, obj := range objects {
			documents = append(documents, sourced{file.Path, obj})
			if obj.GetKind() != "CustomResourceDefinition" {
				continue
			}
			var crd apiextensionsv1.CustomResourceDefinition
			data, err := json.Marshal(obj.Object)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(data, &crd); err != nil {
				return err
			}
			for _, version := range crd.Spec.Versions {
				if !version.Served || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
					continue
				}
				compiled, err := compileSchema(*version.Schema.OpenAPIV3Schema)
				if err != nil {
					return fmt.Errorf("chart CRD %s: %w", crd.Name, err)
				}
				custom[crd.Spec.Group+"/"+version.Name+"/"+crd.Spec.Names.Kind] = compiled
			}
		}
	}
	for _, d := range documents {
		gvk := d.obj.GroupVersionKind()
		var compiled *compiledSchema
		var err error
		if gvk.Group == "provisioning.dpu.nvidia.com" || gvk.Group == "svc.dpu.nvidia.com" {
			compiled, err = releaseSchema(release, gvk.Group, gvk.Kind, gvk.Version)
		} else {
			compiled = custom[gvk.Group+"/"+gvk.Version+"/"+gvk.Kind]
		}
		if err != nil {
			return fmt.Errorf("%s: %w", d.path, err)
		}
		if compiled == nil {
			continue
		}
		if err = compiled.validate(d.obj); err != nil {
			return fmt.Errorf("%s %s/%s: %w", d.path, gvk.Kind, d.obj.GetName(), err)
		}
	}
	return nil
}
