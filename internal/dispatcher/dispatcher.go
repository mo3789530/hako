// Package dispatcher delivers committed Outbox events to Resource Plane
// queues. Delivery is at-least-once; consumers must deduplicate by event ID.
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type Store interface {
	Claim(context.Context, int, time.Duration, time.Time) ([]outbox.Event, error)
	ReadPendingMetrics(context.Context, time.Time) (outbox.PendingMetrics, error)
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
	// WorkloadCommandQueueURLs routes attempt dispatch/cancel events to the
	// dedicated Workload worker queue. There is deliberately no Workspace
	// command-queue fallback when this entry is absent.
	WorkloadCommandQueueURLs map[string]string
	// GitHubChecksQueueURL routes durable Workload Run Check updates. It is
	// optional until the trusted Checks result worker is deployed.
	GitHubChecksQueueURL string
	// WorkspaceServiceQueueURLs routes service reconciliation to a dedicated
	// queue. It remains optional until the Resource Plane executor is deployed.
	WorkspaceServiceQueueURLs map[string]string
	// PreviewRuntimeCommandQueueURLs routes Preview provision/cleanup messages.
	PreviewRuntimeCommandQueueURLs map[string]string
	Now                            func() time.Time
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
	SchemaVersion     int    `json:"schema_version"`
	WorkspaceRevision int64  `json:"workspace_revision,omitempty"`
	OperationID       string `json:"operation_id"`
	ResourcePlaneID   string `json:"resource_plane_id"`
	CorrelationID     string `json:"correlation_id,omitempty"`
}

type workloadScheduleEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	TenantID      string `json:"tenant_id"`
	WorkloadRunID string `json:"workload_run_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type githubCheckEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	TenantID      string `json:"tenant_id"`
	WorkloadRunID string `json:"workload_run_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
	PipelineName  string `json:"pipeline_name,omitempty"`
	Revision      int64  `json:"revision"`
}

var githubPipelineNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
var workloadCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type actionsRunnerCheckEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Revision      int64  `json:"revision"`
}

type workloadDispatchEnvelope struct {
	SchemaVersion    int    `json:"schema_version"`
	TenantID         string `json:"tenant_id"`
	WorkloadRunID    string `json:"workload_run_id"`
	AttemptID        string `json:"attempt_id"`
	Generation       int64  `json:"generation"`
	ResourcePlaneID  string `json:"resource_plane_id"`
	IdempotencyToken string `json:"idempotency_token"`
	CorrelationID    string `json:"correlation_id,omitempty"`
}

type workspaceServiceEnvelope struct {
	SchemaVersion   int             `json:"schema_version"`
	TenantID        string          `json:"tenant_id"`
	WorkspaceID     string          `json:"workspace_id"`
	ResourcePlaneID string          `json:"resource_plane_id"`
	ServiceID       string          `json:"service_id"`
	Revision        int64           `json:"revision"`
	DesiredState    string          `json:"desired_state"`
	CatalogVersion  string          `json:"catalog_version"`
	Config          json.RawMessage `json:"config"`
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
	workloadURLs := make(map[string]string, len(config.WorkloadCommandQueueURLs))
	for id, value := range config.WorkloadCommandQueueURLs {
		if id == "" || value == "" {
			return nil, errors.New("Workload Resource Plane IDs and command queue URLs must not be empty")
		}
		workloadURLs[id] = value
	}
	config.WorkloadCommandQueueURLs = workloadURLs
	serviceURLs := make(map[string]string, len(config.WorkspaceServiceQueueURLs))
	for id, value := range config.WorkspaceServiceQueueURLs {
		if id == "" || value == "" {
			return nil, errors.New("Workspace Service Resource Plane IDs and queue URLs must not be empty")
		}
		serviceURLs[id] = value
	}
	config.WorkspaceServiceQueueURLs = serviceURLs
	previewURLs := make(map[string]string, len(config.PreviewRuntimeCommandQueueURLs))
	for id, value := range config.PreviewRuntimeCommandQueueURLs {
		if id == "" || value == "" {
			return nil, errors.New("Preview runtime Resource Plane IDs and command queue URLs must not be empty")
		}
		previewURLs[id] = value
	}
	config.PreviewRuntimeCommandQueueURLs = previewURLs
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
	if event.AggregateType == previewprotocol.AggregateType &&
		(event.EventType == previewprotocol.EventType(previewprotocol.ActionProvision) || event.EventType == previewprotocol.EventType(previewprotocol.ActionCleanup)) {
		command, err := previewprotocol.Decode(event.Payload, event.EventType, event.AggregateID)
		if err != nil {
			return "", err
		}
		queueURL := d.config.PreviewRuntimeCommandQueueURLs[command.ResourcePlaneID]
		if queueURL == "" {
			return "", fmt.Errorf("no Preview runtime command queue configured for Resource Plane %q", command.ResourcePlaneID)
		}
		return queueURL, nil
	}
	if event.AggregateType == "workspace_service" && event.EventType == "workspace_service.reconcile_requested" {
		var command workspaceServiceEnvelope
		if err := json.Unmarshal(event.Payload, &command); err != nil {
			return "", fmt.Errorf("decode Workspace Service command: %w", err)
		}
		if command.SchemaVersion != 1 || command.TenantID == "" || command.WorkspaceID == "" ||
			command.ServiceID == "" || command.ServiceID != event.AggregateID || command.ResourcePlaneID == "" ||
			command.Revision < 1 || command.CatalogVersion == "" || len(command.Config) == 0 || !json.Valid(command.Config) ||
			(command.DesiredState != "running" && command.DesiredState != "stopped" && command.DesiredState != "deleted") {
			return "", errors.New("Workspace Service command has an unsupported or inconsistent envelope")
		}
		queueURL := d.config.WorkspaceServiceQueueURLs[command.ResourcePlaneID]
		if queueURL == "" {
			return "", fmt.Errorf("no Workspace Service queue configured for Resource Plane %q", command.ResourcePlaneID)
		}
		return queueURL, nil
	}
	if event.AggregateType == "workload_run" && event.EventType == "workload_run.schedule_requested" {
		var message workloadScheduleEnvelope
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			return "", fmt.Errorf("decode Workload schedule request: %w", err)
		}
		if message.SchemaVersion != 1 || message.TenantID == "" || message.WorkloadRunID == "" || message.WorkloadRunID != event.AggregateID ||
			message.CorrelationID != "" && !workloadCorrelationIDPattern.MatchString(message.CorrelationID) {
			return "", errors.New("Workload schedule request has an unsupported or inconsistent envelope")
		}
		if d.config.WorkloadSchedulerQueueURL == "" {
			return "", errors.New("Workload scheduler queue is not configured")
		}
		return d.config.WorkloadSchedulerQueueURL, nil
	}
	if event.AggregateType == "workload_run" && event.EventType == "workload_run.github_check_requested" {
		var message githubCheckEnvelope
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			return "", fmt.Errorf("decode GitHub Check request: %w", err)
		}
		if message.SchemaVersion != 1 || message.TenantID == "" || message.WorkloadRunID == "" ||
			message.WorkloadRunID != event.AggregateID || message.Revision < 1 ||
			message.CorrelationID != "" && !workloadCorrelationIDPattern.MatchString(message.CorrelationID) ||
			message.PipelineName != "" && !githubPipelineNamePattern.MatchString(message.PipelineName) {
			return "", errors.New("GitHub Check request has an unsupported or inconsistent envelope")
		}
		if d.config.GitHubChecksQueueURL == "" {
			return "", errors.New("GitHub Checks result queue is not configured")
		}
		return d.config.GitHubChecksQueueURL, nil
	}
	if event.AggregateType == "github_actions_runner_job" && event.EventType == "github_actions_runner_job.github_check_requested" {
		var message actionsRunnerCheckEnvelope
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			return "", fmt.Errorf("decode Actions Runner Check request: %w", err)
		}
		aggregateID := fmt.Sprintf("%s:%d:%d", message.IntegrationID, message.WorkflowJobID, message.RunAttempt)
		if message.SchemaVersion != 1 || message.IntegrationID == "" || message.WorkflowJobID <= 0 || message.RunAttempt <= 0 ||
			message.Revision < 1 || message.CorrelationID != "" && !workloadCorrelationIDPattern.MatchString(message.CorrelationID) || aggregateID != event.AggregateID {
			return "", errors.New("Actions Runner Check request has an unsupported or inconsistent envelope")
		}
		if d.config.GitHubChecksQueueURL == "" {
			return "", errors.New("GitHub Checks result queue is not configured")
		}
		return d.config.GitHubChecksQueueURL, nil
	}
	if event.AggregateType == "workload_attempt" && (event.EventType == "workload_attempt.dispatch" || event.EventType == "workload_attempt.cancel") {
		var command workloadDispatchEnvelope
		if err := json.Unmarshal(event.Payload, &command); err != nil {
			return "", fmt.Errorf("decode Workload dispatch command: %w", err)
		}
		if command.SchemaVersion != 1 || command.TenantID == "" || command.WorkloadRunID == "" ||
			command.AttemptID == "" || command.AttemptID != event.AggregateID || command.Generation < 1 ||
			command.ResourcePlaneID == "" || command.IdempotencyToken == "" ||
			command.CorrelationID != "" && !workloadCorrelationIDPattern.MatchString(command.CorrelationID) {
			return "", errors.New("Workload dispatch command has an unsupported or inconsistent envelope")
		}
		queueURL := d.config.WorkloadCommandQueueURLs[command.ResourcePlaneID]
		if queueURL == "" {
			return "", fmt.Errorf("no Workload command queue configured for Resource Plane %q", command.ResourcePlaneID)
		}
		return queueURL, nil
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
	validVersion := command.SchemaVersion == 1 && command.WorkspaceRevision == 0 || command.SchemaVersion == 2 && command.WorkspaceRevision > 0
	if !validVersion || command.OperationID == "" || command.OperationID != event.AggregateID || command.ResourcePlaneID == "" ||
		command.CorrelationID != "" && !workloadCorrelationIDPattern.MatchString(command.CorrelationID) {
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

func (s StoreAdapter) ReadPendingMetrics(ctx context.Context, now time.Time) (outbox.PendingMetrics, error) {
	return outbox.ReadPendingMetrics(ctx, s.Pool, s.Policy, now)
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
