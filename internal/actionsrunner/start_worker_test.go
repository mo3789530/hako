package actionsrunner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

type startBrokerStub struct {
	command githubbroker.ActionsRunnerLaunchCommand
	events  *[]string
	err     error
}

func (s startBrokerStub) Start(context.Context, githubbroker.ActionsRunnerRegistrationRequest) (githubbroker.ActionsRunnerLaunchCommand, error) {
	*s.events = append(*s.events, "start")
	return s.command, s.err
}

type runnerLauncherStub struct {
	instance LambdaRunnerInstance
	events   *[]string
	err      error
}

func (s runnerLauncherStub) Launch(context.Context, githubwebhook.ActionsRunnerJob, RunnerRepository) (LambdaRunnerInstance, error) {
	*s.events = append(*s.events, "launch")
	return s.instance, s.err
}

type runnerBinderStub struct {
	events *[]string
	err    error
}

func (s runnerBinderStub) Bind(context.Context, githubwebhook.ActionsRunnerJob, string) error {
	*s.events = append(*s.events, "bind")
	return s.err
}

type runnerTokenIssuerStub struct {
	grant  githubbroker.ActionsRunnerRegistrationGrant
	events *[]string
	err    error
}

func (s runnerTokenIssuerStub) Issue(context.Context, githubbroker.ActionsRunnerRegistrationRequest) (githubbroker.ActionsRunnerRegistrationGrant, error) {
	*s.events = append(*s.events, "token")
	return s.grant, s.err
}

type guestBootstrapStub struct {
	events *[]string
	err    error
}

func (s guestBootstrapStub) Deliver(context.Context, RunnerBootstrapRequest) error {
	*s.events = append(*s.events, "bootstrap")
	return s.err
}

type startupFailureStub struct{ events *[]string }

func (s startupFailureStub) Fail(context.Context, githubbroker.ActionsRunnerRegistrationRequest) error {
	*s.events = append(*s.events, "fail")
	return nil
}

func TestRunnerStartupWorkerUsesFencedOrderedLifecycle(t *testing.T) {
	now := time.Now().UTC()
	request := githubbroker.ActionsRunnerRegistrationRequest{IntegrationID: "integration_runner", WorkflowJobID: 9901, RunAttempt: 1, ClaimID: "runner-claim-1"}
	command := githubbroker.ActionsRunnerLaunchCommand{ResourcePlaneID: "rp-tokyo-01", IntegrationID: request.IntegrationID, TenantID: domain.TenantID("tenant_runner"),
		Installation: 7121, Repository: 7122, WorkflowJobID: request.WorkflowJobID, WorkflowRunID: 9902, RunAttempt: request.RunAttempt,
		WorkflowName: "CI", JobName: "test", HeadBranch: "feature/task", HeadSHA: strings.Repeat("a", 40), Labels: []string{"self-hosted", "hako", "standard"},
		ClaimID: request.ClaimID, LeaseExpires: now.Add(time.Minute), Owner: "acme", Name: "api", State: "starting"}
	instance := LambdaRunnerInstance{ResourcePlaneID: command.ResourcePlaneID, MicroVMID: "runner-microvm-1", Endpoint: "https://runner-microvm-1.lambda-microvm.ap-northeast-1.on.aws",
		ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:runner", ImageVersion: "3"}
	token, err := githubapp.NewRunnerRegistrationToken("synthetic-runner-token", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	worker, err := NewRunnerStartupWorker(RunnerStartupWorkerConfig{OperationTimeout: time.Minute}, startBrokerStub{command: command, events: &events},
		runnerLauncherStub{instance: instance, events: &events}, runnerBinderStub{events: &events},
		runnerTokenIssuerStub{grant: githubbroker.ActionsRunnerRegistrationGrant{Token: token, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, events: &events},
		guestBootstrapStub{events: &events}, startupFailureStub{events: &events}, command.ResourcePlaneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	want := []string{"start", "launch", "bind", "token", "bootstrap"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("runner startup order = %v, want %v", events, want)
	}
}

func TestRunnerStartupWorkerReportsFailureAfterClaimedStart(t *testing.T) {
	now := time.Now().UTC()
	request := githubbroker.ActionsRunnerRegistrationRequest{IntegrationID: "integration_runner", WorkflowJobID: 9901, RunAttempt: 1, ClaimID: "runner-claim-1"}
	command := githubbroker.ActionsRunnerLaunchCommand{ResourcePlaneID: "rp-tokyo-01", IntegrationID: request.IntegrationID, TenantID: domain.TenantID("tenant_runner"),
		Installation: 7121, Repository: 7122, WorkflowJobID: request.WorkflowJobID, WorkflowRunID: 9902, RunAttempt: request.RunAttempt,
		WorkflowName: "CI", JobName: "test", HeadBranch: "main", HeadSHA: strings.Repeat("a", 40), Labels: []string{"self-hosted", "hako"},
		ClaimID: request.ClaimID, LeaseExpires: now.Add(time.Minute), Owner: "acme", Name: "api", State: "starting"}
	events := []string{}
	worker, err := NewRunnerStartupWorker(RunnerStartupWorkerConfig{OperationTimeout: time.Minute}, startBrokerStub{command: command, events: &events},
		runnerLauncherStub{events: &events, err: errors.New("provider failure")}, runnerBinderStub{events: &events},
		runnerTokenIssuerStub{events: &events}, guestBootstrapStub{events: &events}, startupFailureStub{events: &events}, command.ResourcePlaneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.Background(), request); !errors.Is(err, ErrRunnerStartFailed) {
		t.Fatalf("Start() error = %v", err)
	}
	want := []string{"start", "launch", "fail"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("failure cleanup order = %v, want %v", events, want)
	}
}
