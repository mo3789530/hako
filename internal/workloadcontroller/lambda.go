package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const LambdaMicroVMMaxDuration = 8 * time.Hour

// The in-VM agent is the only port exposed through the MicroVM endpoint. The
// short-lived AWS proxy token is additionally scoped to this single port.
const WorkloadAgentPort int32 = 8080
const WorkloadAgentTokenMinutes int32 = 5

var ErrMicroVMNotReady = errors.New("Lambda MicroVM agent is not ready")

type LambdaAPI interface {
	RunMicrovm(context.Context, *lambdamicrovms.RunMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error)
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
	TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
}

// AgentConnection is deliberately ephemeral and must never be journaled,
// logged, or returned to a user-facing API. AWS scopes AuthToken to one VM and
// WorkloadAgentPort for five minutes.
type AgentConnection struct {
	MicroVMID    string `json:"microvm_id"`
	Endpoint     string `json:"endpoint"`
	AuthToken    string `json:"-"`
	AgentTicket  string `json:"-"`
	SourceTicket string `json:"-"`
	LogTicket    string `json:"-"`
}

// AgentRunner must authenticate only to the bound MicroVM, validate source
// digests, and start each identity once. Its result metadata survives VM cleanup.
// /run hooks initialize trusted infrastructure; they must not run repository code.
type AgentRunner interface {
	Start(context.Context, AgentConnection, Specification) error
	GetStatus(context.Context, AgentConnection, workloadjournal.Command) (workloadjournal.ExecutionState, error)
	CollectResult(context.Context, AgentConnection, workloadjournal.Command) (workloadjobs.WorkloadResult, error)
}

type LambdaConfig struct {
	ImageIdentifier    string
	ImageVersion       string
	ImageDigest        string
	VPCEgressConnector string
	IngressConnector   string
	AgentTicketIssuer  workloadagent.TicketIssuer
}

type LambdaMicroVM struct {
	api      LambdaAPI
	store    workloadjournal.Store
	resolver Resolver
	agent    AgentRunner
	config   LambdaConfig
	now      func() time.Time
}

func NewLambdaMicroVM(api LambdaAPI, store workloadjournal.Store, resolver Resolver, agent AgentRunner, config LambdaConfig, now func() time.Time) (*LambdaMicroVM, error) {
	image, err := awsARN(config.ImageIdentifier)
	vpc, verr := awsARN(config.VPCEgressConnector)
	ingress, ierr := awsARN(config.IngressConnector)
	if api == nil || store == nil || resolver == nil || agent == nil || config.AgentTicketIssuer == nil || err != nil || verr != nil || ierr != nil ||
		!strings.HasPrefix(image[5], "microvm-image:") || !strings.HasPrefix(vpc[5], "network-connector:") ||
		image[4] != vpc[4] || image[3] != vpc[3] || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(image[4]) ||
		ingress[3] != image[3] || ingress[4] != "aws" || ingress[5] != "network-connector:aws-network-connector:ALL_INGRESS" ||
		!regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(config.ImageVersion) || !digestPattern.MatchString(config.ImageDigest) {
		return nil, ErrSpecification
	}
	if now == nil {
		now = time.Now
	}
	return &LambdaMicroVM{api: api, store: store, resolver: resolver, agent: agent, config: config, now: now}, nil
}

func awsARN(value string) ([]string, error) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "lambda" || parts[3] == "" || parts[4] == "" || parts[5] == "" {
		return nil, ErrSpecification
	}
	return parts, nil
}

func (r *LambdaMicroVM) snapshot(ctx context.Context, id workloadjournal.Command) (workloadjournal.Snapshot, error) {
	s, err := r.store.Get(ctx, id.TenantID, id.WorkloadRunID, id.AttemptID)
	if err != nil {
		return s, err
	}
	if s.ETag == "" || s.Journal.Validate() != nil || identity(s.Journal) != id {
		return s, workloadjournal.ErrConflict
	}
	return s, nil
}

// input is deterministic across retries: the platform duration uses the frozen
// original request interval; the agent also enforces the absolute Run deadline.
// A VPC connector is mandatory because AWS defaults to public Internet egress.
func (r *LambdaMicroVM) input(spec Specification) (*lambdamicrovms.RunMicrovmInput, error) {
	duration := spec.Deadline.Sub(spec.RequestedAt)
	if spec.RequestedAt.IsZero() || duration < time.Second || duration > LambdaMicroVMMaxDuration || duration%time.Second != 0 ||
		spec.ImageIdentifier != r.config.ImageIdentifier || spec.ImageVersion != r.config.ImageVersion || spec.ImageDigest != r.config.ImageDigest ||
		len(spec.EgressNetworkConnectors) != 1 || spec.EgressNetworkConnectors[0] != r.config.VPCEgressConnector ||
		len(spec.IngressNetworkConnectors) != 1 || spec.IngressNetworkConnectors[0] != r.config.IngressConnector {
		return nil, ErrSpecification
	}
	// Initialization contains immutable identifiers/digests/deadline only.
	payload, err := json.Marshal(struct {
		Identity               workloadjournal.Command `json:"identity"`
		Kind                   domain.WorkloadKind     `json:"kind,omitempty"`
		AgentIntegrationID     string                  `json:"agent_integration_id,omitempty"`
		AgentInstallationID    int64                   `json:"agent_installation_id,omitempty"`
		AgentRepositoryID      int64                   `json:"agent_repository_id,omitempty"`
		AgentTaskContextSHA256 string                  `json:"agent_task_context_sha256,omitempty"`
		SourceDigest           string                  `json:"source_digest"`
		CommitSHA              string                  `json:"commit_sha"`
		TriggerEvent           string                  `json:"trigger_event,omitempty"`
		TriggerBranch          string                  `json:"trigger_branch,omitempty"`
		CorrelationID          string                  `json:"correlation_id,omitempty"`
		JobMemoryLimitMB       int                     `json:"job_memory_limit_mb,omitempty"`
		JobCPULimitMillis      int                     `json:"job_cpu_limit_millis,omitempty"`
		Deadline               time.Time               `json:"deadline"`
	}{spec.Identity, spec.Kind, spec.GitHubIntegrationID, spec.GitHubInstallationID, spec.GitHubRepositoryID, spec.AgentTaskContextSHA256,
		spec.SourceDigest, spec.CommitSHA, spec.TriggerEvent, spec.TriggerBranch, spec.CorrelationID,
		spec.JobMemoryLimitMB, spec.JobCPULimitMillis, spec.Deadline.UTC()})
	if err != nil {
		return nil, err
	}
	idleSeconds := int32(duration / time.Second)
	if idleSeconds < 60 {
		idleSeconds = 60
	}
	return &lambdamicrovms.RunMicrovmInput{ClientToken: aws.String(spec.Identity.IdempotencyToken), ImageIdentifier: aws.String(spec.ImageIdentifier), ImageVersion: aws.String(spec.ImageVersion),
		IdlePolicy:               &types.IdlePolicy{AutoResumeEnabled: aws.Bool(false), MaxIdleDurationSeconds: aws.Int32(idleSeconds), SuspendedDurationSeconds: aws.Int32(0)},
		MaximumDurationInSeconds: aws.Int32(int32(duration / time.Second)), RunHookPayload: aws.String(string(payload)),
		IngressNetworkConnectors: append([]string(nil), spec.IngressNetworkConnectors...), EgressNetworkConnectors: append([]string(nil), spec.EgressNetworkConnectors...)}, nil
}

func (r *LambdaMicroVM) ValidateSpecification(spec Specification) error {
	_, err := r.input(spec)
	return err
}

func (r *LambdaMicroVM) bind(ctx context.Context, id workloadjournal.Command, handle string) (workloadjournal.Snapshot, error) {
	for i := 0; i < 3; i++ {
		s, err := r.snapshot(ctx, id)
		if err != nil {
			return s, err
		}
		if s.Journal.RuntimeHandle != "" {
			if s.Journal.RuntimeHandle != handle {
				return s, workloadjournal.ErrConflict
			}
			return s, nil
		}
		next := s.Journal
		next.RuntimeHandle = handle
		next.Revision++
		next.UpdatedAt = r.now().UTC()
		updated, err := r.store.CompareAndSwap(ctx, s, next)
		if errors.Is(err, workloadjournal.ErrConflict) {
			continue
		}
		return updated, err
	}
	return workloadjournal.Snapshot{}, workloadjournal.ErrConflict
}

func (r *LambdaMicroVM) discover(ctx context.Context, s workloadjournal.Snapshot) (string, error) {
	if s.Journal.RuntimeHandle != "" {
		return s.Journal.RuntimeHandle, nil
	}
	if s.Journal.SpecificationHash == "" {
		return "", nil
	}
	var spec Specification
	var err error
	if len(s.Journal.SpecificationPayload) > 0 {
		if json.Unmarshal(s.Journal.SpecificationPayload, &spec) != nil {
			return "", ErrSpecification
		}
	} else {
		// Compatibility for earlier, hash-only test checkpoints. New launches
		// persist the metadata, so cleanup needs no live repository authorization.
		spec, err = r.resolver.Resolve(ctx, identity(s.Journal))
		if err != nil {
			return "", err
		}
	}
	hash, err := specificationHash(spec, identity(s.Journal))
	if err != nil || hash != s.Journal.SpecificationHash {
		return "", ErrSpecification
	}
	in, err := r.input(spec)
	if err != nil {
		return "", err
	}
	out, err := r.api.RunMicrovm(ctx, in)
	if err != nil {
		return "", err
	}
	if out == nil || aws.ToString(out.MicrovmId) == "" {
		return "", ErrSpecification
	}
	handle := aws.ToString(out.MicrovmId)
	_, err = r.bind(ctx, identity(s.Journal), handle)
	return handle, err
}

func (r *LambdaMicroVM) EnsureRunning(ctx context.Context, spec Specification) (string, error) {
	s, err := r.snapshot(ctx, spec.Identity)
	if err != nil {
		return "", err
	}
	hash, err := specificationHash(spec, spec.Identity)
	if err != nil || hash != s.Journal.SpecificationHash {
		return "", ErrSpecification
	}
	if s.Journal.CancelRequested || terminal(s.Journal.ExecutionState) || !r.now().Before(spec.Deadline) {
		return "", ErrCleanupPending
	}
	if _, err := r.input(spec); err != nil {
		return "", err
	}
	handle, err := r.discover(ctx, s)
	if err != nil {
		return "", err
	}
	latest, err := r.snapshot(ctx, spec.Identity)
	if err != nil {
		return "", err
	}
	if latest.Journal.CancelRequested || terminal(latest.Journal.ExecutionState) {
		return "", ErrCleanupPending
	}
	out, err := r.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(handle)})
	if err != nil {
		return "", err
	}
	if out == nil || aws.ToString(out.MicrovmId) != handle || aws.ToString(out.ImageArn) != spec.ImageIdentifier || aws.ToString(out.ImageVersion) != spec.ImageVersion || aws.ToString(out.ExecutionRoleArn) != "" {
		return "", ErrSpecification
	}
	if out.State == types.MicrovmStateTerminated || out.State == types.MicrovmStateTerminating {
		return "", ErrCleanupPending
	}
	connection, err := r.agentConnection(ctx, spec, handle, out, workloadagent.OperationStart)
	if err != nil {
		return "", err
	}
	if err := r.agent.Start(ctx, connection, spec); err != nil {
		return "", err
	}
	return handle, nil
}

func (r *LambdaMicroVM) agentConnection(ctx context.Context, spec Specification, handle string, vm *lambdamicrovms.GetMicrovmOutput, operation workloadagent.Operation) (AgentConnection, error) {
	if vm == nil || aws.ToString(vm.MicrovmId) != handle || aws.ToString(vm.ImageArn) != spec.ImageIdentifier || aws.ToString(vm.ImageVersion) != spec.ImageVersion || vm.State != types.MicrovmStateRunning {
		return AgentConnection{}, ErrMicroVMNotReady
	}
	endpoint := aws.ToString(vm.Endpoint)
	u, err := url.Parse(endpoint)
	image, _ := awsARN(spec.ImageIdentifier)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".lambda-microvm."+image[3]+".on.aws") || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return AgentConnection{}, ErrMicroVMNotReady
	}
	token, err := r.api.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   aws.String(handle),
		AllowedPorts:        []types.PortSpecification{&types.PortSpecificationMemberPort{Value: WorkloadAgentPort}},
		ExpirationInMinutes: aws.Int32(WorkloadAgentTokenMinutes),
	})
	if err != nil {
		return AgentConnection{}, err
	}
	awsToken := ""
	if token != nil {
		awsToken = token.AuthToken["X-aws-proxy-auth"]
	}
	if len(awsToken) == 0 || len(awsToken) > 8000 || strings.ContainsAny(awsToken, " \t\r\n") {
		return AgentConnection{}, ErrMicroVMNotReady
	}
	var agentTicket string
	if operation == workloadagent.OperationStart || operation == workloadagent.OperationSource {
		agentTicket, err = r.config.AgentTicketIssuer.IssueExecution(handle, spec.Identity, operation, workloadagent.ExecutionBinding{
			Kind: spec.Kind, AgentIntegrationID: spec.GitHubIntegrationID, AgentInstallationID: spec.GitHubInstallationID,
			AgentRepositoryID: spec.GitHubRepositoryID, AgentTaskContextSHA256: spec.AgentTaskContextSHA256,
			SourceDigest: spec.SourceDigest, CommitSHA: spec.CommitSHA, TriggerEvent: spec.TriggerEvent, TriggerBranch: spec.TriggerBranch,
			CorrelationID: spec.CorrelationID, JobMemoryLimitMB: spec.JobMemoryLimitMB, JobCPULimitMillis: spec.JobCPULimitMillis,
			Deadline: spec.Deadline.UTC(),
		})
	} else {
		agentTicket, err = r.config.AgentTicketIssuer.Issue(handle, spec.Identity, operation)
	}
	if err != nil || agentTicket == "" || strings.ContainsAny(agentTicket, " \t\r\n") {
		return AgentConnection{}, ErrMicroVMNotReady
	}
	sourceTicket := ""
	logTicket := ""
	if operation == workloadagent.OperationStart {
		sourceTicket, err = r.config.AgentTicketIssuer.IssueExecution(handle, spec.Identity, workloadagent.OperationSource, workloadagent.ExecutionBinding{
			Kind: spec.Kind, AgentIntegrationID: spec.GitHubIntegrationID, AgentInstallationID: spec.GitHubInstallationID,
			AgentRepositoryID: spec.GitHubRepositoryID, AgentTaskContextSHA256: spec.AgentTaskContextSHA256,
			SourceDigest: spec.SourceDigest, CommitSHA: spec.CommitSHA, TriggerEvent: spec.TriggerEvent, TriggerBranch: spec.TriggerBranch,
			CorrelationID: spec.CorrelationID, JobMemoryLimitMB: spec.JobMemoryLimitMB, JobCPULimitMillis: spec.JobCPULimitMillis,
			Deadline: spec.Deadline.UTC(),
		})
		if err != nil || sourceTicket == "" || strings.ContainsAny(sourceTicket, " \t\r\n") {
			return AgentConnection{}, ErrMicroVMNotReady
		}
	}
	if operation == workloadagent.OperationResult {
		logTicket, err = r.config.AgentTicketIssuer.Issue(handle, spec.Identity, workloadagent.OperationLogs)
		if err != nil || logTicket == "" || strings.ContainsAny(logTicket, " \t\r\n") {
			return AgentConnection{}, ErrMicroVMNotReady
		}
	}
	return AgentConnection{MicroVMID: handle, Endpoint: endpoint, AuthToken: awsToken, AgentTicket: agentTicket, SourceTicket: sourceTicket, LogTicket: logTicket}, nil
}

func (r *LambdaMicroVM) Cancel(ctx context.Context, id workloadjournal.Command) error {
	s, err := r.snapshot(ctx, id)
	if err != nil {
		return err
	}
	if !s.Journal.CancelRequested {
		return ErrInvalidCommand
	}
	handle, err := r.discover(ctx, s)
	if err != nil {
		return err
	}
	if handle == "" {
		return nil
	}
	_, err = r.api.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(handle)})
	return err
}

func (r *LambdaMicroVM) Cleanup(ctx context.Context, id workloadjournal.Command) (bool, error) {
	s, err := r.snapshot(ctx, id)
	if err != nil {
		return false, err
	}
	if !terminal(s.Journal.ExecutionState) {
		return false, ErrCleanupPending
	}
	handle, err := r.discover(ctx, s)
	if err != nil {
		return false, err
	}
	if handle == "" {
		return true, nil
	}
	_, err = r.api.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(handle)})
	if err != nil {
		return false, err
	}
	out, err := r.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(handle)})
	if err != nil {
		return false, err
	}
	if out == nil || aws.ToString(out.MicrovmId) != handle {
		return false, ErrSpecification
	}
	return out.State == types.MicrovmStateTerminated, nil
}

func (r *LambdaMicroVM) GetStatus(ctx context.Context, id workloadjournal.Command) (workloadjournal.ExecutionState, error) {
	s, err := r.snapshot(ctx, id)
	if err != nil {
		return "", err
	}
	connection, err := r.agentConnectionFor(ctx, s, workloadagent.OperationStatus)
	if err != nil {
		return "", err
	}
	return r.agent.GetStatus(ctx, connection, id)
}
func (r *LambdaMicroVM) CollectResult(ctx context.Context, id workloadjournal.Command) (workloadjobs.WorkloadResult, error) {
	s, err := r.snapshot(ctx, id)
	if err != nil {
		return workloadjobs.WorkloadResult{}, err
	}
	connection, err := r.agentConnectionFor(ctx, s, workloadagent.OperationResult)
	if err != nil {
		return workloadjobs.WorkloadResult{}, err
	}
	return r.agent.CollectResult(ctx, connection, id)
}

func (r *LambdaMicroVM) agentConnectionFor(ctx context.Context, s workloadjournal.Snapshot, operation workloadagent.Operation) (AgentConnection, error) {
	if s.Journal.RuntimeHandle == "" || len(s.Journal.SpecificationPayload) == 0 {
		return AgentConnection{}, ErrMicroVMNotReady
	}
	var spec Specification
	if json.Unmarshal(s.Journal.SpecificationPayload, &spec) != nil {
		return AgentConnection{}, ErrSpecification
	}
	hash, err := specificationHash(spec, identity(s.Journal))
	if err != nil || hash != s.Journal.SpecificationHash {
		return AgentConnection{}, ErrSpecification
	}
	vm, err := r.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(s.Journal.RuntimeHandle)})
	if err != nil {
		return AgentConnection{}, err
	}
	return r.agentConnection(ctx, spec, s.Journal.RuntimeHandle, vm, operation)
}
