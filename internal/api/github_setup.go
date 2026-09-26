package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const githubSetupStateTTL = 10 * time.Minute

func beginGitHubInstallationSetup(pool transaction.Beginner, verifier githubapp.SetupVerifier) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil || verifier == nil {
			return databaseUnavailable(c)
		}
		var request struct{}
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidGitHubSetupRequest(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		stateBytes := make([]byte, 32)
		if _, err := rand.Read(stateBytes); err != nil {
			return databaseUnavailable(c)
		}
		verifierBytes := make([]byte, 32)
		if _, err := rand.Read(verifierBytes); err != nil {
			return databaseUnavailable(c)
		}
		state := base64.RawURLEncoding.EncodeToString(stateBytes)
		codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
		now := time.Now().UTC()
		expiresAt := now.Add(githubSetupStateTTL)
		_, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return struct{}{}, err
			}
			stateHash := githubregistry.HashSetupState(state)
			if err := githubregistry.CreateSetupState(ctx, tx, tenantID, user.ID, stateHash, codeVerifier, now, expiresAt); err != nil {
				return struct{}{}, err
			}
			details, _ := json.Marshal(map[string]any{"expires_at": expiresAt})
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
				Action: "tenant.github_installation.setup_start", TargetType: "github_app_installation_setup",
				TargetID: stateHash[:16], Details: details, OccurredAt: now}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusCreated, map[string]any{
			"setup_url":  verifier.SetupURL(state),
			"expires_at": expiresAt,
		})
	}
}

func completeGitHubInstallationSetup(pool transaction.Beginner, verifier githubapp.SetupVerifier) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if pool == nil || verifier == nil {
			return databaseUnavailable(c)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		c.Response().Header().Set("Referrer-Policy", "no-referrer")
		query := c.Request().URL.Query()
		if !singleQueryValue(query["state"]) {
			return invalidGitHubSetupRequest(c)
		}
		state := query.Get("state")
		if len(state) != 43 {
			return invalidGitHubSetupRequest(c)
		}
		ctx := c.Request().Context()
		stateHash := githubregistry.HashSetupState(state)
		now := time.Now().UTC()
		if query.Has("installation_id") || query.Has("setup_action") {
			if !singleQueryValue(query["installation_id"]) || !singleQueryValue(query["setup_action"]) || query.Has("code") {
				return invalidGitHubSetupRequest(c)
			}
			installationID, err := strconv.ParseInt(query.Get("installation_id"), 10, 64)
			setupAction := query.Get("setup_action")
			if err != nil || installationID <= 0 || (setupAction != "install" && setupAction != "update") {
				return invalidGitHubSetupRequest(c)
			}
			setupState, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.SetupState, error) {
				state, err := githubregistry.ValidateSetupState(ctx, tx, stateHash, now)
				if err != nil {
					return githubregistry.SetupState{}, err
				}
				if err := githubregistry.SetSetupInstallation(ctx, tx, stateHash, installationID, now); err != nil {
					return githubregistry.SetupState{}, err
				}
				state.InstallationID = installationID
				return state, nil
			})
			if errors.Is(err, githubregistry.ErrSetupInstallationMismatch) || errors.Is(err, githubregistry.ErrInvalidSetupState) {
				return invalidGitHubSetupRequest(c)
			}
			if err != nil {
				return databaseUnavailable(c)
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			c.Response().Header().Set("Referrer-Policy", "no-referrer")
			return c.Redirect(http.StatusFound, verifier.AuthorizationURL(state, setupState.CodeVerifier))
		}
		if !singleQueryValue(query["code"]) || strings.TrimSpace(query.Get("code")) == "" || query.Has("error") {
			return invalidGitHubSetupRequest(c)
		}
		setupState, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.SetupState, error) {
			return githubregistry.ValidateSetupState(ctx, tx, stateHash, now)
		})
		if errors.Is(err, githubregistry.ErrInvalidSetupState) || err == nil && setupState.InstallationID <= 0 {
			return invalidGitHubSetupRequest(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		identity, err := verifier.VerifyInstallation(ctx, query.Get("code"), setupState.InstallationID, setupState.CodeVerifier)
		if err != nil || identity.ID != setupState.InstallationID {
			if err != nil && !errors.Is(err, githubapp.ErrAuthorizationRejected) {
				return c.JSON(http.StatusBadGateway, errorResponse{Error: errorBody{Code: "github_unavailable", Message: "GitHub Installation verification is temporarily unavailable"}})
			}
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: errorBody{Code: "github_installation_unverified", Message: "GitHub could not verify this Installation for the authorizing user"}})
		}
		completedAt := time.Now().UTC()
		var installation githubregistry.Installation
		installation, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.Installation, error) {
			installation, err := githubregistry.CompleteSetupState(ctx, tx, stateHash, identity.ID, identity.AccountLogin, completedAt)
			if err != nil {
				return githubregistry.Installation{}, err
			}
			details, err := json.Marshal(map[string]any{"installation_id": installation.InstallationID, "account_login": installation.AccountLogin, "status": installation.Status})
			if err != nil {
				return githubregistry.Installation{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: installation.TenantID, ActorID: installation.RequestedBy,
				Action: "tenant.github_installation.activate", TargetType: "github_app_installation",
				TargetID: strconv.FormatInt(installation.InstallationID, 10), Details: details, OccurredAt: completedAt}); err != nil {
				return githubregistry.Installation{}, err
			}
			return installation, nil
		})
		if errors.Is(err, githubregistry.ErrInvalidSetupState) {
			return invalidGitHubSetupRequest(c)
		}
		if errors.Is(err, githubregistry.ErrSetupInstallationMismatch) {
			return invalidGitHubSetupRequest(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationBound) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "installation_conflict", Message: "GitHub Installation is already connected to another Tenant"}})
		}
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"installation": installation, "verified": true})
	}
}

func singleQueryValue(values []string) bool { return len(values) == 1 }

func invalidGitHubSetupRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_github_setup", Message: "GitHub Installation setup state, code, action, or installation_id is invalid or expired"}})
}
