// Package operationresults consumes Resource Plane terminal results in the
// Control Plane and applies them to durable Operation/Workspace state.
package operationresults

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type Store interface {
	Apply(context.Context, resourcecontroller.Result) (bool, error)
}

type Queue interface {
	Receive(context.Context, int, time.Duration) ([]Delivery, error)
	Delete(context.Context, string) error
}

type Delivery struct {
	Body          []byte
	ReceiptHandle string
}

type Config struct {
	BatchSize int
	WaitTime  time.Duration
}

type Consumer struct {
	store  Store
	queue  Queue
	config Config
}

type Stats struct {
	Received  int
	Applied   int
	Duplicate int
	Failed    int
}

func New(store Store, queue Queue, config Config) (*Consumer, error) {
	if store == nil || queue == nil {
		return nil, errors.New("Operation result store and SQS queue are required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 10
	}
	if config.BatchSize < 1 || config.BatchSize > 10 {
		return nil, errors.New("SQS result batch size must be 1-10")
	}
	if config.WaitTime == 0 {
		config.WaitTime = 20 * time.Second
	}
	if config.WaitTime < 0 || config.WaitTime > 20*time.Second {
		return nil, errors.New("SQS result long-poll wait time must be 0-20 seconds")
	}
	return &Consumer{store: store, queue: queue, config: config}, nil
}

// RunOnce acknowledges a result only after its database transaction commits.
// Already-terminal Operations are treated as duplicate/stale results and acked
// without mutating current state.
func (c *Consumer) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	deliveries, err := c.queue.Receive(ctx, c.config.BatchSize, c.config.WaitTime)
	if err != nil {
		return stats, fmt.Errorf("receive Operation results: %w", err)
	}
	stats.Received = len(deliveries)
	var failures []error
	for _, delivery := range deliveries {
		if delivery.ReceiptHandle == "" {
			stats.Failed++
			failures = append(failures, errors.New("SQS result delivery has no receipt handle"))
			continue
		}
		result, err := decode(delivery.Body)
		if err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("decode Operation result: %w", err))
			continue
		}
		applied, err := c.store.Apply(ctx, result)
		if err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("apply Operation result %s: %w", result.OperationID, err))
			continue
		}
		if err := c.queue.Delete(ctx, delivery.ReceiptHandle); err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("delete applied Operation result: %w", err))
			continue
		}
		if applied {
			stats.Applied++
		} else {
			stats.Duplicate++
		}
	}
	return stats, errors.Join(failures...)
}

func decode(payload []byte) (resourcecontroller.Result, error) {
	var result resourcecontroller.Result
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return resourcecontroller.Result{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return resourcecontroller.Result{}, errors.New("result must contain exactly one JSON object")
	}
	if result.OperationID == "" || result.TenantID == "" || result.WorkspaceID == "" || result.ResourcePlaneID == "" || result.Type == "" || result.CompletedAt.IsZero() {
		return resourcecontroller.Result{}, errors.New("result envelope is incomplete or unsupported")
	}
	switch result.SchemaVersion {
	case 1:
		if result.WorkspaceRevision != 0 {
			return resourcecontroller.Result{}, errors.New("schema version 1 result must not set a Workspace revision")
		}
	case 2:
		if result.WorkspaceRevision < 1 {
			return resourcecontroller.Result{}, errors.New("schema version 2 result requires a positive Workspace revision")
		}
	default:
		return resourcecontroller.Result{}, errors.New("result envelope is incomplete or unsupported")
	}
	if result.Status != domain.OperationSucceeded && result.Status != domain.OperationFailed {
		return resourcecontroller.Result{}, errors.New("result status must be succeeded or failed")
	}
	if result.Status == domain.OperationFailed {
		if result.ErrorCode == "" || result.ObservedState != domain.ObservedWorkspaceFailed {
			return resourcecontroller.Result{}, errors.New("failed result requires an error code and failed observed state")
		}
		return result, nil
	}
	wantState := domain.ObservedWorkspaceState("")
	switch result.Type {
	case domain.OperationEnsureRunning, domain.OperationResume:
		wantState = domain.ObservedWorkspaceRunning
	case domain.OperationSuspend:
		wantState = domain.ObservedWorkspaceSuspended
	case domain.OperationDelete:
		wantState = domain.ObservedWorkspaceDeleted
	default:
		return resourcecontroller.Result{}, fmt.Errorf("unsupported result Operation type %q", result.Type)
	}
	if result.ObservedState != wantState {
		return resourcecontroller.Result{}, fmt.Errorf("successful %s result must report observed state %s", result.Type, wantState)
	}
	return result, nil
}

type StoreAdapter struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

func (s StoreAdapter) Apply(ctx context.Context, result resourcecontroller.Result) (bool, error) {
	payload, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("encode Operation result audit payload: %w", err)
	}
	return operations.ApplyResult(ctx, s.Pool, s.Policy, operations.ResultInput{
		OperationID: result.OperationID, WorkspaceRevision: result.WorkspaceRevision, TenantID: result.TenantID, WorkspaceID: result.WorkspaceID,
		ResourcePlaneID: result.ResourcePlaneID, Type: result.Type, Status: result.Status,
		ObservedState: result.ObservedState, ErrorCode: result.ErrorCode, CompletedAt: result.CompletedAt,
		Payload: payload,
	})
}
