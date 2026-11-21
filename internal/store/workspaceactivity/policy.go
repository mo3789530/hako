package workspaceactivity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

const (
	DefaultIdleGracePeriod = 15 * time.Minute
	MinimumIdleGracePeriod = 5 * time.Minute
	MaximumIdleGracePeriod = 24 * time.Hour
)

type Policy struct {
	TenantID           domain.TenantID `json:"tenant_id"`
	Enabled            bool            `json:"enabled"`
	GracePeriodSeconds int             `json:"grace_period_seconds"`
	Revision           int64           `json:"revision"`
	UpdatedAt          *time.Time      `json:"updated_at,omitempty"`
}

func GetPolicy(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) (Policy, error) {
	if ctx == nil || tx == nil || tenantID == "" {
		return Policy{}, errors.New("transaction and Tenant ID are required")
	}
	policy := Policy{TenantID: tenantID, Enabled: false, GracePeriodSeconds: int(DefaultIdleGracePeriod.Seconds()), Revision: 0}
	var updatedAt time.Time
	err := tx.QueryRow(ctx, `SELECT enabled, grace_period_seconds, revision, updated_at
		FROM tenant_workspace_idle_policies WHERE tenant_id = $1`, tenantID).
		Scan(&policy.Enabled, &policy.GracePeriodSeconds, &policy.Revision, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("get Tenant Workspace idle policy: %w", err)
	}
	policy.UpdatedAt = &updatedAt
	return policy, nil
}

func PutPolicy(ctx context.Context, tx pgx.Tx, policy Policy, now time.Time) (Policy, error) {
	if ctx == nil || tx == nil || policy.TenantID == "" || policy.GracePeriodSeconds < int(MinimumIdleGracePeriod.Seconds()) ||
		policy.GracePeriodSeconds > int(MaximumIdleGracePeriod.Seconds()) {
		return Policy{}, errors.New("invalid Tenant Workspace idle policy")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var updated Policy
	var updatedAt time.Time
	err := tx.QueryRow(ctx, `INSERT INTO tenant_workspace_idle_policies
		(tenant_id, enabled, grace_period_seconds, revision, updated_at)
		VALUES ($1, $2, $3, 1, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET enabled = EXCLUDED.enabled,
			grace_period_seconds = EXCLUDED.grace_period_seconds,
			revision = tenant_workspace_idle_policies.revision + 1, updated_at = EXCLUDED.updated_at
		RETURNING tenant_id, enabled, grace_period_seconds, revision, updated_at`,
		policy.TenantID, policy.Enabled, policy.GracePeriodSeconds, now.UTC()).
		Scan(&updated.TenantID, &updated.Enabled, &updated.GracePeriodSeconds, &updated.Revision, &updatedAt)
	if err != nil {
		return Policy{}, fmt.Errorf("save Tenant Workspace idle policy: %w", err)
	}
	updated.UpdatedAt = &updatedAt
	return updated, nil
}
