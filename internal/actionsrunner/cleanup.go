// Package actionsrunner implements the trusted lifecycle around ephemeral
// GitHub Actions runner jobs. Resource Plane workers are configured for one
// Plane and cannot terminate a provider resource owned by another Plane.
package actionsrunner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrInvalidCleanupConfig = errors.New("invalid Actions runner cleanup configuration")
	ErrCleanupLeaseLost     = errors.New("Actions runner cleanup lease was lost")
	ErrCleanupDeferred      = errors.New("Actions runner cleanup was deferred")
)

type CleanupQueue interface {
	Claim(context.Context, string, string, time.Time, time.Duration) (githubwebhook.ActionsRunnerCleanupJob, bool, error)
	Complete(context.Context, githubwebhook.ActionsRunnerCleanupJob, time.Time) (bool, error)
	Retry(context.Context, githubwebhook.ActionsRunnerCleanupJob, time.Time, time.Time, string) (bool, error)
}

type RunnerDeregistrator interface {
	Deregister(context.Context, githubwebhook.ActionsRunnerCleanupJob) error
}

type LambdaMicroVMTerminator interface {
	Terminate(context.Context, string, string) error
}

type CleanupWorkerConfig struct {
	ResourcePlaneID  string
	WorkerID         string
	LeaseDuration    time.Duration
	OperationTimeout time.Duration
}

type CleanupWorker struct {
	config        CleanupWorkerConfig
	queue         CleanupQueue
	deregistrator RunnerDeregistrator
	terminator    LambdaMicroVMTerminator
	now           func() time.Time
}

func NewCleanupWorker(config CleanupWorkerConfig, queue CleanupQueue, deregistrator RunnerDeregistrator, terminator LambdaMicroVMTerminator, now func() time.Time) (*CleanupWorker, error) {
	if !validResourcePlaneID(config.ResourcePlaneID) || !validWorkerID(config.WorkerID) ||
		config.LeaseDuration < 5*time.Second || config.LeaseDuration > 10*time.Minute ||
		config.OperationTimeout <= 0 || config.OperationTimeout > 5*time.Minute ||
		config.OperationTimeout+5*time.Second > config.LeaseDuration ||
		queue == nil || deregistrator == nil || terminator == nil {
		return nil, ErrInvalidCleanupConfig
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &CleanupWorker{config: config, queue: queue, deregistrator: deregistrator, terminator: terminator, now: now}, nil
}

// ProcessOne leases one cleanup request, terminates the pinned MicroVM before
// deregistering its repository runner, then acknowledges the fenced lease.
// External operations must be idempotent: a crash after success but before the
// database acknowledgement causes the same cleanup to run again.
func (w *CleanupWorker) ProcessOne(ctx context.Context) (bool, error) {
	if w == nil || ctx == nil {
		return false, ErrInvalidCleanupConfig
	}
	now := w.now().UTC()
	// A stable worker name is not a lease identity: the same process or a
	// restarted worker can recover its previous claim while that call unwinds.
	prefix := w.config.WorkerID[:min(len(w.config.WorkerID), 64)] + "_"
	claimID, err := idgen.New(prefix)
	if err != nil {
		return false, fmt.Errorf("generate Actions runner cleanup claim: %w", err)
	}
	job, ok, err := w.queue.Claim(ctx, w.config.ResourcePlaneID, claimID, now, w.config.LeaseDuration)
	if err != nil || !ok {
		return ok, err
	}
	// Acquiring a remote Broker lease can consume part of its lifetime.
	now = w.now().UTC()
	if job.ClaimID != claimID || job.AttemptCount < 1 || job.State != "claimed" || job.TenantID == "" ||
		job.IntegrationID == "" || job.WorkflowJobID <= 0 || job.RunAttempt <= 0 || job.Installation <= 0 || job.Repository <= 0 ||
		!job.LeaseExpires.After(now.Add(5*time.Second)) {
		return true, ErrCleanupLeaseLost
	}
	// Leave time to persist the acknowledgement/retry before another worker
	// can recover the lease, even when the queue returns a shorter lease.
	timeout := min(w.config.OperationTimeout, job.LeaseExpires.Sub(now)-5*time.Second)
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	errorCode := ""
	if job.LambdaMicroVMID != nil {
		if job.ResourcePlaneID == nil || *job.ResourcePlaneID != w.config.ResourcePlaneID {
			errorCode = "provider_unavailable"
		} else if err := w.terminator.Terminate(operationCtx, *job.ResourcePlaneID, *job.LambdaMicroVMID); err != nil {
			errorCode = "microvm_terminate_failed"
		}
	}
	if errorCode == "" && job.RunnerID != nil {
		if err := w.deregistrator.Deregister(operationCtx, job); err != nil {
			errorCode = "runner_deregister_failed"
		}
	}

	finishedAt := w.now().UTC()
	if errorCode != "" {
		retryAt := finishedAt.Add(cleanupRetryDelay(job.AttemptCount))
		updated, err := w.queue.Retry(ctx, job, finishedAt, retryAt, errorCode)
		if err != nil {
			return true, fmt.Errorf("persist Actions runner cleanup retry: %w", err)
		}
		if !updated {
			return true, ErrCleanupLeaseLost
		}
		return true, ErrCleanupDeferred
	}
	updated, err := w.queue.Complete(ctx, job, finishedAt)
	if err != nil {
		return true, fmt.Errorf("complete Actions runner cleanup: %w", err)
	}
	if !updated {
		return true, ErrCleanupLeaseLost
	}
	return true, nil
}

func cleanupRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	delay := 15 * time.Second * time.Duration(1<<shift)
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

type PostgresCleanupQueue struct {
	pool   transaction.Beginner
	policy transaction.Policy
}

func NewPostgresCleanupQueue(pool transaction.Beginner, policy transaction.Policy) (*PostgresCleanupQueue, error) {
	if pool == nil {
		return nil, ErrInvalidCleanupConfig
	}
	return &PostgresCleanupQueue{pool: pool, policy: policy}, nil
}

func (q *PostgresCleanupQueue) Claim(ctx context.Context, planeID, workerID string, now time.Time, lease time.Duration) (githubwebhook.ActionsRunnerCleanupJob, bool, error) {
	type claimResult struct {
		job githubwebhook.ActionsRunnerCleanupJob
		ok  bool
	}
	result, err := transaction.Within(ctx, q.pool, q.policy, func(ctx context.Context, tx pgx.Tx) (claimResult, error) {
		job, ok, err := githubwebhook.ClaimNextActionsRunnerCleanup(ctx, tx, planeID, workerID, now, lease)
		return claimResult{job: job, ok: ok}, err
	})
	return result.job, result.ok, err
}

func (q *PostgresCleanupQueue) Complete(ctx context.Context, job githubwebhook.ActionsRunnerCleanupJob, now time.Time) (bool, error) {
	return transaction.Within(ctx, q.pool, q.policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return githubwebhook.CompleteActionsRunnerCleanup(ctx, tx, job, now)
	})
}

func (q *PostgresCleanupQueue) Retry(ctx context.Context, job githubwebhook.ActionsRunnerCleanupJob, now, retryAt time.Time, code string) (bool, error) {
	return transaction.Within(ctx, q.pool, q.policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return githubwebhook.RetryActionsRunnerCleanup(ctx, tx, job, now, retryAt, code)
	})
}

func validResourcePlaneID(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	first := value[0]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func validWorkerID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
