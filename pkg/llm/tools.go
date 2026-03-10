// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package llm

import "github.com/tmc/langchaingo/llms"

// TroubleshootTools returns the tool definitions for the troubleshooting agent.
func TroubleshootTools(includeCollect bool) []llms.Tool {
	tools := []llms.Tool{}

	if includeCollect {
		tools = append(tools, llms.Tool{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        "collect_sosreport",
				Description: "Collect diagnostic data from the Kubernetes cluster running NVIDIA Network Operator. Returns the path to the unpacked sosreport directory containing logs, CRDs, pod statuses, OFED diagnostics, node info, and more.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"namespace": map[string]any{
							"type":        "string",
							"description": "Namespace where Network Operator is deployed. If omitted, the script auto-detects the namespace.",
						},
					},
					"required": []string{},
				},
			},
		})
	}

	tools = append(tools, llms.Tool{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        "read_file",
			Description: "Read a file or list a directory from the collected sosreport. Use this to traverse and analyze the diagnostic data. If the path is a directory, returns a listing of its contents.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute path to the file or directory to read within the sosreport output.",
					},
				},
				"required": []string{"path"},
			},
		},
	})

	return tools
}
