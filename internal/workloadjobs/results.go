package workloadjobs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var (
	ErrWorkloadResultMismatch = errors.New("Workload result does not match its Run or attempt")
	ErrWorkloadResultInvalid  = errors.New("Workload result is invalid")
	resultErrorCodePattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// WorkloadResult is emitted by a trusted Resource Plane result channel. It
// contains metadata only; log bytes must already be redacted and uploaded.
type WorkloadResult struct {
	SchemaVersion   int                     `json:"schema_version"`
	TenantID        domain.TenantID         `json:"tenant_id"`
	WorkloadRunID   domain.WorkloadRunID    `json:"workload_run_id"`
	AttemptID       string                  `json:"attempt_id"`
	Generation      int64                   `json:"generation"`
	ResourcePlaneID domain.ResourcePlaneID  `json:"resource_plane_id"`
	CommitSHA       string                  `json:"commit_sha"`
	Status          domain.WorkloadRunState `json:"status"`
	ExitCode        int                     `json:"exit_code"`
	ErrorCode       string                  `json:"error_code,omitempty"`
	CompletedAt     time.Time               `json:"completed_at"`
	LogArtifact     *WorkloadLogArtifact    `json:"log_artifact,omitempty"`
}

type WorkloadLogArtifact struct {
	StorageRef string `json:"storage_ref"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	Redacted   bool   `json:"redacted"`
}

type ResultOutcome string

const (
	ResultApplied   ResultOutcome = "applied"
	ResultDuplicate ResultOutcome = "duplicate"
	ResultStale     ResultOutcome = "stale"
)

type ResultHandler struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
	Now    func() time.Time
}

// Handle applies a result only to the matching current attempt. Replayed
// terminal results are idempotent; results from older generations are stale
// and acknowledged without changing the Run.
func (h *ResultHandler) Handle(ctx context.Context, result WorkloadResult) (ResultOutcome, error) {
	if h == nil || h.Pool == nil || ctx == nil {
		return "", errors.New("context and Workload result database are required")
	}
	if err := validateWorkloadResult(result); err != nil {
		return "", err
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	completedAt := result.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = now().UTC()
	}
	artifactID := ""
	if result.LogArtifact != nil {
		var err error
		artifactID, err = idgen.New("artifact_")
		if err != nil {
			return "", fmt.Errorf("generate Workload artifact ID: %w", err)
		}
	}
	return transaction.Within(ctx, h.Pool, h.Policy, func(ctx context.Context, tx pgx.Tx) (ResultOutcome, error) {
		var attemptTenant domain.TenantID
		var attemptRun domain.WorkloadRunID
		var generation int64
		var planeID domain.ResourcePlaneID
		var attemptState string
		if err := tx.QueryRow(ctx, `SELECT tenant_id, workload_run_id, generation, resource_plane_id, state
			FROM workload_run_attempts WHERE id = $1`, result.AttemptID).Scan(&attemptTenant, &attemptRun, &generation, &planeID, &attemptState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrWorkloadResultMismatch
			}
			return "", fmt.Errorf("load Workload result attempt: %w", err)
		}
		if attemptTenant != result.TenantID || attemptRun != result.WorkloadRunID || planeID != result.ResourcePlaneID {
			return "", ErrWorkloadResultMismatch
		}
		if generation != result.Generation {
			return ResultStale, nil
		}
		var currentAttemptID string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT id, generation FROM workload_run_attempts
			WHERE tenant_id = $1 AND workload_run_id = $2 ORDER BY generation DESC LIMIT 1`,
			result.TenantID, result.WorkloadRunID).Scan(&currentAttemptID, &currentGeneration); err != nil {
			return "", fmt.Errorf("load current Workload attempt: %w", err)
		}
		if currentAttemptID != result.AttemptID || currentGeneration != result.Generation {
			return ResultStale, nil
		}
		run, err := workloads.Get(ctx, tx, result.TenantID, result.WorkloadRunID)
		if err != nil {
			return "", err
		}
		if run.ResourcePlaneID != result.ResourcePlaneID || run.CommitSHA != result.CommitSHA {
			return "", ErrWorkloadResultMismatch
		}
		if isTerminalWorkloadState(run.State) {
			return ResultDuplicate, nil
		}
		if run.State != domain.WorkloadProvisioning && run.State != domain.WorkloadRunning {
			return "", ErrWorkloadResultMismatch
		}
		if attemptState == string(domain.WorkloadSucceeded) || attemptState == string(domain.WorkloadFailed) ||
			attemptState == string(domain.WorkloadCancelled) || attemptState == string(domain.WorkloadTimedOut) {
			return ResultDuplicate, nil
		}
		if attemptState != "accepted" && attemptState != "running" && attemptState != "dispatch_pending" {
			return "", ErrWorkloadResultMismatch
		}

		next := result.Status
		errorCode := result.ErrorCode
		if run.DesiredState == domain.WorkloadDesiredCancelled {
			next, errorCode = domain.WorkloadCancelled, "cancelled"
		} else if !completedAt.Before(run.TimeoutAt) {
			next, errorCode = domain.WorkloadTimedOut, "job_timeout"
		}
		if run.State == domain.WorkloadProvisioning {
			run, err = workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID,
				RunID: run.ID, ExpectedRevision: run.Revision, NextState: domain.WorkloadRunning, OccurredAt: completedAt})
			if err != nil {
				return "", err
			}
		}
		if result.LogArtifact != nil {
			if err := workloads.AddArtifact(ctx, tx, domain.WorkloadArtifact{ID: artifactID, TenantID: run.TenantID,
				RunID: run.ID, Kind: "logs", StorageRef: result.LogArtifact.StorageRef,
				SizeBytes: result.LogArtifact.SizeBytes, SHA256: result.LogArtifact.SHA256,
				Redacted: result.LogArtifact.Redacted, CreatedAt: completedAt}); err != nil {
				return "", fmt.Errorf("record Workload log artifact: %w", err)
			}
		}
		if _, err := workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID,
			RunID: run.ID, ExpectedRevision: run.Revision, NextState: next, ErrorCode: errorCode, OccurredAt: completedAt}); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE workload_run_attempts SET state = $1, updated_at = $2
			WHERE id = $3 AND tenant_id = $4 AND workload_run_id = $5 AND generation = $6
			AND state IN ('dispatch_pending','accepted','running')`, string(next), completedAt,
			result.AttemptID, result.TenantID, result.WorkloadRunID, result.Generation); err != nil {
			return "", fmt.Errorf("finish Workload attempt: %w", err)
		}
		return ResultApplied, nil
	})
}

// ValidateResult checks metadata before a trusted RP publisher persists/sends it.
func ValidateResult(result WorkloadResult) error {
	return validateWorkloadResult(result)
}

func validateWorkloadResult(result WorkloadResult) error {
	if result.SchemaVersion != 1 || result.TenantID == "" || result.WorkloadRunID == "" ||
		result.AttemptID == "" || result.Generation < 1 || result.ResourcePlaneID == "" ||
		result.CompletedAt.IsZero() {
		return fmt.Errorf("%w: identity, version, generation, and completion time are required", ErrWorkloadResultInvalid)
	}
	if result.Status != domain.WorkloadSucceeded && result.Status != domain.WorkloadFailed &&
		result.Status != domain.WorkloadCancelled && result.Status != domain.WorkloadTimedOut {
		return fmt.Errorf("%w: status must be terminal", ErrWorkloadResultInvalid)
	}
	if result.Status == domain.WorkloadSucceeded && (result.ExitCode != 0 || result.ErrorCode != "") {
		return fmt.Errorf("%w: successful result must have exit code zero and no error code", ErrWorkloadResultInvalid)
	}
	if result.Status == domain.WorkloadFailed && result.ErrorCode == "" && result.ExitCode == 0 {
		return fmt.Errorf("%w: failed result requires a nonzero exit code or safe error code", ErrWorkloadResultInvalid)
	}
	if result.ErrorCode != "" && !resultErrorCodePattern.MatchString(result.ErrorCode) {
		return fmt.Errorf("%w: error code is not a safe identifier", ErrWorkloadResultInvalid)
	}
	if result.LogArtifact != nil {
		if result.LogArtifact.SizeBytes < 0 || result.LogArtifact.SizeBytes > 100<<20 ||
			len(result.LogArtifact.SHA256) != 64 || !result.LogArtifact.Redacted {
			return fmt.Errorf("%w: log artifact metadata must be bounded, SHA-256 identified, and redacted", ErrWorkloadResultInvalid)
		}
		if _, err := hex.DecodeString(result.LogArtifact.SHA256); err != nil {
			return fmt.Errorf("%w: log artifact SHA-256 is invalid", ErrWorkloadResultInvalid)
		}
	}
	return nil
}
