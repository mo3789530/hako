package previewcontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type previewWorkspaceRuntimeFake struct {
	commands   []resourcecontroller.Command
	state      domain.ObservedWorkspaceState
	stateErr   error
	kind       string
	externalID string
	generation int64
	bindingErr error
}

type previewRuntimeResultCapture struct {
	results []Result
}

type previewSourceMaterializerFake struct {
	source workloadsource.MaterializedSource
	err    error
	calls  int
}

func (f *previewSourceMaterializerFake) Materialize(_ context.Context, _ previewprotocol.Command) (workloadsource.MaterializedSource, error) {
	f.calls++
	return f.source, f.err
}

type previewSourceInstallerFake struct {
	calls   int
	command previewprotocol.Command
	binding Binding
	source  workloadsource.MaterializedSource
	err     error
}

func (f *previewSourceInstallerFake) Install(_ context.Context, command previewprotocol.Command, binding Binding, source workloadsource.MaterializedSource) error {
	f.calls++
	f.command, f.binding, f.source = command, binding, source
	return f.err
}

func (s *previewRuntimeResultCapture) Report(_ context.Context, result Result) error {
	s.results = append(s.results, result)
	return nil
}

func (r *previewWorkspaceRuntimeFake) Execute(_ context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	r.commands = append(r.commands, command)
	if command.Type == domain.OperationDelete {
		return domain.ObservedWorkspaceDeleted, r.stateErr
	}
	return r.state, r.stateErr
}

func (r *previewWorkspaceRuntimeFake) ResourceBinding(_ context.Context, command resourcecontroller.Command) (string, string, int64, error) {
	if len(r.commands) == 0 || r.commands[len(r.commands)-1] != command {
		return "", "", 0, errors.New("binding query was not for the most recent runtime command")
	}
	return r.kind, r.externalID, r.generation, r.bindingErr
}

func TestWorkspaceRuntimeAdapterFencesProvisionAndCleanup(t *testing.T) {
	runtime := &previewWorkspaceRuntimeFake{state: domain.ObservedWorkspaceRunning, kind: previewRuntimeResourceKind,
		externalID: "microvm-preview-1", generation: 1}
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	adapter, err := NewWorkspaceRuntimeAdapter("rp-test", runtime, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	command := validCommand(now, previewprotocol.ActionProvision)
	command.ResourcePlaneID = "rp-test"
	first, err := adapter.Ensure(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.Ensure(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.MicroVMID != runtime.externalID || first.GatewayRouteID != previewGatewayRouteID(command) ||
		len(runtime.commands) != 2 || runtime.commands[0] != runtime.commands[1] {
		t.Fatalf("provision retries did not converge on one journaled runtime identity: first=%+v second=%+v commands=%+v", first, second, runtime.commands)
	}
	ensure := runtime.commands[0]
	if ensure.SchemaVersion != 2 || ensure.WorkspaceRevision != 1 || ensure.Type != domain.OperationEnsureRunning ||
		ensure.TenantID != domain.TenantID(command.TenantID) || ensure.WorkspaceID != domain.WorkspaceID(command.RuntimeWorkspaceID) ||
		ensure.ResourcePlaneID != "rp-test" || ensure.OperationID == "" {
		t.Fatalf("Preview provision was not mapped to its isolated Lambda Workspace journal: %+v", ensure)
	}

	cleanup := command
	cleanup.Action = previewprotocol.ActionCleanup
	cleanup.HeadSHA = ""
	cleanup.HeadRef = ""
	runtime.state = domain.ObservedWorkspaceDeleted
	if err := adapter.Cleanup(context.Background(), cleanup); err != nil {
		t.Fatal(err)
	}
	delete := runtime.commands[len(runtime.commands)-1]
	if delete.Type != domain.OperationDelete || delete.WorkspaceRevision != 2 || delete.WorkspaceID != ensure.WorkspaceID || delete.OperationID == ensure.OperationID {
		t.Fatalf("Preview cleanup did not advance the same journal to a fenced delete: ensure=%+v delete=%+v", ensure, delete)
	}
	if err := adapter.Cleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("idempotent cleanup retry: %v", err)
	}
	if runtime.commands[len(runtime.commands)-1] != delete {
		t.Fatal("cleanup retry did not preserve the deterministic delete Operation")
	}
}

func TestWorkspaceRuntimeAdapterInstallsPinnedSourceAfterRuntimeBinding(t *testing.T) {
	now := time.Now().UTC()
	command := validCommand(now, previewprotocol.ActionProvision)
	command.ResourcePlaneID = "rp-test"
	source := &previewSourceMaterializerFake{source: workloadsource.MaterializedSource{Digest: strings.Repeat("b", 64),
		ArtifactPrefix: "s3://hako-source/previews/tenant_1/prvws_0123456789abcdef0123456789abcdef/preview-claim-1234/"}}
	installer := &previewSourceInstallerFake{}
	runtime := &previewWorkspaceRuntimeFake{state: domain.ObservedWorkspaceRunning, kind: previewRuntimeResourceKind,
		externalID: "microvm-preview-source", generation: 1}
	adapter, err := NewWorkspaceRuntimeAdapterWithSource("rp-test", runtime, source, installer, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := adapter.Ensure(context.Background(), command)
	if err != nil {
		t.Fatalf("provision and install pinned Preview source: %v", err)
	}
	if source.calls != 1 || installer.calls != 1 || installer.command != command || installer.binding != binding || installer.source != source.source ||
		binding.MicroVMID != runtime.externalID {
		t.Fatalf("Preview source was not installed against the exact runtime binding: source_calls=%d installer=%+v binding=%+v", source.calls, installer, binding)
	}
	if err := adapter.Compensate(context.Background(), command, binding); err != nil {
		t.Fatalf("compensate provision without requiring fresh source credentials: %v", err)
	}
	if source.calls != 1 || installer.calls != 1 {
		t.Fatalf("compensation attempted to rematerialize source: source_calls=%d install_calls=%d", source.calls, installer.calls)
	}
}

func TestWorkspaceRuntimeAdapterCompensatesPartialProvision(t *testing.T) {
	runtime := &previewWorkspaceRuntimeFake{state: domain.ObservedWorkspaceRunning, kind: previewRuntimeResourceKind,
		externalID: "microvm-preview-2", generation: 1}
	adapter, err := NewWorkspaceRuntimeAdapter("rp-test", runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := validCommand(time.Now().UTC(), previewprotocol.ActionProvision)
	command.ResourcePlaneID = "rp-test"
	if err := adapter.Compensate(context.Background(), command, Binding{}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.commands) != 2 || runtime.commands[0].Type != domain.OperationEnsureRunning || runtime.commands[1].Type != domain.OperationDelete ||
		runtime.commands[0].WorkspaceID != runtime.commands[1].WorkspaceID {
		t.Fatalf("compensation did not recover then delete the exact deterministic journal binding: %+v", runtime.commands)
	}
}

func TestWorkspaceRuntimeAdapterDoesNotDeleteWhenPartialBindingConflictsWithJournal(t *testing.T) {
	runtime := &previewWorkspaceRuntimeFake{state: domain.ObservedWorkspaceRunning, kind: previewRuntimeResourceKind,
		externalID: "microvm-preview-canonical", generation: 1}
	adapter, err := NewWorkspaceRuntimeAdapter("rp-test", runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := validCommand(time.Now().UTC(), previewprotocol.ActionProvision)
	if err := adapter.Compensate(context.Background(), command, Binding{MicroVMID: "microvm-unexpected"}); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("compensation accepted a provider ID different from the durable journal: %v", err)
	}
	if len(runtime.commands) != 1 || runtime.commands[0].Type != domain.OperationEnsureRunning {
		t.Fatalf("compensation issued delete after a journal/provider binding mismatch: %+v", runtime.commands)
	}
}

func TestWorkspaceRuntimeAdapterRejectsForeignPlaneAndInvalidProviderBinding(t *testing.T) {
	runtime := &previewWorkspaceRuntimeFake{state: domain.ObservedWorkspaceRunning, kind: previewRuntimeResourceKind,
		externalID: "microvm-preview-3", generation: 1}
	adapter, err := NewWorkspaceRuntimeAdapter("rp-test", runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := validCommand(time.Now().UTC(), previewprotocol.ActionProvision)
	command.ResourcePlaneID = "rp-other"
	if _, err := adapter.Ensure(context.Background(), command); !errors.Is(err, ErrRuntimeUnavailable) || len(runtime.commands) != 0 {
		t.Fatalf("foreign Plane command reached the Workspace runtime: err=%v calls=%d", err, len(runtime.commands))
	}
	command.ResourcePlaneID = "rp-test"
	runtime.kind = "ec2"
	if _, err := adapter.Ensure(context.Background(), command); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("unsupported runtime kind was accepted: %v", err)
	}
	runtime.kind = previewRuntimeResourceKind
	runtime.generation = 0
	if _, err := adapter.Ensure(context.Background(), command); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("missing durable allocation generation was accepted: %v", err)
	}
}

func TestPreviewControllerComposesWorkspaceRuntimeAdapterAndResult(t *testing.T) {
	now := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	runtime := &previewWorkspaceRuntimeFake{state: domain.ObservedWorkspaceRunning, kind: previewRuntimeResourceKind,
		externalID: "microvm-preview-e2e", generation: 1}
	adapter, err := NewWorkspaceRuntimeAdapter("rp-test", runtime, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sink := &previewRuntimeResultCapture{}
	controller, err := New("rp-test", adapter, sink, func() time.Time { return now }, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	provision := validCommand(now, previewprotocol.ActionProvision)
	encoded, err := previewprotocol.Encode(provision)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), encoded, previewprotocol.EventType(provision.Action), provision.RuntimeWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 1 || sink.results[0].Status != "succeeded" || sink.results[0].Binding == nil ||
		sink.results[0].Binding.MicroVMID != runtime.externalID || sink.results[0].Binding.GatewayRouteID != previewGatewayRouteID(provision) {
		t.Fatalf("Preview result did not publish the journal-bound MicroVM identity: %+v", sink.results)
	}

	cleanup := cleanupCommand(provision)
	encoded, err = previewprotocol.Encode(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(context.Background(), encoded, previewprotocol.EventType(cleanup.Action), cleanup.RuntimeWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 2 || sink.results[1].Action != previewprotocol.ActionCleanup || sink.results[1].Status != "succeeded" || sink.results[1].Binding != nil {
		t.Fatalf("Preview cleanup result is invalid: %+v", sink.results)
	}
}
