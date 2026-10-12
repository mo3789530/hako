package workspaceruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

type memoryStore struct {
	data map[string]workspacejournal.Snapshot
	seq  int
}

func newMemoryStore() *memoryStore { return &memoryStore{data: map[string]workspacejournal.Snapshot{}} }

func storeKey(tenant domain.TenantID, workspace domain.WorkspaceID, plane domain.ResourcePlaneID) string {
	return string(tenant) + "/" + string(workspace) + "/" + string(plane)
}

func (s *memoryStore) Create(_ context.Context, journal workspacejournal.Journal) (workspacejournal.Snapshot, error) {
	key := storeKey(journal.TenantID, journal.WorkspaceID, journal.ResourcePlaneID)
	if _, exists := s.data[key]; exists {
		return workspacejournal.Snapshot{}, workspacejournal.ErrConflict
	}
	s.seq++
	snapshot := workspacejournal.Snapshot{Journal: journal, ETag: etag(s.seq)}
	s.data[key] = snapshot
	return snapshot, nil
}

func (s *memoryStore) Get(_ context.Context, tenant domain.TenantID, workspace domain.WorkspaceID, plane domain.ResourcePlaneID) (workspacejournal.Snapshot, error) {
	snapshot, ok := s.data[storeKey(tenant, workspace, plane)]
	if !ok {
		return workspacejournal.Snapshot{}, workspacejournal.ErrNotFound
	}
	return snapshot, nil
}

func (s *memoryStore) CompareAndSwap(_ context.Context, current workspacejournal.Snapshot, next workspacejournal.Journal) (workspacejournal.Snapshot, error) {
	key := storeKey(next.TenantID, next.WorkspaceID, next.ResourcePlaneID)
	stored, ok := s.data[key]
	if !ok || stored.ETag != current.ETag {
		return workspacejournal.Snapshot{}, workspacejournal.ErrConflict
	}
	if err := workspacejournal.ValidateUpdate(current, next); err != nil {
		return workspacejournal.Snapshot{}, err
	}
	s.seq++
	snapshot := workspacejournal.Snapshot{Journal: next, ETag: etag(s.seq)}
	s.data[key] = snapshot
	return snapshot, nil
}

func etag(n int) string { return `"test-` + string(rune('0'+n%10)) + `"` }

type resolverFunc func(context.Context, resourcecontroller.Command) (LaunchSpec, error)

func (f resolverFunc) Resolve(ctx context.Context, command resourcecontroller.Command) (LaunchSpec, error) {
	return f(ctx, command)
}

type apiStub struct {
	runCalls       int
	lastRun        *lambdamicrovms.RunMicrovmInput
	runErr         error
	getCalls       int
	state          types.MicrovmState
	getErr         error
	terminateErr   error
	suspendCalls   int
	resumeCalls    int
	terminateCalls int
}

func (a *apiStub) RunMicrovm(_ context.Context, input *lambdamicrovms.RunMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	a.runCalls++
	a.lastRun = input
	if a.runErr != nil {
		return nil, a.runErr
	}
	return &lambdamicrovms.RunMicrovmOutput{MicrovmId: aws.String("microvm-1"), State: types.MicrovmStatePending}, nil
}

func (a *apiStub) GetMicrovm(_ context.Context, input *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	a.getCalls++
	if a.getErr != nil {
		return nil, a.getErr
	}
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: input.MicrovmIdentifier, State: a.state,
		ImageArn: aws.String("arn:aws:lambda:us-east-1:123456789012:microvm-image:hako-standard"), ImageVersion: aws.String("3"),
		ExecutionRoleArn:         aws.String("arn:aws:iam::123456789012:role/hako-workspace"),
		EgressNetworkConnectors:  []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:egress"},
		IngressNetworkConnectors: []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:ingress"}}, nil
}

func (a *apiStub) SuspendMicrovm(context.Context, *lambdamicrovms.SuspendMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.SuspendMicrovmOutput, error) {
	a.suspendCalls++
	a.state = types.MicrovmStateSuspended
	return &lambdamicrovms.SuspendMicrovmOutput{}, nil
}

func (a *apiStub) ResumeMicrovm(context.Context, *lambdamicrovms.ResumeMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error) {
	a.resumeCalls++
	a.state = types.MicrovmStateRunning
	return &lambdamicrovms.ResumeMicrovmOutput{}, nil
}

func (a *apiStub) TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	a.terminateCalls++
	if a.terminateErr != nil {
		return nil, a.terminateErr
	}
	a.state = types.MicrovmStateTerminated
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

func validSpec() LaunchSpec {
	return LaunchSpec{RuntimeClass: "standard", ImageIdentifier: "arn:aws:lambda:us-east-1:123456789012:microvm-image:hako-standard",
		ImageVersion: "3", ExecutionRoleARN: "arn:aws:iam::123456789012:role/hako-workspace",
		EgressNetworkConnectors:  []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:egress"},
		IngressNetworkConnectors: []string{"arn:aws:lambda:us-east-1:123456789012:network-connector:ingress"},
		MaximumDurationInSeconds: 3600, AutoResumeEnabled: false, MaxIdleDurationSeconds: 3600,
		SuspendedDurationSeconds: 0, RunHookPayload: `{"workspace":"ws_test"}`}
}

func newRuntime(t *testing.T, api *apiStub, store *memoryStore, resolve resolverFunc, timeout time.Duration) *LambdaMicroVM {
	t.Helper()
	runtime, err := NewLambdaMicroVM(api, store, resolve, Config{SupportedRuntimeClasses: map[string]bool{"standard": true}, PollInterval: 10 * time.Millisecond, PollTimeout: timeout}, func() time.Time { return time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func testCommand(kind domain.OperationType, revision int64, operation string) resourcecontroller.Command {
	return resourcecontroller.Command{SchemaVersion: 2, WorkspaceRevision: revision, OperationID: domain.OperationID(operation), TenantID: "tenant_test", WorkspaceID: "ws_test", ResourcePlaneID: "rp_test", Type: kind, CreatedAt: time.Now().UTC()}
}

func TestEnsurePinsLaunchAndDuplicateDoesNotAllocateTwice(t *testing.T) {
	api, store := &apiStub{state: types.MicrovmStateRunning}, newMemoryStore()
	runtime := newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }, time.Second)
	command := testCommand(domain.OperationEnsureRunning, 1, "op_ensure")
	observed, err := runtime.Execute(context.Background(), command)
	if err != nil || observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("Execute() = %q, %v", observed, err)
	}
	if api.runCalls != 1 || api.lastRun == nil || aws.ToString(api.lastRun.ImageIdentifier) != validSpec().ImageIdentifier || aws.ToString(api.lastRun.ImageVersion) != "3" || aws.ToString(api.lastRun.ClientToken) == "" || aws.ToString(api.lastRun.RunHookPayload) != validSpec().RunHookPayload {
		t.Fatalf("unexpected frozen launch request: calls=%d input=%+v", api.runCalls, api.lastRun)
	}
	observed, err = runtime.Execute(context.Background(), command)
	if err != nil || observed != domain.ObservedWorkspaceRunning || api.runCalls != 1 {
		t.Fatalf("duplicate Execute() = %q, %v; Run calls=%d", observed, err, api.runCalls)
	}
	if observed, err = runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 2, "op_reconcile")); err != nil || observed != domain.ObservedWorkspaceRunning || api.runCalls != 1 {
		t.Fatalf("ensure reconciliation = %q, %v; Run calls=%d", observed, err, api.runCalls)
	}
	stored, err := store.Get(context.Background(), command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil || len(stored.Journal.LaunchSpecPayload) == 0 || stored.Journal.ProviderID != "microvm-1" {
		t.Fatalf("launch recovery state not durable: %+v, %v", stored.Journal, err)
	}
}

func TestAmbiguousLaunchRetryUsesPinnedRequestAfterResolverChanges(t *testing.T) {
	api, store := &apiStub{runErr: errors.New("connection reset after request"), state: types.MicrovmStateRunning}, newMemoryStore()
	currentSpec := validSpec()
	runtime := newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return currentSpec, nil }, time.Second)
	command := testCommand(domain.OperationEnsureRunning, 1, "op_ambiguous")
	if _, err := runtime.Execute(context.Background(), command); err == nil {
		t.Fatal("ambiguous RunMicrovm error was swallowed")
	}
	firstToken := aws.ToString(api.lastRun.ClientToken)
	firstImage := aws.ToString(api.lastRun.ImageIdentifier)
	currentSpec.ImageIdentifier = "arn:aws:lambda:us-east-1:123456789012:microvm-image:changed"
	api.runErr = nil
	if observed, err := runtime.Execute(context.Background(), command); err != nil || observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("retry Execute() = %q, %v", observed, err)
	}
	if api.runCalls != 2 || aws.ToString(api.lastRun.ClientToken) != firstToken || aws.ToString(api.lastRun.ImageIdentifier) != firstImage {
		t.Fatalf("retry changed idempotent request: calls=%d token=%q image=%q", api.runCalls, aws.ToString(api.lastRun.ClientToken), aws.ToString(api.lastRun.ImageIdentifier))
	}
}

func TestUnsupportedClassAndProviderErrorFailClosed(t *testing.T) {
	api, store := &apiStub{}, newMemoryStore()
	runtime := newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) {
		spec := validSpec()
		spec.RuntimeClass = "large"
		return spec, nil
	}, time.Second)
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_unsupported")); !errors.Is(err, ErrSpecification) || api.runCalls != 0 {
		t.Fatalf("unsupported class should fail before provider call: err=%v runs=%d", err, api.runCalls)
	}
	api, store = &apiStub{}, newMemoryStore()
	runtime = newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) {
		spec := validSpec()
		spec.RunHookPayload = `{"session_token":"do-not-store"}`
		return spec, nil
	}, time.Second)
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_secret")); !errors.Is(err, ErrSpecification) || api.runCalls != 0 {
		t.Fatalf("secret-bearing run hook must be rejected before provider call: err=%v runs=%d", err, api.runCalls)
	}

	api, store = &apiStub{state: types.MicrovmStateRunning, getErr: errors.New("provider unavailable")}, newMemoryStore()
	runtime = newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }, time.Second)
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_api_error")); err == nil || api.runCalls != 1 {
		t.Fatalf("provider Get error not returned: err=%v runs=%d", err, api.runCalls)
	}
}

func TestStaleAndTombstonedCommandRejectedBeforeProviderCall(t *testing.T) {
	api, store := &apiStub{state: types.MicrovmStateRunning}, newMemoryStore()
	runtime := newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }, time.Second)
	ensure := testCommand(domain.OperationEnsureRunning, 2, "op_ensure")
	if _, err := runtime.Execute(context.Background(), ensure); err != nil {
		t.Fatal(err)
	}
	if got, err := runtime.Execute(context.Background(), testCommand(domain.OperationDelete, 3, "op_delete")); err != nil || got != domain.ObservedWorkspaceDeleted {
		t.Fatalf("delete = %q, %v", got, err)
	}
	before := api.runCalls + api.getCalls + api.terminateCalls
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_stale")); !errors.Is(err, workspacejournal.ErrStale) {
		t.Fatalf("stale command error = %v", err)
	}
	if api.runCalls+api.getCalls+api.terminateCalls != before || before < 1 {
		t.Fatalf("stale command called provider: before=%d after=%d", before, api.runCalls+api.getCalls+api.terminateCalls)
	}
}

func TestPollDeadlineAndSuspendResumeLifecycle(t *testing.T) {
	api, store := &apiStub{state: types.MicrovmStatePending}, newMemoryStore()
	runtime := newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }, 25*time.Millisecond)
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_timeout")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending MicroVM should honor poll timeout; got %v", err)
	}

	api, store = &apiStub{state: types.MicrovmStateRunning}, newMemoryStore()
	runtime = newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }, time.Second)
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_lifecycle")); err != nil {
		t.Fatal(err)
	}
	if got, err := runtime.Execute(context.Background(), testCommand(domain.OperationSuspend, 2, "op_suspend")); err != nil || got != domain.ObservedWorkspaceSuspended || api.suspendCalls != 1 {
		t.Fatalf("suspend = %q, %v calls=%d", got, err, api.suspendCalls)
	}
	if got, err := runtime.Execute(context.Background(), testCommand(domain.OperationResume, 3, "op_resume")); err != nil || got != domain.ObservedWorkspaceRunning || api.resumeCalls != 1 {
		t.Fatalf("resume = %q, %v calls=%d", got, err, api.resumeCalls)
	}
}

func TestDeletePersistsCleanupFailureAndRetriesIdempotently(t *testing.T) {
	api, store := &apiStub{state: types.MicrovmStateRunning}, newMemoryStore()
	runtime := newRuntime(t, api, store, func(context.Context, resourcecontroller.Command) (LaunchSpec, error) { return validSpec(), nil }, time.Second)
	if _, err := runtime.Execute(context.Background(), testCommand(domain.OperationEnsureRunning, 1, "op_cleanup_ensure")); err != nil {
		t.Fatal(err)
	}
	deleteCommand := testCommand(domain.OperationDelete, 2, "op_cleanup_delete")
	api.terminateErr = errors.New("temporary provider failure")
	if _, err := runtime.Execute(context.Background(), deleteCommand); err == nil {
		t.Fatal("TerminateMicrovm failure was swallowed")
	}
	failed, err := store.Get(context.Background(), deleteCommand.TenantID, deleteCommand.WorkspaceID, deleteCommand.ResourcePlaneID)
	if err != nil || !failed.Journal.Tombstone || failed.Journal.State != workspacejournal.Failed || failed.Journal.CleanupState != workspacejournal.CleanupFailed {
		t.Fatalf("cleanup failure was not durably recorded: %+v, %v", failed.Journal, err)
	}
	api.terminateErr = nil
	if observed, err := runtime.Execute(context.Background(), deleteCommand); err != nil || observed != domain.ObservedWorkspaceDeleted {
		t.Fatalf("delete retry = %q, %v", observed, err)
	}
	completed, err := store.Get(context.Background(), deleteCommand.TenantID, deleteCommand.WorkspaceID, deleteCommand.ResourcePlaneID)
	if err != nil || completed.Journal.State != workspacejournal.Deleted || completed.Journal.CleanupState != workspacejournal.CleanupConfirmed || api.terminateCalls != 2 {
		t.Fatalf("delete retry did not confirm cleanup: %+v, calls=%d, err=%v", completed.Journal, api.terminateCalls, err)
	}
}

var _ LambdaAPI = (*apiStub)(nil)
