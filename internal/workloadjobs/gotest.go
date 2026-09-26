// Package workloadjobs connects the local Fake Runtime to persisted Workload
// Run lifecycle and artifact metadata.
package workloadjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var (
	ErrNotRunnable    = errors.New("Workload Run is not a pending Go test Job")
	ErrCommitMismatch = errors.New("Job commit does not match persisted Workload Run")
)

// GoTestExecutor is the isolated executor boundary; implementations must not
// run repository code on the worker host.
type GoTestExecutor interface {
	RunGoTest(context.Context, fake.JobSpec) (fake.JobResult, error)
}

// ArtifactWriter stores already-redacted log bytes and returns an opaque,
// unsigned URI suitable for workload_artifacts.storage_ref.
type ArtifactWriter interface {
	PutRedactedLogs(context.Context, domain.TenantID, domain.WorkloadRunID, []byte, string) (string, error)
}

type Service struct {
	Pool      transaction.Beginner
	Policy    transaction.Policy
	Executor  GoTestExecutor
	Artifacts ArtifactWriter
	Now       func() time.Time
}

// ExecuteGoTest transitions a pending Job through provisioning/running, runs
// the verified immutable commit, writes redacted log bytes to the artifact
// store, then atomically records artifact metadata and the terminal state.
func (s *Service) ExecuteGoTest(ctx context.Context, spec fake.JobSpec) (domain.WorkloadRun, error) {
	if s == nil || ctx == nil || s.Pool == nil || s.Executor == nil || s.Artifacts == nil {
		return domain.WorkloadRun{}, errors.New("context, database, executor, and artifact writer are required")
	}
	if spec.TenantID == "" || spec.RunID == "" || spec.RepositoryPath == "" {
		return domain.WorkloadRun{}, errors.New("Tenant, Workload Run, and checked-out Repository are required")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	startedAt := now().UTC()
	run, err := transaction.Within(ctx, s.Pool, s.Policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		current, err := workloads.Get(ctx, tx, spec.TenantID, spec.RunID)
		if err != nil {
			return domain.WorkloadRun{}, err
		}
		if current.Kind != domain.WorkloadJob || current.State != domain.WorkloadPending ||
			current.DesiredState != domain.WorkloadDesiredRunning || current.CommitSHA == "" {
			return domain.WorkloadRun{}, ErrNotRunnable
		}
		if current.CommitSHA != spec.CommitSHA {
			return domain.WorkloadRun{}, ErrCommitMismatch
		}
		current, err = workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: current.TenantID,
			RunID: current.ID, ExpectedRevision: current.Revision, NextState: domain.WorkloadProvisioning, OccurredAt: startedAt})
		if err != nil {
			return domain.WorkloadRun{}, err
		}
		return workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: current.TenantID,
			RunID: current.ID, ExpectedRevision: current.Revision, NextState: domain.WorkloadRunning, OccurredAt: startedAt})
	})
	if err != nil {
		return domain.WorkloadRun{}, fmt.Errorf("start Go test Workload Run: %w", err)
	}

	remaining := run.TimeoutAt.Sub(startedAt)
	if remaining <= 0 {
		return s.finish(ctx, run, nil, domain.WorkloadTimedOut, "job_timeout", startedAt, now())
	}
	if spec.Timeout <= 0 || spec.Timeout > remaining {
		spec.Timeout = remaining
	}
	result, executeErr := s.Executor.RunGoTest(ctx, spec)
	if executeErr != nil {
		finished, finishErr := s.finish(ctx, run, nil, domain.WorkloadFailed, "executor_error", startedAt, now())
		if finishErr != nil {
			return domain.WorkloadRun{}, errors.Join(executeErr, finishErr)
		}
		return finished, fmt.Errorf("execute isolated Go test Job: %w", executeErr)
	}
	if result.TenantID != run.TenantID || result.RunID != run.ID || result.CommitSHA != run.CommitSHA {
		return s.finish(ctx, run, nil, domain.WorkloadFailed, "executor_result_mismatch", startedAt, now())
	}
	digest := sha256.Sum256(result.RedactedLogs)
	if result.LogsSHA256 != hex.EncodeToString(digest[:]) {
		return s.finish(ctx, run, nil, domain.WorkloadFailed, "invalid_log_digest", startedAt, now())
	}
	if result.TimedOut {
		return s.finish(ctx, run, &result, domain.WorkloadTimedOut, "job_timeout", startedAt, now())
	}
	if result.ExitCode != 0 {
		return s.finish(ctx, run, &result, domain.WorkloadFailed, "test_failed", startedAt, now())
	}
	return s.finish(ctx, run, &result, domain.WorkloadSucceeded, "", startedAt, now())
}

func (s *Service) finish(ctx context.Context, started domain.WorkloadRun, result *fake.JobResult,
	next domain.WorkloadRunState, errorCode string, startedAt, completedAt time.Time) (domain.WorkloadRun, error) {
	var storageRef string
	if result != nil {
		var err error
		storageRef, err = s.Artifacts.PutRedactedLogs(ctx, started.TenantID, started.ID, result.RedactedLogs, result.LogsSHA256)
		if err != nil {
			finished, finishErr := s.finish(ctx, started, nil, domain.WorkloadFailed, "artifact_storage_error", startedAt, completedAt)
			if finishErr != nil {
				return domain.WorkloadRun{}, errors.Join(err, finishErr)
			}
			return finished, fmt.Errorf("store redacted Workload logs: %w", err)
		}
	}
	var artifactID string
	if result != nil {
		var err error
		artifactID, err = idgen.New("artifact_")
		if err != nil {
			return domain.WorkloadRun{}, fmt.Errorf("generate Workload log artifact ID: %w", err)
		}
	}
	return transaction.Within(ctx, s.Pool, s.Policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		current, err := workloads.Get(ctx, tx, started.TenantID, started.ID)
		if err != nil {
			return domain.WorkloadRun{}, err
		}
		if current.State != domain.WorkloadRunning || current.Revision != started.Revision {
			return domain.WorkloadRun{}, workloads.ErrTransitionConflict
		}
		if result != nil {
			if err := workloads.AddArtifact(ctx, tx, domain.WorkloadArtifact{ID: artifactID,
				TenantID: started.TenantID, RunID: started.ID, Kind: "logs", StorageRef: storageRef,
				SizeBytes: int64(len(result.RedactedLogs)), SHA256: result.LogsSHA256, Redacted: true, CreatedAt: completedAt.UTC()}); err != nil {
				return domain.WorkloadRun{}, err
			}
		}
		finished, err := workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: started.TenantID,
			RunID: started.ID, ExpectedRevision: current.Revision, NextState: next,
			ErrorCode: errorCode, OccurredAt: completedAt.UTC()})
		if err != nil {
			return domain.WorkloadRun{}, err
		}
		if finished.StartedAt == nil || finished.StartedAt.Before(startedAt) {
			return domain.WorkloadRun{}, errors.New("Workload Run start time was not persisted")
		}
		return finished, nil
	})
}
