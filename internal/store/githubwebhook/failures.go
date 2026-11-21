package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrTenantIntegrationNotFound = errors.New("Tenant GitHub App integration not found")

type FailedDelivery struct {
	DeliveryID    string     `json:"delivery_id"`
	EventType     string     `json:"event_type"`
	Action        string     `json:"action"`
	ActorLogin    string     `json:"actor_login,omitempty"`
	ReceivedAt    time.Time  `json:"received_at"`
	PayloadPurged *time.Time `json:"payload_purged_at,omitempty"`
	ReplayCount   int        `json:"replay_count"`
}

// PlatformFailedDelivery is the operator-safe view across Tenant-owned App
// integrations. NormalizedEvent is read only to project identifiers and is
// never returned to the API caller.
type PlatformFailedDelivery struct {
	FailedDelivery
	TenantID       domain.TenantID `json:"tenant_id"`
	IntegrationID  string          `json:"integration_id"`
	InstallationID *int64          `json:"installation_id,omitempty"`
	RepositoryID   *int64          `json:"repository_id,omitempty"`
	ReplayEligible bool            `json:"replay_eligible"`
}

// ListPlatformFailedDeliveries returns a bounded page of redacted failed
// deliveries owned by Tenant integrations. Legacy shared deliveries are
// intentionally excluded because they have no unambiguous Tenant owner.
func ListPlatformFailedDeliveries(ctx context.Context, pool transaction.Beginner, policy transaction.Policy,
	limit, offset int, now time.Time) ([]PlatformFailedDelivery, int64, error) {
	if ctx == nil || pool == nil || limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		return nil, 0, errors.New("database and valid pagination are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	type page struct {
		items []PlatformFailedDelivery
		total int64
	}
	result, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (page, error) {
		var total int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM github_webhook_deliveries d
			JOIN tenant_github_integrations i ON i.id=d.integration_id
			WHERE d.integration_id <> 'shared' AND d.processing_status='failed'`).Scan(&total); err != nil {
			return page{}, fmt.Errorf("count platform failed webhook deliveries: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT d.integration_id,i.tenant_id,d.delivery_id,d.event_type,d.action,d.actor_login,
			d.received_at,d.payload_purged_at,d.replay_count,d.installation_id,d.normalized_event_json,
			(d.event_schema_version=$3 AND d.payload_purged_at IS NULL AND d.payload_expires_at>$4 AND i.status='active'
			 AND i.credential_generation=d.credential_generation AND c.status='approved' AND r.mode='tenant_owned'
			 AND r.owner_tenant_id=i.tenant_id AND EXISTS (
				SELECT 1 FROM tenant_github_app_installation_bindings b
				JOIN tenant_github_installations ti ON ti.tenant_id=b.tenant_id AND ti.installation_id=b.installation_id
				WHERE b.tenant_id=i.tenant_id AND b.integration_id=i.id AND b.installation_id=d.installation_id
				AND ti.status='active' AND ti.suspended_at IS NULL
			 )) AS replay_eligible
			FROM github_webhook_deliveries d
			JOIN tenant_github_integrations i ON i.id=d.integration_id
			JOIN github_app_registrations r ON r.id=i.app_registration_id AND r.github_app_id=i.github_app_id
			JOIN github_credential_bindings c ON c.tenant_id=i.tenant_id AND c.id=i.credential_binding_id AND c.github_app_id=i.github_app_id
			WHERE d.integration_id <> 'shared' AND d.processing_status='failed'
			ORDER BY d.received_at DESC,d.integration_id,d.delivery_id
			LIMIT $1 OFFSET $2`, limit, offset, hakowebhook.RepositoryEventSchemaVersion, now.UTC())
		if err != nil {
			return page{}, fmt.Errorf("list platform failed webhook deliveries: %w", err)
		}
		defer rows.Close()
		result := page{items: make([]PlatformFailedDelivery, 0), total: total}
		for rows.Next() {
			var item PlatformFailedDelivery
			var normalized string
			if err := rows.Scan(&item.IntegrationID, &item.TenantID, &item.DeliveryID, &item.EventType, &item.Action,
				&item.ActorLogin, &item.ReceivedAt, &item.PayloadPurged, &item.ReplayCount, &item.InstallationID, &normalized, &item.ReplayEligible); err != nil {
				return page{}, fmt.Errorf("scan platform failed webhook delivery: %w", err)
			}
			var event struct {
				Repository *struct {
					GitHubID int64 `json:"github_id"`
				} `json:"repository"`
			}
			if json.Unmarshal([]byte(normalized), &event) == nil && event.Repository != nil && event.Repository.GitHubID > 0 {
				repositoryID := event.Repository.GitHubID
				item.RepositoryID = &repositoryID
			}
			result.items = append(result.items, item)
		}
		if err := rows.Err(); err != nil {
			return page{}, fmt.Errorf("read platform failed webhook deliveries: %w", err)
		}
		return result, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return result.items, result.total, nil
}

// ListTenantFailedDeliveries returns only redacted delivery metadata. It never
// reads the raw body or normalized event and first verifies that the requested
// integration belongs to the path Tenant.
func ListTenantFailedDeliveries(ctx context.Context, pool transaction.Beginner, policy transaction.Policy,
	tenantID domain.TenantID, integrationID string, limit, offset int) ([]FailedDelivery, int64, error) {
	if ctx == nil || pool == nil || tenantID == "" || !validStoredIntegrationID(integrationID) || integrationID == "shared" ||
		limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		return nil, 0, errors.New("Tenant, owned integration, database, and valid pagination are required")
	}
	type page struct {
		items []FailedDelivery
		total int64
	}
	result, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (page, error) {
		var integrationExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_github_integrations WHERE tenant_id=$1 AND id=$2)`, tenantID, integrationID).Scan(&integrationExists); err != nil {
			return page{}, fmt.Errorf("authorize failed Tenant webhook delivery listing: %w", err)
		}
		if !integrationExists {
			return page{}, ErrTenantIntegrationNotFound
		}
		var total int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM github_webhook_deliveries d
			JOIN tenant_github_integrations i ON i.id=d.integration_id AND i.tenant_id=$1
			WHERE d.integration_id=$2 AND d.processing_status='failed'`, tenantID, integrationID).Scan(&total); err != nil {
			return page{}, fmt.Errorf("count failed Tenant webhook deliveries: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT d.delivery_id,d.event_type,d.action,d.actor_login,d.received_at,d.payload_purged_at,d.replay_count
			FROM github_webhook_deliveries d
			JOIN tenant_github_integrations i ON i.id=d.integration_id AND i.tenant_id=$1
			WHERE d.integration_id=$2 AND d.processing_status='failed'
			ORDER BY d.received_at DESC,d.delivery_id
			LIMIT $3 OFFSET $4`, tenantID, integrationID, limit, offset)
		if err != nil {
			return page{}, fmt.Errorf("list failed Tenant webhook deliveries: %w", err)
		}
		defer rows.Close()
		result := page{items: make([]FailedDelivery, 0), total: total}
		for rows.Next() {
			var item FailedDelivery
			if err := rows.Scan(&item.DeliveryID, &item.EventType, &item.Action, &item.ActorLogin, &item.ReceivedAt, &item.PayloadPurged, &item.ReplayCount); err != nil {
				return page{}, fmt.Errorf("scan failed Tenant webhook delivery: %w", err)
			}
			result.items = append(result.items, item)
		}
		if err := rows.Err(); err != nil {
			return page{}, fmt.Errorf("read failed Tenant webhook deliveries: %w", err)
		}
		return result, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return result.items, result.total, nil
}
