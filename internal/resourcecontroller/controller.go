// Package resourcecontroller defines the Resource Plane command handler. The
// fake implementation is independent of Control Plane storage; results are
// reported through a transport-specific sink.
package resourcecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

type Command struct {
	SchemaVersion     int                    `json:"schema_version"`
	WorkspaceRevision int64                  `json:"workspace_revision,omitempty"`
	OperationID       domain.OperationID     `json:"operation_id"`
	TenantID          domain.TenantID        `json:"tenant_id"`
	WorkspaceID       domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID   domain.ResourcePlaneID `json:"resource_plane_id"`
	Type              domain.OperationType   `json:"type"`
	CreatedAt         time.Time              `json:"created_at"`
}

type Result struct {
	SchemaVersion     int                           `json:"schema_version"`
	WorkspaceRevision int64                         `json:"workspace_revision,omitempty"`
	OperationID       domain.OperationID            `json:"operation_id"`
	TenantID          domain.TenantID               `json:"tenant_id"`
	WorkspaceID       domain.WorkspaceID            `json:"workspace_id"`
	ResourcePlaneID   domain.ResourcePlaneID        `json:"resource_plane_id"`
	Type              domain.OperationType          `json:"type"`
	Status            domain.OperationStatus        `json:"status"`
	ObservedState     domain.ObservedWorkspaceState `json:"observed_state"`
	ErrorCode         string                        `json:"error_code,omitempty"`
	CompletedAt       time.Time                     `json:"completed_at"`
}

// Runtime applies a Workspace command. Implementations that manage real
// resources must durably and atomically compare WorkspaceRevision against a
// per-Workspace high-water mark before side effects, preserving delete
// tombstones so delayed commands cannot resurrect deleted Workspaces. The
// Fake Runtime only models this fence in process memory.
type Runtime interface {
	Execute(context.Context, Command) (domain.ObservedWorkspaceState, error)
}

// ResultSink must durably report a result before Handle returns success. SQS
// adapters can then delete the command; failed reporting leaves it retryable.
type ResultSink interface {
	Report(context.Context, Result) error
}

type Controller struct {
	resourcePlaneID  domain.ResourcePlaneID
	runtime          Runtime
	sink             ResultSink
	now              func() time.Time
	executionTimeout time.Duration
	maxCommandAge    time.Duration
}

func New(resourcePlaneID domain.ResourcePlaneID, runtime Runtime, sink ResultSink, now func() time.Time) (*Controller, error) {
	return NewWithExecutionTimeoutAndMaxCommandAge(resourcePlaneID, runtime, sink, now, 15*time.Minute, 30*time.Minute)
}

func NewWithExecutionTimeout(resourcePlaneID domain.ResourcePlaneID, runtime Runtime, sink ResultSink, now func() time.Time, timeout time.Duration) (*Controller, error) {
	return NewWithExecutionTimeoutAndMaxCommandAge(resourcePlaneID, runtime, sink, now, timeout, 30*time.Minute)
}

func NewWithExecutionTimeoutAndMaxCommandAge(resourcePlaneID domain.ResourcePlaneID, runtime Runtime, sink ResultSink, now func() time.Time, timeout, maxCommandAge time.Duration) (*Controller, error) {
	if resourcePlaneID == "" || runtime == nil || sink == nil {
		return nil, errors.New("Resource Plane ID, runtime, and result sink are required")
	}
	if timeout <= 0 {
		return nil, errors.New("Runtime execution timeout must be positive")
	}
	if maxCommandAge <= 0 {
		return nil, errors.New("maximum command age must be positive")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Controller{resourcePlaneID: resourcePlaneID, runtime: runtime, sink: sink, now: now, executionTimeout: timeout, maxCommandAge: maxCommandAge}, nil
}

// Handle validates an Operation command, applies it through the Runtime, and
// reports a terminal result. Duplicate commands are safe when the Runtime is
// idempotent by Operation ID and the result receiver uses compare-and-swap.
func (c *Controller) Handle(ctx context.Context, payload []byte) error {
	command, err := decodeCommand(payload)
	if err != nil {
		return err
	}
	if command.ResourcePlaneID != c.resourcePlaneID {
		return fmt.Errorf("command targets Resource Plane %q, this controller is %q", command.ResourcePlaneID, c.resourcePlaneID)
	}
	now := c.now().UTC()
	if command.CreatedAt.After(now.Add(5 * time.Minute)) {
		return errors.New("Resource Plane command creation time is too far in the future")
	}
	if now.Sub(command.CreatedAt) > c.maxCommandAge {
		result := Result{
			SchemaVersion: command.SchemaVersion, WorkspaceRevision: command.WorkspaceRevision, OperationID: command.OperationID, TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
			ResourcePlaneID: command.ResourcePlaneID, Type: command.Type, Status: domain.OperationFailed,
			ObservedState: domain.ObservedWorkspaceFailed, ErrorCode: "operation_expired", CompletedAt: now,
		}
		if err := c.sink.Report(ctx, result); err != nil {
			return fmt.Errorf("report expired Operation result: %w", err)
		}
		return nil
	}
	executionCtx, cancel := context.WithTimeout(ctx, c.executionTimeout)
	observed, executeErr := c.runtime.Execute(executionCtx, command)
	cancel()
	result := Result{
		SchemaVersion: command.SchemaVersion, WorkspaceRevision: command.WorkspaceRevision, OperationID: command.OperationID, TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, Type: command.Type, Status: domain.OperationSucceeded,
		ObservedState: observed, CompletedAt: c.now().UTC(),
	}
	if executeErr != nil {
		result.Status = domain.OperationFailed
		result.ObservedState = domain.ObservedWorkspaceFailed
		result.ErrorCode = "runtime_error"
		if errors.Is(executeErr, context.DeadlineExceeded) {
			result.ErrorCode = "operation_timeout"
		}
	}
	if err := c.sink.Report(ctx, result); err != nil {
		return fmt.Errorf("report Operation result: %w", err)
	}
	return nil
}

func decodeCommand(payload []byte) (Command, error) {
	var command Command
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return Command{}, fmt.Errorf("decode Resource Plane command: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Command{}, errors.New("Resource Plane command must contain exactly one JSON object")
	}
	if command.OperationID == "" || command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" || command.CreatedAt.IsZero() {
		return Command{}, errors.New("Resource Plane command has an incomplete or unsupported envelope")
	}
	switch command.SchemaVersion {
	case 1:
		if command.WorkspaceRevision != 0 {
			return Command{}, errors.New("schema version 1 command must not set a Workspace revision")
		}
	case 2:
		if command.WorkspaceRevision < 1 {
			return Command{}, errors.New("schema version 2 command requires a positive Workspace revision")
		}
	default:
		return Command{}, errors.New("Resource Plane command has an incomplete or unsupported envelope")
	}
	switch command.Type {
	case domain.OperationEnsureRunning, domain.OperationSuspend, domain.OperationResume, domain.OperationDelete:
		return command, nil
	default:
		return Command{}, fmt.Errorf("unsupported Resource Plane Operation type %q", strings.TrimSpace(string(command.Type)))
	}
}
