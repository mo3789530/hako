package workloadagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	anthropicMessagesEndpoint = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion       = "2023-06-01"
	maxAnthropicRequestBytes  = 1 << 20
	maxAnthropicResponseBytes = 1 << 20
	maxAnthropicToolInput     = 64 << 10
)

var anthropicModelPattern = regexp.MustCompile(`^claude-[a-z0-9][a-z0-9.-]{0,127}$`)

var ErrNativeAgentModelUnavailable = errors.New("Native Agent model request failed")

// AnthropicMessage is the provider-neutral message envelope used by the
// Native Agent tool loop. Content is a JSON array so provider content blocks
// can be returned unchanged in the next request.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type AnthropicToolChoice struct {
	Type                   string `json:"type"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type AnthropicMessagesRequest struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	System     string               `json:"system,omitempty"`
	Messages   []AnthropicMessage   `json:"messages"`
	Tools      []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice *AnthropicToolChoice `json:"tool_choice,omitempty"`
}

type AnthropicContentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Text      string          `json:"text,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type AnthropicMessagesResponse struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Role       string                 `json:"role"`
	Model      string                 `json:"model"`
	Content    []json.RawMessage      `json:"content"`
	StopReason string                 `json:"stop_reason"`
	Usage      AnthropicMessagesUsage `json:"usage"`
}

type AnthropicMessagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type AnthropicMessagesDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// AnthropicMessagesClient calls only Anthropic's fixed HTTPS Messages
// endpoint. It disables proxy discovery and redirects and never includes
// response bodies or credential values in errors.
type AnthropicMessagesClient struct {
	model string
	doer  AnthropicMessagesDoer
}

func NewAnthropicMessagesClient(model string, doer AnthropicMessagesDoer) (*AnthropicMessagesClient, error) {
	if !anthropicModelPattern.MatchString(model) {
		return nil, ErrNativeAgentModelUnavailable
	}
	if doer == nil {
		transport := &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   2,
		}
		doer = &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	return &AnthropicMessagesClient{model: model, doer: doer}, nil
}

func (c *AnthropicMessagesClient) CreateMessage(ctx context.Context, credential []byte, input AnthropicMessagesRequest) (AnthropicMessagesResponse, error) {
	if c == nil || c.doer == nil || ctx == nil || ctx.Err() != nil || !validAnthropicCredential(credential) ||
		input.Model != "" && input.Model != c.model || input.MaxTokens < 1 || input.MaxTokens > 8192 ||
		len(input.Messages) == 0 || len(input.Messages) > 64 || len(input.Tools) > 16 || !validAnthropicRequest(input) {
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	input.Model = c.model
	body, err := json.Marshal(input)
	if err != nil || len(body) == 0 || len(body) > maxAnthropicRequestBytes {
		clear(body)
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	defer clear(body)
	u, _ := url.Parse(anthropicMessagesEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	req.Header.Set("x-api-key", string(credential))
	resp, err := c.doer.Do(req)
	if err != nil || resp == nil || resp.Body == nil {
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxAnthropicResponseBytes {
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAnthropicResponseBytes+1))
	if err != nil || len(responseBody) == 0 || len(responseBody) > maxAnthropicResponseBytes {
		clear(responseBody)
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	defer clear(responseBody)
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" {
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	var result AnthropicMessagesResponse
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Model != c.model || !validAnthropicResponse(result) {
		return AnthropicMessagesResponse{}, ErrNativeAgentModelUnavailable
	}
	return result, nil
}

func validAnthropicCredential(credential []byte) bool {
	return len(credential) > 0 && len(credential) <= 8<<10 && !bytesContainNUL(credential)
}

func validAnthropicRequest(input AnthropicMessagesRequest) bool {
	if input.System != "" && len(input.System) > 32<<10 {
		return false
	}
	for _, message := range input.Messages {
		if message.Role != "user" && message.Role != "assistant" || len(message.Content) == 0 || len(message.Content) > maxAnthropicRequestBytes {
			return false
		}
		var blocks []json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil || len(blocks) == 0 || len(blocks) > 64 {
			return false
		}
	}
	seen := map[string]struct{}{}
	for _, tool := range input.Tools {
		if !validNativeAgentToolName(tool.Name) || len(tool.Description) == 0 || len(tool.Description) > 2048 ||
			len(tool.InputSchema) == 0 || len(tool.InputSchema) > 16<<10 {
			return false
		}
		if _, ok := seen[tool.Name]; ok {
			return false
		}
		seen[tool.Name] = struct{}{}
		var schema map[string]json.RawMessage
		if json.Unmarshal(tool.InputSchema, &schema) != nil || len(schema) == 0 || string(schema["type"]) != `"object"` {
			return false
		}
	}
	if input.ToolChoice != nil && (input.ToolChoice.Type != "auto" && input.ToolChoice.Type != "any" && input.ToolChoice.Type != "none") {
		return false
	}
	return true
}

var nativeAgentToolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validNativeAgentToolName(name string) bool { return nativeAgentToolName.MatchString(name) }

func validAnthropicResponse(response AnthropicMessagesResponse) bool {
	if response.ID == "" || len(response.ID) > 128 || response.Type != "message" || response.Role != "assistant" ||
		response.Model == "" || !anthropicModelPattern.MatchString(response.Model) || len(response.Content) == 0 || len(response.Content) > 32 ||
		(response.StopReason != "end_turn" && response.StopReason != "tool_use" && response.StopReason != "max_tokens") ||
		response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 {
		return false
	}
	toolUse := false
	for _, raw := range response.Content {
		if len(raw) == 0 || len(raw) > maxAnthropicToolInput+4096 {
			return false
		}
		var block AnthropicContentBlock
		if json.Unmarshal(raw, &block) != nil {
			return false
		}
		switch block.Type {
		case "text":
			if len(block.Text) > 32<<10 {
				return false
			}
		case "tool_use":
			if !validNativeAgentToolName(block.Name) || block.ID == "" || len(block.ID) > 128 || len(block.Input) == 0 || len(block.Input) > maxAnthropicToolInput {
				return false
			}
			var input map[string]json.RawMessage
			if json.Unmarshal(block.Input, &input) != nil || input == nil {
				return false
			}
			toolUse = true
		case "thinking":
			if len(block.Thinking) > 32<<10 || len(block.Signature) > 4096 {
				return false
			}
		default:
			return false
		}
	}
	if response.StopReason == "tool_use" && !toolUse || response.StopReason == "end_turn" && toolUse {
		return false
	}
	return true
}

func (r AnthropicMessagesResponse) ToolUses() ([]AnthropicContentBlock, error) {
	if !validAnthropicResponse(r) {
		return nil, ErrNativeAgentModelUnavailable
	}
	tools := make([]AnthropicContentBlock, 0)
	for _, raw := range r.Content {
		var block AnthropicContentBlock
		if json.Unmarshal(raw, &block) != nil {
			return nil, ErrNativeAgentModelUnavailable
		}
		if block.Type == "tool_use" {
			tools = append(tools, block)
		}
	}
	return tools, nil
}

func (r AnthropicMessagesResponse) AssistantMessage() (AnthropicMessage, error) {
	if !validAnthropicResponse(r) {
		return AnthropicMessage{}, ErrNativeAgentModelUnavailable
	}
	content, err := json.Marshal(r.Content)
	if err != nil || len(content) == 0 || len(content) > maxAnthropicResponseBytes {
		return AnthropicMessage{}, fmt.Errorf("%w: invalid assistant message", ErrNativeAgentModelUnavailable)
	}
	return AnthropicMessage{Role: "assistant", Content: content}, nil
}
