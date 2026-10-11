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
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type tenantGitHubAppMigrationRequest struct {
	Action             string  `json:"action"`
	ExpectedRevision   int64   `json:"expected_revision"`
	ExpectedGeneration int64   `json:"expected_generation"`
	InstallationIDs    []int64 `json:"installation_ids"`
	Confirm            bool    `json:"confirm,omitempty"`
}

func migrateTenantGitHubApp(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		request := tenantGitHubAppMigrationRequest{}
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return githubAppError(c, githubregistry.ErrInvalidApp)
		}
		if request.Action != "plan" && request.Action != "backfill" || request.Action == "backfill" && !request.Confirm {
			return githubAppError(c, githubregistry.ErrInvalidApp)
		}
		tenant := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		integrationID := c.Param("integration_id")
		migrationRequest := githubregistry.TenantAppMigrationRequest{
			TenantID: tenant, IntegrationID: integrationID,
			ExpectedRevision: request.ExpectedRevision, ExpectedGeneration: request.ExpectedGeneration,
			InstallationIDs: request.InstallationIDs,
		}
		now := time.Now().UTC()
		plan, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.TenantAppMigrationPlan, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenant, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return githubregistry.TenantAppMigrationPlan{}, err
			}
			var result githubregistry.TenantAppMigrationPlan
			var err error
			if request.Action == "backfill" {
				result, err = githubregistry.BackfillTenantAppInstallationBindingsTx(ctx, tx, migrationRequest, now)
			} else {
				result, err = githubregistry.PlanTenantAppMigrationTx(ctx, tx, migrationRequest)
			}
			if err != nil {
				return githubregistry.TenantAppMigrationPlan{}, err
			}
			if request.Action == "backfill" {
				details, _ := json.Marshal(map[string]any{
					"integration_id":        result.IntegrationID,
					"config_revision":       result.ConfigRevision,
					"credential_generation": result.CredentialGeneration,
					"installation_ids":      request.InstallationIDs,
				})
				if err := audit.Append(ctx, tx, audit.Event{TenantID: tenant, ActorID: user.ID, Action: "tenant.github_app.installation_backfill", TargetType: "github_app_integration", TargetID: integrationID, Details: details, OccurredAt: now}); err != nil {
					return githubregistry.TenantAppMigrationPlan{}, err
				}
			}
			return result, nil
		})
		if err != nil {
			if errors.Is(err, githubregistry.ErrTenantAppMigrationInvalid) {
				return githubAppError(c, githubregistry.ErrInvalidApp)
			}
			return githubAppError(c, err)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, GitHubAppMetadataResult{Migration: &plan})
	}
}
