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
	"github.com/mo3789530/hako/internal/softwarecatalog"
	"github.com/mo3789530/hako/internal/store/audit"
	storecatalog "github.com/mo3789530/hako/internal/store/softwarecatalog"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type softwareCatalogRequest struct {
	OwnerTeam          string   `json:"owner_team"`
	InstallationID     int64    `json:"installation_id"`
	GitHubRepositoryID int64    `json:"github_repository_id"`
	Runtime            string   `json:"runtime"`
	Language           string   `json:"language"`
	Dependencies       []string `json:"dependencies"`
	ProductionURL      string   `json:"production_url,omitempty"`
}

func listTenantSoftwareCatalog(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		items, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]softwarecatalog.Service, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return nil, err
			}
			return storecatalog.List(ctx, tx, string(tenantID))
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"items": items})
	}
}

func upsertTenantSoftwareCatalogService(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request softwareCatalogRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidSoftwareCatalog(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		item := softwarecatalog.Service{TenantID: string(tenantID), ServiceID: strings.TrimSpace(c.Param("service_id")),
			OwnerTeam: strings.TrimSpace(request.OwnerTeam), InstallationID: request.InstallationID,
			GitHubRepositoryID: request.GitHubRepositoryID, Runtime: strings.TrimSpace(request.Runtime),
			Language: strings.TrimSpace(request.Language), Dependencies: append([]string(nil), request.Dependencies...),
			ProductionURL: strings.TrimSpace(request.ProductionURL)}
		if softwarecatalog.Validate(item) != nil {
			return invalidSoftwareCatalog(c)
		}
		now := time.Now().UTC()
		stored, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (softwarecatalog.Service, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return softwarecatalog.Service{}, err
			}
			stored, err := storecatalog.Upsert(ctx, tx, item, string(user.ID), now)
			if err != nil {
				return softwarecatalog.Service{}, err
			}
			details, err := json.Marshal(map[string]any{"runtime": stored.Runtime, "language": stored.Language,
				"installation_id": stored.InstallationID, "github_repository_id": stored.GitHubRepositoryID,
				"dependency_count": len(stored.Dependencies), "production_url_configured": stored.ProductionURL != ""})
			if err != nil {
				return softwarecatalog.Service{}, errors.New("encode Software Catalog audit details")
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
				Action: "tenant.software_catalog.upsert", TargetType: "software_catalog_service",
				TargetID: stored.ServiceID, Details: details, OccurredAt: now}); err != nil {
				return softwarecatalog.Service{}, err
			}
			return stored, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, storecatalog.ErrRepositoryNotFound) {
			return tenantNotFound(c)
		}
		if errors.Is(err, softwarecatalog.ErrInvalid) || errors.Is(err, storecatalog.ErrDependencyNotFound) {
			return invalidSoftwareCatalog(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, stored)
	}
}

func invalidSoftwareCatalog(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_software_catalog_service",
		Message: "Service ID, owner team, Tenant Repository, Lambda MicroVM runtime, supported language, valid dependencies, and an HTTPS production URL are required"}})
}
