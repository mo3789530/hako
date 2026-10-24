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
	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	storecatalog "github.com/mo3789530/hako/internal/store/softwarecatalog"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type softwareCatalogDeployPolicyResult struct {
	ServiceID        string              `json:"service_id"`
	Policy           deploypolicy.Policy `json:"policy"`
	ExecutionEnabled bool                `json:"execution_enabled"`
}

type softwareCatalogDeployPolicyVerificationRequest struct {
	Token string `json:"token"`
}

type softwareCatalogDeployPolicyVerificationResult struct {
	Valid            bool   `json:"valid"`
	ExecutionEnabled bool   `json:"execution_enabled"`
	Repository       string `json:"repository"`
	RepositoryID     string `json:"repository_id"`
	Ref              string `json:"ref"`
	Environment      string `json:"environment"`
	WorkflowRef      string `json:"workflow_ref"`
	EventName        string `json:"event_name"`
	RunID            string `json:"run_id"`
	RunAttempt       string `json:"run_attempt"`
}

func verifySoftwareCatalogDeployPolicy(pool transaction.Beginner, verifier interface {
	Verify(context.Context, string, deploypolicy.Policy, string, int64) (deploypolicy.Claims, error)
}) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		if verifier == nil {
			return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "oidc_verifier_unavailable", Message: "GitHub OIDC verification is not configured"}})
		}
		var request softwareCatalogDeployPolicyVerificationRequest
		if err := decodeJSONRequest(c, &request); err != nil || len(request.Token) == 0 || len(request.Token) > 64<<10 || strings.TrimSpace(request.Token) != request.Token {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_oidc_token", Message: "A bounded GitHub OIDC token is required"}})
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		serviceID := strings.TrimSpace(c.Param("service_id"))
		type binding struct {
			repository   string
			repositoryID int64
			policy       deploypolicy.Policy
		}
		resolved, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (binding, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return binding{}, err
			}
			repository, repositoryID, policy, err := storecatalog.ReadDeployPolicyBinding(ctx, tx, string(tenantID), serviceID)
			return binding{repository: repository, repositoryID: repositoryID, policy: policy}, err
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, storecatalog.ErrServiceNotFound) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		claims, err := verifier.Verify(c.Request().Context(), request.Token, resolved.policy, resolved.repository, resolved.repositoryID)
		if errors.Is(err, deploypolicy.ErrUnavailable) {
			return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "oidc_verifier_unavailable", Message: "GitHub OIDC verification is temporarily unavailable"}})
		}
		if err != nil {
			return c.JSON(http.StatusForbidden, errorResponse{Error: errorBody{Code: "oidc_claims_denied", Message: "GitHub OIDC claims do not match the Tenant deployment policy"}})
		}
		return c.JSON(http.StatusOK, softwareCatalogDeployPolicyVerificationResult{Valid: true, ExecutionEnabled: false,
			Repository: claims.Repository, RepositoryID: claims.RepositoryID, Ref: claims.Ref, Environment: claims.Environment,
			WorkflowRef: claims.WorkflowRef, EventName: claims.EventName, RunID: claims.RunID, RunAttempt: claims.RunAttempt})
	}
}

func getSoftwareCatalogDeployPolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		serviceID := strings.TrimSpace(c.Param("service_id"))
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (softwareCatalogDeployPolicyResult, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return softwareCatalogDeployPolicyResult{}, err
			}
			policy, err := storecatalog.ReadDeployPolicy(ctx, tx, string(tenantID), serviceID)
			return softwareCatalogDeployPolicyResult{ServiceID: serviceID, Policy: policy, ExecutionEnabled: false}, err
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, storecatalog.ErrServiceNotFound) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, result)
	}
}

func setSoftwareCatalogDeployPolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var policy deploypolicy.Policy
		if err := decodeJSONRequest(c, &policy); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidSoftwareCatalogDeployPolicy(c)
		}
		if policy.Validate() != nil {
			return invalidSoftwareCatalogDeployPolicy(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		serviceID := strings.TrimSpace(c.Param("service_id"))
		now := time.Now().UTC()
		result := softwareCatalogDeployPolicyResult{ServiceID: serviceID, Policy: policy, ExecutionEnabled: false}
		_, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return struct{}{}, err
			}
			if err := storecatalog.SetDeployPolicy(ctx, tx, string(tenantID), serviceID, string(user.ID), policy, now); err != nil {
				return struct{}{}, err
			}
			details, err := json.Marshal(map[string]any{"enabled": policy.Enabled, "audience": policy.Audience,
				"ref_count": len(policy.AllowedRefs), "environment_count": len(policy.Environments),
				"require_human_approval": policy.RequireHumanApproval, "execution_enabled": false})
			if err != nil {
				return struct{}{}, errors.New("encode Software Catalog deploy policy audit details")
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "tenant.software_catalog.deploy_policy.set",
				TargetType: "software_catalog_service", TargetID: serviceID, Details: details, OccurredAt: now}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, storecatalog.ErrServiceNotFound) {
			return tenantNotFound(c)
		}
		if errors.Is(err, deploypolicy.ErrInvalid) {
			return invalidSoftwareCatalogDeployPolicy(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, result)
	}
}

func invalidSoftwareCatalogDeployPolicy(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_deploy_policy",
		Message: "Enabled OIDC policy requires the AWS STS audience, exact branch/tag refs, named environments, exact workflow refs, and explicit human approval"}})
}
