package previews

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/quotas"
)

type RepositoryPreview struct {
	TenantID          domain.TenantID `json:"tenant_id"`
	InstallationID    int64           `json:"installation_id"`
	RepositoryID      int64           `json:"repository_id"`
	PullRequestNumber int64           `json:"pull_request_number"`
	HeadSHA           string          `json:"head_sha"`
	HeadRef           string          `json:"head_ref"`
	Generation        int64           `json:"generation"`
	DesiredState      string          `json:"desired_state"`
	State             string          `json:"state"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	CleanupAttempts   int             `json:"cleanup_attempts"`
	CleanupNextAt     *time.Time      `json:"cleanup_next_attempt_at,omitempty"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// RuntimeAllocation is one immutable PR-head generation. Provider identifiers
// are stored only in this Control Plane record and are intentionally omitted
// from the user-facing RepositoryPreview response.
type RuntimeAllocation struct {
	CleanupIdentity
	Generation      int64
	HeadSHA         string
	HeadRef         string
	DesiredState    string
	State           string
	ExpiresAt       time.Time
	ResourcePlaneID domain.ResourcePlaneID
	WorkspaceID     domain.WorkspaceID
	MicroVMID       string
	GatewayRouteID  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ListForRepository returns a bounded, Tenant-scoped view of Preview lifecycle
// metadata. It intentionally exposes no runtime credentials or provider IDs.
func ListForRepository(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, installationID, repositoryID int64, limit int) ([]RepositoryPreview, error) {
	if ctx == nil || tx == nil || tenantID == "" || installationID <= 0 || repositoryID <= 0 || limit < 1 || limit > 100 {
		return nil, errors.New("valid Tenant, repository, transaction, and bounded limit are required")
	}
	rows, err := tx.Query(ctx, `SELECT tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,
		generation,desired_state,state,expires_at,cleanup_attempts,cleanup_next_attempt_at,updated_at
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
			&item.HeadSHA, &item.HeadRef, &item.Generation, &item.DesiredState, &item.State, &item.ExpiresAt,
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
	var priorDesired, priorState, priorDeliveryID string
	var priorEventAt time.Time
	exists := true
	err := tx.QueryRow(ctx, `SELECT desired_state,state,last_event_at,last_delivery_id FROM repository_previews
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4`,
		tenantID, event.InstallationID, event.Repository.GitHubID, event.PullRequest.Number).
		Scan(&priorDesired, &priorState, &priorEventAt, &priorDeliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		exists = false
	} else if err != nil {
		return false, fmt.Errorf("read current Repository Preview before admission: %w", err)
	}
	if exists && (priorEventAt.After(eventAt) || priorEventAt.Equal(eventAt) && priorDeliveryID >= deliveryID) {
		return false, nil
	}
	var lifetimeSeconds int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT preview_lifetime_seconds FROM tenant_quota_limits WHERE tenant_id=$1),86400)`, tenantID).Scan(&lifetimeSeconds); err != nil {
		return false, fmt.Errorf("resolve Tenant Preview lifetime: %w", err)
	}
	if lifetimeSeconds < 300 || lifetimeSeconds > 86400 {
		return false, ErrInvalidDelivery
	}
	if desired == "absent" && !exists {
		// A close can arrive before an open event; there is no runtime to clean.
		state = "cleaned"
	}
	if desired == "absent" && exists && priorState == "quota_exceeded" {
		// A quota-rejected PR has no runtime allocation to clean up.
		state = "cleaned"
	}
	needsSlot := desired == "present" && (!exists || priorDesired == "absent" && event.Action == "reopened" &&
		(priorState == "cleaned" || priorState == "quota_exceeded"))
	if needsSlot {
		admitted, err := admitPreview(ctx, tx, tenantID, now.UTC())
		if err != nil {
			return false, err
		}
		if !admitted {
			desired, state = "absent", "quota_exceeded"
		}
	}
	expiresAt := now.UTC().Add(time.Duration(lifetimeSeconds) * time.Second)
	var expiresAtValue any = expiresAt
	if state == "quota_exceeded" || state == "cleaned" && desired == "absent" && !exists {
		expiresAtValue = nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)
		ON CONFLICT (tenant_id,installation_id,github_repository_id,pull_request_number) DO UPDATE SET
			head_sha=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.head_sha ELSE EXCLUDED.head_sha END,
			head_ref=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.head_ref ELSE EXCLUDED.head_ref END,
			desired_state=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.desired_state ELSE EXCLUDED.desired_state END,
			state=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.state ELSE EXCLUDED.state END,
			generation=repository_previews.generation + CASE WHEN
				(repository_previews.desired_state IS DISTINCT FROM CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.desired_state ELSE EXCLUDED.desired_state END)
				OR (repository_previews.head_sha IS DISTINCT FROM CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13<>'reopened' THEN repository_previews.head_sha ELSE EXCLUDED.head_sha END)
				THEN 1 ELSE 0 END,
			last_delivery_id=EXCLUDED.last_delivery_id, last_event_at=EXCLUDED.last_event_at,
			expires_at=CASE WHEN EXCLUDED.desired_state='present' AND repository_previews.desired_state='absent' AND $13='reopened' THEN EXCLUDED.expires_at ELSE repository_previews.expires_at END,
			cleanup_attempts=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN 0 ELSE repository_previews.cleanup_attempts END,
			cleanup_next_attempt_at=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN NULL ELSE repository_previews.cleanup_next_attempt_at END,
			cleanup_last_error=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN '' ELSE repository_previews.cleanup_last_error END,
			cleanup_claim_id=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN NULL ELSE repository_previews.cleanup_claim_id END,
			cleanup_lease_expires_at=CASE WHEN EXCLUDED.desired_state='present' AND (repository_previews.desired_state='present' OR $13='reopened') THEN NULL ELSE repository_previews.cleanup_lease_expires_at END,
			updated_at=EXCLUDED.updated_at
		WHERE repository_previews.last_event_at < EXCLUDED.last_event_at
		   OR (repository_previews.last_event_at = EXCLUDED.last_event_at AND repository_previews.last_delivery_id < EXCLUDED.last_delivery_id)`,
		string(tenantID), event.InstallationID, event.Repository.GitHubID, event.PullRequest.Number,
		event.PullRequest.HeadSHA, event.PullRequest.HeadRef, desired, state, deliveryID, eventAt, expiresAtValue, now.UTC(), event.Action)
	if err != nil {
		return false, fmt.Errorf("upsert repository Preview desired state: %w", err)
	}
	updated := tag.RowsAffected() == 1
	if updated {
		if err := syncRuntimeAllocations(ctx, tx, CleanupIdentity{TenantID: tenantID, InstallationID: event.InstallationID,
			RepositoryID: event.Repository.GitHubID, PullRequestNumber: event.PullRequest.Number}, now.UTC()); err != nil {
			return false, err
		}
	}
	return updated, nil
}

func syncRuntimeAllocations(ctx context.Context, tx pgx.Tx, identity CleanupIdentity, now time.Time) error {
	if ctx == nil || tx == nil || identity.TenantID == "" || identity.InstallationID <= 0 || identity.RepositoryID <= 0 || identity.PullRequestNumber <= 0 || now.IsZero() {
		return ErrInvalidDelivery
	}
	var generation int64
	var desired, state, headSHA, headRef string
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `SELECT generation,desired_state,state,head_sha,head_ref,expires_at FROM repository_previews
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4`,
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber).
		Scan(&generation, &desired, &state, &headSHA, &headRef, &expiresAt)
	if err != nil {
		return fmt.Errorf("read accepted Preview generation: %w", err)
	}
	if desired == "present" && state == "awaiting_runtime" && expiresAt != nil {
		if _, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations
			SET desired_state='absent',state='cleanup_pending',updated_at=$6
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
			  AND generation<$5 AND desired_state='present' AND state<>'cleaned'`,
			identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber, generation, now.UTC()); err != nil {
			return fmt.Errorf("retire stale Preview runtime generations: %w", err)
		}
		var integrationID string
		var configRevision, credentialGeneration int64
		pinErr := tx.QueryRow(ctx, `SELECT i.id,i.config_revision,i.credential_generation
			FROM tenant_github_app_installation_bindings binding
			JOIN tenant_github_integrations i ON i.tenant_id=binding.tenant_id AND i.id=binding.integration_id
			JOIN tenant_github_installations installation ON installation.tenant_id=binding.tenant_id AND installation.installation_id=binding.installation_id
			JOIN tenant_github_repositories repository ON repository.tenant_id=binding.tenant_id
			  AND repository.installation_id=binding.installation_id AND repository.github_repository_id=$3
			JOIN github_app_registrations registration ON registration.id=i.app_registration_id
			  AND registration.github_app_id=i.github_app_id AND registration.mode='tenant_owned'
			  AND registration.owner_tenant_id=i.tenant_id
			JOIN github_credential_bindings credential ON credential.tenant_id=i.tenant_id
			  AND credential.id=i.credential_binding_id AND credential.github_app_id=i.github_app_id AND credential.status='approved'
			WHERE binding.tenant_id=$1 AND binding.installation_id=$2 AND i.status='active'
			  AND installation.status='active' AND installation.suspended_at IS NULL`,
			identity.TenantID, identity.InstallationID, identity.RepositoryID).Scan(&integrationID, &configRevision, &credentialGeneration)
		if pinErr != nil && !errors.Is(pinErr, pgx.ErrNoRows) {
			return fmt.Errorf("resolve Tenant GitHub App pin for Preview generation: %w", pinErr)
		}
		var integrationValue, revisionValue, credentialGenerationValue any
		if pinErr == nil {
			integrationValue, revisionValue, credentialGenerationValue = integrationID, configRevision, credentialGeneration
		}
		if _, err := tx.Exec(ctx, `INSERT INTO repository_preview_runtime_allocations
			(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,
			 github_integration_id,github_config_revision,github_credential_generation,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'present','awaiting_runtime',$8,$9,$10,$11,$12,$12)
			ON CONFLICT (tenant_id,installation_id,github_repository_id,pull_request_number,generation) DO NOTHING`,
			identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber,
			generation, headSHA, headRef, expiresAt.UTC(), integrationValue, revisionValue, credentialGenerationValue, now.UTC()); err != nil {
			return fmt.Errorf("record current Preview runtime generation: %w", err)
		}
		return nil
	}
	if desired == "absent" {
		if _, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations
			SET desired_state='absent',state=CASE WHEN state='cleaned' THEN state ELSE 'cleanup_pending' END,updated_at=$5
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
			  AND desired_state='present'`, identity.TenantID, identity.InstallationID,
			identity.RepositoryID, identity.PullRequestNumber, now.UTC()); err != nil {
			return fmt.Errorf("queue Preview runtime generations for cleanup: %w", err)
		}
	}
	return nil
}

// admitPreview advances the shared Tenant quota OCC guard before counting both
// Workload and pull-request Previews. A cleanup_pending PR keeps its slot until
// a worker confirms physical cleanup.
func admitPreview(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, now time.Time) (bool, error) {
	limits, err := quotas.AdvanceAdmissionGuard(ctx, tx, tenantID, now)
	if err != nil {
		return false, fmt.Errorf("advance Tenant Preview quota guard: %w", err)
	}
	var active int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM workload_runs WHERE tenant_id=$1 AND kind='preview' AND state NOT IN ('succeeded','failed','cancelled','timed_out')) +
		(SELECT COUNT(*) FROM repository_previews WHERE tenant_id=$1 AND state IN ('awaiting_runtime','running','cleanup_pending'))`, tenantID).Scan(&active); err != nil {
		return false, fmt.Errorf("count active Tenant Previews: %w", err)
	}
	return active < int64(limits.PreviewRuns), nil
}

// ExpireDue transitions a bounded batch of expired present Previews into the
// durable cleanup queue. It deliberately does not mark resources cleaned; a
// runtime cleanup worker must confirm physical deletion before that transition.
func ExpireDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int) (int64, error) {
	if ctx == nil || tx == nil || now.IsZero() || limit < 1 || limit > 500 {
		return 0, errors.New("invalid Preview expiry sweep")
	}
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT tenant_id,installation_id,github_repository_id,pull_request_number
		FROM repository_previews
		WHERE desired_state='present' AND expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at,tenant_id,installation_id,github_repository_id,pull_request_number
		LIMIT $2
	)
	UPDATE repository_previews p
	SET desired_state='absent',state='cleanup_pending',generation=p.generation+1,cleanup_next_attempt_at=NULL,cleanup_claim_id=NULL,cleanup_lease_expires_at=NULL,updated_at=$1
	FROM due
	WHERE p.tenant_id=due.tenant_id AND p.installation_id=due.installation_id
	  AND p.github_repository_id=due.github_repository_id AND p.pull_request_number=due.pull_request_number
	  AND p.desired_state='present' AND p.expires_at <= $1
	RETURNING p.tenant_id,p.installation_id,p.github_repository_id,p.pull_request_number`, now.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("expire due repository Previews: %w", err)
	}
	type identity struct {
		tenant         domain.TenantID
		installationID int64
		repositoryID   int64
		pullRequestNo  int64
	}
	items := make([]identity, 0, limit)
	for rows.Next() {
		var item identity
		if err := rows.Scan(&item.tenant, &item.installationID, &item.repositoryID, &item.pullRequestNo); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired Repository Preview: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate expired Repository Previews: %w", err)
	}
	rows.Close()
	for _, item := range items {
		if _, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations
			SET desired_state='absent',state='cleanup_pending',updated_at=$5
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
			  AND desired_state='present' AND state<>'cleaned'`,
			item.tenant, item.installationID, item.repositoryID, item.pullRequestNo, now.UTC()); err != nil {
			return 0, fmt.Errorf("queue expired Preview runtime generations for cleanup: %w", err)
		}
	}
	return int64(len(items)), nil
}
