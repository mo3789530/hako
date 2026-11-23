package workloadagentcore

import (
	"context"
	"errors"
	"io"

	agentcore "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// AgentCoreAPI is the narrow AWS SDK boundary used by AWSRuntimeProvider.
// It keeps provider request construction testable without AWS credentials or
// service calls.
type AgentCoreAPI interface {
	InvokeAgentRuntime(context.Context, *agentcore.InvokeAgentRuntimeInput, ...func(*agentcore.Options)) (*agentcore.InvokeAgentRuntimeOutput, error)
	StopRuntimeSession(context.Context, *agentcore.StopRuntimeSessionInput, ...func(*agentcore.Options)) (*agentcore.StopRuntimeSessionOutput, error)
}

// AWSRuntimeProvider adapts the official AgentCore Runtime SDK to the
// attempt-fenced invocation and cleanup interfaces. Each SDK call is limited
// to one transport attempt: InvokeAgentRuntime has no idempotency token, so a
// transparent retry could run the coding agent twice. The durable worker owns
// retries and reuses the same Stop client token for cleanup.
type AWSRuntimeProvider struct {
	api AgentCoreAPI
}

func NewAWSRuntimeProvider(api AgentCoreAPI) (*AWSRuntimeProvider, error) {
	if api == nil {
		return nil, errors.New("AgentCore API client is required")
	}
	return &AWSRuntimeProvider{api: api}, nil
}

var _ RuntimeInvoker = (*AWSRuntimeProvider)(nil)
var _ RuntimeSessionStopper = (*AWSRuntimeProvider)(nil)

func (p *AWSRuntimeProvider) Invoke(ctx context.Context, runtime Runtime, sessionID string, payload []byte) ([]byte, error) {
	if p == nil || p.api == nil || ctx == nil || ctx.Err() != nil ||
		!runtimeARN.MatchString(runtime.ARN) || !qualifier.MatchString(runtime.Qualifier) ||
		!identityPart.MatchString(sessionID) || len(payload) == 0 || len(payload) > maxInvocationBodyBytes {
		return nil, ErrNotReady
	}
	output, err := p.api.InvokeAgentRuntime(ctx, &agentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  stringPointer(runtime.ARN),
		Qualifier:        stringPointer(runtime.Qualifier),
		RuntimeSessionId: stringPointer(sessionID),
		ContentType:      stringPointer("application/json"),
		Accept:           stringPointer("application/json"),
		Payload:          append([]byte(nil), payload...),
	}, func(options *agentcore.Options) { options.RetryMaxAttempts = 1 })
	if err != nil || output == nil || output.Response == nil {
		return nil, ErrInvocationOutcomeUnknown
	}
	defer output.Response.Close()
	if output.StatusCode == nil || *output.StatusCode != 200 {
		return nil, ErrInvocationOutcomeUnknown
	}
	response, err := io.ReadAll(io.LimitReader(output.Response, int64(maxInvocationResponseBytes)+1))
	if err != nil || len(response) == 0 || len(response) > maxInvocationResponseBytes {
		return nil, ErrInvocationOutcomeUnknown
	}
	return response, nil
}

func (p *AWSRuntimeProvider) StopRuntimeSession(ctx context.Context, runtime Runtime, sessionID, clientToken string) error {
	if p == nil || p.api == nil || ctx == nil || ctx.Err() != nil ||
		!runtimeARN.MatchString(runtime.ARN) || !qualifier.MatchString(runtime.Qualifier) ||
		!identityPart.MatchString(sessionID) || len(clientToken) < 1 || len(clientToken) > 128 {
		return ErrNotReady
	}
	output, err := p.api.StopRuntimeSession(ctx, &agentcore.StopRuntimeSessionInput{
		AgentRuntimeArn:  stringPointer(runtime.ARN),
		Qualifier:        stringPointer(runtime.Qualifier),
		RuntimeSessionId: stringPointer(sessionID),
		ClientToken:      stringPointer(clientToken),
	}, func(options *agentcore.Options) { options.RetryMaxAttempts = 1 })
	if err != nil || output == nil || output.StatusCode == nil || *output.StatusCode < 200 || *output.StatusCode >= 300 {
		return ErrCleanupUnconfirmed
	}
	if output.RuntimeSessionId != nil && *output.RuntimeSessionId != sessionID {
		return ErrCleanupUnconfirmed
	}
	return nil
}

func stringPointer(value string) *string { return &value }
