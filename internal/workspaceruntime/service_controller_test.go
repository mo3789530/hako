package workspaceruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

type serviceControllerAPIFake struct {
	getOutput  *lambdamicrovms.GetMicrovmOutput
	tokenInput *lambdamicrovms.CreateMicrovmAuthTokenInput
	getCalls   int
	tokenCalls int
	getErr     error
	tokenErr   error
}

func (f *serviceControllerAPIFake) GetMicrovm(_ context.Context, _ *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	f.getCalls++
	return f.getOutput, f.getErr
}

func (f *serviceControllerAPIFake) CreateMicrovmAuthToken(_ context.Context, input *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	f.tokenCalls++
	f.tokenInput = input
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: map[string]string{"X-aws-proxy-auth": "microvm-token"}}, nil
}

type serviceGuestFake struct {
	endpoint, token, capability string
	command                     domain.WorkspaceServiceCommand
	secrets                     map[string]string
	observation                 workspaceServiceObservation
	err                         error
	onReconcile                 func()
}

type allocationChangingJournalStore struct {
	workspacejournal.Store
	gets int
}

func (s *allocationChangingJournalStore) Get(ctx context.Context, tenant domain.TenantID, workspace domain.WorkspaceID, plane domain.ResourcePlaneID) (workspacejournal.Snapshot, error) {
	snapshot, err := s.Store.Get(ctx, tenant, workspace, plane)
	if err != nil {
		return workspacejournal.Snapshot{}, err
	}
	s.gets++
	if s.gets == 2 {
		snapshot.Journal.ProviderID = "microvm-2"
		snapshot.Journal.AllocationGeneration = 2
	}
	return snapshot, nil
}

func (f *serviceGuestFake) Reconcile(_ context.Context, endpoint, token, capability string, command domain.WorkspaceServiceCommand, secrets map[string]string) (workspaceServiceObservation, error) {
	f.endpoint, f.token, f.capability, f.command, f.secrets = endpoint, token, capability, command, secrets
	if f.onReconcile != nil {
		f.onReconcile()
	}
	return f.observation, f.err
}

type serviceReportFake struct {
	reports []WorkspaceServiceRuntimeReport
	err     error
}

type serviceDesiredResolverFake struct {
	command  domain.WorkspaceServiceCommand
	commands []domain.WorkspaceServiceCommand
	calls    int
	err      error
}

type workspaceServiceSecretProviderFake struct {
	values      map[string]string
	err         error
	called      int
	retireErr   error
	retireCalls int
	commitErr   error
	commitCalls int
}

func (f *workspaceServiceSecretProviderFake) Resolve(_ context.Context, _ domain.WorkspaceServiceCommand) (map[string]string, error) {
	f.called++
	return f.values, f.err
}

func (f *workspaceServiceSecretProviderFake) Retire(_ context.Context, command domain.WorkspaceServiceCommand) error {
	f.retireCalls++
	if command.DesiredState != "deleted" {
		return ErrWorkspaceServiceSecret
	}
	return f.retireErr
}

func (f *workspaceServiceSecretProviderFake) CommitRotation(_ context.Context, command domain.WorkspaceServiceCommand) error {
	f.commitCalls++
	if command.CredentialGeneration < 2 || command.DesiredState != "running" {
		return ErrWorkspaceServiceSecret
	}
	return f.commitErr
}

func (f *serviceDesiredResolverFake) Resolve(_ context.Context, trigger domain.WorkspaceServiceCommand) (domain.WorkspaceServiceCommand, error) {
	if f.err != nil {
		return domain.WorkspaceServiceCommand{}, f.err
	}
	if len(f.commands) > 0 {
		index := f.calls
		if index >= len(f.commands) {
			index = len(f.commands) - 1
		}
		f.calls++
		return f.commands[index], nil
	}
	if f.command.ServiceID == "" {
		return trigger, nil
	}
	return f.command, nil
}

func TestWorkspaceServiceConsumerRetriesWhenDesiredStateChangesDuringGuestApply(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	start := runtimeServiceCommand(t)
	stop := start
	stop.Revision++
	stop.DesiredState = "stopped"
	launchPayload, _ := json.Marshal(validSpec())
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: start.TenantID, WorkspaceID: start.WorkspaceID,
		ResourcePlaneID: start.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Running, "microvm-1", 1
	if _, err := store.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{getOutput: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws"),
		ImageArn: aws.String(validSpec().ImageIdentifier), ImageVersion: aws.String(validSpec().ImageVersion),
		ExecutionRoleArn: aws.String(validSpec().ExecutionRoleARN), EgressNetworkConnectors: validSpec().EgressNetworkConnectors,
		IngressNetworkConnectors: validSpec().IngressNetworkConnectors}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}
	guest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: start.ServiceID, Revision: start.Revision,
		ObservedRevision: start.Revision, DesiredState: start.DesiredState, ObservedState: "healthy", Healthy: true}}
	reports := &serviceReportFake{}
	resolver := &serviceDesiredResolverFake{commands: []domain.WorkspaceServiceCommand{start, stop}}
	consumer, err := NewWorkspaceServiceCommandConsumer(start.ResourcePlaneID, "us-east-1", store, api, issuer, guest, reports, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	queued, _ := json.Marshal(start)
	if err := consumer.Handle(ctx, queued); !errors.Is(err, ErrWorkspaceServiceDelivery) {
		t.Fatalf("stale successful Guest apply should leave SQS record retryable: %v", err)
	}
	if guest.command.Revision != start.Revision || len(reports.reports) != 0 || resolver.calls != 2 {
		t.Fatalf("stale Guest result was reported/acknowledged: command=%+v reports=%+v resolver_calls=%d", guest.command, reports.reports, resolver.calls)
	}

	// The SQS retry is only a wake-up: it resolves and applies the latest stop.
	guest.observation = workspaceServiceObservation{ServiceID: stop.ServiceID, Revision: stop.Revision,
		ObservedRevision: stop.Revision, DesiredState: stop.DesiredState, ObservedState: "stopped"}
	if err := consumer.Handle(ctx, queued); err != nil {
		t.Fatalf("retry did not reconcile latest stop: %v", err)
	}
	if guest.command.Revision != stop.Revision || guest.command.DesiredState != "stopped" || len(reports.reports) != 1 ||
		reports.reports[0].Revision != stop.Revision || reports.reports[0].ObservedState != "stopped" {
		t.Fatalf("retry did not converge on latest desired state: command=%+v reports=%+v", guest.command, reports.reports)
	}
}

func TestWorkspaceServiceConsumerRetriesWhenAllocationChangesDuringGuestApply(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemoryStore()
	store := &allocationChangingJournalStore{Store: baseStore}
	command := runtimeServiceCommand(t)
	launchPayload, _ := json.Marshal(validSpec())
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Running, "microvm-1", 1
	if _, err := baseStore.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{getOutput: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws"),
		ImageArn: aws.String(validSpec().ImageIdentifier), ImageVersion: aws.String(validSpec().ImageVersion),
		ExecutionRoleArn: aws.String(validSpec().ExecutionRoleARN), EgressNetworkConnectors: validSpec().EgressNetworkConnectors,
		IngressNetworkConnectors: validSpec().IngressNetworkConnectors}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision,
		ObservedRevision: command.Revision, DesiredState: command.DesiredState, ObservedState: "healthy", Healthy: true}}
	reports := &serviceReportFake{}
	consumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api,
		&workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}, guest, reports,
		&serviceDesiredResolverFake{command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	queued, _ := json.Marshal(command)
	if err := consumer.Handle(ctx, queued); !errors.Is(err, ErrWorkspaceServiceDelivery) {
		t.Fatalf("Guest report from replaced MicroVM allocation must remain retryable: %v", err)
	}
	if len(reports.reports) != 0 {
		t.Fatalf("old allocation must not confirm Service state or cleanup: %+v", reports.reports)
	}
}

func (f *serviceReportFake) Report(_ context.Context, report WorkspaceServiceRuntimeReport) error {
	f.reports = append(f.reports, report)
	return f.err
}

func TestWorkspaceServiceCommandConsumerBindsMicroVMAndReportsObservation(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	command := runtimeServiceCommand(t)
	launchPayload, err := json.Marshal(validSpec())
	if err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Running, "microvm-1", 1
	if _, err := store.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{getOutput: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws"),
		ImageArn: aws.String(validSpec().ImageIdentifier), ImageVersion: aws.String(validSpec().ImageVersion),
		ExecutionRoleArn: aws.String(validSpec().ExecutionRoleARN), EgressNetworkConnectors: validSpec().EgressNetworkConnectors,
		IngressNetworkConnectors: validSpec().IngressNetworkConnectors}}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private, Now: func() time.Time { return time.Now().UTC() }}
	guest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision,
		ObservedRevision: command.Revision, DesiredState: command.DesiredState, ObservedState: "healthy", Healthy: true}}
	reports := &serviceReportFake{}
	resolver := &serviceDesiredResolverFake{command: command}
	consumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api, issuer, guest, reports, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(command)
	if err := consumer.Handle(ctx, encoded); err != nil {
		t.Fatalf("handle Workspace Service command: %v", err)
	}
	if api.tokenInput == nil || aws.ToString(api.tokenInput.MicrovmIdentifier) != "microvm-1" || len(api.tokenInput.AllowedPorts) != 1 ||
		api.tokenInput.AllowedPorts[0].(*types.PortSpecificationMemberPort).Value != workspaceServiceAgentPort ||
		aws.ToInt32(api.tokenInput.ExpirationInMinutes) != workspaceServiceTokenMinutes {
		t.Fatalf("AWS auth token was not limited to this MicroVM's Agent port: %+v", api.tokenInput)
	}
	if guest.endpoint != aws.ToString(api.getOutput.Endpoint) || guest.token != "microvm-token" || guest.capability == "" || guest.command.ServiceID != command.ServiceID {
		t.Fatalf("Workspace Service command did not reach the bound Guest: %+v", guest)
	}
	claims, err := workspaceagent.VerifyServiceCapability(guest.capability, map[string]ed25519.PublicKey{"service-key": public},
		workspaceagent.ServiceCapability{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID, ResourcePlane: command.ResourcePlaneID,
			MicroVMID: "microvm-1", ServiceID: command.ServiceID, Revision: command.Revision, DesiredState: command.DesiredState,
			CatalogVersion: command.CatalogVersion}, command, time.Now().UTC())
	if err != nil || claims.MicroVMID != "microvm-1" {
		t.Fatalf("Guest capability is not bound to the current MicroVM: claims=%+v err=%v", claims, err)
	}
	if len(reports.reports) != 1 || reports.reports[0].MicroVMID != "microvm-1" || reports.reports[0].ObservedState != "healthy" {
		t.Fatalf("runtime observation not reported: %+v", reports.reports)
	}

	// An old queue record must trigger reconciliation of the latest DSQL state,
	// not replay its stale start command after a replacement VM is running.
	latest := command
	latest.Revision++
	latest.DesiredState = "stopped"
	latestAPI := &serviceControllerAPIFake{getOutput: api.getOutput}
	latestGuest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: latest.ServiceID, Revision: latest.Revision,
		ObservedRevision: latest.Revision, DesiredState: latest.DesiredState, ObservedState: "stopped"}}
	latestReports := &serviceReportFake{}
	latestConsumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, latestAPI, issuer,
		latestGuest, latestReports, &serviceDesiredResolverFake{command: latest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := latestConsumer.Handle(ctx, encoded); err != nil {
		t.Fatalf("reconcile current desired state from a stale queue trigger: %v", err)
	}
	if latestGuest.command.Revision != latest.Revision || latestGuest.command.DesiredState != "stopped" ||
		len(latestReports.reports) != 1 || latestReports.reports[0].Revision != latest.Revision {
		t.Fatalf("stale queue trigger was not replaced with latest desired state: guest=%+v reports=%+v", latestGuest.command, latestReports.reports)
	}

	guestSentinel := "guest returned secret endpoint details"
	latestGuest.err = errors.New(guestSentinel)
	err = latestConsumer.Handle(ctx, encoded)
	if !errors.Is(err, ErrWorkspaceServiceDelivery) || strings.Contains(err.Error(), guestSentinel) {
		t.Fatalf("Guest error escaped the retry boundary: %v", err)
	}
	latestGuest.err = nil
	reportSentinel := "provider response exposed secret ARN"
	latestReports.err = errors.New(reportSentinel)
	err = latestConsumer.Handle(ctx, encoded)
	if !errors.Is(err, ErrWorkspaceServiceDelivery) || strings.Contains(err.Error(), reportSentinel) {
		t.Fatalf("report sink error escaped the retry boundary: %v", err)
	}
}

func TestWorkspaceServiceConsumerDeliversGeneratedSecretsOnlyToBoundGuest(t *testing.T) {
	ctx := context.Background()
	command := generatedSecretCommand(t, "running")
	store := newMemoryStore()
	launchPayload, _ := json.Marshal(validSpec())
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Running, "microvm-1", 1
	if _, err := store.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{getOutput: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws"),
		ImageArn: aws.String(validSpec().ImageIdentifier), ImageVersion: aws.String(validSpec().ImageVersion),
		ExecutionRoleArn: aws.String(validSpec().ExecutionRoleARN), EgressNetworkConnectors: validSpec().EgressNetworkConnectors,
		IngressNetworkConnectors: validSpec().IngressNetworkConnectors}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := "0123456789012345678901234567890123456789012"
	provider := &workspaceServiceSecretProviderFake{values: map[string]string{"POSTGRES_PASSWORD": secret}}
	guest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision,
		ObservedRevision: command.Revision, DesiredState: command.DesiredState, ObservedState: "healthy", Healthy: true}}
	consumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api,
		&workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}, guest, &serviceReportFake{},
		&serviceDesiredResolverFake{command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer.WithSecretProvider(provider)
	encoded, _ := json.Marshal(command)
	if err := consumer.Handle(ctx, encoded); err != nil {
		t.Fatalf("handle PostgreSQL Service command: %v", err)
	}
	if provider.called != 1 || guest.secrets["POSTGRES_PASSWORD"] != secret {
		t.Fatalf("generated credential did not reach the exact Guest: provider calls=%d guest=%v", provider.called, guest.secrets)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(command.Config), secret) {
		t.Fatal("generated credential was included in command/event data")
	}
}

func TestWorkspaceServiceConsumerRetiresSecretsOnlyAfterConfirmedGuestRemoval(t *testing.T) {
	ctx := context.Background()
	command := generatedSecretCommand(t, "deleted")
	store := newMemoryStore()
	launchPayload, _ := json.Marshal(validSpec())
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Running, "microvm-1", 1
	if _, err := store.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{getOutput: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws"),
		ImageArn: aws.String(validSpec().ImageIdentifier), ImageVersion: aws.String(validSpec().ImageVersion),
		ExecutionRoleArn: aws.String(validSpec().ExecutionRoleARN), EgressNetworkConnectors: validSpec().EgressNetworkConnectors,
		IngressNetworkConnectors: validSpec().IngressNetworkConnectors}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider := &workspaceServiceSecretProviderFake{}
	guest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision,
		ObservedRevision: command.Revision, DesiredState: "deleted", ObservedState: "deleted"}}
	reports := &serviceReportFake{}
	consumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api,
		&workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}, guest, reports,
		&serviceDesiredResolverFake{command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer.WithSecretProvider(provider)
	encoded, _ := json.Marshal(command)
	if err := consumer.Handle(ctx, encoded); err != nil || provider.retireCalls != 1 || provider.called != 0 || len(reports.reports) != 1 || reports.reports[0].ObservedState != "deleted" {
		t.Fatalf("confirmed deletion did not retire generated credential before reporting: retires=%d reads=%d reports=%+v err=%v", provider.retireCalls, provider.called, reports.reports, err)
	}

	failedProvider := &workspaceServiceSecretProviderFake{}
	failedGuest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision,
		ObservedRevision: 0, DesiredState: "deleted", ObservedState: "failed", ErrorCode: "runtime_error"}, err: errors.New("remove failed")}
	failedReports := &serviceReportFake{}
	failedConsumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api,
		&workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}, failedGuest, failedReports,
		&serviceDesiredResolverFake{command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	failedConsumer.WithSecretProvider(failedProvider)
	if err := failedConsumer.Handle(ctx, encoded); err == nil || failedProvider.retireCalls != 0 || len(failedReports.reports) != 1 {
		t.Fatalf("failed Guest removal retired its secret or was not retryable: retires=%d reports=%+v err=%v", failedProvider.retireCalls, failedReports.reports, err)
	}
}

func TestWorkspaceServiceConsumerPromotesCredentialOnlyAfterHealthyGuestApply(t *testing.T) {
	ctx := context.Background()
	initialCommand := generatedSecretCommand(t, "running")
	secretManager := &serviceSecretsManagerFake{secrets: make(map[string]generatedServiceSecret)}
	secretProvider, err := NewAWSWorkspaceServiceSecretProvider(secretManager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secretProvider.Resolve(ctx, initialCommand); err != nil {
		t.Fatalf("create initial current credential: %v", err)
	}
	command := initialCommand
	command.Revision, command.CredentialGeneration = 2, 2
	store := newMemoryStore()
	launchPayload, _ := json.Marshal(validSpec())
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Running, "microvm-1", 1
	if _, err := store.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{getOutput: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws"),
		ImageArn: aws.String(validSpec().ImageIdentifier), ImageVersion: aws.String(validSpec().ImageVersion),
		ExecutionRoleArn: aws.String(validSpec().ExecutionRoleARN), EgressNetworkConnectors: validSpec().EgressNetworkConnectors,
		IngressNetworkConnectors: validSpec().IngressNetworkConnectors}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: 2,
		ObservedRevision: 2, DesiredState: "running", ObservedState: "healthy", Healthy: true}}
	reports := &serviceReportFake{}
	consumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api,
		&workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}, guest, reports,
		&serviceDesiredResolverFake{command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumer.WithSecretProvider(secretProvider)
	encoded, _ := json.Marshal(command)
	if err := consumer.Handle(ctx, encoded); err != nil || len(reports.reports) != 1 {
		t.Fatalf("healthy Guest apply did not promote the staged credential: reports=%+v err=%v", reports.reports, err)
	}
	name := workspaceServiceSecretName(command, "POSTGRES_PASSWORD")
	versions, stages := fakeSecretVersions(secretManager.secrets[name])
	versionID := workspaceServiceCredentialVersionID(command, "POSTGRES_PASSWORD")
	if !hasSecretVersionStage(stages, versionID, "AWSCURRENT") || hasSecretVersionStage(stages, versionID, "AWSPENDING") || !validGeneratedServiceSecret(versions[versionID]) {
		t.Fatalf("healthy Guest did not promote and clear the pending version: stages=%#v", stages)
	}

	unhealthyManager := &serviceSecretsManagerFake{secrets: make(map[string]generatedServiceSecret)}
	unhealthyProvider, err := NewAWSWorkspaceServiceSecretProvider(unhealthyManager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unhealthyProvider.Resolve(ctx, initialCommand); err != nil {
		t.Fatalf("create initial credential for unhealthy Guest case: %v", err)
	}
	unhealthyGuest := &serviceGuestFake{observation: workspaceServiceObservation{ServiceID: command.ServiceID, Revision: 2,
		ObservedRevision: 2, DesiredState: "running", ObservedState: "unhealthy", Healthy: false}}
	unhealthyConsumer, err := NewWorkspaceServiceCommandConsumer(command.ResourcePlaneID, "us-east-1", store, api,
		&workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}, unhealthyGuest, &serviceReportFake{},
		&serviceDesiredResolverFake{command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unhealthyConsumer.WithSecretProvider(unhealthyProvider)
	if err := unhealthyConsumer.Handle(ctx, encoded); err == nil {
		t.Fatal("unhealthy Guest apply unexpectedly acknowledged the credential rotation")
	}
	unhealthyName := workspaceServiceSecretName(command, "POSTGRES_PASSWORD")
	unhealthyVersions, unhealthyStages := fakeSecretVersions(unhealthyManager.secrets[unhealthyName])
	unhealthyVersionID := workspaceServiceCredentialVersionID(command, "POSTGRES_PASSWORD")
	if hasSecretVersionStage(unhealthyStages, unhealthyVersionID, "AWSCURRENT") || !hasSecretVersionStage(unhealthyStages, unhealthyVersionID, "AWSPENDING") || unhealthyVersions[unhealthyVersionID] == "" {
		t.Fatalf("unhealthy Guest apply promoted the staged credential: stages=%#v", unhealthyStages)
	}
}

func TestWorkspaceServiceLambdaReturnsOnlyFailedRecords(t *testing.T) {
	consumer, err := NewWorkspaceServiceCommandConsumer("rp_test", "us-east-1", newMemoryStore(), nil, nil, nil, nil, nil, nil)
	if err == nil || consumer != nil {
		t.Fatal("consumer must require all runtime dependencies")
	}
	if _, err := NewWorkspaceServiceLambdaHandler(nil); err == nil {
		t.Fatal("nil consumer must be rejected")
	}
}

func TestWorkspaceServiceConsumerRejectsForeignPlaneAndNonRunningJournal(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	command := runtimeServiceCommand(t)
	launchPayload, _ := json.Marshal(validSpec())
	journal, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1, OperationID: "op_workspace", Type: domain.OperationEnsureRunning},
		"pinned-launch-token", launchPayload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID, journal.AllocationGeneration = workspacejournal.Suspended, "microvm-1", 1
	if _, err := store.Create(ctx, journal); err != nil {
		t.Fatal(err)
	}
	api := &serviceControllerAPIFake{}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspaceagent.ServiceCapabilityIssuer{KeyID: "service-key", PrivateKey: private}
	consumer, err := NewWorkspaceServiceCommandConsumer("rp_other", "us-east-1", store, api,
		issuer, &serviceGuestFake{}, &serviceReportFake{}, &serviceDesiredResolverFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(command)
	if err := consumer.Handle(ctx, encoded); !errors.Is(err, ErrWorkspaceServiceDelivery) {
		t.Fatalf("foreign Plane command error = %v", err)
	}
	if api.getCalls != 0 || api.tokenCalls != 0 {
		t.Fatalf("foreign Plane command reached AWS API: get=%d token=%d", api.getCalls, api.tokenCalls)
	}
	consumer, err = NewWorkspaceServiceCommandConsumer("rp_test", "us-east-1", store, api,
		issuer, &serviceGuestFake{}, &serviceReportFake{}, &serviceDesiredResolverFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Handle(ctx, encoded); !errors.Is(err, ErrWorkspaceServiceDelivery) || api.getCalls != 0 {
		t.Fatalf("suspended Workspace must fail before provider lookup: err=%v get_calls=%d", err, api.getCalls)
	}
}

func TestWorkspaceServiceLambdaAdapterUsesPartialBatchFailures(t *testing.T) {
	// A valid record is acknowledged while a failed record is retried without
	// discarding the successful record's acknowledgement.
	command := runtimeServiceCommand(t)
	command.CorrelationID = "req_cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	failedPayload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	consumer := serviceCommandRuntimeFunc(func(_ context.Context, payload []byte) error {
		if bytes.Equal(payload, failedPayload) {
			return errors.New("sensitive provider response: secret reference leaked")
		}
		return nil
	})
	var logs bytes.Buffer
	handler, err := NewWorkspaceServiceLambdaHandlerWithLogger(consumer, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "ok-1", Body: "ack"}, {MessageId: "retry-2", Body: string(failedPayload)},
	}})
	if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "retry-2" {
		t.Fatalf("partial batch failure response = %+v, %v", response, err)
	}
	if !strings.Contains(logs.String(), "failure_class=command_delivery") || !strings.Contains(logs.String(), "sqs_message_id=retry-2") ||
		!strings.Contains(logs.String(), "correlation_id=req_cccccccc-cccc-4ccc-8ccc-cccccccccccc") ||
		!strings.Contains(logs.String(), "workspace_id=ws_test") || !strings.Contains(logs.String(), "service_id=svc_test") ||
		strings.Contains(logs.String(), "sensitive provider response") || strings.Contains(logs.String(), "secret reference leaked") ||
		strings.Contains(logs.String(), "postgres:18") {
		t.Fatalf("Workspace Service failure log leaked raw details or omitted identifiers: %q", logs.String())
	}
}

func TestWorkspaceServiceHTTPGuestUsesFixedAuthenticatedRoute(t *testing.T) {
	command := runtimeServiceCommand(t)
	responseBody, _ := json.Marshal(workspaceServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision,
		ObservedRevision: command.Revision, DesiredState: command.DesiredState, ObservedState: "healthy", Healthy: true})
	called := false
	guest := NewWorkspaceServiceHTTPGuest(&http.Client{Transport: serviceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.Method != http.MethodPost || request.URL.String() != "https://vm.example/v1/workspaces/ws_test/services/reconcile" ||
			request.Header.Get("X-aws-proxy-auth") != "aws-short-token" || request.Header.Get("Authorization") != "Bearer guest-capability" ||
			request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Guest request used unexpected URL or credentials: %s %+v", request.URL, request.Header)
		}
		var got struct {
			Command domain.WorkspaceServiceCommand `json:"command"`
			Secrets map[string]string              `json:"secrets"`
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil || got.Command.ServiceID != command.ServiceID || got.Command.Revision != command.Revision || len(got.Secrets) != 0 {
			t.Fatalf("Guest request command = %+v, error=%v", got, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(responseBody))}, nil
	})})
	observation, err := guest.Reconcile(context.Background(), "https://vm.example", "aws-short-token", "guest-capability", command, nil)
	if err != nil || !called || observation.ObservedState != "healthy" || !observation.Healthy {
		t.Fatalf("Guest response = %+v, called=%v, error=%v", observation, called, err)
	}
}

func TestSignedWorkspaceServiceReportSinkSignsExactBodyAndPath(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	report := WorkspaceServiceRuntimeReport{TenantID: "tenant_test", WorkspaceID: "ws_test", ResourcePlaneID: "rp_test",
		MicroVMID: "microvm-1", AllocationGeneration: 2, ServiceID: "svc_test", Revision: 4, ObservedRevision: 4, DesiredState: "running",
		ObservedState: "healthy", Healthy: true, ObservedAt: now}
	sink, err := NewSignedWorkspaceServiceReportSink("https://control.example", key, &http.Client{Transport: serviceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://control.example/v1/internal/resource-planes/rp_test/workspace-services/svc_test/reports" {
			t.Fatalf("unexpected report request URL: %s", request.URL)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		timestamp, reportID := request.Header.Get("X-Hako-Timestamp"), request.Header.Get("X-Hako-Report-ID")
		if timestamp != fmt.Sprint(now.Unix()) || len(reportID) != 48 || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("report authentication headers = %+v", request.Header)
		}
		bodyHash := sha256.Sum256(body)
		message := strings.Join([]string{http.MethodPost, request.URL.Path, timestamp, reportID, hex.EncodeToString(bodyHash[:])}, "\n")
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(message))
		if !hmac.Equal(mac.Sum(nil), mustDecodeHex(t, request.Header.Get("X-Hako-Signature"))) {
			t.Fatal("Workspace Service report signature does not match canonical request")
		}
		var got WorkspaceServiceRuntimeReport
		if err := json.Unmarshal(body, &got); err != nil || got != report {
			t.Fatalf("signed report body = %+v, error=%v", got, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accepted":true,"revision":4}`))}, nil
	})}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sink.newID = func() ([]byte, error) { return bytes.Repeat([]byte{0x2a}, 24), nil }
	if err := sink.Report(context.Background(), report); err != nil {
		t.Fatalf("send signed Workspace Service report: %v", err)
	}
}

func TestSignedWorkspaceServiceReportSinkRetriesRejectedReport(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	report := WorkspaceServiceRuntimeReport{TenantID: "tenant_test", WorkspaceID: "ws_test", ResourcePlaneID: "rp_test",
		MicroVMID: "microvm-1", AllocationGeneration: 1, ServiceID: "svc_test", Revision: 4, ObservedAt: now}
	sink, err := NewSignedWorkspaceServiceReportSink("https://control.example", key, &http.Client{Transport: serviceRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accepted":false,"revision":4}`))}, nil
	})}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Report(context.Background(), report); !errors.Is(err, ErrWorkspaceServiceDelivery) {
		t.Fatalf("Control Plane rejection must leave report delivery retryable: %v", err)
	}
}

func TestSignedWorkspaceServiceCommandResolverAuthenticatesAndReturnsLatestRevision(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	trigger := runtimeServiceCommand(t)
	trigger.CorrelationID = "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	current := trigger
	current.Revision = trigger.Revision + 1
	current.DesiredState = "stopped"
	responseBody, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewSignedWorkspaceServiceCommandResolver("https://control.example", trigger.ResourcePlaneID, key,
		&http.Client{Transport: serviceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			wantURL := "https://control.example/v1/internal/resource-planes/rp_test/workspace-services/svc_test/desired-state"
			if request.Method != http.MethodPost || request.URL.String() != wantURL || request.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected desired-state request: %s %s headers=%v", request.Method, request.URL, request.Header)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var lookup struct {
				TenantID      domain.TenantID    `json:"tenant_id"`
				WorkspaceID   domain.WorkspaceID `json:"workspace_id"`
				CorrelationID string             `json:"correlation_id"`
			}
			if json.Unmarshal(body, &lookup) != nil || lookup.TenantID != trigger.TenantID || lookup.WorkspaceID != trigger.WorkspaceID ||
				lookup.CorrelationID != trigger.CorrelationID {
				t.Fatalf("desired-state request disclosed wrong scope: %s", body)
			}
			timestamp, requestID := request.Header.Get("X-Hako-Timestamp"), request.Header.Get("X-Hako-Report-ID")
			if timestamp != fmt.Sprint(now.Unix()) || len(requestID) != 48 {
				t.Fatalf("desired-state HMAC headers invalid: timestamp=%q requestID=%q", timestamp, requestID)
			}
			bodyHash := sha256.Sum256(body)
			message := strings.Join([]string{http.MethodPost, request.URL.EscapedPath(), timestamp, requestID, hex.EncodeToString(bodyHash[:])}, "\n")
			mac := hmac.New(sha256.New, key)
			_, _ = mac.Write([]byte(message))
			if !hmac.Equal(mac.Sum(nil), mustDecodeHex(t, request.Header.Get("X-Hako-Signature"))) {
				t.Fatal("desired-state request signature does not cover method, path, timestamp, ID, and body")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(responseBody))}, nil
		})}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	resolver.newID = func() ([]byte, error) { return bytes.Repeat([]byte{0x42}, 24), nil }
	got, err := resolver.Resolve(context.Background(), trigger)
	if err != nil || got.Revision != current.Revision || got.DesiredState != "stopped" {
		t.Fatalf("resolve latest desired state = %+v, err=%v", got, err)
	}
	wrong := current
	wrong.ResourcePlaneID = "rp_foreign"
	wrongResolver, err := NewSignedWorkspaceServiceCommandResolver("https://control.example", trigger.ResourcePlaneID, key,
		&http.Client{Transport: serviceRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(mustJSON(t, wrong)))}, nil
		})}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongResolver.Resolve(context.Background(), trigger); !errors.Is(err, ErrWorkspaceServiceDelivery) {
		t.Fatalf("foreign Plane desired state must be rejected: %v", err)
	}
	wrong = current
	wrong.CorrelationID = "req_bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	wrongResolver, err = NewSignedWorkspaceServiceCommandResolver("https://control.example", trigger.ResourcePlaneID, key,
		&http.Client{Transport: serviceRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(mustJSON(t, wrong)))}, nil
		})}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongResolver.Resolve(context.Background(), trigger); !errors.Is(err, ErrWorkspaceServiceDelivery) {
		t.Fatalf("changed correlation ID must be rejected: %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type serviceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f serviceRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

type serviceCommandRuntimeFunc func(context.Context, []byte) error

func (f serviceCommandRuntimeFunc) Handle(ctx context.Context, payload []byte) error {
	return f(ctx, payload)
}

func runtimeServiceCommand(t *testing.T) domain.WorkspaceServiceCommand {
	t.Helper()
	template, err := servicecatalog.Resolve("redis")
	if err != nil {
		t.Fatal(err)
	}
	config, err := servicecatalog.CanonicalJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	return domain.WorkspaceServiceCommand{SchemaVersion: 1, TenantID: "tenant_test", WorkspaceID: "ws_test", ResourcePlaneID: "rp_test",
		ServiceID: "svc_test", Revision: 4, DesiredState: "running", CatalogVersion: template.Name + "/" + template.Version, Config: config}
}
