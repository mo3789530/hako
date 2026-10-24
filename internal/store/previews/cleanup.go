package previews

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

const (
	cleanupRetryBase = 5 * time.Second
	cleanupRetryMax  = time.Hour
)

// CleanupIdentity identifies a PR Preview without relying on untrusted names
// or exposing provider resource identifiers in the Control Plane API.
type CleanupIdentity struct {
	TenantID          domain.TenantID
	InstallationID    int64
	RepositoryID      int64
	PullRequestNumber int64
}

type CleanupJob struct {
	CleanupIdentity
	HeadSHA        string
	HeadRef        string
	ClaimID        string
	LeaseExpiresAt time.Time
	Attempt        int
}

// ClaimDue leases a bounded set of absent Previews. An expired claim can be
// recovered after a worker crash; all completion operations are fenced by the
// claim ID, attempt, and exact lease expiry.
func ClaimDue(ctx context.Context, tx pgx.Tx, claimID string, now time.Time, lease time.Duration, limit int) ([]CleanupJob, error) {
	if ctx == nil || tx == nil || !validCleanupClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 {
		return nil, errors.New("transaction, valid claim ID, current time, 5-second to 10-minute lease, and limit 1-100 are required")
	}
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT tenant_id,installation_id,github_repository_id,pull_request_number
		FROM repository_previews
		WHERE desired_state='absent' AND state='cleanup_pending'
		  AND ((cleanup_claim_id IS NULL AND (cleanup_next_attempt_at IS NULL OR cleanup_next_attempt_at <= $1))
		       OR (cleanup_claim_id IS NOT NULL AND cleanup_lease_expires_at <= $1))
		ORDER BY COALESCE(cleanup_next_attempt_at,updated_at),updated_at,tenant_id,installation_id,github_repository_id,pull_request_number
		LIMIT $2
	)
	UPDATE repository_previews p
	SET cleanup_claim_id=$3,cleanup_lease_expires_at=$4,cleanup_attempts=p.cleanup_attempts+1,updated_at=$1
	FROM due
	WHERE p.tenant_id=due.tenant_id AND p.installation_id=due.installation_id
	  AND p.github_repository_id=due.github_repository_id AND p.pull_request_number=due.pull_request_number
	  AND p.desired_state='absent' AND p.state='cleanup_pending'
	  AND ((p.cleanup_claim_id IS NULL AND (p.cleanup_next_attempt_at IS NULL OR p.cleanup_next_attempt_at <= $1))
	       OR (p.cleanup_claim_id IS NOT NULL AND p.cleanup_lease_expires_at <= $1))
		RETURNING p.tenant_id,p.installation_id,p.github_repository_id,p.pull_request_number,p.head_sha,p.head_ref,
		          p.cleanup_claim_id,p.cleanup_lease_expires_at,p.cleanup_attempts`,
		now.UTC(), limit, claimID, now.UTC().Add(lease))
	if err != nil {
		return nil, fmt.Errorf("claim due Preview cleanup: %w", err)
	}
	defer rows.Close()
	jobs := make([]CleanupJob, 0, limit)
	for rows.Next() {
		var job CleanupJob
		if err := rows.Scan(&job.TenantID, &job.InstallationID, &job.RepositoryID, &job.PullRequestNumber,
			&job.HeadSHA, &job.HeadRef, &job.ClaimID, &job.LeaseExpiresAt, &job.Attempt); err != nil {
			return nil, fmt.Errorf("scan claimed Preview cleanup: %w", err)
		}
		job.LeaseExpiresAt = job.LeaseExpiresAt.UTC()
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed Preview cleanup: %w", err)
	}
	return jobs, nil
}

// RecordCleanupFailure persists a fixed failure category and schedules an
// exponentially backed-off retry only for the current unexpired lease. It
// never stores raw provider errors.
func RecordCleanupFailure(ctx context.Context, tx pgx.Tx, job CleanupJob, now time.Time, errorCode string) (time.Time, bool, error) {
	if ctx == nil || tx == nil || !validCleanupJob(job) || now.IsZero() || !validCleanupErrorCode(errorCode) {
		return time.Time{}, false, errors.New("valid claimed Preview, transaction, time, and error category are required")
	}
	retryAt := now.UTC().Add(cleanupRetryDelay(job.Attempt))
	tag, err := tx.Exec(ctx, `UPDATE repository_previews
		SET cleanup_claim_id=NULL,cleanup_lease_expires_at=NULL,cleanup_next_attempt_at=$8,cleanup_last_error=$9,updated_at=$7
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
		  AND desired_state='absent' AND state='cleanup_pending' AND cleanup_claim_id=$5
		  AND cleanup_attempts=$6 AND cleanup_lease_expires_at=$10 AND cleanup_lease_expires_at>$7`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.ClaimID,
		job.Attempt, now.UTC(), retryAt, errorCode, job.LeaseExpiresAt.UTC())
	if err != nil {
		return time.Time{}, false, fmt.Errorf("record Preview cleanup failure: %w", err)
	}
	return retryAt, tag.RowsAffected() == 1, nil
}

// MarkCleaned is called only after the runtime executor confirms idempotent
// deletion of the Preview's MicroVM and Gateway route.
func MarkCleaned(ctx context.Context, tx pgx.Tx, job CleanupJob, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validCleanupJob(job) || now.IsZero() {
		return false, errors.New("valid claimed Preview and transaction time are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE repository_previews
		SET state='cleaned',cleanup_claim_id=NULL,cleanup_lease_expires_at=NULL,cleanup_next_attempt_at=NULL,cleanup_last_error='',updated_at=$7
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
		  AND desired_state='absent' AND state='cleanup_pending' AND cleanup_claim_id=$5
		  AND cleanup_attempts=$6 AND cleanup_lease_expires_at=$8 AND cleanup_lease_expires_at>$7`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.ClaimID,
		job.Attempt, now.UTC(), job.LeaseExpiresAt.UTC())
	if err != nil {
		return false, fmt.Errorf("mark Preview cleanup complete: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var alreadyCleaned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM repository_previews WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3
		AND pull_request_number=$4 AND desired_state='absent' AND state='cleaned')`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber).Scan(&alreadyCleaned); err != nil {
		return false, fmt.Errorf("verify Preview cleanup completion: %w", err)
	}
	return alreadyCleaned, nil
}

func cleanupRetryDelay(attempt int) time.Duration {
	delay := cleanupRetryBase
	for i := 1; i < attempt && delay < cleanupRetryMax; i++ {
		delay *= 2
	}
	if delay > cleanupRetryMax {
		return cleanupRetryMax
	}
	return delay
}

func validCleanupIdentity(identity CleanupIdentity) bool {
	return identity.TenantID != "" && identity.InstallationID > 0 && identity.RepositoryID > 0 && identity.PullRequestNumber > 0
}

func validCleanupJob(job CleanupJob) bool {
	return validCleanupIdentity(job.CleanupIdentity) && validCleanupClaimID(job.ClaimID) && !job.LeaseExpiresAt.IsZero() && job.Attempt > 0
}

func validCleanupClaimID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func validCleanupErrorCode(code string) bool {
	switch code {
	case "preview_binding_missing", "gateway_route_remove_failed", "microvm_terminate_failed", "provider_unavailable":
		return true
	default:
		return false
	}
}
