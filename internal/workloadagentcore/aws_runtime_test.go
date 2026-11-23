package workloadagentcore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	agentcore "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

type fakeAgentCoreAPI struct {
	invokeInput  *agentcore.InvokeAgentRuntimeInput
	stopInput    *agentcore.StopRuntimeSessionInput
	invokeOutput *agentcore.InvokeAgentRuntimeOutput
	stopOutput   *agentcore.StopRuntimeSessionOutput
	invokeErr    error
	stopErr      error
	invokeRetry  int
	stopRetry    int
}

func (f *fakeAgentCoreAPI) InvokeAgentRuntime(_ context.Context, input *agentcore.InvokeAgentRuntimeInput,
	options ...func(*agentcore.Options)) (*agentcore.InvokeAgentRuntimeOutput, error) {
	f.invokeInput = input
	for _, option := range options {
		var configured agentcore.Options
		option(&configured)
		f.invokeRetry = configured.RetryMaxAttempts
	}
	return f.invokeOutput, f.invokeErr
}

func (f *fakeAgentCoreAPI) StopRuntimeSession(_ context.Context, input *agentcore.StopRuntimeSessionInput,
	options ...func(*agentcore.Options)) (*agentcore.StopRuntimeSessionOutput, error) {
	f.stopInput = input
	for _, option := range options {
		var configured agentcore.Options
		option(&configured)
		f.stopRetry = configured.RetryMaxAttempts
	}
	return f.stopOutput, f.stopErr
}

func TestAWSRuntimeProviderInvokeUsesFrozenIdentityAndSingleAttempt(t *testing.T) {
	target := Runtime{ARN: "arn:aws:bedrock-agentcore:ap-northeast-1:123456789012:runtime/hako", Qualifier: "DEFAULT"}
	api := &fakeAgentCoreAPI{invokeOutput: &agentcore.InvokeAgentRuntimeOutput{
		StatusCode: int32Pointer(200), Response: io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}}
	provider, err := NewAWSRuntimeProvider(api)
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Invoke(context.Background(), target, "session_1", []byte(`{"input":1}`))
	if err != nil || string(response) != `{"ok":true}` || api.invokeRetry != 1 {
		t.Fatalf("invoke = %q, retry=%d, err=%v", response, api.invokeRetry, err)
	}
	input := api.invokeInput
	if input == nil || input.AgentRuntimeArn == nil || *input.AgentRuntimeArn != target.ARN ||
		input.Qualifier == nil || *input.Qualifier != target.Qualifier ||
		input.RuntimeSessionId == nil || *input.RuntimeSessionId != "session_1" ||
		input.ContentType == nil || *input.ContentType != "application/json" ||
		input.Accept == nil || *input.Accept != "application/json" || string(input.Payload) != `{"input":1}` {
		t.Fatalf("invoke used unexpected target or payload: %+v", input)
	}
}

func TestAWSRuntimeProviderRejectsAmbiguousAndOversizedInvoke(t *testing.T) {
	target := Runtime{ARN: "arn:aws:bedrock-agentcore:ap-northeast-1:123456789012:runtime/hako", Qualifier: "DEFAULT"}
	for name, output := range map[string]*agentcore.InvokeAgentRuntimeOutput{
		"status":   {StatusCode: int32Pointer(202), Response: io.NopCloser(strings.NewReader(`{}`))},
		"oversize": {StatusCode: int32Pointer(200), Response: io.NopCloser(io.LimitReader(strings.NewReader(strings.Repeat("x", maxInvocationResponseBytes+1)), maxInvocationResponseBytes+1))},
	} {
		t.Run(name, func(t *testing.T) {
			provider, _ := NewAWSRuntimeProvider(&fakeAgentCoreAPI{invokeOutput: output})
			if _, err := provider.Invoke(context.Background(), target, "session_1", []byte(`{}`)); !errors.Is(err, ErrInvocationOutcomeUnknown) {
				t.Fatalf("ambiguous response accepted: %v", err)
			}
		})
	}
	provider, _ := NewAWSRuntimeProvider(&fakeAgentCoreAPI{invokeErr: errors.New("provider details")})
	if _, err := provider.Invoke(context.Background(), target, "session_1", []byte(`{}`)); !errors.Is(err, ErrInvocationOutcomeUnknown) {
		t.Fatalf("provider error was not treated as ambiguous: %v", err)
	}
}

func TestAWSRuntimeProviderStopUsesStableTokenAndConfirmsExactSession(t *testing.T) {
	target := Runtime{ARN: "arn:aws:bedrock-agentcore:ap-northeast-1:123456789012:runtime/hako", Qualifier: "DEFAULT"}
	api := &fakeAgentCoreAPI{stopOutput: &agentcore.StopRuntimeSessionOutput{
		StatusCode: int32Pointer(200), RuntimeSessionId: stringPointer("session_1"),
	}}
	provider, _ := NewAWSRuntimeProvider(api)
	if err := provider.StopRuntimeSession(context.Background(), target, "session_1", "stable-token"); err != nil {
		t.Fatal(err)
	}
	input := api.stopInput
	if api.stopRetry != 1 || input == nil || input.AgentRuntimeArn == nil || *input.AgentRuntimeArn != target.ARN ||
		input.Qualifier == nil || *input.Qualifier != target.Qualifier ||
		input.RuntimeSessionId == nil || *input.RuntimeSessionId != "session_1" ||
		input.ClientToken == nil || *input.ClientToken != "stable-token" {
		t.Fatalf("unexpected stop request: %+v retry=%d", input, api.stopRetry)
	}
	api.stopOutput.RuntimeSessionId = stringPointer("different-session")
	if err := provider.StopRuntimeSession(context.Background(), target, "session_1", "stable-token"); !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("foreign session stop was confirmed: %v", err)
	}
	api.stopErr = errors.New("ambiguous provider error")
	if err := provider.StopRuntimeSession(context.Background(), target, "session_1", "stable-token"); !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("ambiguous stop was confirmed: %v", err)
	}
}

func TestAWSRuntimeProviderRejectsInvalidCallBeforeProvider(t *testing.T) {
	api := &fakeAgentCoreAPI{}
	provider, _ := NewAWSRuntimeProvider(api)
	_, err := provider.Invoke(context.Background(), Runtime{}, "session_1", []byte("x"))
	if !errors.Is(err, ErrNotReady) || api.invokeInput != nil {
		t.Fatalf("invalid invocation reached provider: input=%+v err=%v", api.invokeInput, err)
	}
}

func int32Pointer(value int32) *int32 { return &value }
