package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/quotas"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type tenantQuotaRequest struct {
	Workspaces             int16 `json:"workspaces_limit"`
	ConcurrentWorkloadRuns int16 `json:"workload_runs_limit"`
	PreviewRuns            int16 `json:"preview_runs_limit"`
	PreviewLifetimeSeconds int64 `json:"preview_lifetime_seconds_limit"`
	ArtifactBytes          int64 `json:"artifact_bytes_limit"`
	ServiceMemoryBytes     int64 `json:"service_memory_bytes_limit"`
	ServiceMilliCPU        int64 `json:"service_milli_cpu_limit"`
}

func getTenantQuotas(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		limits, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return quotas.Limits{}, err
			}
			return quotas.Read(ctx, tx, tenantID)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, limits)
	}
}

func putTenantQuotas(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request tenantQuotaRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidTenantQuotaRequest(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		now := time.Now().UTC()
		limits, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return quotas.Limits{}, err
			}
			current, err := quotas.Read(ctx, tx, tenantID)
			if err != nil {
				return quotas.Limits{}, err
			}
			if request.PreviewRuns == 0 {
				request.PreviewRuns = current.PreviewRuns
			}
			if request.PreviewLifetimeSeconds == 0 {
				request.PreviewLifetimeSeconds = current.PreviewLifetimeSeconds
			}
			updated, err := quotas.SetWithPreview(ctx, tx, tenantID, user.ID, request.Workspaces, request.ConcurrentWorkloadRuns,
				request.PreviewRuns, request.PreviewLifetimeSeconds, request.ArtifactBytes, request.ServiceMemoryBytes, request.ServiceMilliCPU, now)
			if err != nil {
				return quotas.Limits{}, err
			}
			details, err := json.Marshal(map[string]any{"workspaces_limit": updated.Workspaces, "workload_runs_limit": updated.ConcurrentWorkloadRuns,
				"preview_runs_limit": updated.PreviewRuns, "preview_lifetime_seconds_limit": updated.PreviewLifetimeSeconds,
				"artifact_bytes_limit": updated.ArtifactBytes, "service_memory_bytes_limit": updated.WorkspaceServiceMemoryBytes,
				"service_milli_cpu_limit": updated.WorkspaceServiceMilliCPU})
			if err != nil {
				return quotas.Limits{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
				Action: "tenant.quota.update", TargetType: "tenant_quota", TargetID: string(tenantID), Details: details, OccurredAt: now}); err != nil {
				return quotas.Limits{}, err
			}
			return updated, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, quotas.ErrInvalidLimits) {
			return invalidTenantQuotaRequest(c)
		}
		if errors.Is(err, quotas.ErrBelowCurrentUsage) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "quota_below_current_usage", Message: "Tenant quota cannot be lower than currently reserved usage"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, limits)
	}
}

func invalidTenantQuotaRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_quota", Message: "Workspace limit 1-10, Workload Run limit 1-5, Preview limit 1-5, Preview lifetime 300-86400 seconds, artifact bytes 1-107374182400, Service memory bytes 1-2147483648, and Service millicores 1-1500 are required"}})
}
