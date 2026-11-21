package previewcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

const (
	previewRuntimeResourceKind = "lambda_microvm"
	previewRoutePrefix         = "preview-route-"
)

type WorkspaceRuntime interface {
	resourcecontroller.Runtime
	resourcecontroller.ResourceBindingReporter
}

// WorkspaceRuntimeAdapter maps Preview-generation commands onto the durable
// Resource Plane Workspace Runtime journal. Each PR generation already has a
// distinct deterministic runtime Workspace ID, so its revision fence is
// independent from both developer Workspaces and later PR generations.
// GatewayRouteID is a logical, deterministic routing identity; the Gateway
// resolves the actual MicroVM from the same Resource Plane journal.
type WorkspaceRuntimeAdapter struct {
	resourcePlaneID domain.ResourcePlaneID
	runtime         WorkspaceRuntime
	source          PreviewSourceMaterializerProvider
	installer       PreviewSourceInstaller
	now             func() time.Time
}

type PreviewSourceMaterializerProvider interface {
	Materialize(context.Context, previewprotocol.Command) (workloadsource.MaterializedSource, error)
}

type PreviewSourceInstaller interface {
	Install(context.Context, previewprotocol.Command, Binding, workloadsource.MaterializedSource) error
}

func NewWorkspaceRuntimeAdapter(resourcePlaneID domain.ResourcePlaneID, runtime WorkspaceRuntime, now func() time.Time) (*WorkspaceRuntimeAdapter, error) {
	if !resultPlaneIDPattern.MatchString(string(resourcePlaneID)) || runtime == nil {
		return nil, ErrConfiguration
	}
	if now == nil {
		now = time.Now
	}
	return &WorkspaceRuntimeAdapter{resourcePlaneID: resourcePlaneID, runtime: runtime, now: now}, nil
}

func NewWorkspaceRuntimeAdapterWithSource(resourcePlaneID domain.ResourcePlaneID, runtime WorkspaceRuntime,
	source PreviewSourceMaterializerProvider, installer PreviewSourceInstaller, now func() time.Time) (*WorkspaceRuntimeAdapter, error) {
	if source == nil || installer == nil {
		return nil, ErrConfiguration
	}
	adapter, err := NewWorkspaceRuntimeAdapter(resourcePlaneID, runtime, now)
	if err != nil {
		return nil, err
	}
	adapter.source, adapter.installer = source, installer
	return adapter, nil
}

func (r *WorkspaceRuntimeAdapter) Ensure(ctx context.Context, command previewprotocol.Command) (Binding, error) {
	if err := r.validateCommand(ctx, command, previewprotocol.ActionProvision); err != nil {
		return Binding{}, err
	}
	binding, err := r.ensureRuntime(ctx, command)
	if err != nil {
		return Binding{}, err
	}
	if r.source != nil && r.installer != nil {
		source, err := r.source.Materialize(ctx, command)
		if err != nil || !validPreviewSource(source, workloadjournalIdentity(command)) {
			return Binding{}, ErrPreviewSourceUnavailable
		}
		if err := r.installer.Install(ctx, command, binding, source); err != nil {
			return Binding{}, ErrPreviewSourceUnavailable
		}
	}
	return binding, nil
}

func (r *WorkspaceRuntimeAdapter) ensureRuntime(ctx context.Context, command previewprotocol.Command) (Binding, error) {
	workspaceCommand := r.workspaceCommand(command, domain.OperationEnsureRunning)
	state, err := r.runtime.Execute(ctx, workspaceCommand)
	if err != nil || state != domain.ObservedWorkspaceRunning {
		return Binding{}, ErrMicroVMLaunch
	}
	kind, microVMID, allocationGeneration, err := r.runtime.ResourceBinding(ctx, workspaceCommand)
	if err != nil || kind != previewRuntimeResourceKind || !safeProviderID(microVMID) || allocationGeneration < 1 {
		return Binding{}, ErrRuntimeUnavailable
	}
	return Binding{MicroVMID: microVMID, GatewayRouteID: previewGatewayRouteID(command)}, nil
}

func (r *WorkspaceRuntimeAdapter) Cleanup(ctx context.Context, command previewprotocol.Command) error {
	if err := r.validateCommand(ctx, command, previewprotocol.ActionCleanup); err != nil {
		return err
	}
	state, err := r.runtime.Execute(ctx, r.workspaceCommand(command, domain.OperationDelete))
	if err != nil || state != domain.ObservedWorkspaceDeleted {
		return ErrMicroVMTerminate
	}
	return nil
}

// Compensate first retries the idempotent ensure operation so an ambiguous
// RunMicrovm response can be recovered from its frozen journal request, then
// deletes that exact journaled allocation. It never scans or adopts MicroVMs.
func (r *WorkspaceRuntimeAdapter) Compensate(ctx context.Context, command previewprotocol.Command, binding Binding) error {
	if err := r.validateCommand(ctx, command, previewprotocol.ActionProvision); err != nil {
		return err
	}
	if binding.GatewayRouteID != "" && binding.GatewayRouteID != previewGatewayRouteID(command) {
		return ErrRuntimeUnavailable
	}
	if binding.MicroVMID != "" && !safeProviderID(binding.MicroVMID) {
		return ErrRuntimeUnavailable
	}
	recovered, err := r.ensureRuntime(ctx, command)
	if err != nil {
		return err
	}
	if binding.MicroVMID != "" && recovered.MicroVMID != binding.MicroVMID {
		return ErrRuntimeUnavailable
	}
	return r.Cleanup(ctx, cleanupCommand(command))
}

func workloadjournalIdentity(command previewprotocol.Command) workloadjournal.Command {
	return workloadjournal.Command{TenantID: command.TenantID, WorkloadRunID: command.RuntimeWorkspaceID,
		AttemptID: command.ClaimID, Generation: command.Generation, ResourcePlaneID: command.ResourcePlaneID}
}

func (r *WorkspaceRuntimeAdapter) validateCommand(ctx context.Context, command previewprotocol.Command, action string) error {
	if r == nil || r.runtime == nil || ctx == nil || ctx.Err() != nil || command.Action != action ||
		command.ResourcePlaneID != string(r.resourcePlaneID) ||
		command.Validate(previewprotocol.EventType(action), command.RuntimeWorkspaceID) != nil {
		return ErrRuntimeUnavailable
	}
	return nil
}

func (r *WorkspaceRuntimeAdapter) workspaceCommand(command previewprotocol.Command, operation domain.OperationType) resourcecontroller.Command {
	revision := int64(1)
	if operation == domain.OperationDelete {
		revision = 2
	}
	return resourcecontroller.Command{
		SchemaVersion: 2, WorkspaceRevision: revision,
		OperationID: previewOperationID(command, operation), TenantID: domain.TenantID(command.TenantID),
		WorkspaceID: domain.WorkspaceID(command.RuntimeWorkspaceID), ResourcePlaneID: domain.ResourcePlaneID(command.ResourcePlaneID),
		Type: operation, CreatedAt: r.now().UTC(),
	}
}

func previewOperationID(command previewprotocol.Command, operation domain.OperationType) domain.OperationID {
	digest := sha256.Sum256([]byte(command.ResourcePlaneID + "\x00" + command.RuntimeWorkspaceID))
	return domain.OperationID("preview-" + string(operation) + "-" + hex.EncodeToString(digest[:16]))
}

func cleanupCommand(command previewprotocol.Command) previewprotocol.Command {
	command.Action = previewprotocol.ActionCleanup
	command.HeadSHA = ""
	command.HeadRef = ""
	return command
}

func previewGatewayRouteID(command previewprotocol.Command) string {
	digest := sha256.Sum256([]byte(command.ResourcePlaneID + "\x00" + command.RuntimeWorkspaceID))
	return previewRoutePrefix + hex.EncodeToString(digest[:16])
}

var _ Runtime = (*WorkspaceRuntimeAdapter)(nil)
