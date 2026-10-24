// Package githubchecks delivers durable Workload Run status to GitHub Checks.
// It never executes or retries the underlying Job.
package githubchecks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/workloads"
)

const maxDeliveryMessageBytes = 16 << 10
const maxRetryVisibility = 12 * time.Hour

type Delivery struct {
	Body          []byte
	ReceiptHandle string
}

type Queue interface {
	Receive(context.Context, int, time.Duration) ([]Delivery, error)
	ChangeVisibility(context.Context, string, time.Duration) error
	Delete(context.Context, string) error
}

type DesiredCheck struct {
	Check workloads.GitHubCheck
	Run   domain.WorkloadRun
}

type Store interface {
	Load(context.Context, domain.TenantID, domain.WorkloadRunID, string) (DesiredCheck, error)
	SaveID(context.Context, domain.TenantID, domain.WorkloadRunID, string, string, int64, time.Time) error
	LoadActionsRunner(context.Context, string, int64, int64) (githubwebhook.ActionsRunnerCheck, error)
	SaveActionsRunnerID(context.Context, githubwebhook.ActionsRunnerCheck, int64, time.Time) error
}

type CredentialProvider interface {
	ChecksRunToken(context.Context, domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error)
	ActionsRunnerChecksToken(context.Context, string, int64, int64) (githubapp.CheckToken, githubapp.RepositoryAccess, error)
}

type Config struct {
	BatchSize int
	WaitTime  time.Duration
	Now       func() time.Time
}

type Consumer struct {
	queue  Queue
	store  Store
	creds  CredentialProvider
	api    *githubapp.ChecksClient
	config Config
}

type Stats struct {
	Received int
	Applied  int
	Failed   int
	Deferred int
}

type request struct {
	SchemaVersion int    `json:"schema_version"`
	TenantID      string `json:"tenant_id"`
	WorkloadRunID string `json:"workload_run_id"`
	PipelineName  string `json:"pipeline_name,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	IntegrationID string `json:"integration_id,omitempty"`
	WorkflowJobID int64  `json:"workflow_job_id,omitempty"`
	RunAttempt    int64  `json:"run_attempt,omitempty"`
	Revision      int64  `json:"revision"`
}

func New(queue Queue, store Store, credentials CredentialProvider, api *githubapp.ChecksClient, config Config) (*Consumer, error) {
	if queue == nil || store == nil || credentials == nil || api == nil {
		return nil, errors.New("GitHub Check queue, store, credentials and API client are required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 1
	}
	if config.BatchSize < 1 || config.BatchSize > 10 {
		return nil, errors.New("GitHub Check queue batch size must be 1-10")
	}
	if config.WaitTime == 0 {
		config.WaitTime = 20 * time.Second
	}
	if config.WaitTime < 0 || config.WaitTime > 20*time.Second {
		return nil, errors.New("GitHub Check queue wait must be 0-20 seconds")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Consumer{queue: queue, store: store, creds: credentials, api: api, config: config}, nil
}

// RunOnce acknowledges only results that are durably reflected in DSQL. Any
// API/credential/store failure leaves the SQS message for redelivery; this
// retries Check delivery independently and never re-executes the Job.
func (c *Consumer) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	deliveries, err := c.queue.Receive(ctx, c.config.BatchSize, c.config.WaitTime)
	if err != nil {
		return stats, fmt.Errorf("receive GitHub Check requests: %w", err)
	}
	stats.Received = len(deliveries)
	var failures []error
	for _, delivery := range deliveries {
		if delivery.ReceiptHandle == "" {
			stats.Failed++
			failures = append(failures, errors.New("GitHub Check delivery has no receipt handle"))
			continue
		}
		request, err := decode(delivery.Body)
		if err == nil {
			err = c.deliver(ctx, request)
		}
		if err != nil {
			stats.Failed++
			wrapped := fmt.Errorf("deliver GitHub Check: %w", err)
			if delay, retryAt := retryVisibility(err, c.config.Now().UTC()); retryAt {
				if visibilityErr := c.queue.ChangeVisibility(ctx, delivery.ReceiptHandle, delay); visibilityErr != nil {
					failures = append(failures, errors.Join(wrapped, fmt.Errorf("defer GitHub Check delivery: %w", visibilityErr)))
				} else {
					stats.Deferred++
					failures = append(failures, wrapped)
				}
			} else {
				failures = append(failures, wrapped)
			}
			continue
		}
		if err := c.queue.Delete(ctx, delivery.ReceiptHandle); err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("acknowledge durable GitHub Check delivery: %w", err))
			continue
		}
		stats.Applied++
	}
	return stats, errors.Join(failures...)
}

// retryVisibility applies only a validated upstream RetryAt hint. Other
// transient failures retain the queue's normal visibility/redrive behavior.
func retryVisibility(err error, now time.Time) (time.Duration, bool) {
	var credentialErr *githubapp.CredentialError
	if !errors.As(err, &credentialErr) || !credentialErr.Retryable || credentialErr.RetryAt.IsZero() || now.IsZero() {
		return 0, false
	}
	delay := credentialErr.RetryAt.Sub(now)
	if delay < time.Second {
		delay = time.Second
	}
	if delay > maxRetryVisibility {
		delay = maxRetryVisibility
	}
	if remainder := delay % time.Second; remainder != 0 {
		delay += time.Second - remainder
	}
	return delay, true
}

func (c *Consumer) deliver(ctx context.Context, message request) error {
	if message.SchemaVersion != 1 {
		return errors.New("GitHub Check delivery envelope is incomplete or unsupported")
	}
	if message.TenantID == "" && message.WorkloadRunID == "" {
		return c.deliverActionsRunner(ctx, message)
	}
	if message.TenantID == "" || message.WorkloadRunID == "" || message.Revision < 1 || message.IntegrationID != "" || message.WorkflowJobID != 0 || message.RunAttempt != 0 ||
		message.PipelineName != "" && !githubPipelineNamePattern.MatchString(message.PipelineName) ||
		message.CorrelationID != "" && !githubCheckCorrelationIDPattern.MatchString(message.CorrelationID) {
		return errors.New("GitHub Check delivery envelope is incomplete or ambiguous")
	}
	desired, err := c.store.Load(ctx, domain.TenantID(message.TenantID), domain.WorkloadRunID(message.WorkloadRunID), message.PipelineName)
	if err != nil {
		return fmt.Errorf("load durable Check state: %w", err)
	}
	check, run := desired.Check, desired.Run
	expectedName := "hako/" + string(run.Kind)
	if message.PipelineName != "" {
		expectedName += "/" + message.PipelineName
	}
	if message.Revision > check.Revision || check.PipelineName != message.PipelineName || check.TenantID != run.TenantID || check.WorkloadRunID != run.ID ||
		check.TenantID != domain.TenantID(message.TenantID) || check.WorkloadRunID != domain.WorkloadRunID(message.WorkloadRunID) ||
		message.CorrelationID != run.CorrelationID ||
		check.GitHubInstallationID != run.GitHubInstallation || check.GitHubRepositoryID != run.GitHubRepository ||
		check.CommitSHA != run.CommitSHA || check.ExternalID != string(run.ID) || check.Name != expectedName || check.Revision < 1 {
		return errors.New("durable GitHub Check identity does not match its Workload Run")
	}
	token, repository, err := c.creds.ChecksRunToken(ctx, run)
	if err != nil {
		return fmt.Errorf("resolve Run-pinned GitHub Checks credential: %w", err)
	}
	checkID := check.CheckRunID
	var completedAt *time.Time
	if check.CompletedAt != nil {
		completed := check.CompletedAt.UTC()
		completedAt = &completed
	}
	result, err := c.api.EnsureCheckRun(ctx, token, repository, checkID, githubapp.CheckRunRequest{
		Name: check.Name, ExternalID: check.ExternalID, HeadSHA: check.CommitSHA,
		Status: githubapp.CheckRunStatus(check.Status), Conclusion: githubapp.CheckRunConclusion(check.Conclusion),
		StartedAt: check.StartedAt.UTC(), CompletedAt: completedAt,
		Output: githubapp.CheckOutput{Title: check.OutputTitle, Summary: check.OutputSummary},
	})
	if err != nil {
		return fmt.Errorf("publish GitHub Check: %w", err)
	}
	// EnsureCheckRun returns the same ID on create/recovery; persist it only
	// after the identity-checked API response succeeds.
	if err := c.store.SaveID(ctx, check.TenantID, check.WorkloadRunID, check.PipelineName, check.CommitSHA, result.ID, c.config.Now().UTC()); err != nil {
		return fmt.Errorf("persist GitHub Check Run ID: %w", err)
	}
	slog.InfoContext(ctx, "delivered Hako GitHub Check", "correlation_id", run.CorrelationID,
		"tenant_id", run.TenantID, "workload_run_id", run.ID, "pipeline", check.PipelineName, "revision", check.Revision)
	return nil
}

var githubPipelineNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
var githubCheckCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (c *Consumer) deliverActionsRunner(ctx context.Context, message request) error {
	if message.SchemaVersion != 1 || message.IntegrationID == "" || len(message.IntegrationID) > 128 ||
		message.WorkflowJobID <= 0 || message.RunAttempt <= 0 || message.Revision < 1 || message.TenantID != "" || message.WorkloadRunID != "" ||
		message.PipelineName != "" || message.CorrelationID != "" && !githubCheckCorrelationIDPattern.MatchString(message.CorrelationID) {
		return errors.New("Actions Runner Check delivery envelope is incomplete or unsupported")
	}
	check, err := c.store.LoadActionsRunner(ctx, message.IntegrationID, message.WorkflowJobID, message.RunAttempt)
	if err != nil {
		return fmt.Errorf("load durable Actions Runner Check state: %w", err)
	}
	if check.IntegrationID != message.IntegrationID || check.WorkflowJobID != message.WorkflowJobID || check.RunAttempt != message.RunAttempt ||
		message.Revision > check.Revision || message.CorrelationID != check.CorrelationID || check.TenantID == "" || check.Installation <= 0 || check.Repository <= 0 ||
		check.WorkflowRunID <= 0 || check.CommitSHA == "" || check.Name == "" || check.ExternalID == "" || check.Revision < 1 {
		return errors.New("durable Actions Runner Check identity does not match its workflow job")
	}
	token, repository, err := c.creds.ActionsRunnerChecksToken(ctx, check.IntegrationID, check.WorkflowJobID, check.RunAttempt)
	if err != nil {
		return fmt.Errorf("resolve job-pinned GitHub Checks credential: %w", err)
	}
	var completedAt *time.Time
	if check.CompletedAt != nil {
		completed := check.CompletedAt.UTC()
		completedAt = &completed
	}
	result, err := c.api.EnsureCheckRun(ctx, token, repository, check.CheckRunID, githubapp.CheckRunRequest{
		Name: check.Name, ExternalID: check.ExternalID, HeadSHA: check.CommitSHA, Status: githubapp.CheckRunStatus(check.Status),
		Conclusion: githubapp.CheckRunConclusion(check.Conclusion), StartedAt: check.StartedAt.UTC(), CompletedAt: completedAt,
		Output: githubapp.CheckOutput{Title: check.OutputTitle, Summary: check.OutputSummary},
	})
	if err != nil {
		return fmt.Errorf("publish Actions Runner GitHub Check: %w", err)
	}
	if err := c.store.SaveActionsRunnerID(ctx, check, result.ID, c.config.Now().UTC()); err != nil {
		return fmt.Errorf("persist Actions Runner GitHub Check ID: %w", err)
	}
	slog.InfoContext(ctx, "delivered Hako Actions Runner GitHub Check", "correlation_id", check.CorrelationID,
		"tenant_id", check.TenantID, "integration_id", check.IntegrationID, "workflow_job_id", check.WorkflowJobID,
		"run_attempt", check.RunAttempt, "revision", check.Revision)
	return nil
}

func decode(payload []byte) (request, error) {
	if len(payload) == 0 || len(payload) > maxDeliveryMessageBytes {
		return request{}, errors.New("GitHub Check delivery must be 1-16 KiB")
	}
	var message request
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return request{}, errors.New("GitHub Check delivery is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return request{}, errors.New("GitHub Check delivery must contain exactly one JSON object")
	}
	return message, nil
}
