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

var ErrCleanupNotPending = errors.New("Repository Preview is not pending cleanup")

// CleanupIdentity identifies a PR Preview without relying on untrusted names
// or exposing provider resource identifiers in the Control Plane API.
type CleanupIdentity struct {
	TenantID          domain.TenantID
	InstallationID    int64
	RepositoryID      int64
	PullRequestNumber int64
}

// RecordCleanupFailure persists a fixed failure category and schedules an
// exponentially backed-off retry. It never stores raw provider errors.
func RecordCleanupFailure(ctx context.Context, tx pgx.Tx, identity CleanupIdentity, now time.Time, errorCode string) (int, time.Time, bool, error) {
	if ctx == nil || tx == nil || !validCleanupIdentity(identity) || now.IsZero() || !validCleanupErrorCode(errorCode) {
		return 0, time.Time{}, false, errors.New("valid Preview cleanup identity, transaction, time, and error category are required")
	}
	var previousAttempts int
	err := tx.QueryRow(ctx, `SELECT cleanup_attempts FROM repository_previews
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
		  AND desired_state='absent' AND state='cleanup_pending'
		  AND (cleanup_next_attempt_at IS NULL OR cleanup_next_attempt_at <= $5)`,
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber, now.UTC()).Scan(&previousAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read Preview cleanup attempts: %w", err)
	}
	attempt := previousAttempts + 1
	delay := cleanupRetryDelay(attempt)
	retryAt := now.UTC().Add(delay)
	tag, err := tx.Exec(ctx, `UPDATE repository_previews
		SET cleanup_attempts=$5, cleanup_next_attempt_at=$6, cleanup_last_error=$7, updated_at=$8
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
		  AND desired_state='absent' AND state='cleanup_pending' AND cleanup_attempts=$9
		  AND (cleanup_next_attempt_at IS NULL OR cleanup_next_attempt_at <= $10)`,
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber,
		attempt, retryAt, errorCode, now.UTC(), previousAttempts, now.UTC())
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("record Preview cleanup failure: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return 0, time.Time{}, false, nil
	}
	return attempt, retryAt, true, nil
}

// MarkCleaned is called only after the runtime executor confirms idempotent
// deletion of the Preview's MicroVM and Gateway route.
func MarkCleaned(ctx context.Context, tx pgx.Tx, identity CleanupIdentity, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validCleanupIdentity(identity) || now.IsZero() {
		return false, errors.New("valid Preview cleanup identity, transaction, and time are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE repository_previews
		SET state='cleaned', cleanup_next_attempt_at=NULL, cleanup_last_error='', updated_at=$5
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
		  AND desired_state='absent' AND state='cleanup_pending'`,
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber, now.UTC())
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
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber).Scan(&alreadyCleaned); err != nil {
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

func validCleanupErrorCode(code string) bool {
	switch code {
	case "preview_binding_missing", "gateway_route_remove_failed", "microvm_terminate_failed", "provider_unavailable":
		return true
	default:
		return false
	}
}
