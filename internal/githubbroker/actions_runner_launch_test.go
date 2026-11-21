package githubbroker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVerifiedActionsRunnerLaunchRunsPreflightBeforeStarter(t *testing.T) {
	request := ActionsRunnerRegistrationRequest{IntegrationID: "integration_test", WorkflowJobID: 123, RunAttempt: 2, ClaimID: "claim-test"}
	proof := []byte(`{"workflow_path":".github/workflows/claude.yml"}`)
	now := time.Now().UTC()
	command := ActionsRunnerLaunchCommand{ResourcePlaneID: "rp-tokyo-01", IntegrationID: request.IntegrationID, TenantID: "tenant_test",
		Installation: 11, Repository: 12, WorkflowJobID: request.WorkflowJobID, WorkflowRunID: 13, RunAttempt: request.RunAttempt,
		WorkflowName: "Claude", JobName: "review", HeadBranch: "main", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Labels: []string{"self-hosted", "hako"}, ClaimID: request.ClaimID, LeaseExpires: now.Add(time.Minute), Owner: "acme", Name: "api", State: "starting",
		DiagnosticsUploadToken: strings.Repeat("a", 64)}
	preflightCalled, starterCalled := false, false
	service, err := NewVerifiedActionsRunnerLaunchService([]string{"rp-tokyo-01"}, func(_ context.Context, got ActionsRunnerRegistrationRequest) ([]byte, error) {
		preflightCalled = true
		if got != request {
			t.Fatalf("preflight request = %+v", got)
		}
		return proof, nil
	}, func(_ context.Context, planeID string, got ActionsRunnerRegistrationRequest, gotProof []byte) (ActionsRunnerLaunchCommand, error) {
		starterCalled = true
		if planeID != "rp-tokyo-01" || got != request || string(gotProof) != string(proof) {
			t.Fatalf("starter args = %q %+v %s", planeID, got, gotProof)
		}
		return command, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now }
	if got, err := service.Start(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request); err != nil || !reflect.DeepEqual(got, command) {
		t.Fatalf("Start() = %+v, %v", got, err)
	}
	if !preflightCalled || !starterCalled {
		t.Fatalf("preflight/starter called = %v/%v", preflightCalled, starterCalled)
	}
}

func TestVerifiedActionsRunnerLaunchDeniesBeforeStarterOnPreflightFailure(t *testing.T) {
	request := ActionsRunnerRegistrationRequest{IntegrationID: "integration_test", WorkflowJobID: 123, RunAttempt: 1, ClaimID: "claim-test"}
	starterCalled := false
	service, err := NewVerifiedActionsRunnerLaunchService([]string{"rp-tokyo-01"}, func(context.Context, ActionsRunnerRegistrationRequest) ([]byte, error) {
		return nil, errors.New("untrusted provenance")
	}, func(context.Context, string, ActionsRunnerRegistrationRequest, []byte) (ActionsRunnerLaunchCommand, error) {
		starterCalled = true
		return ActionsRunnerLaunchCommand{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(context.Background(), "spiffe://hako/resource-plane/rp-tokyo-01", request); !errors.Is(err, ErrActionsRunnerLaunchDenied) {
		t.Fatalf("Start() error = %v", err)
	}
	if starterCalled {
		t.Fatal("launch starter ran after failed provenance preflight")
	}
}
