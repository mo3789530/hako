package actionsrunner

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
)

func TestRunnerStartupHandlerReturnsOnlyFailedSQSRecords(t *testing.T) {
	now := time.Now().UTC()
	request := githubbroker.ActionsRunnerRegistrationRequest{IntegrationID: "integration_runner", WorkflowJobID: 9901, RunAttempt: 1, ClaimID: "runner-claim-1"}
	command := githubbroker.ActionsRunnerLaunchCommand{ResourcePlaneID: "rp-tokyo-01", IntegrationID: request.IntegrationID, TenantID: domain.TenantID("tenant_runner"),
		Installation: 7121, Repository: 7122, WorkflowJobID: request.WorkflowJobID, WorkflowRunID: 9902, RunAttempt: request.RunAttempt,
		WorkflowName: "CI", JobName: "test", HeadBranch: "main", HeadSHA: strings.Repeat("a", 40), Labels: []string{"self-hosted", "hako"},
		ClaimID: request.ClaimID, LeaseExpires: now.Add(time.Minute), Owner: "acme", Name: "api", State: "starting"}
	instance := LambdaRunnerInstance{ResourcePlaneID: command.ResourcePlaneID, MicroVMID: "runner-vm-1", Endpoint: "https://runner-vm-1.lambda-microvm.ap-northeast-1.on.aws",
		ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:runner", ImageVersion: "1"}
	token, err := githubapp.NewRunnerRegistrationToken("synthetic-token", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	eventsSeen := []string{}
	worker, err := NewRunnerStartupWorker(RunnerStartupWorkerConfig{OperationTimeout: time.Minute}, startBrokerStub{command: command, events: &eventsSeen},
		runnerLauncherStub{instance: instance, events: &eventsSeen}, runnerBinderStub{events: &eventsSeen},
		runnerTokenIssuerStub{grant: githubbroker.ActionsRunnerRegistrationGrant{Token: token, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, events: &eventsSeen},
		&guestBootstrapStub{events: &eventsSeen}, startupFailureStub{events: &eventsSeen}, command.ResourcePlaneID)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewRunnerStartupHandler(worker)
	if err != nil {
		t.Fatal(err)
	}
	goodBody := `{"schema_version":1,"integration_id":"integration_runner","workflow_job_id":9901,"run_attempt":1,"claim_id":"runner-claim-1"}`
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "good-record", Body: goodBody},
		{MessageId: "poison-record", Body: `{"integration_id":"integration_runner","workflow_job_id":9902,"run_attempt":1,"claim_id":"runner-claim-2","tenant_id":"not-accepted"}`},
	}})
	if err != nil || !reflect.DeepEqual(response.BatchItemFailures, []events.SQSBatchItemFailure{{ItemIdentifier: "poison-record"}}) {
		t.Fatalf("partial batch response=%+v error=%v", response, err)
	}
	wantEvents := []string{"start", "launch", "bind", "token", "bootstrap"}
	if !reflect.DeepEqual(eventsSeen, wantEvents) {
		t.Fatalf("successful SQS record lifecycle=%v, want=%v", eventsSeen, wantEvents)
	}
}

func TestDecodeRunnerStartupRequestRejectsOversizedAndMalformedBodies(t *testing.T) {
	for _, body := range []string{"", strings.Repeat("x", 2049), `{"schema_version":1,"integration_id":"integration_runner","workflow_job_id":1,"run_attempt":1,"claim_id":"runner-claim-1","tenant_id":"attacker"}`, `{"schema_version":2,"integration_id":"integration_runner","workflow_job_id":1,"run_attempt":1,"claim_id":"runner-claim-1"}`, `{} {}`} {
		if _, err := decodeRunnerStartupRequest(body); err == nil {
			t.Fatalf("invalid SQS body accepted: %.80q", body)
		}
	}
}
