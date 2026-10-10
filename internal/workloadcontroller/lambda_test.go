package workloadcontroller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type lambdaStub struct {
	config           LambdaConfig
	inputs           []*lambdamicrovms.RunMicrovmInput
	state            types.MicrovmState
	runErrOnce       bool
	terminateErr     error
	terminatePending bool
	role             string
	endpoint         string
}

func (a *lambdaStub) RunMicrovm(_ context.Context, in *lambdamicrovms.RunMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	a.inputs = append(a.inputs, in)
	if a.runErrOnce {
		a.runErrOnce = false
		return nil, errors.New("lost RunMicrovm response")
	}
	return &lambdamicrovms.RunMicrovmOutput{MicrovmId: aws.String("microvm_1")}, nil
}
func (a *lambdaStub) GetMicrovm(_ context.Context, in *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: in.MicrovmIdentifier, ImageArn: aws.String(a.config.ImageIdentifier), ImageVersion: aws.String(a.config.ImageVersion),
		State: a.state, Endpoint: aws.String(a.endpoint), ExecutionRoleArn: aws.String(a.role)}, nil
}
func (a *lambdaStub) TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	if a.terminateErr != nil {
		return nil, a.terminateErr
	}
	if a.terminatePending {
		a.state = types.MicrovmStateTerminating
	} else {
		a.state = types.MicrovmStateTerminated
	}
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

type agentStub struct {
	startErr error
	starts   int
	handle   string
	result   workloadjobs.WorkloadResult
}

func (a *agentStub) Start(_ context.Context, handle, endpoint string, s Specification) error {
	a.starts++
	a.handle = handle
	return a.startErr
}
func (a *agentStub) GetStatus(context.Context, workloadjournal.Command) (workloadjournal.ExecutionState, error) {
	if a.result.Status == "" {
		return workloadjournal.Running, nil
	}
	return workloadjournal.ExecutionState(a.result.Status), nil
}
func (a *agentStub) CollectResult(context.Context, workloadjournal.Command) (workloadjobs.WorkloadResult, error) {
	return a.result, nil
}

func setupLambda(t *testing.T) (*Controller, *LambdaMicroVM, *lambdaStub, *agentStub, *time.Time) {
	t.Helper()
	c, _, _, now := setupController(t)
	config := LambdaConfig{ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job", ImageVersion: "1", ImageDigest: strings.Repeat("a", 64),
		VPCEgressConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted", IngressConnector: "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS"}
	old := c.resolver
	c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
		s, err := old.Resolve(ctx, id)
		s.ImageIdentifier = config.ImageIdentifier
		s.ImageVersion = config.ImageVersion
		s.Deadline = s.RequestedAt.Add(8 * time.Hour)
		s.IngressNetworkConnectors = []string{config.IngressConnector}
		s.EgressNetworkConnectors = []string{config.VPCEgressConnector}
		return s, err
	})
	api := &lambdaStub{config: config, state: types.MicrovmStateRunning, endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws"}
	agent := &agentStub{}
	r, err := NewLambdaMicroVM(api, c.acceptor.store, c.resolver, agent, config, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	c.runtime = r
	return c, r, api, agent, now
}

func TestLambdaRetriesExactRunParameters(t *testing.T) {
	c, _, api, agent, now := setupLambda(t)
	api.runErrOnce = true
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err == nil {
		t.Fatal("lost response ignored")
	}
	*now = now.Add(time.Minute)
	s, err := c.Ensure(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.inputs) != 2 || !reflect.DeepEqual(api.inputs[0], api.inputs[1]) {
		t.Fatal("RunMicrovm retry changed parameters")
	}
	in := api.inputs[0]
	if aws.ToString(in.ClientToken) != "token_1" || aws.ToInt32(in.MaximumDurationInSeconds) != 28800 || in.ExecutionRoleArn != nil || aws.ToString(in.ImageVersion) != "1" || in.IdlePolicy == nil || aws.ToBool(in.IdlePolicy.AutoResumeEnabled) || aws.ToInt32(in.IdlePolicy.MaxIdleDurationSeconds) != 28800 {
		t.Fatal("unexpected AWS launch parameters")
	}
	if s.Journal.RuntimeHandle != "microvm_1" || agent.handle != "microvm_1" || agent.starts != 1 {
		t.Fatal("allocation binding missing")
	}
}

func TestLambdaPendingAgentReusesBoundMicroVM(t *testing.T) {
	c, _, api, agent, _ := setupLambda(t)
	agent.startErr = ErrMicroVMNotReady
	data := envelope(t, nil)
	s, err := c.Ensure(context.Background(), data)
	if !errors.Is(err, ErrMicroVMNotReady) || s.Journal.ExecutionState != workloadjournal.Launching {
		t.Fatal("marked runtime running before agent ready")
	}
	agent.startErr = nil
	if _, err := c.Ensure(context.Background(), data); err != nil || len(api.inputs) != 1 {
		t.Fatal("restarted instead of probing saved binding", err)
	}
}

func TestLambdaUnknownLaunchThenCancelDiscoversAndStops(t *testing.T) {
	c, runtime, api, agent, _ := setupLambda(t)
	api.runErrOnce = true
	data := envelope(t, nil)
	_, _ = c.Ensure(context.Background(), data)
	runtime.resolver = resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) {
		return Specification{}, errors.New("repository binding revoked")
	})
	s, err := c.Cancel(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.inputs) != 2 || !reflect.DeepEqual(api.inputs[0], api.inputs[1]) || s.Journal.CleanupState != workloadjournal.CleanupConfirmed || agent.starts != 0 {
		t.Fatal("unknown launch cancellation did not recover safely")
	}
}

func TestLambdaCleanupWaitsForConfirmedTermination(t *testing.T) {
	c, _, api, _, _ := setupLambda(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	api.terminatePending = true
	s, err := c.Cancel(context.Background(), data)
	if !errors.Is(err, ErrCleanupPending) || s.Journal.CleanupState == workloadjournal.CleanupConfirmed {
		t.Fatal("TerminateMicrovm ack treated as cleanup confirmation")
	}
	api.terminatePending = false
	s, err = c.RetryCleanup(context.Background(), data)
	if err != nil || s.Journal.CleanupState != workloadjournal.CleanupConfirmed {
		t.Fatal("cleanup did not recover", err)
	}
}

func TestLambdaRejectsUnsupportedDurationAndUnsafeNetwork(t *testing.T) {
	for _, field := range []string{"duration", "default_network", "internet", "mutable_image", "digest"} {
		t.Run(field, func(t *testing.T) {
			c, r, api, _, _ := setupLambda(t)
			old := c.resolver
			c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
				s, err := old.Resolve(ctx, id)
				switch field {
				case "duration":
					s.Deadline = s.RequestedAt.Add(9 * time.Hour)
				case "default_network":
					s.EgressNetworkConnectors = nil
				case "internet":
					s.EgressNetworkConnectors = []string{"INTERNET_EGRESS"}
				case "mutable_image":
					s.ImageVersion = "LATEST"
				case "digest":
					s.ImageDigest = strings.Repeat("e", 64)
				}
				return s, err
			})
			r.resolver = c.resolver
			s, err := c.Ensure(context.Background(), envelope(t, nil))
			if !errors.Is(err, ErrSpecification) || len(api.inputs) != 0 || s.Journal.ExecutionState != workloadjournal.Accepted {
				t.Fatal("unsafe/unsupported request launched", err)
			}
		})
	}
}

func TestLambdaRejectsUnsafeAgentEndpointAndExecutionRole(t *testing.T) {
	for _, field := range []string{"endpoint", "role"} {
		t.Run(field, func(t *testing.T) {
			c, _, api, agent, _ := setupLambda(t)
			if field == "endpoint" {
				api.endpoint = "https://attacker.example"
			} else {
				api.role = "arn:aws:iam::123456789012:role/broad"
			}
			if _, err := c.Ensure(context.Background(), envelope(t, nil)); err == nil || agent.starts != 0 {
				t.Fatal("unsafe provider response accepted")
			}
		})
	}
}

func TestLambdaCancelBeforeLaunchHasNoAWSComputeCall(t *testing.T) {
	c, _, api, agent, _ := setupLambda(t)
	if _, err := c.Cancel(context.Background(), envelope(t, nil)); err != nil {
		t.Fatal(err)
	}
	if len(api.inputs) != 0 || agent.starts != 0 {
		t.Fatal("cancel before launch created allocation")
	}
}

func TestLambdaInvalidConfig(t *testing.T) {
	c, r, api, agent, _ := setupLambda(t)
	for _, field := range []string{"image", "vpc", "ingress", "version", "digest"} {
		config := r.config
		switch field {
		case "image":
			config.ImageIdentifier = "bad"
		case "vpc":
			config.VPCEgressConnector = ""
		case "ingress":
			config.IngressConnector = "bad"
		case "version":
			config.ImageVersion = "LATEST"
		case "digest":
			config.ImageDigest = "bad"
		}
		if _, err := NewLambdaMicroVM(api, c.acceptor.store, c.resolver, agent, config, nil); !errors.Is(err, ErrSpecification) {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestLambdaAgentCompletionFlowsThroughResultAndCleanup(t *testing.T) {
	c, _, api, agent, _ := setupLambda(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	agent.result = successfulResult(t, c)
	s, err := c.Poll(context.Background(), data, publishFunc(func(_ context.Context, result workloadjobs.WorkloadResult) error {
		if result.Status != agent.result.Status {
			t.Error("agent result changed")
		}
		return nil
	}))
	if err != nil || !s.Journal.ResultPublished || s.Journal.CleanupState != workloadjournal.CleanupConfirmed || api.state != types.MicrovmStateTerminated {
		t.Fatal("AWS adapter result lifecycle failed", err)
	}
}

func TestLambdaTerminationFailureRetainsBindingForRetry(t *testing.T) {
	c, _, api, _, _ := setupLambda(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	api.terminateErr = errors.New("terminate unavailable")
	s, err := c.Cancel(context.Background(), data)
	if err == nil || s.Journal.RuntimeHandle != "microvm_1" || s.Journal.CleanupState != workloadjournal.CleanupFailed {
		t.Fatal("lost binding on stop failure")
	}
	api.terminateErr = nil
	if _, err := c.RetryCleanup(context.Background(), data); err != nil || len(api.inputs) != 1 {
		t.Fatal("cleanup retry relaunched allocation", err)
	}
}
