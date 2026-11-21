package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
)

// LoadAuthorizedRepositoryEvent returns only an authenticated, currently
// authorized repository event for this Tenant. Callers must still apply their
// own trigger/provenance policy before creating a Workload Run.
func LoadAuthorizedRepositoryEvent(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, integrationID, deliveryID string) (hakowebhook.RepositoryEvent, error) {
	if ctx == nil || tx == nil || tenantID == "" || !validStoredIntegrationID(integrationID) || !coreValidDeliveryID(deliveryID) {
		return hakowebhook.RepositoryEvent{}, ErrUnsupportedWebhookEvent
	}
	var status, normalizedJSON string
	var schemaVersion int
	var generation int64
	err := tx.QueryRow(ctx, `SELECT processing_status,event_schema_version,normalized_event_json,credential_generation
		FROM github_webhook_deliveries WHERE integration_id=$1 AND delivery_id=$2`, integrationID, deliveryID).
		Scan(&status, &schemaVersion, &normalizedJSON, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return hakowebhook.RepositoryEvent{}, pgx.ErrNoRows
	}
	if err != nil {
		return hakowebhook.RepositoryEvent{}, fmt.Errorf("load repository event for workload: %w", err)
	}
	if (status != "received" && status != "processed") || schemaVersion != hakowebhook.RepositoryEventSchemaVersion {
		return hakowebhook.RepositoryEvent{}, ErrUnsupportedWebhookEvent
	}
	var event hakowebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(normalizedJSON), &event); err != nil || event.SchemaVersion != schemaVersion || event.DeliveryID != deliveryID ||
		event.InstallationID <= 0 || event.Repository == nil || event.Repository.GitHubID <= 0 || event.CommitSHA == "" || event.Ref == "" {
		return hakowebhook.RepositoryEvent{}, ErrUnsupportedWebhookEvent
	}
	allowed, err := authorizeStoredTenantEvent(ctx, tx, integrationID, generation, event)
	if err != nil {
		return hakowebhook.RepositoryEvent{}, err
	}
	if !allowed {
		return hakowebhook.RepositoryEvent{}, ErrStaleTenantWebhook
	}
	if integrationID == "shared" {
		var tenantBound, ownedIntegrationActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM github_app_installation_bindings b
			JOIN tenant_github_installations i ON i.tenant_id=b.tenant_id AND i.installation_id=b.installation_id
			WHERE b.tenant_id=$1 AND b.installation_id=$2 AND i.status='active' AND i.suspended_at IS NULL
		), EXISTS (
			SELECT 1 FROM tenant_github_integrations i
			JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id
			WHERE i.tenant_id=$1 AND i.status='active' AND b.installation_id=$2
		)`, tenantID, event.InstallationID).Scan(&tenantBound, &ownedIntegrationActive); err != nil {
			return hakowebhook.RepositoryEvent{}, fmt.Errorf("resolve shared webhook Tenant binding: %w", err)
		}
		if !tenantBound || ownedIntegrationActive {
			return hakowebhook.RepositoryEvent{}, ErrStaleTenantWebhook
		}
	} else {
		var integrationTenant string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM tenant_github_integrations WHERE id=$1 AND status='active'`, integrationID).Scan(&integrationTenant); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return hakowebhook.RepositoryEvent{}, ErrStaleTenantWebhook
			}
			return hakowebhook.RepositoryEvent{}, fmt.Errorf("resolve Tenant integration for repository event: %w", err)
		}
		if domain.TenantID(integrationTenant) != tenantID {
			return hakowebhook.RepositoryEvent{}, ErrStaleTenantWebhook
		}
	}
	return event, nil
}
