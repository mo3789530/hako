package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
)

type tenantWorkspaceIdlePolicyRequest struct {
	Enabled            *bool `json:"enabled"`
	GracePeriodSeconds *int  `json:"grace_period_seconds"`
}

func getTenantWorkspaceIdlePolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		policy, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaceactivity.Policy, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return workspaceactivity.Policy{}, err
			}
			return workspaceactivity.GetPolicy(ctx, tx, tenantID)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, policy)
	}
}

func putTenantWorkspaceIdlePolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request tenantWorkspaceIdlePolicyRequest
		if err := decodeJSONRequest(c, &request); err != nil || request.Enabled == nil || request.GracePeriodSeconds == nil {
			return invalidWorkspaceIdlePolicy(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		policy := workspaceactivity.Policy{TenantID: tenantID, Enabled: *request.Enabled, GracePeriodSeconds: *request.GracePeriodSeconds}
		if policy.GracePeriodSeconds < int(workspaceactivity.MinimumIdleGracePeriod.Seconds()) ||
			policy.GracePeriodSeconds > int(workspaceactivity.MaximumIdleGracePeriod.Seconds()) {
			return invalidWorkspaceIdlePolicy(c)
		}
		now := time.Now().UTC()
		policy, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaceactivity.Policy, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return workspaceactivity.Policy{}, err
			}
			updated, err := workspaceactivity.PutPolicy(ctx, tx, policy, now)
			if err != nil {
				return workspaceactivity.Policy{}, err
			}
			details, err := json.Marshal(map[string]any{"enabled": updated.Enabled, "grace_period_seconds": updated.GracePeriodSeconds, "revision": updated.Revision})
			if err != nil {
				return workspaceactivity.Policy{}, fmt.Errorf("encode Workspace idle policy audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
				Action: "tenant.workspace_idle_policy.update", TargetType: "tenant_workspace_idle_policy", TargetID: string(tenantID),
				Details: details, OccurredAt: now}); err != nil {
				return workspaceactivity.Policy{}, err
			}
			return updated, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, policy)
	}
}

func invalidWorkspaceIdlePolicy(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request",
		Message: "Workspace idle policy requires enabled and a grace period between 300 and 86400 seconds"}})
}
