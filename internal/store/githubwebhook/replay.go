package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrInvalidDeliveryReplay     = errors.New("invalid GitHub webhook replay request")
	ErrDeliveryReplayNotFound    = errors.New("GitHub webhook delivery not found")
	ErrDeliveryReplayNotEligible = errors.New("GitHub webhook delivery is not eligible for replay")
	ErrDeliveryReplayRateLimited = errors.New("GitHub webhook delivery replay rate limit exceeded")
)

const (
	MaxWebhookReplayReasonBytes = 500
	MaxWebhookReplaysPerHour    = 3
)

// RequestTenantDeliveryReplay requeues only a failed Tenant-owned delivery.
// It rechecks current integration/Installation policy, updates the row and
// records the actor/reason atomically. The processor repeats authorization
// before applying any event side effect.
func RequestTenantDeliveryReplay(ctx context.Context, pool transaction.Beginner, policy transaction.Policy,
	tenantID domain.TenantID, integrationID, deliveryID string, actorID domain.UserID, reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if pool == nil || ctx == nil || tenantID == "" || actorID == "" || !validStoredIntegrationID(integrationID) || integrationID == "shared" ||
		!coreValidDeliveryID(deliveryID) || !validReplayReason(reason) || now.IsZero() {
		return ErrInvalidDeliveryReplay
	}
	now = now.UTC()
	_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var status string
		var schemaVersion int
		var normalizedJSON string
		var generation int64
		var payloadExpiresAt time.Time
		var payloadPurgedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT d.processing_status,d.event_schema_version,d.normalized_event_json,d.credential_generation,d.payload_expires_at,d.payload_purged_at
			FROM github_webhook_deliveries d
			JOIN tenant_github_integrations i ON i.tenant_id=$1 AND i.id=d.integration_id
			WHERE d.integration_id=$2 AND d.delivery_id=$3 FOR UPDATE`, tenantID, integrationID, deliveryID).
			Scan(&status, &schemaVersion, &normalizedJSON, &generation, &payloadExpiresAt, &payloadPurgedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, ErrDeliveryReplayNotFound
		}
		if err != nil {
			return struct{}{}, fmt.Errorf("load Tenant webhook delivery for replay: %w", err)
		}
		if status != "failed" || schemaVersion != hakowebhook.RepositoryEventSchemaVersion || payloadPurgedAt != nil || !payloadExpiresAt.After(now) {
			return struct{}{}, ErrDeliveryReplayNotEligible
		}
		var event hakowebhook.RepositoryEvent
		if json.Unmarshal([]byte(normalizedJSON), &event) != nil || event.DeliveryID != deliveryID || event.InstallationID <= 0 {
			return struct{}{}, ErrDeliveryReplayNotEligible
		}
		allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
		if err != nil {
			return struct{}{}, err
		}
		if !allowed {
			return struct{}{}, ErrStaleTenantWebhook
		}
		cutoff := now.Add(-time.Hour)
		tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries
			SET processing_status='received',
				replay_count=CASE WHEN replay_window_started_at IS NULL OR replay_window_started_at < $4 THEN 1 ELSE replay_count+1 END,
				replay_window_started_at=CASE WHEN replay_window_started_at IS NULL OR replay_window_started_at < $4 THEN $5 ELSE replay_window_started_at END
			WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='failed'
			AND (replay_window_started_at IS NULL OR replay_window_started_at < $4 OR replay_count < $3)`,
			integrationID, deliveryID, MaxWebhookReplaysPerHour, cutoff, now)
		if err != nil {
			return struct{}{}, fmt.Errorf("requeue Tenant webhook delivery: %w", err)
		}
		if tag.RowsAffected() != 1 {
			var currentStatus string
			var count int
			var windowStarted *time.Time
			if err := tx.QueryRow(ctx, `SELECT processing_status,replay_count,replay_window_started_at FROM github_webhook_deliveries
				WHERE integration_id=$1 AND delivery_id=$2`, integrationID, deliveryID).Scan(&currentStatus, &count, &windowStarted); err != nil {
				return struct{}{}, ErrDeliveryReplayNotEligible
			}
			if currentStatus == "failed" && windowStarted != nil && !windowStarted.Before(cutoff) && count >= MaxWebhookReplaysPerHour {
				return struct{}{}, ErrDeliveryReplayRateLimited
			}
			return struct{}{}, ErrDeliveryReplayNotEligible
		}
		details, _ := json.Marshal(struct {
			IntegrationID string `json:"integration_id"`
			DeliveryID    string `json:"delivery_id"`
			Reason        string `json:"reason"`
		}{integrationID, deliveryID, reason})
		if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: actorID, Action: "github.webhook.replay_requested",
			TargetType: "github_webhook_delivery", TargetID: deliveryID, Details: details, OccurredAt: now}); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

// RequestPlatformDeliveryReplay lets a verified platform operator requeue a
// Tenant-owned delivery while preserving current credential/Installation
// authorization. The replay state change and platform audit event are atomic.
func RequestPlatformDeliveryReplay(ctx context.Context, pool transaction.Beginner, policy transaction.Policy,
	integrationID, deliveryID string, actorID domain.UserID, reason string, now time.Time) (domain.TenantID, error) {
	reason = strings.TrimSpace(reason)
	if pool == nil || ctx == nil || actorID == "" || !validStoredIntegrationID(integrationID) || integrationID == "shared" ||
		!coreValidDeliveryID(deliveryID) || !validReplayReason(reason) || now.IsZero() {
		return "", ErrInvalidDeliveryReplay
	}
	now = now.UTC()
	tenantID, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.TenantID, error) {
		var status string
		var schemaVersion int
		var normalizedJSON string
		var generation int64
		var payloadExpiresAt time.Time
		var payloadPurgedAt *time.Time
		var tenant string
		err := tx.QueryRow(ctx, `SELECT d.processing_status,d.event_schema_version,d.normalized_event_json,d.credential_generation,
			d.payload_expires_at,d.payload_purged_at,i.tenant_id
			FROM github_webhook_deliveries d
			JOIN tenant_github_integrations i ON i.id=d.integration_id
			WHERE d.integration_id=$1 AND d.delivery_id=$2 AND i.status='active' FOR UPDATE`, integrationID, deliveryID).
			Scan(&status, &schemaVersion, &normalizedJSON, &generation, &payloadExpiresAt, &payloadPurgedAt, &tenant)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrDeliveryReplayNotFound
		}
		if err != nil {
			return "", fmt.Errorf("load platform webhook delivery for replay: %w", err)
		}
		if status != "failed" || schemaVersion != hakowebhook.RepositoryEventSchemaVersion || payloadPurgedAt != nil || !payloadExpiresAt.After(now) {
			return "", ErrDeliveryReplayNotEligible
		}
		var event hakowebhook.RepositoryEvent
		if json.Unmarshal([]byte(normalizedJSON), &event) != nil || event.DeliveryID != deliveryID || event.InstallationID <= 0 {
			return "", ErrDeliveryReplayNotEligible
		}
		allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
		if err != nil {
			return "", err
		}
		if !allowed {
			return "", ErrStaleTenantWebhook
		}
		cutoff := now.Add(-time.Hour)
		tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries
			SET processing_status='received',
				replay_count=CASE WHEN replay_window_started_at IS NULL OR replay_window_started_at < $4 THEN 1 ELSE replay_count+1 END,
				replay_window_started_at=CASE WHEN replay_window_started_at IS NULL OR replay_window_started_at < $4 THEN $5 ELSE replay_window_started_at END
			WHERE integration_id=$1 AND delivery_id=$2 AND processing_status='failed'
			AND payload_purged_at IS NULL AND payload_expires_at>$5
			AND (replay_window_started_at IS NULL OR replay_window_started_at < $4 OR replay_count < $3)`,
			integrationID, deliveryID, MaxWebhookReplaysPerHour, cutoff, now)
		if err != nil {
			return "", fmt.Errorf("requeue platform webhook delivery: %w", err)
		}
		if tag.RowsAffected() != 1 {
			var currentStatus string
			var count int
			var windowStarted *time.Time
			if err := tx.QueryRow(ctx, `SELECT processing_status,replay_count,replay_window_started_at FROM github_webhook_deliveries
				WHERE integration_id=$1 AND delivery_id=$2`, integrationID, deliveryID).Scan(&currentStatus, &count, &windowStarted); err != nil {
				return "", ErrDeliveryReplayNotEligible
			}
			if currentStatus == "failed" && windowStarted != nil && !windowStarted.Before(cutoff) && count >= MaxWebhookReplaysPerHour {
				return "", ErrDeliveryReplayRateLimited
			}
			return "", ErrDeliveryReplayNotEligible
		}
		auditID, err := idgen.New("paud_")
		if err != nil {
			return "", fmt.Errorf("generate platform webhook replay audit ID: %w", err)
		}
		details, _ := json.Marshal(struct {
			IntegrationID string `json:"integration_id"`
			DeliveryID    string `json:"delivery_id"`
			Reason        string `json:"reason"`
		}{integrationID, deliveryID, reason})
		if _, err := tx.Exec(ctx, `INSERT INTO platform_audit_events
			(id,actor_user_id,action,target_type,target_id,details_json,occurred_at)
			VALUES ($1,$2,'github.webhook.delivery.replay_requested','github_webhook_delivery',$3,$4,$5)`,
			auditID, actorID, integrationID+"/"+deliveryID, string(details), now); err != nil {
			return "", fmt.Errorf("append platform webhook replay audit: %w", err)
		}
		return domain.TenantID(tenant), nil
	})
	return tenantID, err
}

func validReplayReason(reason string) bool {
	if len(reason) < 8 || len(reason) > MaxWebhookReplayReasonBytes || strings.ContainsAny(reason, "\r\n\x00") {
		return false
	}
	for _, char := range reason {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}
