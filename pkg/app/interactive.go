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

package app

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/llm"
)

// runInteractiveSession runs an interactive chat session with the LLM.
func (l *Launcher) runInteractiveSession(clusterConfig []config.ClusterConfig) (map[string]string, error) {
	session, err := llm.NewChatSession(clusterConfig, l.options.LLMApiKey, l.options.LLMApiUrl, l.options.LLMVendor, l.options.LLMModel,
		l.options.Kubeconfig, l.options.SosreportPath, l.options.LLMThrottle)
	if err != nil {
		return nil, fmt.Errorf("failed to create chat session: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Println("\n=== Interactive LLM Session ===")
	fmt.Println("Ask questions about network configuration, describe your requirements,")
	fmt.Println("or ask for help troubleshooting Network Operator issues.")
	fmt.Println("Type 'generate' to generate manifests based on the recommended profile.")
	fmt.Println("Type 'exit' or 'quit' to cancel.")
	fmt.Println("================================")
	fmt.Println()

	for {
		fmt.Print("You: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(input)

		// Check for exit commands
		if strings.EqualFold(input, "exit") || strings.EqualFold(input, "quit") {
			return nil, fmt.Errorf("session cancelled by user")
		}

		// Check for generate command
		if strings.EqualFold(input, "generate") {
			fmt.Println("\nExtracting profile from last response...")
			profile, err := session.ExtractProfile()
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				fmt.Println("Please ask a question first to get a profile recommendation.")
				continue
			}

			if profile["fabric"] == "" || profile["deploymentType"] == "" {
				fmt.Println("No profile recommendation found in the last response.")
				fmt.Println("Ask a profile selection question first (e.g., what deployment type should I use?).")
				continue
			}

			confidence := profile["confidence"]
			if confidence == "low" {
				fmt.Printf("\nWarning: The LLM has low confidence in this recommendation.\n")
				fmt.Printf("Reason: %s\n", profile["reasoning"])
				fmt.Print("Do you want to proceed anyway? (yes/no): ")

				confirm, _ := reader.ReadString('\n')
				confirm = strings.TrimSpace(strings.ToLower(confirm))
				if confirm != "yes" && confirm != "y" {
					fmt.Println("Cancelled. Ask another question or refine your requirements.")
					continue
				}
			}

			fmt.Println("\nProceeding with profile generation...")
			return profile, nil
		}

		// Send message to LLM — single progress indicator, updated in-place
		progress := l.ui.StartProgress("Waiting for AI response")
		session.OnStatus = func(action, message string) {
			switch action {
			case "update":
				progress.Update(message)
			case "done":
				// Finish current step and start a new spinner
				progress.Success(message)
				progress = l.ui.StartProgress("Waiting for AI response")
			}
		}
		response, err := session.SendMessage(context.Background(), input)
		if err != nil {
			progress.Fail("AI request failed")
			fmt.Printf("Error: %v\n", err)
			continue
		}
		progress.Success("Response received")

		// Strip JSON block from display unless debug logging is enabled
		displayResponse := response
		if l.options.LogLevel != "debug" {
			displayResponse = llm.StripJSONBlock(displayResponse)
		}

		fmt.Printf("\nAssistant: %s", displayResponse)
		fmt.Println(llm.InteractivePromptSuffix)
		fmt.Println()
	}
}
