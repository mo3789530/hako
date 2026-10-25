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
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var errGitHubAppNotReady = errors.New("Tenant GitHub App proof verifier is unavailable")
var errGitHubAppVerificationFailed = errors.New("Tenant GitHub App identity verification failed")

type TenantGitHubAppProofVerifier interface {
	// Implementations used by the API must invoke the trusted credential
	// broker. The API process must never assume a key-reader role or read a
	// Tenant private-key Secret itself.
	VerifyApp(context.Context, githubregistry.AppProofCredential) error
}

type TenantGitHubAppProofVerifierFunc func(context.Context, githubregistry.AppProofCredential) error

func (f TenantGitHubAppProofVerifierFunc) VerifyApp(ctx context.Context, credential githubregistry.AppProofCredential) error {
	return f(ctx, credential)
}

type GitHubAppMetadataResult struct {
	Integration *githubregistry.AppIntegration         `json:"integration,omitempty"`
	Items       []githubregistry.AppIntegration        `json:"items,omitempty"`
	Deliveries  []GitHubWebhookDeliverySummary         `json:"deliveries,omitempty"`
	Migration   *githubregistry.TenantAppMigrationPlan `json:"migration,omitempty"`
	Total       int64                                  `json:"total,omitempty"`
	Limit       int                                    `json:"limit,omitempty"`
	Offset      int                                    `json:"offset,omitempty"`
	SetupURL    string                                 `json:"setup_url,omitempty"`
	ExpiresAt   *time.Time                             `json:"expires_at,omitempty"`
}

func listTenantGitHubFailedDeliveries(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if pool == nil {
			return databaseUnavailable(c)
		}
		limit, offset, err := parseWorkspacePage(c)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_pagination", Message: "Invalid pagination query"}})
		}
		items, total, err := webhookstore.ListTenantFailedDeliveries(c.Request().Context(), pool, transaction.DefaultPolicy(),
			domain.TenantID(strings.TrimSpace(c.Param("tenant_id"))), strings.TrimSpace(c.Param("integration_id")), limit, offset)
		if errors.Is(err, webhookstore.ErrTenantIntegrationNotFound) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		deliveries := make([]GitHubWebhookDeliverySummary, 0, len(items))
		for _, item := range items {
			deliveries = append(deliveries, GitHubWebhookDeliverySummary{DeliveryID: item.DeliveryID, EventType: item.EventType,
				Action: item.Action, ActorLogin: item.ActorLogin, ReceivedAt: item.ReceivedAt, PayloadPurgedAt: item.PayloadPurged, ReplayCount: item.ReplayCount})
		}
		return c.JSON(http.StatusOK, GitHubAppMetadataResult{Deliveries: deliveries, Total: total, Limit: limit, Offset: offset})
	}
}

type webhookReplayRequest struct {
	Reason string `json:"reason"`
}

type webhookReplayResponse struct {
	DeliveryID string `json:"delivery_id"`
	Status     string `json:"status"`
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

func replayTenantGitHubDelivery(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var input webhookReplayRequest
		if err := decodeJSONRequest(c, &input); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return githubWebhookReplayError(c, http.StatusBadRequest, "invalid_request", "A replay reason is required")
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		integrationID := strings.TrimSpace(c.Param("integration_id"))
		deliveryID := strings.TrimSpace(c.Param("delivery_id"))
		if tenantID == "" || integrationID == "" || deliveryID == "" || len(strings.TrimSpace(input.Reason)) < 8 || len(strings.TrimSpace(input.Reason)) > webhookstore.MaxWebhookReplayReasonBytes {
			return githubWebhookReplayError(c, http.StatusBadRequest, "invalid_request", "A replay reason between 8 and 500 bytes is required")
		}
		err := webhookstore.RequestTenantDeliveryReplay(c.Request().Context(), pool, transaction.DefaultPolicy(), tenantID,
			integrationID, deliveryID, user.ID, input.Reason, time.Now().UTC())
		switch {
		case err == nil:
			return c.JSON(http.StatusAccepted, webhookReplayResponse{DeliveryID: deliveryID, Status: "received"})
		case errors.Is(err, webhookstore.ErrInvalidDeliveryReplay):
			return githubWebhookReplayError(c, http.StatusBadRequest, "invalid_request", "Replay request is invalid")
		case errors.Is(err, webhookstore.ErrDeliveryReplayNotFound):
			return tenantNotFound(c)
		case errors.Is(err, webhookstore.ErrDeliveryReplayRateLimited):
			c.Response().Header().Set("Retry-After", "3600")
			return githubWebhookReplayError(c, http.StatusTooManyRequests, "rate_limited", "Replay rate limit exceeded")
		case errors.Is(err, webhookstore.ErrDeliveryReplayNotEligible), errors.Is(err, webhookstore.ErrStaleTenantWebhook):
			return githubWebhookReplayError(c, http.StatusConflict, "replay_not_eligible", "Delivery cannot be replayed under current Tenant policy")
		default:
			return databaseUnavailable(c)
		}
	}
}

func githubWebhookReplayError(c *echo.Context, status int, code, message string) error {
	return c.JSON(status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func githubAppMetadata(pool transaction.Beginner, action string, proofVerifier TenantGitHubAppProofVerifier) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if action == "verify" {
			return verifyGitHubAppMetadata(c, pool, proofVerifier)
		}
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenant := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		id := c.Param("integration_id")
		var draft githubregistry.AppDraftRequest
		var revision struct {
			ExpectedRevision   int64 `json:"expected_revision"`
			ExpectedGeneration int64 `json:"expected_generation"`
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
			if revision.ExpectedRevision < 1 || action == "select-default" && revision.ExpectedGeneration < 1 {
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
			case "select-default":
				app, err = githubregistry.SelectDefaultAppGeneration(ctx, tx, tenant, id, revision.ExpectedRevision, revision.ExpectedGeneration, now)
			default:
				app, err = githubregistry.GetApp(ctx, tx, tenant, id)
			}
			if err != nil {
				return result{}, err
			}
			if action == "disable" || action == "select-default" || (action == "add" && created) {
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

func verifyGitHubAppMetadata(c *echo.Context, pool transaction.Beginner, verifier TenantGitHubAppProofVerifier) error {
	user, ok := HakoUserFromContext(c)
	if !ok || pool == nil {
		return databaseUnavailable(c)
	}
	tenant := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
	id := c.Param("integration_id")
	var revision struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if err := decodeJSONRequest(c, &revision); err != nil || revision.ExpectedRevision < 1 {
		return githubAppError(c, githubregistry.ErrInvalidApp)
	}
	if verifier == nil {
		return githubAppError(c, errGitHubAppNotReady)
	}
	now := time.Now().UTC()
	type readResult struct {
		credential githubregistry.AppProofCredential
		app        githubregistry.AppIntegration
		ready      bool
	}
	read, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (readResult, error) {
		if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenant, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
			return readResult{}, err
		}
		app, err := githubregistry.GetApp(ctx, tx, tenant, id)
		if err != nil {
			return readResult{}, err
		}
		if app.ConfigRevision != revision.ExpectedRevision {
			return readResult{}, githubregistry.ErrAppConflict
		}
		if app.Status == "ready" {
			return readResult{app: app, ready: true}, nil
		}
		credential, err := githubregistry.ResolveAppProofCredential(ctx, tx, tenant, id, app.ConfigRevision, app.CredentialGeneration)
		return readResult{credential: credential, app: app}, err
	})
	if err != nil {
		return githubAppError(c, err)
	}
	if read.ready {
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, GitHubAppMetadataResult{Integration: &read.app})
	}
	proofCtx, cancelProof := context.WithTimeout(c.Request().Context(), 45*time.Second)
	defer cancelProof()
	if err := verifier.VerifyApp(proofCtx, read.credential); err != nil {
		return githubAppError(c, errGitHubAppVerificationFailed)
	}
	app, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.AppIntegration, error) {
		if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenant, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
			return githubregistry.AppIntegration{}, err
		}
		verified, err := githubregistry.MarkAppVerified(ctx, tx, tenant, id, read.credential.ConfigRevision, read.credential.CredentialGeneration, now)
		if err != nil {
			return githubregistry.AppIntegration{}, err
		}
		details, _ := json.Marshal(map[string]any{"github_app_id": verified.GitHubAppID, "status": verified.Status, "config_revision": verified.ConfigRevision})
		if err := audit.Append(ctx, tx, audit.Event{TenantID: tenant, ActorID: user.ID, Action: "tenant.github_app.verify", TargetType: "github_app_integration", TargetID: verified.ID, Details: details, OccurredAt: now}); err != nil {
			return githubregistry.AppIntegration{}, err
		}
		return verified, nil
	})
	if err != nil {
		return githubAppError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, GitHubAppMetadataResult{Integration: &app})
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
	case errors.Is(err, githubregistry.ErrTenantAppMigrationConflict):
		return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "migration_conflict", Message: "one or more Installations cannot be safely bound to this integration"}})
	case errors.Is(err, githubregistry.ErrLegacyRunsNotDrained):
		return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "legacy_runs_not_drained", Message: "legacy Workload Runs must be terminal before selecting a Tenant GitHub App"}})
	case errors.Is(err, errGitHubAppNotReady):
		return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "github_app_not_ready", Message: "Tenant App verification is unavailable or the integration is not ready for activation"}})
	case errors.Is(err, errGitHubAppVerificationFailed):
		return c.JSON(http.StatusBadGateway, errorResponse{Error: errorBody{Code: "github_app_verification_failed", Message: "GitHub App identity could not be verified"}})
	default:
		return databaseUnavailable(c)
	}
}
