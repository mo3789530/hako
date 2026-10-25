package workloadagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type anthropicRoundTripFunc func(*http.Request) (*http.Response, error)

func (f anthropicRoundTripFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAnthropicMessagesClientUsesFixedEndpointAndParsesClientToolUse(t *testing.T) {
	credential := []byte("sk-ant-synthetic")
	doer := anthropicRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != anthropicMessagesEndpoint ||
			request.Header.Get("Content-Type") != "application/json" || request.Header.Get("anthropic-version") != anthropicAPIVersion ||
			request.Header.Get("x-api-key") != string(credential) {
			t.Errorf("unexpected Anthropic request: method=%s url=%s headers=%v", request.Method, request.URL, request.Header)
		}
		var input AnthropicMessagesRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Model != "claude-sonnet-4-6" || input.MaxTokens != 512 ||
			len(input.Tools) != 1 || input.ToolChoice == nil || !input.ToolChoice.DisableParallelToolUse {
			t.Errorf("unexpected Messages request: %+v err=%v", input, err)
		}
		return anthropicJSONResponse(http.StatusOK, `{"id":"msg_123","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_123","name":"read_file","input":{"path":"README.md"}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":4,"cache_read_input_tokens":0}}`), nil
	})
	client, err := NewAnthropicMessagesClient("claude-sonnet-4-6", doer)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateMessage(context.Background(), credential, AnthropicMessagesRequest{
		MaxTokens: 512, Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"inspect the project"}]`)}},
		Tools:      []AnthropicTool{{Name: "read_file", Description: "Read a repository file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
		ToolChoice: &AnthropicToolChoice{Type: "auto", DisableParallelToolUse: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := response.ToolUses()
	if err != nil || len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("parsed tool calls=%+v error=%v", tools, err)
	}
	assistant, err := response.AssistantMessage()
	if err != nil || assistant.Role != "assistant" || !strings.Contains(string(assistant.Content), "toolu_123") {
		t.Fatalf("assistant message=%+v error=%v", assistant, err)
	}
}

func TestAnthropicMessagesClientRejectsInvalidRequestsAndRedactsProviderErrors(t *testing.T) {
	if _, err := NewAnthropicMessagesClient("unknown-model", nil); err == nil {
		t.Fatal("invalid model name was accepted")
	}
	client, err := NewAnthropicMessagesClient("claude-sonnet-4-6", anthropicRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return anthropicJSONResponse(http.StatusUnauthorized, `{"error":"sk-ant-provider-secret"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateMessage(context.Background(), []byte("credential"), AnthropicMessagesRequest{
		MaxTokens: 100, Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"do work"}]`)}},
	})
	if err == nil || strings.Contains(err.Error(), "sk-ant-provider-secret") || strings.Contains(err.Error(), "credential") {
		t.Fatalf("provider response was not safely rejected: %v", err)
	}
	_, err = client.CreateMessage(context.Background(), []byte("credential"), AnthropicMessagesRequest{
		Model: "claude-opus-5", MaxTokens: 100, Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[]`)}},
	})
	if err == nil {
		t.Fatal("request override changed the operator-selected model")
	}
}

func TestAnthropicMessagesClientRejectsMalformedOrUnboundedToolResponses(t *testing.T) {
	for _, body := range []string{
		`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_1","name":"../run","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{} }],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"bash","text":"rm -rf /"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	} {
		client, err := NewAnthropicMessagesClient("claude-sonnet-4-6", anthropicRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return anthropicJSONResponse(http.StatusOK, body), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.CreateMessage(context.Background(), []byte("credential"), AnthropicMessagesRequest{
			MaxTokens: 100, Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"task"}]`)}},
		})
		if err == nil {
			t.Errorf("accepted invalid response: %s", body)
		}
	}
}

func anthropicJSONResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}
