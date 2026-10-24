package previews

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
)

type RepositoryPreview struct {
	TenantID          domain.TenantID `json:"tenant_id"`
	InstallationID    int64           `json:"installation_id"`
	RepositoryID      int64           `json:"repository_id"`
	PullRequestNumber int64           `json:"pull_request_number"`
	HeadSHA           string          `json:"head_sha"`
	HeadRef           string          `json:"head_ref"`
	DesiredState      string          `json:"desired_state"`
	State             string          `json:"state"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	CleanupAttempts   int             `json:"cleanup_attempts"`
	CleanupNextAt     *time.Time      `json:"cleanup_next_attempt_at,omitempty"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// ListForRepository returns a bounded, Tenant-scoped view of Preview lifecycle
// metadata. It intentionally exposes no runtime credentials or provider IDs.
func ListForRepository(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64, limit int) ([]RepositoryPreview, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || limit < 1 || limit > 100 {
		return nil, errors.New("valid Tenant, repository, transaction, and bounded limit are required")
	}
	rows, err := tx.Query(ctx, `SELECT tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,
		desired_state,state,expires_at,cleanup_attempts,cleanup_next_attempt_at,updated_at
		FROM repository_previews WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3
		ORDER BY updated_at DESC,pull_request_number DESC LIMIT $4`, tenantID, installationID, repositoryID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Repository Previews: %w", err)
	}
	defer rows.Close()
	items := make([]RepositoryPreview, 0)
	for rows.Next() {
		var item RepositoryPreview
		if err := rows.Scan(&item.TenantID, &item.InstallationID, &item.RepositoryID, &item.PullRequestNumber,
			&item.HeadSHA, &item.HeadRef, &item.DesiredState, &item.State, &item.ExpiresAt,
			&item.CleanupAttempts, &item.CleanupNextAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Repository Preview: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Repository Previews: %w", err)
	}
	return items, nil
}

var ErrInvalidDelivery = errors.New("invalid repository Preview delivery")

// ApplyPullRequestDelivery records the current desired Preview state for one
// Tenant-owned pull request. The inbox caller has already verified the GitHub
// signature, Installation, Repository ownership, and Preview opt-in.
func ApplyPullRequestDelivery(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, deliveryID string,
	event githubwebhook.RepositoryEvent, receivedAt, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || tenantID == "" || len(deliveryID) != 36 ||
		event.GitHubEvent != "pull_request" || event.PullRequest == nil || event.InstallationID <= 0 ||
		event.Repository == nil || event.Repository.GitHubID <= 0 || event.PullRequest.Number <= 0 ||
		event.PullRequest.HeadRef == "" || len(event.PullRequest.HeadSHA) != 40 || receivedAt.IsZero() || now.IsZero() {
		return false, ErrInvalidDelivery
	}
	var desired, state string
	switch {
	case event.PullRequest.State == "open" && (event.Action == "opened" || event.Action == "reopened" || event.Action == "synchronize"):
		desired, state = "present", "awaiting_runtime"
	case event.PullRequest.State == "closed" && event.Action == "closed":
		desired, state = "absent", "cleanup_pending"
	default:
		return false, nil
	}
	eventAt := receivedAt.UTC()
	if !event.PullRequest.UpdatedAt.IsZero() {
		eventAt = event.PullRequest.UpdatedAt.UTC()
	}
	var lifetimeSeconds int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT preview_lifetime_seconds FROM tenant_quota_limits WHERE tenant_id=$1),86400)`, tenantID).Scan(&lifetimeSeconds); err != nil {
		return false, fmt.Errorf("resolve Tenant Preview lifetime: %w", err)
	}
	if lifetimeSeconds < 300 || lifetimeSeconds > 86400 {
		return false, ErrInvalidDelivery
	}
	expiresAt := now.UTC().Add(time.Duration(lifetimeSeconds) * time.Second)
	tag, err := tx.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)
		ON CONFLICT (tenant_id,installation_id,github_repository_id,pull_request_number) DO UPDATE SET
			head_sha=EXCLUDED.head_sha, head_ref=EXCLUDED.head_ref,
			desired_state=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.desired_state ELSE EXCLUDED.desired_state END,
			state=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.state ELSE EXCLUDED.state END,
			last_delivery_id=EXCLUDED.last_delivery_id, last_event_at=EXCLUDED.last_event_at,
			expires_at=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13='reopened' THEN EXCLUDED.expires_at ELSE repository_previews.expires_at END,
			cleanup_attempts=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN 0 ELSE repository_previews.cleanup_attempts END,
			cleanup_next_attempt_at=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN NULL ELSE repository_previews.cleanup_next_attempt_at END,
			cleanup_last_error=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN '' ELSE repository_previews.cleanup_last_error END,
			updated_at=EXCLUDED.updated_at
		WHERE repository_previews.last_event_at < EXCLUDED.last_event_at
		   OR (repository_previews.last_event_at = EXCLUDED.last_event_at AND repository_previews.last_delivery_id < EXCLUDED.last_delivery_id)`,
		string(tenantID), event.InstallationID, event.Repository.GitHubID, event.PullRequest.Number,
		event.PullRequest.HeadSHA, event.PullRequest.HeadRef, desired, state, deliveryID, eventAt, expiresAt, now.UTC(), event.Action)
	if err != nil {
		return false, fmt.Errorf("upsert repository Preview desired state: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ExpireDue transitions a bounded batch of expired present Previews into the
// durable cleanup queue. It deliberately does not mark resources cleaned; a
// runtime cleanup worker must confirm physical deletion before that transition.
func ExpireDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int) (int64, error) {
	if ctx == nil || tx == nil || now.IsZero() || limit < 1 || limit > 500 {
		return 0, errors.New("invalid Preview expiry sweep")
	}
	tag, err := tx.Exec(ctx, `WITH due AS (
		SELECT tenant_id,installation_id,github_repository_id,pull_request_number
		FROM repository_previews
		WHERE desired_state='present' AND expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at,tenant_id,installation_id,github_repository_id,pull_request_number
		LIMIT $2
	)
	UPDATE repository_previews p
	SET desired_state='absent',state='cleanup_pending',cleanup_next_attempt_at=NULL,updated_at=$1
	FROM due
	WHERE p.tenant_id=due.tenant_id AND p.installation_id=due.installation_id
	  AND p.github_repository_id=due.github_repository_id AND p.pull_request_number=due.pull_request_number
	  AND p.desired_state='present' AND p.expires_at <= $1`, now.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("expire due repository Previews: %w", err)
	}
	return tag.RowsAffected(), nil
}
