package actionsrunner

import (
	"context"
	"errors"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

var ErrRunnerStartFailed = errors.New("GitHub Actions runner startup failed")

type RunnerLaunchBroker interface {
	Start(context.Context, githubbroker.ActionsRunnerRegistrationRequest) (githubbroker.ActionsRunnerLaunchCommand, error)
}

type RunnerBootstrapTokenIssuer interface {
	Issue(context.Context, githubbroker.ActionsRunnerRegistrationRequest) (githubbroker.ActionsRunnerRegistrationGrant, error)
}

type RunnerModelCredentialIssuer interface {
	IssueModelCredential(context.Context, githubbroker.ActionsRunnerRegistrationRequest, string) ([]byte, error)
}

type RunnerMicroVMLauncher interface {
	Launch(context.Context, githubwebhook.ActionsRunnerJob, RunnerRepository) (LambdaRunnerInstance, error)
}

type RunnerClaimBinder interface {
	Bind(context.Context, githubwebhook.ActionsRunnerJob, string) error
}

type RunnerGuestBootstrapper interface {
	Deliver(context.Context, RunnerBootstrapRequest) error
}

type RunnerStartupFailureReporter interface {
	Fail(context.Context, githubbroker.ActionsRunnerRegistrationRequest) error
}

type RunnerStartupWorkerConfig struct {
	OperationTimeout time.Duration
}

type RunnerStartupWorker struct {
	config          RunnerStartupWorkerConfig
	start           RunnerLaunchBroker
	launcher        RunnerMicroVMLauncher
	binder          RunnerClaimBinder
	tokens          RunnerBootstrapTokenIssuer
	bootstrap       RunnerGuestBootstrapper
	failures        RunnerStartupFailureReporter
	resourcePlaneID string
}

func NewRunnerStartupWorker(config RunnerStartupWorkerConfig, start RunnerLaunchBroker, launcher RunnerMicroVMLauncher,
	binder RunnerClaimBinder, tokens RunnerBootstrapTokenIssuer,
	bootstrap RunnerGuestBootstrapper, failures RunnerStartupFailureReporter, resourcePlaneID string) (*RunnerStartupWorker, error) {
	if config.OperationTimeout < 5*time.Second || config.OperationTimeout > 5*time.Minute || start == nil || launcher == nil ||
		binder == nil || tokens == nil || bootstrap == nil || failures == nil || !validResourcePlaneID(resourcePlaneID) {
		return nil, ErrInvalidRunnerLaunch
	}
	return &RunnerStartupWorker{config: config, resourcePlaneID: resourcePlaneID, start: start, launcher: launcher, binder: binder, tokens: tokens, bootstrap: bootstrap, failures: failures}, nil
}

// Start performs the ordered Resource Plane lifecycle: atomically acquire a
// starting claim, launch the trusted Lambda MicroVM profile, persist its exact
// identity, obtain one broker-authorized repository token, then deliver the
// signed bootstrap. Any failure after claim acquisition is reported through
// the broker so the durable cleanup outbox can terminate and deregister.
func (w *RunnerStartupWorker) Start(ctx context.Context, request githubbroker.ActionsRunnerRegistrationRequest) error {
	if w == nil || ctx == nil || !validActionsRunnerBootstrapRequest(request) {
		return ErrRunnerStartFailed
	}
	operationCtx, cancel := context.WithTimeout(ctx, w.config.OperationTimeout)
	defer cancel()
	command, err := w.start.Start(operationCtx, request)
	if err != nil {
		return ErrRunnerStartFailed
	}
	fail := func() {
		failureCtx, failureCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer failureCancel()
		_ = w.failures.Fail(failureCtx, request)
	}
	job := githubwebhook.ActionsRunnerJob{IntegrationID: command.IntegrationID, TenantID: domain.TenantID(command.TenantID), Installation: command.Installation,
		Repository: command.Repository, JobID: command.WorkflowJobID, WorkflowRunID: command.WorkflowRunID, RunAttempt: command.RunAttempt,
		WorkflowName: command.WorkflowName, JobName: command.JobName, HeadBranch: command.HeadBranch, HeadSHA: command.HeadSHA,
		Labels: append([]string(nil), command.Labels...), State: command.State, ClaimID: command.ClaimID, LeaseExpires: command.LeaseExpires}
	repository := RunnerRepository{Owner: command.Owner, Name: command.Name}
	if !validActionsRunnerLaunchCommandForWorker(command, request, w.resourcePlaneID) || validateRunnerLaunchJob(job) != nil {
		fail()
		return ErrRunnerStartFailed
	}
	instance, err := w.launcher.Launch(operationCtx, job, repository)
	if err != nil {
		fail()
		return ErrRunnerStartFailed
	}
	if instance.ResourcePlaneID != command.ResourcePlaneID || w.binder.Bind(operationCtx, job, instance.MicroVMID) != nil {
		fail()
		return ErrRunnerStartFailed
	}
	grant, err := w.tokens.Issue(operationCtx, request)
	if err != nil || grant.Repository != (githubapp.RepositoryAccess{Owner: repository.Owner, Name: repository.Name}) {
		fail()
		return ErrRunnerStartFailed
	}
	var modelCredential []byte
	if credentialIssuer, ok := w.tokens.(RunnerModelCredentialIssuer); ok {
		modelCredential, err = credentialIssuer.IssueModelCredential(operationCtx, request, instance.MicroVMID)
		if err != nil {
			clear(modelCredential)
			fail()
			return ErrRunnerStartFailed
		}
	}
	defer clear(modelCredential)
	if err := w.bootstrap.Deliver(operationCtx, RunnerBootstrapRequest{Instance: instance, Job: job, Repository: repository, Token: grant.Token,
		ModelCredential: modelCredential}); err != nil {
		fail()
		return ErrRunnerStartFailed
	}
	return nil
}

func validActionsRunnerBootstrapRequest(request githubbroker.ActionsRunnerRegistrationRequest) bool {
	return request.IntegrationID != "" && request.WorkflowJobID > 0 && request.RunAttempt > 0 && len(request.ClaimID) >= 8 && len(request.ClaimID) <= 128
}

func validActionsRunnerLaunchCommandForWorker(command githubbroker.ActionsRunnerLaunchCommand,
	request githubbroker.ActionsRunnerRegistrationRequest, resourcePlaneID string) bool {
	return command.ResourcePlaneID == resourcePlaneID && command.IntegrationID == request.IntegrationID && command.WorkflowJobID == request.WorkflowJobID &&
		command.RunAttempt == request.RunAttempt && command.ClaimID == request.ClaimID && command.State == "starting" &&
		command.LeaseExpires.After(time.Now().UTC()) && command.TenantID != "" && command.Installation > 0 && command.Repository > 0 &&
		command.WorkflowRunID > 0 && len(command.Labels) > 0 && len(command.Labels) <= 100 && validRepositoryPart(command.Owner) && validRepositoryPart(command.Name)
}
