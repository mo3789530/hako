package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
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
	onTerminate      func()
	role             string
	endpoint         string
	token            map[string]string
	tokenInputs      []*lambdamicrovms.CreateMicrovmAuthTokenInput
}

type recordingTicketIssuer struct {
	issued []struct {
		microVMID string
		identity  workloadjournal.Command
		operation workloadagent.Operation
		execution *workloadagent.ExecutionBinding
	}
	err error
}

func (i *recordingTicketIssuer) Issue(vm string, id workloadjournal.Command, op workloadagent.Operation) (string, error) {
	i.issued = append(i.issued, struct {
		microVMID string
		identity  workloadjournal.Command
		operation workloadagent.Operation
		execution *workloadagent.ExecutionBinding
	}{vm, id, op, nil})
	return "signed-agent-ticket", i.err
}

func (i *recordingTicketIssuer) IssueExecution(vm string, id workloadjournal.Command, op workloadagent.Operation, execution workloadagent.ExecutionBinding) (string, error) {
	i.issued = append(i.issued, struct {
		microVMID string
		identity  workloadjournal.Command
		operation workloadagent.Operation
		execution *workloadagent.ExecutionBinding
	}{vm, id, op, &execution})
	return "signed-agent-ticket", i.err
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
func (a *lambdaStub) CreateMicrovmAuthToken(_ context.Context, in *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	a.tokenInputs = append(a.tokenInputs, in)
	return &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: a.token}, nil
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
	if a.onTerminate != nil {
		a.onTerminate()
	}
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

type agentStub struct {
	startErr   error
	starts     int
	handle     string
	result     workloadjobs.WorkloadResult
	connection AgentConnection
}

func (a *agentStub) Start(_ context.Context, connection AgentConnection, s Specification) error {
	a.starts++
	a.handle = connection.MicroVMID
	a.connection = connection
	return a.startErr
}
func (a *agentStub) GetStatus(_ context.Context, connection AgentConnection, _ workloadjournal.Command) (workloadjournal.ExecutionState, error) {
	a.connection = connection
	if a.result.Status == "" {
		return workloadjournal.Running, nil
	}
	return workloadjournal.ExecutionState(a.result.Status), nil
}
func (a *agentStub) CollectResult(_ context.Context, connection AgentConnection, _ workloadjournal.Command) (workloadjobs.WorkloadResult, error) {
	a.connection = connection
	return a.result, nil
}

func setupLambda(t *testing.T) (*Controller, *LambdaMicroVM, *lambdaStub, *agentStub, *time.Time) {
	t.Helper()
	c, _, _, now := setupController(t)
	config := LambdaConfig{ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job", ImageVersion: "1", ImageDigest: strings.Repeat("a", 64),
		VPCEgressConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted", IngressConnector: "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS",
		AgentTicketIssuer: &recordingTicketIssuer{}}
	old := c.resolver
	c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
		s, err := old.Resolve(ctx, id)
		s.ImageIdentifier = config.ImageIdentifier
		s.ImageVersion = config.ImageVersion
		s.JobMemoryLimitMB = 8192
		s.JobCPULimitMillis = 4000
		s.Deadline = s.RequestedAt.Add(8 * time.Hour)
		s.IngressNetworkConnectors = []string{config.IngressConnector}
		s.EgressNetworkConnectors = []string{config.VPCEgressConnector}
		return s, err
	})
	api := &lambdaStub{config: config, state: types.MicrovmStateRunning, endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", token: map[string]string{"X-aws-proxy-auth": "short-lived-test-token"}}
	agent := &agentStub{}
	r, err := NewLambdaMicroVM(api, c.acceptor.store, c.resolver, agent, config, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	c.runtime = r
	return c, r, api, agent, now
}

func TestLambdaRetriesExactRunParameters(t *testing.T) {
	c, runtime, api, agent, now := setupLambda(t)
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
	issuer := runtime.config.AgentTicketIssuer.(*recordingTicketIssuer)
	if len(issuer.issued) == 0 || issuer.issued[0].execution == nil ||
		issuer.issued[0].execution.JobMemoryLimitMB != 8192 || issuer.issued[0].execution.JobCPULimitMillis != 4000 {
		t.Fatal("Agent ticket did not bind the selected Runtime Profile capacity")
	}
	var runPayload struct {
		JobMemoryLimitMB  int `json:"job_memory_limit_mb"`
		JobCPULimitMillis int `json:"job_cpu_limit_millis"`
	}
	if err := json.Unmarshal([]byte(aws.ToString(api.inputs[0].RunHookPayload)), &runPayload); err != nil ||
		runPayload.JobMemoryLimitMB != 8192 || runPayload.JobCPULimitMillis != 4000 {
		t.Fatalf("Run hook did not bind Runtime Profile capacity: %+v err=%v", runPayload, err)
	}
	if agent.connection.Endpoint != api.endpoint || agent.connection.AuthToken != "short-lived-test-token" || agent.connection.AgentTicket != "signed-agent-ticket" || agent.connection.SourceTicket != "signed-agent-ticket" || len(api.tokenInputs) != 1 {
		t.Fatal("agent connection did not receive the short-lived endpoint credential")
	}
	tokenInput := api.tokenInputs[0]
	if aws.ToString(tokenInput.MicrovmIdentifier) != "microvm_1" || aws.ToInt32(tokenInput.ExpirationInMinutes) != WorkloadAgentTokenMinutes || len(tokenInput.AllowedPorts) != 1 {
		t.Fatal("auth token was not scoped to the expected VM and lifetime")
	}
	port, ok := tokenInput.AllowedPorts[0].(*types.PortSpecificationMemberPort)
	if !ok || port.Value != WorkloadAgentPort {
		t.Fatal("auth token was not restricted to the hako-agent port")
	}
}

func TestLambdaRejectsMissingOrMalformedAgentToken(t *testing.T) {
	for name, token := range map[string]map[string]string{
		"missing":      nil,
		"wrong header": {"Authorization": "not-an-aws-proxy-token"},
		"newline":      {"X-aws-proxy-auth": "token\r\ninjected: yes"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _, api, _, _ := setupLambda(t)
			api.token = token
			if _, err := c.Ensure(context.Background(), envelope(t, nil)); !errors.Is(err, ErrMicroVMNotReady) {
				t.Fatalf("unsafe token accepted: %v", err)
			}
		})
	}
}

func TestLambdaStatusAndResultUseFreshScopedAgentConnections(t *testing.T) {
	c, runtime, api, agent, _ := setupLambda(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	accepted, err := c.acceptor.Accept(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	command := identity(accepted.Journal)
	if _, err := runtime.GetStatus(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	statusToken := agent.connection.AuthToken
	if _, err := runtime.CollectResult(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(api.tokenInputs) != 3 || agent.connection.AuthToken == "" || agent.connection.AuthToken != statusToken {
		// The fake token provider is deterministic; every operation must still
		// request a new token instead of persisting one in the journal.
		t.Fatal("agent status/result bypassed short-lived token issuance")
	}
	for _, in := range api.tokenInputs[1:] {
		port, ok := in.AllowedPorts[0].(*types.PortSpecificationMemberPort)
		if !ok || port.Value != WorkloadAgentPort || aws.ToInt32(in.ExpirationInMinutes) != WorkloadAgentTokenMinutes {
			t.Fatal("status/result token exceeded the agent port or lifetime scope")
		}
	}
	issuer := runtime.config.AgentTicketIssuer.(*recordingTicketIssuer)
	if len(issuer.issued) != 5 || issuer.issued[0].operation != workloadagent.OperationStart || issuer.issued[1].operation != workloadagent.OperationSource ||
		issuer.issued[2].operation != workloadagent.OperationStatus || issuer.issued[3].operation != workloadagent.OperationResult || issuer.issued[4].operation != workloadagent.OperationLogs {
		t.Fatal("application tickets were not operation-scoped")
	}
	if issuer.issued[0].execution == nil || issuer.issued[1].execution == nil || *issuer.issued[0].execution != *issuer.issued[1].execution ||
		issuer.issued[2].execution != nil || issuer.issued[3].execution != nil || issuer.issued[4].execution != nil {
		t.Fatal("Start/Source tickets did not share the frozen execution binding or read tickets carried one")
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

func TestLambdaRequiresAgentTicketIssuer(t *testing.T) {
	_, runtime, api, agent, _ := setupLambda(t)
	config := runtime.config
	config.AgentTicketIssuer = nil
	if _, err := NewLambdaMicroVM(api, runtime.store, runtime.resolver, agent, config, nil); !errors.Is(err, ErrSpecification) {
		t.Fatalf("runtime accepted missing app-layer agent credential issuer: %v", err)
	}
}

func TestLambdaIssuesSeparateAttemptBoundAgentChangesTicket(t *testing.T) {
	_, runtime, _, _, now := setupLambda(t)
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	spec := Specification{Identity: id, Kind: domain.WorkloadAgent, SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40),
		TriggerEvent: "issue_comment", TriggerBranch: "main", GitHubIntegrationID: "integration_1", GitHubInstallationID: 71,
		GitHubRepositoryID: 81, AgentTaskContextSHA256: strings.Repeat("c", 64), Deadline: now.Add(time.Hour).UTC(),
		ImageIdentifier: runtime.config.ImageIdentifier, ImageVersion: runtime.config.ImageVersion}
	vm := &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm_1"), ImageArn: aws.String("arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job"),
		ImageVersion: aws.String("1"), State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm_1.lambda-microvm.ap-northeast-1.on.aws"),
		ExecutionRoleArn: aws.String("arn:aws:iam::123456789012:role/hako-runtime")}
	connection, err := runtime.agentConnection(context.Background(), spec, "microvm_1", vm, workloadagent.OperationResult)
	if err != nil || connection.ChangesTicket == "" || connection.LogTicket == "" {
		t.Fatalf("Agent result connection did not get separate artifact tickets: connection=%+v err=%v", connection, err)
	}
	issuer := runtime.config.AgentTicketIssuer.(*recordingTicketIssuer)
	if len(issuer.issued) != 3 || issuer.issued[0].operation != workloadagent.OperationResult ||
		issuer.issued[1].operation != workloadagent.OperationLogs || issuer.issued[2].operation != workloadagent.OperationChanges ||
		issuer.issued[2].execution == nil || issuer.issued[2].execution.Kind != domain.WorkloadAgent ||
		issuer.issued[2].execution.AgentTaskContextSHA256 != spec.AgentTaskContextSHA256 {
		t.Fatalf("Agent change bundle ticket is not independently execution-bound: %+v", issuer.issued)
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
