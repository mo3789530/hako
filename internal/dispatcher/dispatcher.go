// Package dispatcher delivers committed Outbox events to Resource Plane
// queues. Delivery is at-least-once; consumers must deduplicate by event ID.
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type Store interface {
	Claim(context.Context, int, time.Duration, time.Time) ([]outbox.Event, error)
	MarkPublished(context.Context, outbox.Event, time.Time) error
	ReleaseAfterFailure(context.Context, outbox.Event, time.Time) error
}

type Publisher interface {
	Publish(context.Context, string, outbox.Event) error
}

type Config struct {
	BatchSize     int
	LeaseDuration time.Duration
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	// WorkloadSchedulerQueueURL routes workload_run.schedule_requested events.
	// It is optional until the Workload scheduler queue is provisioned.
	WorkloadSchedulerQueueURL string
	Now                       func() time.Time
}

type Dispatcher struct {
	store   Store
	publish Publisher
	queues  map[string]string
	config  Config
}

type Stats struct {
	Claimed   int
	Published int
	Failed    int
}

type commandEnvelope struct {
	SchemaVersion   int    `json:"schema_version"`
	OperationID     string `json:"operation_id"`
	ResourcePlaneID string `json:"resource_plane_id"`
}

type workloadScheduleEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	WorkloadRunID string `json:"workload_run_id"`
}

func New(store Store, publisher Publisher, queueURLs map[string]string, config Config) (*Dispatcher, error) {
	if store == nil || publisher == nil {
		return nil, errors.New("outbox store and publisher are required")
	}
	if len(queueURLs) == 0 {
		return nil, errors.New("at least one Resource Plane queue is required")
	}
	for resourcePlaneID, queueURL := range queueURLs {
		if resourcePlaneID == "" || queueURL == "" {
			return nil, errors.New("Resource Plane IDs and queue URLs must not be empty")
		}
	}
	if config.BatchSize == 0 {
		config.BatchSize = 10
	}
	if config.BatchSize < 1 || config.BatchSize > 1000 {
		return nil, errors.New("dispatcher batch size must be 1-1000")
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 30 * time.Second
	}
	if config.LeaseDuration < time.Second {
		return nil, errors.New("dispatcher lease duration must be at least one second")
	}
	if config.BaseBackoff == 0 {
		config.BaseBackoff = time.Second
	}
	if config.MaxBackoff == 0 {
		config.MaxBackoff = time.Minute
	}
	if config.BaseBackoff < 0 || config.MaxBackoff < config.BaseBackoff {
		return nil, errors.New("dispatcher retry backoff is invalid")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	copyURLs := make(map[string]string, len(queueURLs))
	for id, value := range queueURLs {
		copyURLs[id] = value
	}
	return &Dispatcher{store: store, publish: publisher, queues: copyURLs, config: config}, nil
}

// RunOnce claims a batch, publishes each command, and acknowledges successful
// sends. A send followed by an acknowledgement failure can be delivered again.
func (d *Dispatcher) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	now := d.config.Now().UTC()
	events, err := d.store.Claim(ctx, d.config.BatchSize, d.config.LeaseDuration, now)
	if err != nil {
		return stats, fmt.Errorf("claim Outbox events: %w", err)
	}
	stats.Claimed = len(events)
	var failures []error
	for _, event := range events {
		queueURL, err := d.route(event)
		if err == nil {
			err = d.publish.Publish(ctx, queueURL, event)
		}
		if err != nil {
			stats.Failed++
			retryAt := now.Add(d.backoff(event.Attempt))
			if releaseErr := d.store.ReleaseAfterFailure(ctx, event, retryAt); releaseErr != nil {
				failures = append(failures, fmt.Errorf("publish Outbox event %s: %v; release lease: %w", event.ID, err, releaseErr))
			} else {
				failures = append(failures, fmt.Errorf("publish Outbox event %s: %w", event.ID, err))
			}
			continue
		}
		if err := d.store.MarkPublished(ctx, event, d.config.Now().UTC()); err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("acknowledge published Outbox event %s: %w", event.ID, err))
			continue
		}
		stats.Published++
	}
	return stats, errors.Join(failures...)
}

func (d *Dispatcher) route(event outbox.Event) (string, error) {
	if event.AggregateType == "workload_run" && event.EventType == "workload_run.schedule_requested" {
		var message workloadScheduleEnvelope
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			return "", fmt.Errorf("decode Workload schedule request: %w", err)
		}
		if message.SchemaVersion != 1 || message.WorkloadRunID == "" || message.WorkloadRunID != event.AggregateID {
			return "", errors.New("Workload schedule request has an unsupported or inconsistent envelope")
		}
		if d.config.WorkloadSchedulerQueueURL == "" {
			return "", errors.New("Workload scheduler queue is not configured")
		}
		return d.config.WorkloadSchedulerQueueURL, nil
	}
	resourcePlaneID, err := validateCommand(event)
	if err != nil {
		return "", err
	}
	queueURL := d.queues[resourcePlaneID]
	if queueURL == "" {
		return "", fmt.Errorf("no Resource Plane queue configured for %q", resourcePlaneID)
	}
	return queueURL, nil
}

func validateCommand(event outbox.Event) (string, error) {
	if event.AggregateType != "operation" || event.EventType != "operation.requested" {
		return "", fmt.Errorf("unsupported Outbox event type %q/%q", event.AggregateType, event.EventType)
	}
	var command commandEnvelope
	if err := json.Unmarshal(event.Payload, &command); err != nil {
		return "", fmt.Errorf("decode Outbox command: %w", err)
	}
	if command.SchemaVersion != 1 || command.OperationID == "" || command.OperationID != event.AggregateID || command.ResourcePlaneID == "" {
		return "", errors.New("Outbox command has an unsupported or inconsistent envelope")
	}
	return command.ResourcePlaneID, nil
}

func (d *Dispatcher) backoff(attempt int) time.Duration {
	delay := d.config.BaseBackoff
	for i := 1; i < attempt && delay < d.config.MaxBackoff; i++ {
		if delay > d.config.MaxBackoff/2 {
			return d.config.MaxBackoff
		}
		delay *= 2
	}
	if delay > d.config.MaxBackoff {
		return d.config.MaxBackoff
	}
	return delay
}

// StoreAdapter binds the generic dispatcher contract to the transactional
// Outbox repository and the application's OCC retry policy.
type StoreAdapter struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

func (s StoreAdapter) Claim(ctx context.Context, limit int, lease time.Duration, now time.Time) ([]outbox.Event, error) {
	return outbox.Claim(ctx, s.Pool, s.Policy, limit, lease, now)
}

func (s StoreAdapter) MarkPublished(ctx context.Context, event outbox.Event, now time.Time) error {
	return outbox.MarkPublished(ctx, s.Pool, s.Policy, event, now)
}

func (s StoreAdapter) ReleaseAfterFailure(ctx context.Context, event outbox.Event, retryAt time.Time) error {
	return outbox.ReleaseAfterFailure(ctx, s.Pool, s.Policy, event, retryAt)
}
