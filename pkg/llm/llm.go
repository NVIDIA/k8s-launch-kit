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
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/googleai"
	"github.com/tmc/langchaingo/llms/openai"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const maxToolCallIterations = 20
const sosreportScriptPath = "scripts/kubectl-netop_sosreport"

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

func SelectPrompt(promptPath string, config []config.ClusterConfig, llmApiKey string, llmApiUrl string, llmVendor string) (map[string]string, error) {
	return SelectPromptWithModel(promptPath, config, llmApiKey, llmApiUrl, llmVendor, "")
}

func SelectPromptWithModel(promptPath string, config []config.ClusterConfig, llmApiKey string, llmApiUrl string, llmVendor string, llmModel string) (map[string]string, error) {
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
	s = strings.TrimSuffix(s, "```")

	return strings.TrimSpace(s)
}

// StripJSONBlock removes a trailing JSON object from a response string.
// This is used to hide the structured output from the user in non-debug mode.
// Only strips if the JSON block is valid and looks like our structured output.
func StripJSONBlock(s string) string {
	// Find the last } in the string
	lastBrace := strings.LastIndex(s, "}")
	if lastBrace == -1 {
		return s
	}

	// Try progressively earlier { positions to find valid JSON with our fields
	for i := lastBrace; i >= 0; i-- {
		if s[i] != '{' {
			continue
		}

		candidate := s[i : lastBrace+1]

		// Must be valid JSON
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
			continue
		}

		// Must have "confidence" field (present in both profile and troubleshooting output)
		if _, ok := parsed["confidence"]; !ok {
			continue
		}

		// Found it — remove from the response
		result := strings.TrimRight(s[:i], "\n\r\t ")
		trailing := strings.TrimLeft(s[lastBrace+1:], "\n\r\t ")
		if trailing != "" {
			result += "\n" + trailing
		}
		return result
	}

	return s
}

// InteractivePromptSuffix is appended to each LLM response in interactive mode
const InteractivePromptSuffix = "\n\n---\nIf you would like to generate the manifests for the recommended profile, type 'generate'. If you want to ask another question, type it here."

// StatusFunc is a callback for reporting tool execution status to the UI.
// action is "update" (change spinner text) or "done" (finish current step with checkmark).
type StatusFunc func(action, message string)

// ChatSession manages an interactive conversation with the LLM
type ChatSession struct {
	llm           llms.Model
	messages      []llms.MessageContent
	systemPrompt  string
	clusterConfig string
	lastResponse  string
	// Tool calling support for troubleshooting
	tools           []llms.Tool
	scriptPath      string
	kubeconfig      string
	sosreportDir    string // set after collection or pre-provided
	sosreportTarget string // user-specified output path for collection
	// OnStatus is called to report tool execution progress to the UI
	OnStatus StatusFunc
	// Rate limiting (enabled via --llm-throttle)
	throttleEnabled bool
	lastRequestTime time.Time
}

// NewChatSession creates a new interactive chat session.
// If kubeconfig is provided, troubleshooting tools (collect_sosreport, read_file) are enabled.
// If sosreportPath is provided, the collect_sosreport tool is skipped (pre-collected data).
func NewChatSession(clusterConfig []config.ClusterConfig, llmApiKey, llmApiUrl, llmVendor, llmModel string,
	kubeconfig, sosreportPath string, throttle bool) (*ChatSession, error) {
	llm, err := createLLM(llmApiKey, llmApiUrl, llmVendor, llmModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	data, err := os.ReadFile("system-prompt")
	if err != nil {
		return nil, fmt.Errorf("failed to read system prompt: %w", err)
	}

	var configJSON []byte
	if clusterConfig != nil {
		configJSON, err = json.Marshal(clusterConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal cluster config: %w", err)
		}
	}

	systemPrompt := string(data)
	if len(configJSON) > 0 {
		systemPrompt = fmt.Sprintf("%s\n%s", systemPrompt, string(configJSON))
	}

	session := &ChatSession{
		llm:             llm,
		messages:        []llms.MessageContent{},
		systemPrompt:    systemPrompt,
		clusterConfig:   string(configJSON),
		scriptPath:      sosreportScriptPath,
		kubeconfig:      kubeconfig,
		throttleEnabled: throttle,
	}

	// Configure troubleshooting tools
	canCollect := kubeconfig != ""
	if sosreportPath != "" {
		// Check if the path already has sosreport data
		if hasSosreportData(sosreportPath) {
			session.sosreportDir = sosreportPath
			session.tools = TroubleshootTools(canCollect)
			session.systemPrompt += fmt.Sprintf("\n\nPre-collected sosreport data is available at: %s\nYou can use read_file immediately to examine the diagnostic data. No need to call collect_sosreport.", sosreportPath)
		} else if canCollect {
			// Path specified but empty/missing — will be used as collection target
			session.sosreportTarget = sosreportPath
			session.tools = TroubleshootTools(true)
		} else {
			// No data and can't collect
			session.tools = TroubleshootTools(false)
		}
	} else if canCollect {
		session.tools = TroubleshootTools(true)
	}

	return session, nil
}

// SendMessage sends a user message and returns the LLM response.
// If tools are configured, handles tool call loops automatically.
func (c *ChatSession) SendMessage(ctx context.Context, userMessage string) (string, error) {
	// Add user message to history
	c.messages = append(c.messages, llms.TextParts(llms.ChatMessageTypeHuman, userMessage))

	// Compact old tool results to reduce token usage (only with throttling enabled)
	if c.throttleEnabled {
		c.compactToolResults()
	}

	// Build messages with system prompt
	allMessages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, c.systemPrompt),
	}
	allMessages = append(allMessages, c.messages...)

	log.Log.V(2).Info("Sending message to LLM", "userMessage", userMessage,
		"systemPromptLen", len(c.systemPrompt), "messageCount", len(allMessages), "toolCount", len(c.tools))

	// Build call options
	opts := []llms.CallOption{llms.WithTemperature(0.5)}
	if len(c.tools) > 0 {
		opts = append(opts, llms.WithTools(c.tools))
	}

	// Tool-call loop: iterate until no more tool calls or max iterations
	for i := 0; i < maxToolCallIterations; i++ {
		// Throttle requests to avoid rate limits
		c.throttle()

		response, err := c.llm.GenerateContent(ctx, allMessages, opts...)
		if err != nil {
			return "", fmt.Errorf("failed to generate response: %w", err)
		}

		if len(response.Choices) == 0 {
			return "", fmt.Errorf("no response from LLM")
		}

		// Aggregate tool calls and text from all choices.
		// Some providers (e.g. Anthropic) put each content block into a separate Choice:
		// Choices[0] may be text, Choices[1+] may be tool_use blocks.
		var allToolCalls []llms.ToolCall
		var textContent string
		for _, choice := range response.Choices {
			if len(choice.ToolCalls) > 0 {
				allToolCalls = append(allToolCalls, choice.ToolCalls...)
			}
			if choice.Content != "" {
				if textContent != "" {
					textContent += "\n"
				}
				textContent += choice.Content
			}
		}

		// If no tool calls, we have the final response
		if len(allToolCalls) == 0 {
			c.lastResponse = textContent
			c.messages = append(c.messages, llms.TextParts(llms.ChatMessageTypeAI, textContent))
			log.Log.V(2).Info("LLM Response", "response", textContent)
			return textContent, nil
		}

		log.Log.V(2).Info("LLM requested tool calls", "count", len(allToolCalls))

		// Execute each tool call and append as individual message pairs.
		// langchaingo's Anthropic provider only processes Parts[0] per message,
		// so we must send one AI message + one tool response message per tool call.
		for _, tc := range allToolCalls {
			// AI message with tool_use
			aiMsg := llms.MessageContent{
				Role:  llms.ChatMessageTypeAI,
				Parts: []llms.ContentPart{tc},
			}
			c.messages = append(c.messages, aiMsg)
			allMessages = append(allMessages, aiMsg)

			// Build display name for the tool
			toolStatus := tc.FunctionCall.Name
			switch tc.FunctionCall.Name {
			case "collect_sosreport":
				toolStatus = "Collecting sosreport from cluster"
			case "read_file":
				var readArgs map[string]interface{}
				if err := json.Unmarshal([]byte(tc.FunctionCall.Arguments), &readArgs); err == nil {
					if p, ok := readArgs["path"].(string); ok {
						toolStatus = fmt.Sprintf("Reading: %s", p)
					}
				}
			}

			// Update the spinner text and execute
			c.reportStatus("update", toolStatus)
			toolStart := time.Now()
			result := c.executeTool(tc)
			toolDuration := time.Since(toolStart)
			log.Log.V(2).Info("Tool call executed", "tool", tc.FunctionCall.Name, "args", tc.FunctionCall.Arguments, "resultLength", len(result))

			// Anchor the completed step — show duration for slow operations
			doneMsg := toolStatus
			if toolDuration >= 2*time.Second {
				doneMsg = fmt.Sprintf("%s (%s)", toolStatus, formatToolDuration(toolDuration))
			}
			c.reportStatus("done", doneMsg)

			// Tool response message
			toolMsg := llms.MessageContent{
				Role: llms.ChatMessageTypeTool,
				Parts: []llms.ContentPart{
					llms.ToolCallResponse{
						ToolCallID: tc.ID,
						Name:       tc.FunctionCall.Name,
						Content:    result,
					},
				},
			}
			c.messages = append(c.messages, toolMsg)
			allMessages = append(allMessages, toolMsg)
		}

		// Update spinner for the next LLM call
		c.reportStatus("update", "Waiting for AI response")
	}

	return "", fmt.Errorf("tool call loop exceeded maximum iterations (%d)", maxToolCallIterations)
}

// hasSosreportData checks if a directory contains sosreport data.
func hasSosreportData(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	// Check for diagnostic-summary.txt as a reliable indicator
	if _, err := os.Stat(path + "/diagnostic-summary.txt"); err == nil {
		return true
	}
	// Check for typical sosreport subdirectories
	for _, subdir := range []string{"crds", "operator", "nodes", "metadata"} {
		if _, err := os.Stat(path + "/" + subdir); err == nil {
			return true
		}
	}
	return false
}

// reportStatus calls the status callback if set.
func (c *ChatSession) reportStatus(action, message string) {
	if c.OnStatus != nil {
		c.OnStatus(action, message)
	}
}

// formatToolDuration formats a duration for display in completed step messages.
func formatToolDuration(d time.Duration) string {
	d = d.Round(time.Second)
	s := int(d.Seconds())
	if s >= 60 {
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}

// throttle waits if needed to respect rate limits based on estimated token usage.
// With a 30K token/min limit (~120K chars/min), it calculates wait time proportional
// to the total message size being sent.
func (c *ChatSession) throttle() {
	if !c.throttleEnabled {
		return
	}
	if c.lastRequestTime.IsZero() {
		c.lastRequestTime = time.Now()
		return
	}

	// Estimate total chars in the request
	totalChars := len(c.systemPrompt)
	for _, msg := range c.messages {
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case llms.TextContent:
				totalChars += len(p.Text)
			case llms.ToolCallResponse:
				totalChars += len(p.Content)
			case llms.ToolCall:
				totalChars += len(p.FunctionCall.Arguments) + len(p.FunctionCall.Name)
			}
		}
	}

	// ~4 chars per token, 30K tokens/min = 120K chars/min
	// Wait proportionally: (chars / 120K) * 60s, with a minimum of 2s
	const charsPerMinute = 120000
	waitSeconds := float64(totalChars) / float64(charsPerMinute) * 60.0
	if waitSeconds < 2.0 {
		waitSeconds = 2.0
	}
	waitDuration := time.Duration(waitSeconds * float64(time.Second))

	elapsed := time.Since(c.lastRequestTime)
	if elapsed < waitDuration {
		time.Sleep(waitDuration - elapsed)
	}
	c.lastRequestTime = time.Now()
}

const maxToolResultLen = 2000 // compact old tool results to this length

// compactToolResults replaces large tool result contents in older messages
// with a short summary to reduce token usage on subsequent API calls.
// Only the most recent 4 tool result messages are kept at full size.
func (c *ChatSession) compactToolResults() {
	const keepRecent = 4

	// Count tool messages from the end
	toolMsgCount := 0
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == llms.ChatMessageTypeTool {
			toolMsgCount++
		}
	}

	if toolMsgCount <= keepRecent {
		return
	}

	// Compact older tool messages
	compactCount := toolMsgCount - keepRecent
	seen := 0
	for i := 0; i < len(c.messages) && seen < compactCount; i++ {
		if c.messages[i].Role != llms.ChatMessageTypeTool {
			continue
		}
		seen++
		for j, part := range c.messages[i].Parts {
			if tcr, ok := part.(llms.ToolCallResponse); ok && len(tcr.Content) > maxToolResultLen {
				tcr.Content = tcr.Content[:maxToolResultLen] + "\n[... truncated from history ...]"
				c.messages[i].Parts[j] = tcr
			}
		}
	}
}

// executeTool dispatches a tool call to the appropriate handler.
func (c *ChatSession) executeTool(tc llms.ToolCall) string {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.FunctionCall.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: failed to parse tool arguments: %v", err)
	}

	switch tc.FunctionCall.Name {
	case "collect_sosreport":
		return c.executeCollectSosreport(args)
	case "read_file":
		return c.executeReadFile(args)
	default:
		return fmt.Sprintf("Error: unknown tool: %s", tc.FunctionCall.Name)
	}
}

func (c *ChatSession) executeCollectSosreport(args map[string]interface{}) string {
	// Prevent re-collection if sosreport data already exists
	if c.sosreportDir != "" {
		return fmt.Sprintf("Sosreport data already available at: %s\nUse read_file to examine the diagnostic data.", c.sosreportDir)
	}

	if c.kubeconfig == "" {
		return "Error: no kubeconfig configured. Cannot collect sosreport without cluster access."
	}
	if c.scriptPath == "" {
		return "Error: sosreport script path not configured. Use --sosreport-script or run 'make download-sosreport'."
	}

	namespace, _ := args["namespace"].(string)

	dir, err := ExecuteCollectSosreport(c.scriptPath, c.kubeconfig, namespace, c.sosreportTarget)
	if err != nil {
		return fmt.Sprintf("Error collecting sosreport: %v", err)
	}

	c.sosreportDir = dir
	return fmt.Sprintf("Sosreport collected successfully. Data is available at: %s\nYou can now use read_file to examine the diagnostic data.", dir)
}

func (c *ChatSession) executeReadFile(args map[string]interface{}) string {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "Error: 'path' parameter is required"
	}

	if c.sosreportDir == "" {
		return "Error: no sosreport data available. Use collect_sosreport first, or provide --sosreport-path."
	}

	return ExecuteReadFile(path, c.sosreportDir)
}

// ExtractProfile extracts the profile configuration from the last LLM response
func (c *ChatSession) ExtractProfile() (map[string]string, error) {
	if c.lastResponse == "" {
		return nil, fmt.Errorf("no response to extract profile from")
	}

	response := trimMarkdownJSON(c.lastResponse)

	// Find valid JSON object with "confidence" field (our structured output marker)
	lastBrace := strings.LastIndex(response, "}")
	if lastBrace == -1 {
		return nil, fmt.Errorf("no valid JSON found in response")
	}

	var rawResponse map[string]interface{}
	for i := lastBrace; i >= 0; i-- {
		if response[i] != '{' {
			continue
		}
		candidate := response[i : lastBrace+1]
		if err := json.Unmarshal([]byte(candidate), &rawResponse); err == nil {
			if _, ok := rawResponse["confidence"]; ok {
				break
			}
			rawResponse = nil
		}
	}

	if rawResponse == nil {
		return nil, fmt.Errorf("no valid JSON found in response")
	}

	// Convert all values to strings
	jsonResponse := make(map[string]string)
	for k, v := range rawResponse {
		jsonResponse[k] = fmt.Sprintf("%v", v)
	}

	return jsonResponse, nil
}
