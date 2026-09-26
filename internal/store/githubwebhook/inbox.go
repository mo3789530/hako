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

type Inbox struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

// ProcessInstallationRepositoryDelivery applies one stored schema-v1
// Installation repository event and marks its inbox row processed in the
// caller's transaction. An inactive Installation or unsupported event leaves
// the delivery unchanged so another consumer or a later retry can handle it.
func ProcessInstallationRepositoryDelivery(ctx context.Context, tx pgx.Tx, deliveryID string, processedAt time.Time) (bool, error) {
	if tx == nil || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	var status string
	var schemaVersion int
	var eventJSON string
	err := tx.QueryRow(ctx, `SELECT processing_status, event_schema_version, normalized_event_json
		FROM github_webhook_deliveries WHERE delivery_id = $1`, deliveryID).Scan(&status, &schemaVersion, &eventJSON)
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
		WHERE delivery_id = $1 AND processing_status = 'received' AND event_schema_version = $2`, deliveryID, schemaVersion)
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
	if tx == nil || !coreValidDeliveryID(deliveryID) {
		return false, errors.New("transaction and valid GitHub Delivery ID are required")
	}
	var status, eventType, action, eventJSON string
	var schemaVersion int
	err := tx.QueryRow(ctx, `SELECT processing_status, event_type, action, event_schema_version, normalized_event_json
		FROM github_webhook_deliveries WHERE delivery_id = $1`, deliveryID).
		Scan(&status, &eventType, &action, &schemaVersion, &eventJSON)
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
		WHERE delivery_id = $1 AND processing_status = 'received' AND event_schema_version = $2`, deliveryID, schemaVersion)
	if err != nil {
		return false, fmt.Errorf("mark GitHub Installation event processed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("GitHub Installation event processing state changed concurrently")
	}
	return true, nil
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

// Record persists an authenticated delivery and returns true only when this
// call inserted it. Retries with the same ID and payload are acknowledged as
// duplicates; reusing an ID for different bytes is rejected.
func (i Inbox) Record(ctx context.Context, delivery githubwebhook.VerifiedDelivery, receivedAt time.Time) (bool, error) {
	if i.Pool == nil {
		return false, errors.New("GitHub webhook inbox database is required")
	}
	if delivery.DeliveryID == "" || delivery.Event == "" || len(delivery.Payload) == 0 {
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
	var installationID any
	if normalized.InstallationID > 0 {
		installationID = normalized.InstallationID
	}
	digest := sha256.Sum256(delivery.Payload)
	payloadHash := hex.EncodeToString(digest[:])
	return transaction.Within(ctx, i.Pool, i.Policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		tag, err := tx.Exec(ctx, `INSERT INTO github_webhook_deliveries
			(delivery_id, event_type, action, payload_json, payload_sha256, received_at, processing_status, event_schema_version, normalized_event_json, installation_id)
			VALUES ($1, $2, $3, $4, $5, $6, 'received', $7, $8, $9)
			ON CONFLICT (delivery_id) DO NOTHING`, delivery.DeliveryID, delivery.Event, delivery.Action, string(delivery.Payload), payloadHash, receivedAt, githubwebhook.RepositoryEventSchemaVersion, string(normalizedJSON), installationID)
		if err != nil {
			return false, fmt.Errorf("insert GitHub webhook delivery: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return true, nil
		}
		var existingHash string
		var schemaVersion int
		if err := tx.QueryRow(ctx, `SELECT payload_sha256, event_schema_version FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&existingHash, &schemaVersion); err != nil {
			return false, fmt.Errorf("read existing GitHub webhook delivery: %w", err)
		}
		if existingHash != payloadHash {
			return false, ErrDeliveryIDConflict
		}
		if schemaVersion == 0 {
			if _, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries
				SET event_schema_version = $2, normalized_event_json = $3, installation_id = $5
				WHERE delivery_id = $1 AND payload_sha256 = $4 AND event_schema_version = 0`,
				delivery.DeliveryID, githubwebhook.RepositoryEventSchemaVersion, string(normalizedJSON), payloadHash, installationID); err != nil {
				return false, fmt.Errorf("backfill normalized GitHub event: %w", err)
			}
		}
		return false, nil
	})
}
