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
	tag, err := tx.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
		ON CONFLICT (tenant_id,installation_id,github_repository_id,pull_request_number) DO UPDATE SET
			head_sha=EXCLUDED.head_sha, head_ref=EXCLUDED.head_ref, desired_state=EXCLUDED.desired_state,
			state=EXCLUDED.state, last_delivery_id=EXCLUDED.last_delivery_id, last_event_at=EXCLUDED.last_event_at,
			cleanup_attempts=CASE WHEN EXCLUDED.desired_state='present' THEN 0 ELSE repository_previews.cleanup_attempts END,
			cleanup_next_attempt_at=NULL, cleanup_last_error='', updated_at=EXCLUDED.updated_at
		WHERE repository_previews.last_event_at < EXCLUDED.last_event_at
		   OR (repository_previews.last_event_at = EXCLUDED.last_event_at AND repository_previews.last_delivery_id < EXCLUDED.last_delivery_id)`,
		string(tenantID), event.InstallationID, event.Repository.GitHubID, event.PullRequest.Number,
		event.PullRequest.HeadSHA, event.PullRequest.HeadRef, desired, state, deliveryID, eventAt, now.UTC())
	if err != nil {
		return false, fmt.Errorf("upsert repository Preview desired state: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
