// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"helm.sh/helm/v3/pkg/postrender"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	kubeutilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	operatorDeploymentName = "dpf-operator-controller-manager"
	operatorContainerName  = "manager"
	operatorCommand        = "/operator"
	operatorConfigNameFlag = "--config-name"
	operatorConfigNSFlag   = "--config-namespace"
)

// NewOperatorPostRenderer binds the exact DPFOperatorConfig emitted by Render
// to the DPF 26.8 operator Deployment while retaining the chart's other flags.
// The release namespace is supplied by the same Inputs used to render resources.
func NewOperatorPostRenderer(in Inputs) (postrender.PostRenderer, error) {
	if len(validation.IsDNS1123Label(in.OperatorNamespace)) != 0 {
		return nil, fmt.Errorf("operatorNamespace must be a nonempty DNS label")
	}
	if len(validation.IsDNS1123Label(in.OperatorConfigName)) != 0 {
		return nil, fmt.Errorf("operatorConfigName must be a valid Kubernetes object name")
	}
	return operatorPostRenderer{namespace: in.OperatorNamespace, configName: in.OperatorConfigName}, nil
}

type operatorPostRenderer struct {
	namespace  string
	configName string
}

func (r operatorPostRenderer) Run(rendered *bytes.Buffer) (*bytes.Buffer, error) {
	if rendered == nil {
		return nil, fmt.Errorf("rendered Helm manifests must not be nil")
	}
	reader := kubeutilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(rendered.Bytes())))
	var docs [][]byte
	matched := 0
	for ordinal := 1; ; ordinal++ {
		raw, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Helm manifest document %d: %w", ordinal, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var object unstructured.Unstructured
		if err := sigsyaml.Unmarshal(raw, &object.Object); err != nil {
			return nil, fmt.Errorf("decode Helm manifest document %d: %w", ordinal, err)
		}
		// Some selected chart templates attach the release namespace even to
		// cluster-scoped resources. Use the same reviewed scope table as the
		// bundle validator, for both preview and the actual Helm install.
		namespaced, known := bundle.ResourceNamespaced(object.GroupVersionKind().Group, object.GetKind())
		if known && !namespaced && object.GetNamespace() != "" {
			if object.GetNamespace() != r.namespace {
				return nil, fmt.Errorf("cluster-scoped %s/%s has a namespace outside the selected release", object.GetKind(), object.GetName())
			}
			unstructured.RemoveNestedField(object.Object, "metadata", "namespace")
			raw, err = sigsyaml.Marshal(object.Object)
			if err != nil {
				return nil, fmt.Errorf("encode cluster-scoped %s: %w", object.GetKind(), err)
			}
		}
		if object.GetKind() != "Deployment" || !isOperatorDeployment(&object) {
			docs = append(docs, bytes.Clone(raw))
			continue
		}
		if object.GetAPIVersion() != "apps/v1" {
			return nil, fmt.Errorf("DPF operator Deployment has unexpected apiVersion %q", object.GetAPIVersion())
		}
		matched++
		if matched > 1 {
			return nil, fmt.Errorf("multiple DPF operator Deployments found")
		}
		if object.GetName() != operatorDeploymentName {
			return nil, fmt.Errorf("DPF operator Deployment has unexpected name %q", object.GetName())
		}
		if object.GetNamespace() != r.namespace {
			return nil, fmt.Errorf("DPF operator Deployment namespace %q does not match release namespace %q", object.GetNamespace(), r.namespace)
		}
		if err := bindOperatorContainer(&object, r.namespace, r.configName); err != nil {
			return nil, err
		}
		updated, err := sigsyaml.Marshal(object.Object)
		if err != nil {
			return nil, fmt.Errorf("encode DPF operator Deployment: %w", err)
		}
		docs = append(docs, updated)
	}
	if matched == 0 {
		return nil, fmt.Errorf("rendered chart has no DPF operator Deployment")
	}
	var output bytes.Buffer
	for i, doc := range docs {
		if i != 0 {
			output.WriteString("---\n")
		}
		output.Write(bytes.TrimSpace(doc))
		output.WriteByte('\n')
	}
	return &output, nil
}

func isOperatorDeployment(object *unstructured.Unstructured) bool {
	if object.GetName() == operatorDeploymentName || object.GetLabels()["dpu.nvidia.com/component"] == operatorDeploymentName {
		return true
	}
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return false
	}
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := container["name"].(string); name == operatorContainerName {
			return true
		}
		if command, ok := container["command"].([]any); ok && len(command) > 0 && command[0] == operatorCommand {
			return true
		}
	}
	return false
}

func bindOperatorContainer(object *unstructured.Unstructured, namespace, configName string) error {
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return fmt.Errorf("DPF operator Deployment has no containers")
	}
	managerIndex := -1
	for i, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("DPF operator Deployment container %d is malformed", i)
		}
		name, _ := container["name"].(string)
		if name == operatorContainerName {
			if managerIndex >= 0 {
				return fmt.Errorf("DPF operator Deployment has multiple %q containers", operatorContainerName)
			}
			managerIndex = i
		}
	}
	if managerIndex < 0 {
		return fmt.Errorf("DPF operator Deployment has no %q container", operatorContainerName)
	}
	manager := containers[managerIndex].(map[string]any)
	command, ok := stringSlice(manager["command"])
	if !ok || len(command) != 1 || command[0] != operatorCommand {
		return fmt.Errorf("DPF operator %q container must run %s", operatorContainerName, operatorCommand)
	}
	args, ok := stringSlice(manager["args"])
	if !ok && manager["args"] != nil {
		return fmt.Errorf("DPF operator %q args must be a string list", operatorContainerName)
	}
	args, err = bindFlag(args, operatorConfigNSFlag, namespace)
	if err != nil {
		return err
	}
	args, err = bindFlag(args, operatorConfigNameFlag, configName)
	if err != nil {
		return err
	}
	encodedArgs := make([]any, len(args))
	for i, argument := range args {
		encodedArgs[i] = argument
	}
	manager["args"] = encodedArgs
	containers[managerIndex] = manager
	return unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers")
}

func bindFlag(args []string, flag, expected string) ([]string, error) {
	filtered := make([]string, 0, len(args)+1)
	for i := 0; i < len(args); i++ {
		argument := args[i]
		var value string
		matched := false
		switch {
		case argument == flag:
			matched = true
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, fmt.Errorf("operator args contain %s without a value", flag)
			}
			i++
			value = args[i]
		case strings.HasPrefix(argument, flag+"="):
			matched = true
			value = strings.TrimPrefix(argument, flag+"=")
		}
		if matched {
			if value != expected {
				return nil, fmt.Errorf("operator args %s=%q conflicts with rendered DPFOperatorConfig %q", flag, value, expected)
			}
			continue
		}
		filtered = append(filtered, argument)
	}
	return append(filtered, flag+"="+expected), nil
}

func stringSlice(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		result[i] = text
	}
	return result, true
}
