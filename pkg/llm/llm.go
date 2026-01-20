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

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/googleai"
	"github.com/tmc/langchaingo/llms/openai"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Supported LLM vendors
const (
	VendorOpenAI      = "openai"
	VendorOpenAIAzure = "openai-azure"
	VendorAnthropic   = "anthropic"
	VendorGemini      = "gemini"
)

// createLLM creates an LLM instance based on the vendor configuration.
func createLLM(llmApiKey string, llmApiUrl string, llmVendor string, llmModel string) (llms.Model, error) {
	switch llmVendor {
	case VendorOpenAI:
		options := []openai.Option{
			openai.WithToken(llmApiKey),
		}
		if llmApiUrl != "" {
			options = append(options, openai.WithBaseURL(llmApiUrl))
		}
		if llmModel != "" {
			options = append(options, openai.WithModel(llmModel))
		}
		return openai.New(options...)

	case VendorOpenAIAzure:
		options := []openai.Option{
			openai.WithAPIType(openai.APITypeAzure),
			openai.WithToken(llmApiKey),
			openai.WithBaseURL(llmApiUrl),
			openai.WithModel(llmModel),
			openai.WithEmbeddingModel(llmModel),
			//openai.WithAPIVersion("2025-02-01-preview"),
		}
		return openai.New(options...)

	case VendorAnthropic:
		options := []anthropic.Option{
			anthropic.WithToken(llmApiKey),
		}
		if llmApiUrl != "" {
			options = append(options, anthropic.WithBaseURL(llmApiUrl))
		}
		if llmModel != "" {
			options = append(options, anthropic.WithModel(llmModel))
		}
		return anthropic.New(options...)

	case VendorGemini:
		options := []googleai.Option{
			googleai.WithAPIKey(llmApiKey),
		}
		if llmModel != "" {
			options = append(options, googleai.WithDefaultModel(llmModel))
		}
		return googleai.New(context.Background(), options...)

	default:
		return nil, fmt.Errorf("unsupported LLM vendor: %s. Supported vendors: %s, %s, %s, %s",
			llmVendor, VendorOpenAI, VendorOpenAIAzure, VendorAnthropic, VendorGemini)
	}
}

func SelectPrompt(promptPath string, config config.ClusterConfig, llmApiKey string, llmApiUrl string, llmVendor string) (map[string]string, error) {
	return SelectPromptWithModel(promptPath, config, llmApiKey, llmApiUrl, llmVendor, "")
}

func SelectPromptWithModel(promptPath string, config config.ClusterConfig, llmApiKey string, llmApiUrl string, llmVendor string, llmModel string) (map[string]string, error) {
	llm, err := createLLM(llmApiKey, llmApiUrl, llmVendor, llmModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	data, err := os.ReadFile("system-prompt")
	if err != nil {
		return nil, err
	}

	prompt := string(data)

	configJson, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	prompt = fmt.Sprintf("%s\n%s\nUSER:", prompt, string(configJson))

	data, err = os.ReadFile(promptPath)
	if err != nil {
		return nil, err
	}
	prompt = fmt.Sprintf("%s\n%s", prompt, string(data))

	log.Log.V(1).Info("User prompt", "prompt", string(data))

	response, err := llms.GenerateFromSinglePrompt(context.Background(), llm, prompt, llms.WithTemperature(0.5))
	if err != nil {
		return nil, err
	}

	log.Log.V(1).Info("LLM Response", "response", response)

	// Strip markdown code blocks if present
	response = trimMarkdownJSON(response)

	// First unmarshal to interface{} to handle mixed types (bool, string, number, etc.)
	var rawResponse map[string]interface{}
	err = json.Unmarshal([]byte(response), &rawResponse)
	if err != nil {
		return nil, err
	}

	// Convert all values to strings
	jsonResponse := make(map[string]string)
	for k, v := range rawResponse {
		jsonResponse[k] = fmt.Sprintf("%v", v)
	}

	return jsonResponse, nil
}

// trimMarkdownJSON removes markdown code block formatting from JSON responses.
// Some LLMs wrap JSON in ```json ... ``` even when instructed not to.
func trimMarkdownJSON(s string) string {
	s = strings.TrimSpace(s)

	// Check for ```json or ``` at the start
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}

	// Check for ``` at the end
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}

	return strings.TrimSpace(s)
}

// InteractivePromptSuffix is appended to each LLM response in interactive mode
const InteractivePromptSuffix = "\n\n---\nIf you would like to generate the manifests for the recommended profile, type 'generate'. If you want to ask another question, type it here."

// ChatSession manages an interactive conversation with the LLM
type ChatSession struct {
	llm           llms.Model
	messages      []llms.MessageContent
	systemPrompt  string
	clusterConfig string
	lastResponse  string
}

// NewChatSession creates a new interactive chat session
func NewChatSession(clusterConfig config.ClusterConfig, llmApiKey, llmApiUrl, llmVendor, llmModel string) (*ChatSession, error) {
	llm, err := createLLM(llmApiKey, llmApiUrl, llmVendor, llmModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	data, err := os.ReadFile("system-prompt")
	if err != nil {
		return nil, fmt.Errorf("failed to read system prompt: %w", err)
	}

	configJSON, err := json.Marshal(clusterConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cluster config: %w", err)
	}

	systemPrompt := fmt.Sprintf("%s\n%s", string(data), string(configJSON))

	return &ChatSession{
		llm:           llm,
		messages:      []llms.MessageContent{},
		systemPrompt:  systemPrompt,
		clusterConfig: string(configJSON),
	}, nil
}

// SendMessage sends a user message and returns the LLM response
func (c *ChatSession) SendMessage(ctx context.Context, userMessage string) (string, error) {
	// Add user message to history
	c.messages = append(c.messages, llms.TextParts(llms.ChatMessageTypeHuman, userMessage))

	// Build messages with system prompt
	allMessages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, c.systemPrompt),
	}
	allMessages = append(allMessages, c.messages...)

	log.Log.V(1).Info("Sending message to LLM", "userMessage", userMessage)

	// Generate response
	response, err := c.llm.GenerateContent(ctx, allMessages, llms.WithTemperature(0.5))
	if err != nil {
		return "", fmt.Errorf("failed to generate response: %w", err)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("no response from LLM")
	}

	assistantMessage := response.Choices[0].Content
	c.lastResponse = assistantMessage

	// Add assistant response to history
	c.messages = append(c.messages, llms.TextParts(llms.ChatMessageTypeAI, assistantMessage))

	log.Log.V(1).Info("LLM Response", "response", assistantMessage)

	return assistantMessage, nil
}

// ExtractProfile extracts the profile configuration from the last LLM response
func (c *ChatSession) ExtractProfile() (map[string]string, error) {
	if c.lastResponse == "" {
		return nil, fmt.Errorf("no response to extract profile from")
	}

	// Try to extract JSON from the response
	response := trimMarkdownJSON(c.lastResponse)

	// Try to find JSON object in the response
	startIdx := strings.Index(response, "{")
	endIdx := strings.LastIndex(response, "}")

	if startIdx == -1 || endIdx == -1 || endIdx <= startIdx {
		return nil, fmt.Errorf("no valid JSON found in response")
	}

	jsonStr := response[startIdx : endIdx+1]

	// First unmarshal to interface{} to handle mixed types (bool, string, etc.)
	var rawResponse map[string]interface{}
	err := json.Unmarshal([]byte(jsonStr), &rawResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to parse profile JSON: %w", err)
	}

	// Convert all values to strings
	jsonResponse := make(map[string]string)
	for k, v := range rawResponse {
		jsonResponse[k] = fmt.Sprintf("%v", v)
	}

	return jsonResponse, nil
}
