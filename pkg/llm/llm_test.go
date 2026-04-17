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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateLLM_OpenAI(t *testing.T) {
	llm, err := createLLM("test-api-key", "", VendorOpenAI, "gpt-4")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_OpenAIWithBaseURL(t *testing.T) {
	llm, err := createLLM("test-api-key", "https://custom.openai.example.com", VendorOpenAI, "gpt-4")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_OpenAIAzure(t *testing.T) {
	llm, err := createLLM("test-api-key", "https://azure.openai.example.com", VendorOpenAIAzure, "gpt-4")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_Anthropic(t *testing.T) {
	llm, err := createLLM("test-api-key", "", VendorAnthropic, "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_AnthropicWithBaseURL(t *testing.T) {
	llm, err := createLLM("test-api-key", "https://custom.anthropic.example.com", VendorAnthropic, "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_Gemini(t *testing.T) {
	llm, err := createLLM("test-api-key", "", VendorGemini, "gemini-pro")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_GeminiWithDefaultModel(t *testing.T) {
	llm, err := createLLM("test-api-key", "", VendorGemini, "")
	require.NoError(t, err)
	assert.NotNil(t, llm)
}

func TestCreateLLM_UnsupportedVendor(t *testing.T) {
	llm, err := createLLM("test-api-key", "", "unsupported-vendor", "")
	require.Error(t, err)
	assert.Nil(t, llm)
	assert.Contains(t, err.Error(), "unsupported LLM vendor: unsupported-vendor")
	assert.Contains(t, err.Error(), VendorOpenAI)
	assert.Contains(t, err.Error(), VendorOpenAIAzure)
	assert.Contains(t, err.Error(), VendorAnthropic)
	assert.Contains(t, err.Error(), VendorGemini)
}

func TestVendorConstants(t *testing.T) {
	// Verify vendor constants have expected values
	assert.Equal(t, "openai", VendorOpenAI)
	assert.Equal(t, "openai-azure", VendorOpenAIAzure)
	assert.Equal(t, "anthropic", VendorAnthropic)
	assert.Equal(t, "gemini", VendorGemini)
}

func TestTrimMarkdownJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain JSON",
			input:    `{"key": "value"}`,
			expected: `{"key": "value"}`,
		},
		{
			name:     "JSON with json code block",
			input:    "```json\n{\"key\": \"value\"}\n```",
			expected: `{"key": "value"}`,
		},
		{
			name:     "JSON with plain code block",
			input:    "```\n{\"key\": \"value\"}\n```",
			expected: `{"key": "value"}`,
		},
		{
			name:     "JSON with surrounding whitespace",
			input:    "  \n{\"key\": \"value\"}\n  ",
			expected: `{"key": "value"}`,
		},
		{
			name:     "JSON with code block and whitespace",
			input:    "  ```json\n{\"key\": \"value\"}\n```  ",
			expected: `{"key": "value"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := trimMarkdownJSON(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestChatSession_ExtractProfile(t *testing.T) {
	session := &ChatSession{
		lastResponse: `Based on your requirements, here is my recommendation:

{"fabric": "ethernet", "deploymentType": "sriov", "multirail": "true", "spectrumX": "false", "ai": "true", "confidence": "high", "reasoning": "Test reasoning"}

This configuration will work well for your AI workloads.`,
	}

	profile, err := session.ExtractProfile()
	require.NoError(t, err)
	assert.Equal(t, "ethernet", profile["fabric"])
	assert.Equal(t, "sriov", profile["deploymentType"])
	assert.Equal(t, "true", profile["multirail"])
	assert.Equal(t, "high", profile["confidence"])
}

func TestChatSession_ExtractProfile_BooleanValues(t *testing.T) {
	// Test that boolean values in JSON are converted to strings
	session := &ChatSession{
		lastResponse: `Here is my recommendation:

{"fabric": "ethernet", "deploymentType": "sriov", "multirail": true, "spectrumX": false, "ai": true, "confidence": "high", "reasoning": "Test reasoning"}

This should work.`,
	}

	profile, err := session.ExtractProfile()
	require.NoError(t, err)
	assert.Equal(t, "ethernet", profile["fabric"])
	assert.Equal(t, "sriov", profile["deploymentType"])
	assert.Equal(t, "true", profile["multirail"])
	assert.Equal(t, "false", profile["spectrumX"])
	assert.Equal(t, "true", profile["ai"])
	assert.Equal(t, "high", profile["confidence"])
}

func TestChatSession_ExtractProfile_NoJSON(t *testing.T) {
	session := &ChatSession{
		lastResponse: "This is just text without any JSON",
	}

	_, err := session.ExtractProfile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no valid JSON found")
}

func TestChatSession_ExtractProfile_EmptyResponse(t *testing.T) {
	session := &ChatSession{
		lastResponse: "",
	}

	_, err := session.ExtractProfile()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no response to extract")
}

func TestChatSession_ExtractProfile_SpectrumXFields(t *testing.T) {
	session := &ChatSession{
		lastResponse: `Based on your Spectrum-X requirements:

{"fabric": "ethernet", "deploymentType": "sriov", "multirail": "true", "spectrumX": "true", "spectrumXMultiplaneMode": "swplb", "spectrumXNumberOfPlanes": "4", "spectrumXVersion": "RA2.2", "ai": "true", "confidence": "high", "reasoning": "Spectrum-X with swplb for AI training"}

This is the recommended configuration.`,
	}

	profile, err := session.ExtractProfile()
	require.NoError(t, err)
	assert.Equal(t, "ethernet", profile["fabric"])
	assert.Equal(t, "sriov", profile["deploymentType"])
	assert.Equal(t, "true", profile["multirail"])
	assert.Equal(t, "true", profile["spectrumX"])
	assert.Equal(t, "swplb", profile["spectrumXMultiplaneMode"])
	assert.Equal(t, "4", profile["spectrumXNumberOfPlanes"])
	assert.Equal(t, "RA2.2", profile["spectrumXVersion"])
}

func TestChatSession_ExtractProfile_SpectrumXNumericPlanes(t *testing.T) {
	// Test that numeric numberOfPlanes (without quotes) is handled correctly
	session := &ChatSession{
		lastResponse: `{"fabric": "ethernet", "deploymentType": "sriov", "multirail": true, "spectrumX": true, "spectrumXMultiplaneMode": "hwplb", "spectrumXNumberOfPlanes": 2, "spectrumXVersion": "RA2.2", "ai": true, "confidence": "high", "reasoning": "test"}`,
	}

	profile, err := session.ExtractProfile()
	require.NoError(t, err)
	assert.Equal(t, "true", profile["spectrumX"])
	assert.Equal(t, "hwplb", profile["spectrumXMultiplaneMode"])
	assert.Equal(t, "2", profile["spectrumXNumberOfPlanes"])
	assert.Equal(t, "RA2.2", profile["spectrumXVersion"])
}

func TestInteractivePromptSuffix(t *testing.T) {
	assert.Contains(t, InteractivePromptSuffix, "generate")
	assert.Contains(t, InteractivePromptSuffix, "question")
}
