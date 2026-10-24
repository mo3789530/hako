// Package githubwebhook stores authenticated GitHub webhook deliveries in the
// Control Plane inbox. Normalized events are stored alongside the raw
// authenticated delivery so downstream processing never reparses GitHub JSON.
package githubwebhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrDeliveryIDConflict = githubwebhook.ErrDeliveryIDConflict
var ErrUnsupportedWebhookEvent = errors.New("unsupported GitHub webhook event for processing")
var ErrUnsupportedRepositoryEvent = ErrUnsupportedWebhookEvent
var ErrStaleTenantWebhook = errors.New("Tenant GitHub webhook integration changed before processing")

const RawPayloadRetention = 7 * 24 * time.Hour

var webhookCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Inbox struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

// ProcessInstallationRepositoryDelivery applies one stored schema-v1
// Installation repository event and marks its inbox row processed in the
// caller's transaction. FOR UPDATE fences concurrent processors for this
// delivery; Aurora DSQL adjudicates the read with OCC and the transaction
// runner retries conflicts. An inactive Installation or unsupported event
// leaves the delivery unchanged so another consumer or a later retry can
// handle it.
func ProcessInstallationRepositoryDelivery(ctx context.Context, tx pgx.Tx, deliveryID string, processedAt time.Time) (bool, error) {
	return ProcessInstallationRepositoryDeliveryForIntegration(ctx, tx, "shared", deliveryID, processedAt)
}

func ProcessInstallationRepositoryDeliveryForIntegration(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, processedAt time.Time) (bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	var status string
	var schemaVersion int
	var eventJSON string
	var credentialGeneration int64
	err := tx.QueryRow(ctx, `SELECT processing_status, event_schema_version, normalized_event_json, credential_generation
		FROM github_webhook_deliveries WHERE integration_id = $1 AND delivery_id = $2 FOR UPDATE`, integrationID, deliveryID).Scan(&status, &schemaVersion, &eventJSON, &credentialGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgx.ErrNoRows
	}
	if err != nil {
		return false, fmt.Errorf("read GitHub webhook inbox event: %w", err)
	}
	if status == "processed" {
		return false, nil
	}
	if status != "received" || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return false, ErrUnsupportedWebhookEvent
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		return false, fmt.Errorf("decode normalized GitHub event: %w", err)
	}
	if event.DeliveryID != deliveryID || event.GitHubEvent != "installation_repositories" {
		return false, ErrUnsupportedWebhookEvent
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, credentialGeneration, event)
	if err != nil {
		return false, err
	}
	if !allowed {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, err
		}
		return false, nil
	}
	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT b.tenant_id FROM github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE b.installation_id = $1 AND i.status = 'active' AND i.suspended_at IS NULL`, event.InstallationID).Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, githubregistry.ErrInstallationNotActive
		}
		return false, fmt.Errorf("resolve Tenant for GitHub Installation %d: %w", event.InstallationID, err)
	}
	if err := githubregistry.SyncInstallationRepositories(ctx, tx, domain.TenantID(tenantID), event, processedAt); err != nil {
		return false, fmt.Errorf("sync GitHub Installation repositories: %w", err)
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status = 'processed'
		WHERE integration_id = $1 AND delivery_id = $2 AND processing_status = 'received' AND event_schema_version = $3`, integrationID, deliveryID, schemaVersion)
	if err != nil {
		return false, fmt.Errorf("mark GitHub webhook delivery processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("GitHub webhook delivery processing state changed concurrently")
	}
	return true, nil
}

// ProcessInstallationLifecycleDelivery applies a verified Installation
// lifecycle event only to an Installation that already has a Hako binding.
// Webhooks never create or restore Tenant authorization: setup verification
// remains the only path to an active binding.
func ProcessInstallationLifecycleDelivery(ctx context.Context, tx pgx.Tx, deliveryID string, processedAt time.Time) (bool, error) {
	return ProcessInstallationLifecycleDeliveryForIntegration(ctx, tx, "shared", deliveryID, processedAt)
}

func ProcessInstallationLifecycleDeliveryForIntegration(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, processedAt time.Time) (bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	var status, eventType, action, eventJSON string
	var schemaVersion int
	var credentialGeneration int64
	err := tx.QueryRow(ctx, `SELECT processing_status, event_type, action, event_schema_version, normalized_event_json, credential_generation
		FROM github_webhook_deliveries WHERE integration_id = $1 AND delivery_id = $2 FOR UPDATE`, integrationID, deliveryID).
		Scan(&status, &eventType, &action, &schemaVersion, &eventJSON, &credentialGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgx.ErrNoRows
	}
	if err != nil {
		return false, fmt.Errorf("read GitHub Installation lifecycle event: %w", err)
	}
	if status == "processed" {
		return false, nil
	}
	if status != "received" || eventType != "installation" || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return false, ErrUnsupportedWebhookEvent
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		return false, fmt.Errorf("decode normalized GitHub Installation event: %w", err)
	}
	if event.DeliveryID != deliveryID || event.GitHubEvent != "installation" || event.InstallationID <= 0 || event.Action != action || event.Type != "github.installation."+action {
		return false, ErrUnsupportedWebhookEvent
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, credentialGeneration, event)
	if err != nil {
		return false, err
	}
	if !allowed {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, err
		}
		return false, nil
	}
	switch action {
	case "created", "deleted", "suspend", "unsuspend":
	default:
		return false, ErrUnsupportedWebhookEvent
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	if action != "created" {
		var tenantID string
		err := tx.QueryRow(ctx, `SELECT b.tenant_id FROM github_app_installation_bindings b
			JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
			WHERE b.installation_id = $1`, event.InstallationID).Scan(&tenantID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("resolve Tenant for GitHub Installation %d: %w", event.InstallationID, err)
		}
		if err == nil {
			switch action {
			case "deleted":
				if _, err := tx.Exec(ctx, `UPDATE tenant_github_installations SET status = 'revoked', suspended_at = NULL, updated_at = $3
					WHERE tenant_id = $1 AND installation_id = $2`, tenantID, event.InstallationID, processedAt); err != nil {
					return false, fmt.Errorf("revoke deleted GitHub Installation: %w", err)
				}
				if _, err := tx.Exec(ctx, `DELETE FROM tenant_github_repositories WHERE tenant_id = $1 AND installation_id = $2`, tenantID, event.InstallationID); err != nil {
					return false, fmt.Errorf("remove repositories for deleted GitHub Installation: %w", err)
				}
			case "suspend":
				if _, err := tx.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at = COALESCE(suspended_at, $3), updated_at = $3
					WHERE tenant_id = $1 AND installation_id = $2 AND status = 'active'`, tenantID, event.InstallationID, processedAt); err != nil {
					return false, fmt.Errorf("suspend GitHub Installation: %w", err)
				}
			case "unsuspend":
				if _, err := tx.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at = NULL, updated_at = $3
					WHERE tenant_id = $1 AND installation_id = $2 AND status = 'active'`, tenantID, event.InstallationID, processedAt); err != nil {
					return false, fmt.Errorf("unsuspend GitHub Installation: %w", err)
				}
			}
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status = 'processed'
		WHERE integration_id = $1 AND delivery_id = $2 AND processing_status = 'received' AND event_schema_version = $3`, integrationID, deliveryID, schemaVersion)
	if err != nil {
		return false, fmt.Errorf("mark GitHub Installation event processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("GitHub Installation event processing state changed concurrently")
	}
	return true, nil
}

type RepositoryRunCreator func(context.Context, pgx.Tx, domain.TenantID, string, string, string, time.Time) (bool, error)

// IssueCommentAgentAuthorization is the Hako identity and Tenant scope
// established for a recognized Issue command. It is not itself permission to
// create a Run: callers must still resolve and pin an immutable commit before
// persisting any work.
type IssueCommentAgentAuthorization struct {
	TenantID     domain.TenantID
	UserID       string
	Installation int64
	Repository   githubwebhook.RepositoryRef
	IssueNumber  int64
	CommentID    int64
}

// AuthorizeIssueCommentAgentCommand revalidates the current installation,
// repository opt-in, verified GitHub identity, and Tenant membership for a
// normalized /hako implement Issue comment. It intentionally does not mark
// the delivery processed or create a Run, allowing the caller to resolve a
// server-trusted immutable SHA before doing either atomically.
func AuthorizeIssueCommentAgentCommand(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string) (IssueCommentAgentAuthorization, bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) {
		return IssueCommentAgentAuthorization{}, false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	var status, eventJSON string
	var schemaVersion int
	var generation int64
	var receivedAt time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version,normalized_event_json,credential_generation,received_at
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2 FOR UPDATE`, integrationID, deliveryID).
		Scan(&status, &schemaVersion, &eventJSON, &generation, &receivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueCommentAgentAuthorization{}, false, pgx.ErrNoRows
	}
	if err != nil {
		return IssueCommentAgentAuthorization{}, false, fmt.Errorf("lock Issue command delivery: %w", err)
	}
	if status != "received" || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return IssueCommentAgentAuthorization{}, false, nil
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil || event.DeliveryID != deliveryID || event.GitHubEvent != "issue_comment" ||
		event.Action != "created" || event.InstallationID <= 0 || event.Repository == nil || event.ActorGitHubID <= 0 || event.IssueComment == nil ||
		event.IssueComment.IsPullRequest || event.IssueComment.IssueNumber <= 0 || event.IssueComment.CommentID <= 0 ||
		event.IssueComment.AgentCommand != githubwebhook.AgentCommandImplement || !allowedIssueCommentAssociation(event.IssueComment.AuthorAssociation) {
		return IssueCommentAgentAuthorization{}, false, nil
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
	if err != nil || !allowed {
		return IssueCommentAgentAuthorization{}, false, err
	}
	tenantID, err := resolveRepositoryEventTenant(ctx, tx, integrationID, event.InstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueCommentAgentAuthorization{}, false, nil
	}
	if err != nil {
		return IssueCommentAgentAuthorization{}, false, fmt.Errorf("resolve Tenant for Issue command: %w", err)
	}
	var userID string
	err = tx.QueryRow(ctx, `SELECT u.id FROM users u JOIN tenant_members m ON m.user_id=u.id AND m.tenant_id=$1
		WHERE u.github_user_id=$2`, tenantID, event.ActorGitHubID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueCommentAgentAuthorization{}, false, nil
	}
	if err != nil {
		return IssueCommentAgentAuthorization{}, false, fmt.Errorf("authorize Issue command Tenant member: %w", err)
	}
	var enabled bool
	var enabledAt *time.Time
	var ownerLogin, repositoryName string
	err = tx.QueryRow(ctx, `SELECT factory_enabled,factory_enabled_at,owner_login,repository_name FROM tenant_github_repositories
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3`, tenantID, event.InstallationID, event.Repository.GitHubID).
		Scan(&enabled, &enabledAt, &ownerLogin, &repositoryName)
	if errors.Is(err, pgx.ErrNoRows) || !enabled || enabledAt == nil || receivedAt.Before(*enabledAt) || ownerLogin != event.Repository.Owner || repositoryName != event.Repository.Name {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return IssueCommentAgentAuthorization{}, false, fmt.Errorf("load Tenant Issue command policy: %w", err)
		}
		return IssueCommentAgentAuthorization{}, false, nil
	}
	return IssueCommentAgentAuthorization{TenantID: domain.TenantID(tenantID), UserID: userID, Installation: event.InstallationID,
		Repository: *event.Repository, IssueNumber: event.IssueComment.IssueNumber, CommentID: event.IssueComment.CommentID}, true, nil
}

func allowedIssueCommentAssociation(association string) bool {
	switch association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

// ProcessRepositoryTriggerDelivery applies one authenticated push or pull
// request delivery. Trigger execution is opt-in on the synchronized
// Tenant/Installation/Repository record. The callback creates the Workload
// Run in this same transaction, so processed status, queued Check and Run
// Outbox cannot be separated by a crash.
func ProcessRepositoryTriggerDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, processedAt time.Time, createRun RepositoryRunCreator) (bool, bool, error) {
	if tx == nil || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) || createRun == nil {
		return false, false, errors.New("transaction, valid GitHub Delivery ID, and Workload Run creator are required")
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	var status string
	var schemaVersion int
	var eventJSON string
	var correlationID string
	var generation int64
	var receivedAt time.Time
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version,normalized_event_json,credential_generation,received_at,COALESCE(correlation_id,'')
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2 FOR UPDATE`, integrationID, deliveryID).
		Scan(&status, &schemaVersion, &eventJSON, &generation, &receivedAt, &correlationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, pgx.ErrNoRows
	}
	if err != nil {
		return false, false, fmt.Errorf("lock GitHub repository trigger delivery: %w", err)
	}
	if status == "processed" || status == "failed" {
		return false, false, nil
	}
	if status != "received" || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		return false, false, ErrUnsupportedWebhookEvent
	}
	var event githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil || event.DeliveryID != deliveryID || event.InstallationID <= 0 || event.Repository == nil {
		return false, false, ErrUnsupportedWebhookEvent
	}
	if event.GitHubEvent != "push" && event.GitHubEvent != "pull_request" {
		return false, false, ErrUnsupportedWebhookEvent
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
	if err != nil {
		return false, false, err
	}
	if !allowed {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	tenantID, err := resolveRepositoryEventTenant(ctx, tx, integrationID, event.InstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("resolve Tenant for repository trigger: %w", err)
	}
	var factoryEnabled bool
	var factoryEnabledAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT factory_enabled,factory_enabled_at FROM tenant_github_repositories
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3`, tenantID, event.InstallationID, event.Repository.GitHubID).Scan(&factoryEnabled, &factoryEnabledAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if err := failStaleWebhookDelivery(ctx, tx, integrationID, deliveryID); err != nil {
				return false, false, err
			}
			return true, false, nil
		}
		return false, false, fmt.Errorf("load Tenant repository trigger setting: %w", err)
	}
	runCreated := false
	// Do not backfill or unexpectedly launch work for deliveries that arrived
	// before an Owner/Admin explicitly opted this repository into triggers.
	// The enabled timestamp is cleared on disable and reset on the next enable.
	optedInBeforeDelivery := factoryEnabledAt != nil && !receivedAt.Before(*factoryEnabledAt)
	if factoryEnabled && optedInBeforeDelivery && eligibleRepositoryTrigger(event) {
		runCreated, err = createRun(ctx, tx, domain.TenantID(tenantID), integrationID, deliveryID, correlationID, processedAt)
		if err != nil {
			return false, false, fmt.Errorf("create Workload Run from repository delivery: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status='processed'
		WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='received' AND event_schema_version=$3`,
		integrationID, deliveryID, schemaVersion)
	if err != nil {
		return false, false, fmt.Errorf("mark repository trigger delivery processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, false, errors.New("repository trigger delivery processing state changed concurrently")
	}
	return true, runCreated, nil
}

func resolveRepositoryEventTenant(ctx context.Context, tx pgx.Tx, integrationID string, installationID int64) (string, error) {
	if integrationID == "shared" {
		var tenantID string
		err := tx.QueryRow(ctx, `SELECT b.tenant_id FROM github_app_installation_bindings b
			JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
			WHERE b.installation_id=$1 AND i.status='active' AND i.suspended_at IS NULL`, installationID).Scan(&tenantID)
		return tenantID, err
	}
	var tenantID string
	err := tx.QueryRow(ctx, `SELECT i.tenant_id FROM tenant_github_integrations i
		JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id
		JOIN tenant_github_installations x ON x.tenant_id=b.tenant_id AND x.installation_id=b.installation_id
		WHERE i.id=$1 AND b.installation_id=$2 AND i.status='active' AND x.status='active' AND x.suspended_at IS NULL`, integrationID, installationID).Scan(&tenantID)
	return tenantID, err
}

func eligibleRepositoryTrigger(event githubwebhook.RepositoryEvent) bool {
	if event.GitHubEvent == "push" && event.Deleted {
		return false
	}
	switch event.Type {
	case "repository.push", "repository.pull_request.opened", "repository.pull_request.reopened", "repository.pull_request.synchronize":
		return true
	default:
		return false
	}
}

func coreValidDeliveryID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func authorizeStoredTenantEvent(ctx context.Context, tx pgx.Tx, integrationID string, generation int64, event githubwebhook.RepositoryEvent) (bool, error) {
	if integrationID == "shared" {
		return generation == 0, nil
	}
	if generation < 1 || event.InstallationID <= 0 {
		return false, nil
	}
	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT i.tenant_id FROM tenant_github_integrations i
		JOIN github_app_registrations r ON r.id = i.app_registration_id AND r.github_app_id = i.github_app_id
		JOIN github_credential_bindings c ON c.tenant_id = i.tenant_id AND c.id = i.credential_binding_id AND c.github_app_id = i.github_app_id
		WHERE i.id = $1 AND i.status = 'active' AND i.credential_generation = $2
		AND ($3::bigint IS NULL OR i.github_app_id = $3)
		AND r.mode = 'tenant_owned' AND r.owner_tenant_id = i.tenant_id AND c.status = 'approved'`, integrationID, generation, event.GitHubAppID).Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("revalidate Tenant webhook integration: %w", err)
	}
	if event.GitHubEvent == "installation" && event.Action == "created" {
		return true, nil
	}
	query := `SELECT EXISTS (SELECT 1 FROM tenant_github_app_installation_bindings b
		JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
		WHERE b.tenant_id = $1 AND b.integration_id = $2 AND b.installation_id = $3)`
	if event.GitHubEvent != "installation" {
		query = `SELECT EXISTS (SELECT 1 FROM tenant_github_app_installation_bindings b
			JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
			WHERE b.tenant_id = $1 AND b.integration_id = $2 AND b.installation_id = $3
			AND i.status = 'active' AND i.suspended_at IS NULL)`
	}
	var bound bool
	if err := tx.QueryRow(ctx, query, tenantID, integrationID, event.InstallationID).Scan(&bound); err != nil {
		return false, fmt.Errorf("revalidate Tenant webhook Installation: %w", err)
	}
	return bound, nil
}

func failStaleWebhookDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string) error {
	if _, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries SET processing_status = 'failed'
		WHERE integration_id = $1 AND delivery_id = $2 AND processing_status = 'received'`, integrationID, deliveryID); err != nil {
		return fmt.Errorf("quarantine stale Tenant GitHub webhook delivery: %w", err)
	}
	return nil
}

// Record persists an authenticated delivery and returns true only when this
// call inserted it. Retries with the same ID and payload are acknowledged as
// duplicates; reusing an ID for different bytes is rejected.
func (i Inbox) Record(ctx context.Context, delivery githubwebhook.VerifiedDelivery, receivedAt time.Time) (bool, error) {
	if i.Pool == nil {
		return false, errors.New("GitHub webhook inbox database is required")
	}
	if delivery.DeliveryID == "" || delivery.Event == "" || len(delivery.Payload) == 0 ||
		delivery.CorrelationID != "" && !webhookCorrelationIDPattern.MatchString(delivery.CorrelationID) {
		return false, errors.New("verified GitHub webhook delivery is incomplete")
	}
	normalized, err := githubwebhook.Normalize(delivery)
	if err != nil {
		return false, fmt.Errorf("normalize GitHub webhook delivery: %w", err)
	}
	normalizedJSON, err := json.Marshal(normalized)
	if err != nil {
		return false, fmt.Errorf("encode normalized GitHub event: %w", err)
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	integrationID := delivery.IntegrationID
	if integrationID == "" {
		integrationID = "shared"
	}
	if !validStoredIntegrationID(integrationID) {
		return false, errors.New("verified GitHub webhook integration identity is invalid")
	}
	if (integrationID == "shared" && delivery.CredentialGeneration != 0) || (integrationID != "shared" && delivery.CredentialGeneration < 1) {
		return false, errors.New("verified GitHub webhook credential generation is invalid")
	}
	var installationID any
	if normalized.InstallationID > 0 {
		installationID = normalized.InstallationID
	}
	digest := sha256.Sum256(delivery.Payload)
	payloadHash := hex.EncodeToString(digest[:])
	storedPayload, err := redactedStoredPayload(delivery.Event, delivery.Payload)
	if err != nil {
		return false, fmt.Errorf("redact GitHub webhook payload for storage: %w", err)
	}
	return transaction.Within(ctx, i.Pool, i.Policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		tag, err := tx.Exec(ctx, `INSERT INTO github_webhook_deliveries
			(integration_id, delivery_id, event_type, action, payload_json, payload_sha256, received_at, processing_status, event_schema_version, normalized_event_json, installation_id, credential_generation, actor_login, payload_expires_at, correlation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'received', $8, $9, $10, $11, $12, $13, NULLIF($14,''))
			ON CONFLICT (integration_id, delivery_id) DO NOTHING`, integrationID, delivery.DeliveryID, delivery.Event, delivery.Action, string(storedPayload), payloadHash, receivedAt, githubwebhook.RepositoryEventSchemaVersion, string(normalizedJSON), installationID, delivery.CredentialGeneration, normalized.Actor, receivedAt.Add(RawPayloadRetention), delivery.CorrelationID)
		if err != nil {
			return false, fmt.Errorf("insert GitHub webhook delivery: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return true, nil
		}
		var existingHash string
		var schemaVersion int
		if err := tx.QueryRow(ctx, `SELECT payload_sha256, event_schema_version FROM github_webhook_deliveries WHERE integration_id = $1 AND delivery_id = $2`, integrationID, delivery.DeliveryID).Scan(&existingHash, &schemaVersion); err != nil {
			return false, fmt.Errorf("read existing GitHub webhook delivery: %w", err)
		}
		if existingHash != payloadHash {
			return false, ErrDeliveryIDConflict
		}
		if schemaVersion == 0 {
			if _, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries
				SET event_schema_version = $3, normalized_event_json = $4, installation_id = $6, credential_generation = $7
				WHERE integration_id = $1 AND delivery_id = $2 AND payload_sha256 = $5 AND event_schema_version = 0`,
				integrationID, delivery.DeliveryID, githubwebhook.RepositoryEventSchemaVersion, string(normalizedJSON), payloadHash, installationID, delivery.CredentialGeneration); err != nil {
				return false, fmt.Errorf("backfill normalized GitHub event: %w", err)
			}
		}
		return false, nil
	})
}

// redactedStoredPayload removes free-form Issue comment text from the
// short-lived raw-payload column. The delivery hash is always computed from
// the exact authenticated bytes before this function is called, so duplicate
// and Delivery-ID conflict detection remain byte-accurate.
func redactedStoredPayload(event string, payload []byte) ([]byte, error) {
	if event != "issue_comment" {
		return append([]byte(nil), payload...), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, err
	}
	var comment map[string]json.RawMessage
	if raw, ok := object["comment"]; !ok || json.Unmarshal(raw, &comment) != nil || comment == nil {
		return nil, errors.New("issue comment object is missing or invalid")
	}
	delete(comment, "body")
	redacted, err := json.Marshal(comment)
	if err != nil {
		return nil, err
	}
	object["comment"] = redacted
	return json.Marshal(object)
}

func validStoredIntegrationID(value string) bool {
	if len(value) < 3 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
