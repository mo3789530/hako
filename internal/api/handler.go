// Package api contains the Hako Control Plane Echo handler and API client.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/scheduler"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/placementpolicies"
	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/users"
	"github.com/mo3789530/hako/internal/store/workspaces"
)

const (
	principalKey  = "hako.principal"
	userKey       = "hako.user"
	membershipKey = "hako.membership"
)

type healthResponse struct {
	Status string `json:"status"`
}

type tenantMembershipResponse struct {
	TenantID domain.TenantID   `json:"tenant_id"`
	UserID   domain.UserID     `json:"user_id"`
	Role     domain.TenantRole `json:"role"`
}

type WorkspaceCreateConfig struct {
	Region               string
	RequiredCapabilities []string
	RuntimeClass         string
	Image                string
}

type workspaceCreateRequest struct {
	Name string `json:"name"`
}

type workspaceCreateResponse struct {
	Workspace      domain.Workspace       `json:"workspace"`
	Status         domain.WorkspaceStatus `json:"status"`
	OperationID    domain.OperationID     `json:"operation_id"`
	OperationState domain.OperationStatus `json:"operation_state"`
}

type workspaceActionRequest struct {
	Action domain.OperationType `json:"action"`
}

type tenantPlacementPolicyRequest struct {
	AllowedRegions       []string `json:"allowed_regions"`
	ResourcePlaneIDs     []string `json:"resource_plane_ids"`
	RequiredCapabilities []string `json:"required_capabilities"`
	MaxCostTier          string   `json:"max_cost_tier"`
	MinimumIsolationTier string   `json:"minimum_isolation_tier"`
}

type resourcePlaneHealthRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type workspaceActionResponse struct {
	Workspace      workspaces.View        `json:"workspace"`
	OperationID    domain.OperationID     `json:"operation_id"`
	OperationType  domain.OperationType   `json:"operation_type"`
	OperationState domain.OperationStatus `json:"operation_state"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const maxJSONRequestBytes = 16 << 10

// NewHandler builds the Echo API with a minimal public liveness endpoint and
// verifies Cognito access tokens on all application API routes.
func NewHandler(verifier *auth.CognitoVerifier, pool transaction.Beginner, createConfig ...WorkspaceCreateConfig) *echo.Echo {
	var workspaceConfig WorkspaceCreateConfig
	if len(createConfig) > 0 {
		workspaceConfig = createConfig[0]
	}
	if len(workspaceConfig.RequiredCapabilities) == 0 {
		workspaceConfig.RequiredCapabilities = []string{"microvm"}
	}
	e := echo.New()
	e.HTTPErrorHandler = apiErrorHandler
	e.GET("/healthz", publicHealth)
	protected := e.Group("", CognitoMiddleware(verifier))
	protected.GET("/v1/health", health, RequireScopes(auth.HakoAPIScope))
	protected.GET("/v1/tenants/:tenant_id/membership", tenantMembership,
		RequireScopes(auth.HakoAPIScope),
		HakoUserMiddleware(pool),
		TenantMembershipMiddleware(pool),
	)
	protected.GET("/v1/tenants/:tenant_id/placement-policy", getTenantPlacementPolicy(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool),
	)
	protected.PUT("/v1/tenants/:tenant_id/placement-policy", putTenantPlacementPolicy(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool),
	)
	protected.GET("/v1/admin/resource-planes/:resource_plane_id/health", getResourcePlaneHealth(pool),
		RequireScopes(auth.HakoAPIScope), RequireCognitoGroup("hako-admin"), HakoUserMiddleware(pool),
	)
	protected.PUT("/v1/admin/resource-planes/:resource_plane_id/health", putResourcePlaneHealth(pool),
		RequireScopes(auth.HakoAPIScope), RequireCognitoGroup("hako-admin"), HakoUserMiddleware(pool),
	)
	protected.POST("/v1/tenants/:tenant_id/workspaces", createWorkspace(pool, workspaceConfig),
		RequireScopes(auth.HakoAPIScope),
		HakoUserMiddleware(pool),
		TenantMembershipMiddleware(pool),
	)
	protected.GET("/v1/tenants/:tenant_id/workspaces", listWorkspaces(pool),
		RequireScopes(auth.HakoAPIScope),
		HakoUserMiddleware(pool),
		TenantMembershipMiddleware(pool),
	)
	protected.GET("/v1/tenants/:tenant_id/workspaces/:workspace_id", getWorkspace(pool),
		RequireScopes(auth.HakoAPIScope),
		HakoUserMiddleware(pool),
		TenantMembershipMiddleware(pool),
	)
	protected.POST("/v1/tenants/:tenant_id/workspaces/:workspace_id/actions", requestWorkspaceAction(pool),
		RequireScopes(auth.HakoAPIScope),
		HakoUserMiddleware(pool),
		TenantMembershipMiddleware(pool),
	)
	return e
}

func getResourcePlaneHealth(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		_, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		planeID := strings.TrimSpace(c.Param("resource_plane_id"))
		if !validResourcePlaneID(planeID) {
			return invalidResourcePlaneHealth(c)
		}
		health, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (resourceplanehealth.Health, error) {
			return resourceplanehealth.Get(ctx, tx, domain.ResourcePlaneID(planeID), time.Now().UTC())
		})
		if errors.Is(err, resourceplanehealth.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Resource Plane not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, health)
	}
}

func putResourcePlaneHealth(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		planeID := strings.TrimSpace(c.Param("resource_plane_id"))
		if !validResourcePlaneID(planeID) {
			return invalidResourcePlaneHealth(c)
		}
		var request resourcePlaneHealthRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidResourcePlaneHealth(c)
		}
		status := resourceplanehealth.Status(strings.TrimSpace(request.Status))
		if _, err := resourceplanehealth.Validate(status, request.Reason); err != nil {
			return invalidResourcePlaneHealth(c)
		}
		health, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (resourceplanehealth.Health, error) {
			return resourceplanehealth.Put(ctx, tx, domain.ResourcePlaneID(planeID), status, request.Reason, user.ID, time.Now().UTC())
		})
		if errors.Is(err, resourceplanehealth.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Resource Plane not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, health)
	}
}

func validResourcePlaneID(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	first := value[0]
	if (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func invalidResourcePlaneHealth(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "resource plane health requires a valid ID, status, and bounded reason"}})
}

func getTenantPlacementPolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		policy, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (placementpolicies.Policy, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return placementpolicies.Policy{}, err
			}
			return placementpolicies.Get(ctx, tx, tenantID)
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

func putTenantPlacementPolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request tenantPlacementPolicyRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidPlacementPolicyRequest(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		policy := placementpolicies.Policy{
			TenantID: tenantID, AllowedRegions: request.AllowedRegions,
			ResourcePlaneIDs: request.ResourcePlaneIDs, RequiredCapabilities: request.RequiredCapabilities,
			MaxCostTier: request.MaxCostTier, MinimumIsolationTier: request.MinimumIsolationTier,
		}
		if _, err := placementpolicies.Normalize(policy); err != nil {
			return invalidPlacementPolicyRequest(c)
		}
		policy, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (placementpolicies.Policy, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return placementpolicies.Policy{}, err
			}
			updated, err := placementpolicies.Put(ctx, tx, policy, time.Now().UTC())
			if err != nil {
				return placementpolicies.Policy{}, err
			}
			details, err := json.Marshal(map[string]any{
				"allowed_region_count": len(updated.AllowedRegions), "resource_plane_count": len(updated.ResourcePlaneIDs),
				"required_capability_count": len(updated.RequiredCapabilities),
				"max_cost_tier":             updated.MaxCostTier, "minimum_isolation_tier": updated.MinimumIsolationTier,
			})
			if err != nil {
				return placementpolicies.Policy{}, fmt.Errorf("encode placement policy audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{
				TenantID: tenantID, ActorID: user.ID, Action: "tenant.placement_policy.update",
				TargetType: "tenant_placement_policy", TargetID: string(tenantID), Details: details, OccurredAt: *updated.UpdatedAt,
			}); err != nil {
				return placementpolicies.Policy{}, err
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

func invalidPlacementPolicyRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "placement policy selectors and Cost/Isolation tiers are invalid"}})
}

func requestWorkspaceAction(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request workspaceActionRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidActionRequest(c)
		}
		idempotencyKey := strings.TrimSpace(c.Request().Header.Get("Idempotency-Key"))
		if idempotency.ValidateKey(idempotencyKey) != nil {
			return invalidActionRequest(c)
		}
		if request.Action != domain.OperationSuspend && request.Action != domain.OperationResume && request.Action != domain.OperationDelete {
			return invalidActionRequest(c)
		}
		result, err := workspaces.RequestAction(c.Request().Context(), pool, workspaces.ActionInput{
			TenantID:    domain.TenantID(strings.TrimSpace(c.Param("tenant_id"))),
			WorkspaceID: domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id"))),
			UserID:      user.ID, Action: request.Action, IdempotencyKey: idempotencyKey,
		}, transaction.DefaultPolicy())
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return workspaceNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceActionConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "workspace_state_conflict", Message: "Workspace is not in a state that allows this action"}})
		}
		if errors.Is(err, workspaces.ErrWorkspaceOperationInProgress) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "operation_in_progress", Message: "Workspace already has an operation in progress"}})
		}
		if errors.Is(err, operations.ErrIdempotencyConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "idempotency_conflict", Message: "Idempotency-Key was already used for a different request"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, workspaceActionResponse{
			Workspace: result.Workspace, OperationID: result.Operation.ID,
			OperationType: result.Operation.Type, OperationState: result.Operation.Status,
		})
	}
}

func invalidActionRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "action and a valid Idempotency-Key are required"}})
}

func listWorkspaces(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		limit, offset, err := parseWorkspacePage(c)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_pagination", Message: "limit must be 1-100 and offset must be 0-1000000"}})
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaces.ListResult, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return workspaces.ListResult{}, err
			}
			return workspaces.ListVisible(ctx, tx, tenantID, user.ID, membership.Role, limit, offset)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, result)
	}
}

func getWorkspace(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		view, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaces.View, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return workspaces.View{}, err
			}
			return workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return workspaceNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, view)
	}
}

func parseWorkspacePage(c *echo.Context) (int, int, error) {
	limit, offset := 20, 0
	query := c.QueryParams()
	for name, values := range query {
		if (name != "limit" && name != "offset") || len(values) != 1 || values[0] == "" {
			return 0, 0, errors.New("invalid pagination query")
		}
	}
	if values, ok := query["limit"]; ok {
		raw := values[0]
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return 0, 0, errors.New("invalid limit")
		}
		limit = parsed
	}
	if values, ok := query["offset"]; ok {
		raw := values[0]
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 1000000 {
			return 0, 0, errors.New("invalid offset")
		}
		offset = parsed
	}
	return limit, offset, nil
}

func createWorkspace(pool transaction.Beginner, config WorkspaceCreateConfig) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if pool == nil {
			return databaseUnavailable(c)
		}
		if strings.TrimSpace(config.Image) == "" {
			return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "workspace_provisioning_unavailable", Message: "Workspace provisioning is not configured"}})
		}
		user, ok := HakoUserFromContext(c)
		if !ok {
			return databaseUnavailable(c)
		}
		var request workspaceCreateRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidRequest(c)
		}
		request.Name = strings.TrimSpace(request.Name)
		if request.Name == "" || len(request.Name) > 128 {
			return invalidRequest(c)
		}
		idempotencyKey := strings.TrimSpace(c.Request().Header.Get("Idempotency-Key"))
		if err := idempotency.ValidateKey(idempotencyKey); err != nil {
			return invalidRequest(c)
		}
		runtimeClass := strings.TrimSpace(config.RuntimeClass)
		if runtimeClass == "" {
			runtimeClass = "standard"
		}
		created, err := workspaces.Create(c.Request().Context(), pool, workspaces.CreateInput{
			TenantID:             domain.TenantID(strings.TrimSpace(c.Param("tenant_id"))),
			OwnerID:              user.ID,
			Name:                 request.Name,
			RuntimeClass:         runtimeClass,
			Image:                strings.TrimSpace(config.Image),
			Region:               strings.TrimSpace(config.Region),
			RequiredCapabilities: config.RequiredCapabilities,
			IdempotencyKey:       idempotencyKey,
		}, transaction.DefaultPolicy())
		if errors.Is(err, scheduler.ErrNoEligibleResourcePlane) {
			return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "resource_plane_unavailable", Message: "No active Resource Plane satisfies the Workspace requirements"}})
		}
		if errors.Is(err, workspaces.ErrWorkspaceQuotaExceeded) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "workspace_quota_exceeded", Message: "Tenant workspace limit reached (10)"}})
		}
		if errors.Is(err, workspaces.ErrWorkspaceNameConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "workspace_name_conflict", Message: "Workspace name is already in use in this Tenant"}})
		}
		if errors.Is(err, operations.ErrIdempotencyConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "idempotency_conflict", Message: "Idempotency-Key was already used for a different request"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, workspaceCreateResponse{
			Workspace: created.Workspace, Status: created.Status,
			OperationID: created.Operation.ID, OperationState: created.Operation.Status,
		})
	}
}

func invalidRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Workspace name and a valid Idempotency-Key are required"}})
}

// decodeJSONRequest applies the API's common JSON rules: bounded body size,
// unknown-field rejection, and exactly one JSON value per request.
func decodeJSONRequest(c *echo.Context, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, maxJSONRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func isRequestTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

func requestTooLarge(c *echo.Context) error {
	return c.JSON(http.StatusRequestEntityTooLarge, errorResponse{Error: errorBody{Code: "request_too_large", Message: "Request body is too large"}})
}

func health(c *echo.Context) error {
	return c.JSON(http.StatusOK, healthResponse{Status: "ok"})
}

func publicHealth(c *echo.Context) error {
	return c.JSON(http.StatusOK, healthResponse{Status: "ok"})
}

func tenantMembership(c *echo.Context) error {
	membership, ok := c.Get(membershipKey).(domain.TenantMembership)
	if !ok {
		return c.JSON(http.StatusInternalServerError, errorResponse{Error: errorBody{Code: "internal_error", Message: "Internal server error"}})
	}
	return c.JSON(http.StatusOK, tenantMembershipResponse{
		TenantID: membership.TenantID,
		UserID:   membership.UserID,
		Role:     membership.Role,
	})
}

// CognitoMiddleware validates the access token and stores its verified Hako
// principal in the Echo context.
func CognitoMiddleware(verifier *auth.CognitoVerifier) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			principal, err := auth.Authenticate(c.Request().Context(), c.Request().Header.Get("Authorization"), verifier)
			if err != nil {
				if errors.Is(err, auth.ErrVerifierUnavailable) {
					c.Response().Header().Set("WWW-Authenticate", `Bearer realm="hako"`)
					return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "auth_unavailable", Message: "Authentication service unavailable"}})
				}
				c.Response().Header().Set("WWW-Authenticate", `Bearer realm="hako"`)
				return c.JSON(http.StatusUnauthorized, errorResponse{Error: errorBody{Code: "unauthorized", Message: "Valid bearer access token required"}})
			}
			c.Set(principalKey, principal)
			return next(c)
		}
	}
}

// RequireScopes checks the verified principal for every required OAuth scope.
func RequireScopes(required ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			principal, ok := c.Get(principalKey).(auth.Principal)
			if !ok {
				c.Response().Header().Set("WWW-Authenticate", `Bearer realm="hako"`)
				return c.JSON(http.StatusUnauthorized, errorResponse{Error: errorBody{Code: "unauthorized", Message: "Valid bearer access token required"}})
			}
			granted := make(map[string]struct{}, len(principal.Scopes))
			for _, scope := range principal.Scopes {
				granted[scope] = struct{}{}
			}
			for _, scope := range required {
				if _, ok := granted[scope]; !ok {
					c.Response().Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
					return c.JSON(http.StatusForbidden, errorResponse{Error: errorBody{Code: "insufficient_scope", Message: "Required OAuth scope is missing"}})
				}
			}
			return next(c)
		}
	}
}

// RequireCognitoGroup checks a group claim from the already verified Cognito
// access token. It is intended only for platform-level roles, not Tenant RBAC.
func RequireCognitoGroup(required string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			principal, ok := c.Get(principalKey).(auth.Principal)
			if !ok {
				c.Response().Header().Set("WWW-Authenticate", `Bearer realm="hako"`)
				return c.JSON(http.StatusUnauthorized, errorResponse{Error: errorBody{Code: "unauthorized", Message: "Valid bearer access token required"}})
			}
			for _, group := range principal.Groups {
				if group == required {
					return next(c)
				}
			}
			return c.JSON(http.StatusForbidden, errorResponse{Error: errorBody{Code: "insufficient_platform_role", Message: "Platform administrator role is required"}})
		}
	}
}

// HakoUserMiddleware resolves the verified Cognito subject to a stable Hako
// User. It is applied only to routes that need database-backed identity.
func HakoUserMiddleware(pool transaction.Beginner) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			principal, ok := c.Get(principalKey).(auth.Principal)
			if !ok {
				c.Response().Header().Set("WWW-Authenticate", `Bearer realm="hako"`)
				return c.JSON(http.StatusUnauthorized, errorResponse{Error: errorBody{Code: "unauthorized", Message: "Valid bearer access token required"}})
			}
			if pool == nil {
				return databaseUnavailable(c)
			}
			user, err := users.ResolveCognitoSubject(c.Request().Context(), pool, transaction.DefaultPolicy(), principal.CognitoSubject)
			if err != nil {
				return databaseUnavailable(c)
			}
			c.Set(userKey, user)
			return next(c)
		}
	}
}

// HakoUserFromContext returns the stable Hako User resolved for an
// authenticated API request.
func HakoUserFromContext(c *echo.Context) (domain.User, bool) {
	user, ok := c.Get(userKey).(domain.User)
	return user, ok
}

// TenantMembershipMiddleware checks the caller's membership before allowing a
// tenant-scoped handler to run. Missing tenants and non-members both return
// 404 to avoid disclosing Tenant IDs.
func TenantMembershipMiddleware(pool transaction.Beginner) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			user, ok := HakoUserFromContext(c)
			if !ok {
				return databaseUnavailable(c)
			}
			if pool == nil {
				return databaseUnavailable(c)
			}
			tenantID := strings.TrimSpace(c.Param("tenant_id"))
			if tenantID == "" {
				return tenantNotFound(c)
			}
			membership, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.TenantMembership, error) {
				return authz.RequireTenantMembership(ctx, tx, user.ID, domain.TenantID(tenantID))
			})
			if errors.Is(err, authz.ErrTenantAccessDenied) {
				return tenantNotFound(c)
			}
			if err != nil {
				return databaseUnavailable(c)
			}
			c.Set(membershipKey, membership)
			return next(c)
		}
	}
}

func tenantNotFound(c *echo.Context) error {
	return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Tenant not found"}})
}

func workspaceNotFound(c *echo.Context) error {
	return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Workspace not found"}})
}

func databaseUnavailable(c *echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "database_unavailable", Message: "Database temporarily unavailable"}})
}

func apiErrorHandler(c *echo.Context, err error) {
	if response, unwrapErr := echo.UnwrapResponse(c.Response()); unwrapErr == nil && response.Committed {
		return
	}
	status := echo.StatusCode(err)
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	message := http.StatusText(status)
	if status < http.StatusInternalServerError {
		if httpErr, ok := err.(*echo.HTTPError); ok && httpErr.Message != "" {
			message = httpErr.Message
		}
	}
	_ = c.JSON(status, errorResponse{Error: errorBody{Code: statusCode(status), Message: message}})
}

func statusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	default:
		return "http_error"
	}
}
