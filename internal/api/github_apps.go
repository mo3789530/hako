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
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var errGitHubAppNotReady = errors.New("Tenant GitHub App verification/activation is not implemented")

type GitHubAppMetadataResult struct {
	Integration *githubregistry.AppIntegration  `json:"integration,omitempty"`
	Items       []githubregistry.AppIntegration `json:"items,omitempty"`
	Limit       int                             `json:"limit,omitempty"`
	Offset      int                             `json:"offset,omitempty"`
}

func githubAppOwnerMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		membership, ok := c.Get(membershipKey).(domain.TenantMembership)
		if !ok || (membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin) {
			return tenantNotFound(c)
		}
		return next(c)
	}
}

func githubAppMetadata(pool transaction.Beginner, action string) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenant := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		id := c.Param("integration_id")
		var draft githubregistry.AppDraftRequest
		var revision struct {
			ExpectedRevision int64 `json:"expected_revision"`
		}
		limit, offset := 50, 0
		if action == "list" {
			var err error
			limit, offset, err = parseWorkspacePage(c)
			if _, explicitlySet := c.QueryParams()["limit"]; !explicitlySet {
				limit = 50
			}
			if err != nil || offset > 10000 {
				return githubAppError(c, githubregistry.ErrInvalidApp)
			}
		} else if action == "add" {
			if err := decodeJSONRequest(c, &draft); err != nil {
				if isRequestTooLarge(err) {
					return requestTooLarge(c)
				}
				return githubAppError(c, githubregistry.ErrInvalidApp)
			}
		} else if action != "show" {
			if err := decodeJSONRequest(c, &revision); err != nil {
				if isRequestTooLarge(err) {
					return requestTooLarge(c)
				}
				return githubAppError(c, githubregistry.ErrInvalidApp)
			}
			if revision.ExpectedRevision < 1 {
				return githubAppError(c, githubregistry.ErrInvalidApp)
			}
		}
		integrationID, err := idgen.New("gint_")
		if err != nil {
			return databaseUnavailable(c)
		}
		registrationID, err := idgen.New("gapp_")
		if err != nil {
			return databaseUnavailable(c)
		}
		now := time.Now().UTC()
		type result struct {
			wire    GitHubAppMetadataResult
			created bool
		}
		out, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (result, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenant, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return result{}, err
			}
			var app githubregistry.AppIntegration
			var created bool
			var err error
			if action == "list" {
				items, err := githubregistry.ListApps(ctx, tx, tenant, limit, offset)
				return result{wire: GitHubAppMetadataResult{Items: items, Limit: limit, Offset: offset}}, err
			}
			switch action {
			case "add":
				app, created, err = githubregistry.CreateAppDraft(ctx, tx, tenant, user.ID, integrationID, registrationID, c.Request().Header.Get("Idempotency-Key"), draft, now)
			case "disable":
				app, err = githubregistry.DisableApp(ctx, tx, tenant, id, revision.ExpectedRevision, now)
			default:
				app, err = githubregistry.GetApp(ctx, tx, tenant, id)
			}
			if err != nil {
				return result{}, err
			}
			if action == "verify" || action == "select-default" {
				if app.ConfigRevision != revision.ExpectedRevision {
					return result{}, githubregistry.ErrAppConflict
				}
				return result{}, errGitHubAppNotReady
			}
			if action == "disable" || (action == "add" && created) {
				details, _ := json.Marshal(map[string]any{"github_app_id": app.GitHubAppID, "status": app.Status, "config_revision": app.ConfigRevision})
				if err := audit.Append(ctx, tx, audit.Event{TenantID: tenant, ActorID: user.ID, Action: "tenant.github_app." + action, TargetType: "github_app_integration", TargetID: app.ID, Details: details, OccurredAt: now}); err != nil {
					return result{}, err
				}
			}
			return result{wire: GitHubAppMetadataResult{Integration: &app}, created: created}, nil
		})
		if err != nil {
			return githubAppError(c, err)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		status := http.StatusOK
		if out.created {
			status = http.StatusCreated
		}
		return c.JSON(status, out.wire)
	}
}

func githubAppError(c *echo.Context, err error) error {
	switch {
	case errors.Is(err, authz.ErrTenantAccessDenied), errors.Is(err, githubregistry.ErrAppNotFound), errors.Is(err, githubregistry.ErrAppBindingDenied):
		return tenantNotFound(c)
	case errors.Is(err, githubregistry.ErrInvalidApp):
		return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "valid App metadata, approved binding, and request key/revision are required"}})
	case errors.Is(err, githubregistry.ErrAppIdempotencyConflict):
		return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "idempotency_conflict", Message: "request key was used for different App metadata"}})
	case errors.Is(err, githubregistry.ErrAppConflict):
		return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "state_conflict", Message: "GitHub App metadata conflicts with current state"}})
	case errors.Is(err, errGitHubAppNotReady):
		return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "github_app_not_ready", Message: "Tenant App verification and activation are not yet available"}})
	default:
		return databaseUnavailable(c)
	}
}
