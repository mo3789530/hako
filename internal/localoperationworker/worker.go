// Package localoperationworker executes Control Plane Workspace Operation
// Outbox events against a host-local Resource Controller over a private Unix
// socket and applies the returned terminal result to the Control Plane store.
package localoperationworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/store/outbox"
)

const maxResultBytes = 64 << 10

type Store interface {
	ClaimWorkspaceOperations(context.Context, int, time.Duration, time.Time) ([]outbox.Event, error)
	MarkPublished(context.Context, outbox.Event, time.Time) error
	ReleaseAfterFailure(context.Context, outbox.Event, time.Time) error
}

type ResultStore interface {
	Apply(context.Context, resourcecontroller.Result) (bool, error)
}

type Config struct {
	BatchSize     int
	LeaseDuration time.Duration
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	Now           func() time.Time
}

type Worker struct {
	store  Store
	result ResultStore
	client *http.Client
	config Config
}

type Stats struct {
	Claimed int
	Applied int
	Failed  int
}

func New(store Store, result ResultStore, client *http.Client, config Config) (*Worker, error) {
	if store == nil || result == nil || client == nil {
		return nil, errors.New("local Outbox store, Operation result store, and Controller client are required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 10
	}
	if config.BatchSize < 1 || config.BatchSize > 100 {
		return nil, errors.New("local Outbox batch size must be 1-100")
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 20 * time.Minute
	}
	if config.LeaseDuration < time.Second {
		return nil, errors.New("local Outbox lease must be at least one second")
	}
	if config.BaseBackoff == 0 {
		config.BaseBackoff = time.Second
	}
	if config.MaxBackoff == 0 {
		config.MaxBackoff = time.Minute
	}
	if config.BaseBackoff < 0 || config.MaxBackoff < config.BaseBackoff {
		return nil, errors.New("local Outbox retry backoff is invalid")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{store: store, result: result, client: client, config: config}, nil
}

// RunOnce claims a batch and acknowledges each event only after the runtime
// result has committed. A crash after execution is safe: the stable Operation
// ID is idempotent in LocalMicroVM and result application is compare-and-set.
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	now := w.config.Now().UTC()
	events, err := w.store.ClaimWorkspaceOperations(ctx, w.config.BatchSize, w.config.LeaseDuration, now)
	if err != nil {
		return stats, fmt.Errorf("claim local Workspace Outbox events: %w", err)
	}
	stats.Claimed = len(events)
	var failures []error
	for _, event := range events {
		if event.AggregateType != "operation" || event.EventType != "operation.requested" {
			stats.Failed++
			failures = append(failures, w.release(ctx, event, now, errors.New("event is outside the local Workspace worker contract")))
			continue
		}
		result, err := w.execute(ctx, event.Payload)
		if err == nil {
			_, err = w.result.Apply(ctx, result)
		}
		if err != nil {
			stats.Failed++
			failures = append(failures, w.release(ctx, event, now, err))
			continue
		}
		if err := w.store.MarkPublished(ctx, event, w.config.Now().UTC()); err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("acknowledge local Workspace Operation %s: %w", event.AggregateID, err))
			continue
		}
		stats.Applied++
	}
	return stats, errors.Join(failures...)
}

func (w *Worker) execute(ctx context.Context, payload []byte) (resourcecontroller.Result, error) {
	if len(payload) == 0 || len(payload) > maxResultBytes {
		return resourcecontroller.Result{}, errors.New("local Workspace command payload is invalid")
	}
	var command resourcecontroller.Command
	commandDecoder := json.NewDecoder(bytes.NewReader(payload))
	commandDecoder.DisallowUnknownFields()
	if err := commandDecoder.Decode(&command); err != nil {
		return resourcecontroller.Result{}, errors.New("decode local Workspace command")
	}
	if err := commandDecoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || command.OperationID == "" || command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" || command.Type == "" {
		return resourcecontroller.Result{}, errors.New("local Workspace command identity is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hako-local/v1/operations", bytes.NewReader(payload))
	if err != nil {
		return resourcecontroller.Result{}, errors.New("create local Workspace Controller request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := w.client.Do(request)
	if err != nil {
		return resourcecontroller.Result{}, errors.New("local Workspace Controller request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResultBytes+1))
	if err != nil || len(body) > maxResultBytes {
		return resourcecontroller.Result{}, errors.New("local Workspace Controller response is invalid")
	}
	if response.StatusCode != http.StatusOK {
		return resourcecontroller.Result{}, fmt.Errorf("local Workspace Controller returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result resourcecontroller.Result
	if err := decoder.Decode(&result); err != nil {
		return resourcecontroller.Result{}, errors.New("decode local Workspace Operation result")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return resourcecontroller.Result{}, errors.New("local Workspace Operation result must contain one JSON object")
	}
	if result.SchemaVersion != command.SchemaVersion || result.WorkspaceRevision != command.WorkspaceRevision || result.OperationID != command.OperationID || result.CorrelationID != command.CorrelationID || result.TenantID != command.TenantID || result.WorkspaceID != command.WorkspaceID || result.ResourcePlaneID != command.ResourcePlaneID || result.Type != command.Type {
		return resourcecontroller.Result{}, errors.New("local Workspace Operation result identity does not match its command")
	}
	return result, nil
}

func (w *Worker) release(ctx context.Context, event outbox.Event, now time.Time, cause error) error {
	delay := w.config.BaseBackoff
	for attempt := 1; attempt < event.Attempt && delay < w.config.MaxBackoff; attempt++ {
		if delay > w.config.MaxBackoff/2 {
			delay = w.config.MaxBackoff
			break
		}
		delay *= 2
	}
	if delay > w.config.MaxBackoff {
		delay = w.config.MaxBackoff
	}
	if err := w.store.ReleaseAfterFailure(ctx, event, now.Add(delay)); err != nil {
		return fmt.Errorf("local Workspace event %s failed (%v); release lease failed: %w", event.ID, cause, err)
	}
	return fmt.Errorf("local Workspace event %s failed: %w", event.ID, cause)
}
