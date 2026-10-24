// Package api contains the Hako Control Plane Echo handler and API client.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	githubwebhookcore "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/scheduler"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/operations"
	"github.com/mo3789530/hako/internal/store/placementpolicies"
	"github.com/mo3789530/hako/internal/store/quotas"
	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/users"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/workspacesession"
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
	Region                                   string
	RequiredCapabilities                     []string
	RuntimeClass                             string
	Image                                    string
	WorkloadRunsEnabled                      bool
	WebhookWorkloadTriggersEnabled           bool
	GitHubAppManagementEnabled               bool
	GitHubAppProofVerifier                   TenantGitHubAppProofVerifier
	TenantGitHubWebhookHandler               http.Handler
	TenantGitHubSetupResolver                TenantGitHubSetupVerifierResolver
	ResourcePlaneHealthReportKeyResolver     ResourcePlaneHealthReportKeyResolver
	ResourcePlaneServiceReportKeyResolver    ResourcePlaneServiceReportKeyResolver
	ResourcePlaneWorkloadSpecKeyResolver     ResourcePlaneWorkloadSpecKeyResolver
	ResourcePlaneArtifactCleanupKeyResolver  ResourcePlaneArtifactCleanupKeyResolver
	ResourcePlaneWorkspaceSessionKeyResolver ResourcePlaneWorkspaceSessionKeyResolver
	WorkspaceSessionTicketIssuer             workspacesession.Issuer
	GitHubOIDCPolicyVerifier                 interface {
		Verify(context.Context, string, deploypolicy.Policy, string, int64) (deploypolicy.Claims, error)
	}
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

type workloadRunCreateRequest struct {
	GitHubInstallationID int64  `json:"github_installation_id"`
	GitHubRepositoryID   int64  `json:"github_repository_id"`
	CommitSHA            string `json:"commit_sha"`
	Ref                  string `json:"ref"`
	TimeoutSeconds       int64  `json:"timeout_seconds,omitempty"`
}

var workloadCommitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

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
	return newHandler(verifier, pool, nil, nil, createConfig...)
}

// NewHandlerWithGitHubWebhook adds the unauthenticated-by-Cognito but
// HMAC-authenticated GitHub delivery endpoint when webhookHandler is non-nil.
func NewHandlerWithGitHubWebhook(verifier *auth.CognitoVerifier, pool transaction.Beginner, webhookHandler http.Handler, createConfig ...WorkspaceCreateConfig) *echo.Echo {
	return newHandler(verifier, pool, webhookHandler, nil, createConfig...)
}

// NewHandlerWithGitHubApp enables both the signed webhook ingress and the
// one-time, OAuth-verified Installation setup callback when configured.
func NewHandlerWithGitHubApp(verifier *auth.CognitoVerifier, pool transaction.Beginner, webhookHandler http.Handler, setupVerifier githubapp.SetupVerifier, createConfig ...WorkspaceCreateConfig) *echo.Echo {
	return newHandler(verifier, pool, webhookHandler, setupVerifier, createConfig...)
}

func newHandler(verifier *auth.CognitoVerifier, pool transaction.Beginner, webhookHandler http.Handler, setupVerifier githubapp.SetupVerifier, createConfig ...WorkspaceCreateConfig) *echo.Echo {
	var workspaceConfig WorkspaceCreateConfig
	if len(createConfig) > 0 {
		workspaceConfig = createConfig[0]
	}
	if len(workspaceConfig.RequiredCapabilities) == 0 {
		workspaceConfig.RequiredCapabilities = []string{"microvm"}
	}
	e := echo.New()
	e.HTTPErrorHandler = apiErrorHandler
	e.Use(requestIDMiddleware)
	e.GET("/healthz", publicHealth)
	if webhookHandler != nil {
		e.POST("/v1/integrations/github/webhook", func(c *echo.Context) error {
			request := c.Request().WithContext(githubwebhookcore.WithCorrelationID(c.Request().Context(), RequestIDFromContext(c.Request().Context())))
			webhookHandler.ServeHTTP(c.Response(), request)
			return nil
		})
	}
	if workspaceConfig.TenantGitHubWebhookHandler != nil {
		e.POST("/v1/integrations/github/apps/:integration_id/webhook", func(c *echo.Context) error {
			request := c.Request().WithContext(githubwebhookcore.WithCorrelationID(c.Request().Context(), RequestIDFromContext(c.Request().Context())))
			workspaceConfig.TenantGitHubWebhookHandler.ServeHTTP(c.Response(), request)
			return nil
		})
	}
	if workspaceConfig.ResourcePlaneHealthReportKeyResolver != nil {
		e.POST("/v1/internal/resource-planes/:resource_plane_id/health", putAutomatedResourcePlaneHealth(pool, workspaceConfig.ResourcePlaneHealthReportKeyResolver))
	}
	if workspaceConfig.ResourcePlaneServiceReportKeyResolver != nil {
		e.POST("/v1/internal/resource-planes/:resource_plane_id/workspace-services/:service_id/reports",
			putResourcePlaneWorkspaceServiceReport(pool, workspaceConfig.ResourcePlaneServiceReportKeyResolver))
		e.POST("/v1/internal/resource-planes/:resource_plane_id/workspace-services/:service_id/desired-state",
			resolveResourcePlaneWorkspaceServiceDesiredState(pool, workspaceConfig.ResourcePlaneServiceReportKeyResolver))
		e.PUT("/v1/internal/resource-planes/:resource_plane_id/workspace-connection-leases/:lease_id",
			workspaceConnectionLeaseHandler(pool, workspaceConfig.ResourcePlaneServiceReportKeyResolver, false))
		e.POST("/v1/internal/resource-planes/:resource_plane_id/workspace-connection-leases/:lease_id/release",
			workspaceConnectionLeaseHandler(pool, workspaceConfig.ResourcePlaneServiceReportKeyResolver, true))
	}
	if workspaceConfig.ResourcePlaneWorkloadSpecKeyResolver != nil {
		e.GET("/v1/internal/resource-planes/:resource_plane_id/tenants/:tenant_id/workloads/:workload_run_id/attempts/:attempt_id/generations/:generation/specification",
			getResourcePlaneWorkloadSpecification(pool, workspaceConfig.ResourcePlaneWorkloadSpecKeyResolver))
	}
	if workspaceConfig.ResourcePlaneArtifactCleanupKeyResolver != nil {
		group := e.Group("/v1/internal/resource-planes/:resource_plane_id/workload-artifacts")
		group.GET("/expired", listResourcePlaneExpiredArtifacts(pool, workspaceConfig.ResourcePlaneArtifactCleanupKeyResolver))
		group.POST("/:artifact_id/deleted", acknowledgeResourcePlaneArtifactDeletion(pool, workspaceConfig.ResourcePlaneArtifactCleanupKeyResolver))
	}
	if workspaceConfig.ResourcePlaneWorkspaceSessionKeyResolver != nil {
		e.POST("/v1/internal/resource-planes/:resource_plane_id/workspace-sessions/active",
			getActiveWorkspaceSession(pool, workspaceConfig.ResourcePlaneWorkspaceSessionKeyResolver))
	}
	if setupVerifier != nil || workspaceConfig.TenantGitHubSetupResolver != nil {
		e.GET("/v1/integrations/github/setup/callback", completeGitHubInstallationSetup(pool, setupVerifier, workspaceConfig.TenantGitHubSetupResolver))
	}
	protected := e.Group("", CognitoMiddleware(verifier))
	if workspaceConfig.GitHubAppManagementEnabled {
		apps := protected.Group("/v1/tenants/:tenant_id/github/apps", RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool), githubAppOwnerMiddleware)
		apps.GET("", githubAppMetadata(pool, "list", workspaceConfig.GitHubAppProofVerifier))
		apps.POST("", githubAppMetadata(pool, "add", workspaceConfig.GitHubAppProofVerifier))
		apps.GET("/:integration_id", githubAppMetadata(pool, "show", workspaceConfig.GitHubAppProofVerifier))
		if workspaceConfig.TenantGitHubSetupResolver != nil {
			apps.POST("/:integration_id/setup", beginGitHubInstallationSetupForApp(pool, nil, workspaceConfig.TenantGitHubSetupResolver))
		}
		for _, action := range []string{"disable", "verify", "select-default"} {
			apps.POST("/:integration_id/"+action, githubAppMetadata(pool, action, workspaceConfig.GitHubAppProofVerifier))
		}
		apps.POST("/:integration_id/migration", migrateTenantGitHubApp(pool))
		apps.GET("/:integration_id/deliveries", listTenantGitHubFailedDeliveries(pool))
		apps.POST("/:integration_id/deliveries/:delivery_id/replay", replayTenantGitHubDelivery(pool))
		protected.PUT("/v1/tenants/:tenant_id/github/installations/:installation_id/repositories/:repository_id/factory", configureGitHubRepositoryFactory(pool, workspaceConfig.WebhookWorkloadTriggersEnabled),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
		protected.PUT("/v1/tenants/:tenant_id/github/installations/:installation_id/repositories/:repository_id/actions-runner", configureGitHubRepositoryActionsRunner(pool),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
		protected.PUT("/v1/tenants/:tenant_id/github/installations/:installation_id/repositories/:repository_id/actions-runner/workflow-policy", configureGitHubRepositoryActionsRunnerWorkflowPolicy(pool),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
		protected.PUT("/v1/tenants/:tenant_id/github/installations/:installation_id/repositories/:repository_id/agent-write-policy", configureGitHubRepositoryAgentWritePolicy(pool),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	}
	if setupVerifier != nil {
		protected.POST("/v1/tenants/:tenant_id/github/installations/setup", beginGitHubInstallationSetup(pool, setupVerifier),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	}
	protected.GET("/v1/tenants/:tenant_id/model-credentials", listModelCredentials(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/model-credentials", createModelCredential(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/model-credentials/:provider/rotate", rotateModelCredential(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/model-credentials/:provider/disable", disableModelCredential(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/health", health, RequireScopes(auth.HakoAPIScope))
	protected.GET("/v1/service-catalog", listServiceCatalog, RequireScopes(auth.HakoAPIScope))
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
	protected.GET("/v1/tenants/:tenant_id/workspace-idle-policy", getTenantWorkspaceIdlePolicy(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.PUT("/v1/tenants/:tenant_id/workspace-idle-policy", putTenantWorkspaceIdlePolicy(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/tenants/:tenant_id/quotas", getTenantQuotas(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.PUT("/v1/tenants/:tenant_id/quotas", putTenantQuotas(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/tenants/:tenant_id/software-catalog", listTenantSoftwareCatalog(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.PUT("/v1/tenants/:tenant_id/software-catalog/:service_id", upsertTenantSoftwareCatalogService(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/tenants/:tenant_id/software-catalog/:service_id/deploy-policy", getSoftwareCatalogDeployPolicy(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.PUT("/v1/tenants/:tenant_id/software-catalog/:service_id/deploy-policy", setSoftwareCatalogDeployPolicy(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/software-catalog/:service_id/deploy-policy/verify",
		verifySoftwareCatalogDeployPolicy(pool, workspaceConfig.GitHubOIDCPolicyVerifier),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/tenants/:tenant_id/github/installations", listGitHubInstallations(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool),
	)
	protected.POST("/v1/tenants/:tenant_id/github/installations", requestGitHubInstallation(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool),
	)
	protected.GET("/v1/tenants/:tenant_id/github/installations/:installation_id/repositories", listGitHubRepositories(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool),
	)
	protected.GET("/v1/admin/resource-planes/:resource_plane_id/health", getResourcePlaneHealth(pool),
		RequireScopes(auth.HakoAPIScope), RequireCognitoGroup("hako-admin"), HakoUserMiddleware(pool),
	)
	protected.PUT("/v1/admin/resource-planes/:resource_plane_id/health", putResourcePlaneHealth(pool),
		RequireScopes(auth.HakoAPIScope), RequireCognitoGroup("hako-admin"), HakoUserMiddleware(pool),
	)
	protected.GET("/v1/admin/github/webhook-deliveries", listPlatformGitHubFailedDeliveries(pool),
		RequireScopes(auth.HakoAPIScope), RequireCognitoGroup("hako-admin"), HakoUserMiddleware(pool),
	)
	protected.POST("/v1/admin/github/webhook-deliveries/:integration_id/:delivery_id/replay", replayPlatformGitHubDelivery(pool),
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
	protected.GET("/v1/tenants/:tenant_id/usage", getTenantUsage(pool),
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
	protected.GET("/v1/tenants/:tenant_id/workspaces/:workspace_id/services", listWorkspaceServices(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/tenants/:tenant_id/workspaces/:workspace_id/services/usage", getWorkspaceServiceUsage(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/workspaces/:workspace_id/services", addWorkspaceService(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/workspaces/:workspace_id/services/:service_name/actions", requestWorkspaceServiceAction(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	if workspaceConfig.WorkspaceSessionTicketIssuer != nil {
		protected.POST("/v1/tenants/:tenant_id/workspaces/:workspace_id/sessions", createWorkspaceSession(pool, workspaceConfig.WorkspaceSessionTicketIssuer),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
		protected.DELETE("/v1/tenants/:tenant_id/workspaces/:workspace_id/sessions/:session_id", revokeWorkspaceSession(pool),
			RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	}
	protected.GET("/v1/tenants/:tenant_id/workload-runs", listWorkloadRuns(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/workload-runs", createWorkloadRun(pool, workspaceConfig),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.GET("/v1/tenants/:tenant_id/workload-runs/:run_id", getWorkloadRun(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	protected.POST("/v1/tenants/:tenant_id/workload-runs/:run_id/cancel", cancelWorkloadRun(pool),
		RequireScopes(auth.HakoAPIScope), HakoUserMiddleware(pool), TenantMembershipMiddleware(pool))
	return e
}

type workloadRunListResponse struct {
	Items  []domain.WorkloadRun `json:"items"`
	Total  int64                `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

func createWorkloadRun(pool transaction.Beginner, config WorkspaceCreateConfig) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		if !config.WorkloadRunsEnabled {
			return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "workload_runs_unavailable", Message: "Workload Run scheduling is not enabled"}})
		}
		var request workloadRunCreateRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidWorkloadRunRequest(c)
		}
		request.CommitSHA = strings.TrimSpace(request.CommitSHA)
		request.Ref = strings.TrimSpace(request.Ref)
		if request.GitHubInstallationID <= 0 || request.GitHubRepositoryID <= 0 ||
			!workloadCommitSHA.MatchString(request.CommitSHA) || len(request.Ref) > 1024 || strings.ContainsAny(request.Ref, "\r\n\x00") ||
			request.TimeoutSeconds < 0 || request.TimeoutSeconds > int64(workloads.MaxTimeout/time.Second) {
			return invalidWorkloadRunRequest(c)
		}
		idempotencyKey := strings.TrimSpace(c.Request().Header.Get("Idempotency-Key"))
		if idempotency.ValidateKey(idempotencyKey) != nil {
			return invalidWorkloadRunRequest(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		now := time.Now().UTC()
		runID, err := idgen.New("run_")
		if err != nil {
			return databaseUnavailable(c)
		}
		eventID, err := idgen.New("evt_")
		if err != nil {
			return databaseUnavailable(c)
		}
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(runID), TenantID: tenantID, Kind: domain.WorkloadJob,
			RequestedBy: user.ID, GitHubInstallation: request.GitHubInstallationID, GitHubRepository: request.GitHubRepositoryID,
			CorrelationID: RequestIDFromContext(c.Request().Context()),
			CommitSHA:     request.CommitSHA, Ref: request.Ref, RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
			IdempotencyKey: idempotencyKey, TimeoutSeconds: request.TimeoutSeconds, CreatedAt: now}
		stored, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return domain.WorkloadRun{}, err
			}
			stored, created, err := workloads.CreateOrGet(ctx, tx, run)
			if err != nil || !created {
				return stored, err
			}
			payload, err := json.Marshal(map[string]any{"schema_version": 1, "tenant_id": tenantID, "workload_run_id": stored.ID,
				"correlation_id": stored.CorrelationID})
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
				(id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt)
				VALUES ($1, 'workload_run', $2, 'workload_run.schedule_requested', $3, $4, NULL, 0)`,
				eventID, stored.ID, string(payload), now); err != nil {
				return domain.WorkloadRun{}, fmt.Errorf("enqueue Workload Run: %w", err)
			}
			details, err := json.Marshal(map[string]any{"kind": stored.Kind, "github_repository_id": stored.GitHubRepository})
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "workload_run.create",
				TargetType: "workload_run", TargetID: string(stored.ID), Details: details, OccurredAt: now}); err != nil {
				return domain.WorkloadRun{}, err
			}
			return stored, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workloads.ErrRepositoryInactive) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "repository_not_found", Message: "GitHub Repository is not active for this Tenant"}})
		}
		if errors.Is(err, workloads.ErrIdempotencyConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "idempotency_conflict", Message: "Idempotency-Key was already used for a different request"}})
		}
		if errors.Is(err, workloads.ErrQuotaExceeded) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "workload_quota_exceeded", Message: "Tenant concurrent Workload Run limit reached (5)"}})
		}
		if errors.Is(err, workloads.ErrPreviewQuotaExceeded) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "preview_quota_exceeded", Message: "Tenant Preview count or lifetime limit exceeded"}})
		}
		if errors.Is(err, quotas.ErrArtifactQuotaExceeded) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "artifact_quota_exceeded", Message: "Tenant artifact storage quota has no capacity for another Workload Run"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, stored)
	}
}

func invalidWorkloadRunRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "active GitHub Installation/Repository, immutable commit SHA, and a valid Idempotency-Key are required"}})
}

func listWorkloadRuns(pool transaction.Beginner) echo.HandlerFunc {
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
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workloadRunListResponse, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return workloadRunListResponse{}, err
			}
			var requestedBy domain.UserID
			if membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin {
				requestedBy = user.ID
			}
			items, total, err := workloads.List(ctx, tx, tenantID, requestedBy, limit, offset)
			return workloadRunListResponse{Items: items, Total: total, Limit: limit, Offset: offset}, err
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, result)
	}
}

func getWorkloadRun(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		runID := domain.WorkloadRunID(strings.TrimSpace(c.Param("run_id")))
		run, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			stored, err := workloads.Get(ctx, tx, tenantID, runID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			if membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin && stored.RequestedBy != user.ID {
				return domain.WorkloadRun{}, workloads.ErrNotFound
			}
			return stored, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workloads.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Workload Run not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, run)
	}
}

func cancelWorkloadRun(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		runID := domain.WorkloadRunID(strings.TrimSpace(c.Param("run_id")))
		now := time.Now().UTC()
		run, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			before, err := workloads.Get(ctx, tx, tenantID, runID)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			if membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin && before.RequestedBy != user.ID {
				return domain.WorkloadRun{}, workloads.ErrNotFound
			}
			after, err := workloads.RequestCancel(ctx, tx, tenantID, runID, now)
			if err != nil {
				return domain.WorkloadRun{}, err
			}
			if before.DesiredState != domain.WorkloadDesiredCancelled && after.DesiredState == domain.WorkloadDesiredCancelled && after.State != domain.WorkloadTimedOut {
				details, err := json.Marshal(map[string]string{"state": string(after.State)})
				if err != nil {
					return domain.WorkloadRun{}, err
				}
				if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
					Action: "workload_run.cancel", TargetType: "workload_run", TargetID: string(runID), Details: details, OccurredAt: now}); err != nil {
					return domain.WorkloadRun{}, err
				}
			}
			return after, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workloads.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Workload Run not found"}})
		}
		if errors.Is(err, workloads.ErrTransitionConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "state_conflict", Message: "Workload Run changed while cancellation was requested"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, run)
	}
}

func listGitHubInstallations(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installations, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Installation, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return nil, err
			}
			return githubregistry.List(ctx, tx, tenantID)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"installations": installations})
	}
}

func requestGitHubInstallation(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request struct {
			InstallationID int64  `json:"installation_id"`
			AccountLogin   string `json:"account_login"`
		}
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidGitHubInstallationRequest(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installation := githubregistry.Installation{
			TenantID: tenantID, InstallationID: request.InstallationID,
			AccountLogin: request.AccountLogin, RequestedBy: user.ID,
		}
		if _, err := githubregistry.NormalizeInstallation(installation); err != nil {
			return invalidGitHubInstallationRequest(c)
		}
		now := time.Now().UTC()
		type requestResult struct {
			installation githubregistry.Installation
			created      bool
		}
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (requestResult, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return requestResult{}, err
			}
			requested, created, err := githubregistry.Request(ctx, tx, installation, now)
			if err != nil {
				return requestResult{}, err
			}
			details, err := json.Marshal(map[string]any{"installation_id": requested.InstallationID, "account_login": requested.AccountLogin, "status": requested.Status})
			if err != nil {
				return requestResult{}, fmt.Errorf("encode GitHub installation audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{
				TenantID: tenantID, ActorID: user.ID, Action: "tenant.github_installation.request",
				TargetType: "github_app_installation", TargetID: fmt.Sprint(requested.InstallationID), Details: details, OccurredAt: now,
			}); err != nil {
				return requestResult{}, err
			}
			return requestResult{installation: requested, created: created}, nil
		})
		installation, created := result.installation, result.created
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "installation_conflict", Message: "GitHub App installation request conflicts with existing tenant state"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		status := http.StatusAccepted
		if !created {
			status = http.StatusOK
		}
		return c.JSON(status, map[string]any{"installation": installation, "verified": false})
	}
}

func invalidGitHubInstallationRequest(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "a positive installation_id and valid account_login are required"}})
}

func listGitHubRepositories(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installationID, err := strconv.ParseInt(strings.TrimSpace(c.Param("installation_id")), 10, 64)
		if err != nil || installationID <= 0 {
			return invalidGitHubInstallationRequest(c)
		}
		repositories, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]githubregistry.Repository, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return nil, err
			}
			return githubregistry.ListRepositories(ctx, tx, tenantID, installationID)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationNotActive) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Active GitHub Installation not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"repositories": repositories})
	}
}

type repositoryFactoryRequest struct {
	Enabled *bool `json:"enabled"`
}

func configureGitHubRepositoryAgentWritePolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installationID, installationErr := strconv.ParseInt(strings.TrimSpace(c.Param("installation_id")), 10, 64)
		repositoryID, repositoryErr := strconv.ParseInt(strings.TrimSpace(c.Param("repository_id")), 10, 64)
		if tenantID == "" || installationErr != nil || installationID <= 0 || repositoryErr != nil || repositoryID <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_repository", Message: "Valid Installation and Repository IDs are required"}})
		}
		var policy agenttask.WritePolicy
		if err := decodeJSONRequest(c, &policy); err != nil || policy.Validate() != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_agent_write_policy", Message: "Agent write policy is invalid or exceeds its bounds"}})
		}
		now := time.Now().UTC()
		stored, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (agenttask.WritePolicy, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return agenttask.WritePolicy{}, err
			}
			updated, err := githubregistry.SetRepositoryAgentWritePolicy(ctx, tx, tenantID, installationID, repositoryID, policy)
			if err != nil {
				return agenttask.WritePolicy{}, err
			}
			details, err := json.Marshal(map[string]any{
				"installation_id": installationID, "repository_id": repositoryID, "enabled": updated.Enabled,
				"allow_pull_requests": updated.AllowPullRequests, "branch_prefix_count": len(updated.BranchPrefixes),
				"required_check_count": len(updated.RequiredChecks),
			})
			if err != nil {
				return agenttask.WritePolicy{}, fmt.Errorf("encode Agent write policy audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
				Action: "tenant.github_repository.agent_write_policy.configure", TargetType: "github_repository",
				TargetID: fmt.Sprint(repositoryID), Details: details, OccurredAt: now}); err != nil {
				return agenttask.WritePolicy{}, err
			}
			return updated, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationNotActive) || errors.Is(err, githubregistry.ErrRepositoryNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Active Factory-enabled GitHub Repository not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"write_policy": stored, "execution_enabled": false})
	}
}

func configureGitHubRepositoryFactory(pool transaction.Beginner, workloadRunsEnabled bool) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		if !workloadRunsEnabled {
			return c.JSON(http.StatusServiceUnavailable, errorResponse{Error: errorBody{Code: "workload_runs_unavailable", Message: "Webhook-triggered Workload Runs are not enabled"}})
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installationID, installationErr := strconv.ParseInt(strings.TrimSpace(c.Param("installation_id")), 10, 64)
		repositoryID, repositoryErr := strconv.ParseInt(strings.TrimSpace(c.Param("repository_id")), 10, 64)
		if tenantID == "" || installationErr != nil || installationID <= 0 || repositoryErr != nil || repositoryID <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_repository", Message: "Valid Installation and Repository IDs are required"}})
		}
		var request repositoryFactoryRequest
		if err := decodeJSONRequest(c, &request); err != nil || request.Enabled == nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "enabled must be explicitly set to true or false"}})
		}
		now := time.Now().UTC()
		repository, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.Repository, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return githubregistry.Repository{}, err
			}
			updated, err := githubregistry.SetRepositoryFactoryEnabled(ctx, tx, tenantID, installationID, repositoryID, *request.Enabled, now)
			if err != nil {
				return githubregistry.Repository{}, err
			}
			details, err := json.Marshal(map[string]any{"installation_id": installationID, "repository_id": repositoryID, "factory_enabled": updated.FactoryEnabled})
			if err != nil {
				return githubregistry.Repository{}, fmt.Errorf("encode Repository factory audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "tenant.github_repository.factory.configure",
				TargetType: "github_repository", TargetID: fmt.Sprint(repositoryID), Details: details, OccurredAt: now}); err != nil {
				return githubregistry.Repository{}, err
			}
			return updated, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationNotActive) || errors.Is(err, githubregistry.ErrRepositoryNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Active registered GitHub Repository not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"repository": repository})
	}
}

func configureGitHubRepositoryActionsRunner(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installationID, installationErr := strconv.ParseInt(strings.TrimSpace(c.Param("installation_id")), 10, 64)
		repositoryID, repositoryErr := strconv.ParseInt(strings.TrimSpace(c.Param("repository_id")), 10, 64)
		if tenantID == "" || installationErr != nil || installationID <= 0 || repositoryErr != nil || repositoryID <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_repository", Message: "Valid Installation and Repository IDs are required"}})
		}
		var request repositoryFactoryRequest
		if err := decodeJSONRequest(c, &request); err != nil || request.Enabled == nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "enabled must be explicitly set to true or false"}})
		}
		now := time.Now().UTC()
		repository, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.Repository, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return githubregistry.Repository{}, err
			}
			updated, err := githubregistry.SetRepositoryActionsRunnerEnabled(ctx, tx, tenantID, installationID, repositoryID, *request.Enabled, now)
			if err != nil {
				return githubregistry.Repository{}, err
			}
			details, err := json.Marshal(map[string]any{"installation_id": installationID, "repository_id": repositoryID, "actions_runner_enabled": updated.ActionsRunnerEnabled})
			if err != nil {
				return githubregistry.Repository{}, fmt.Errorf("encode Repository Actions runner audit details: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "tenant.github_repository.actions_runner.configure",
				TargetType: "github_repository", TargetID: fmt.Sprint(repositoryID), Details: details, OccurredAt: now}); err != nil {
				return githubregistry.Repository{}, err
			}
			return updated, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationNotActive) || errors.Is(err, githubregistry.ErrRepositoryNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Active registered GitHub Repository not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"repository": repository})
	}
}

func configureGitHubRepositoryActionsRunnerWorkflowPolicy(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installationID, installationErr := strconv.ParseInt(strings.TrimSpace(c.Param("installation_id")), 10, 64)
		repositoryID, repositoryErr := strconv.ParseInt(strings.TrimSpace(c.Param("repository_id")), 10, 64)
		if tenantID == "" || installationErr != nil || installationID <= 0 || repositoryErr != nil || repositoryID <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_repository", Message: "Valid Installation and Repository IDs are required"}})
		}
		var policy githubregistry.ActionsRunnerWorkflowPolicy
		if err := decodeJSONRequest(c, &policy); err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_workflow_policy", Message: "A valid workflow path, ref/event allowlist, and runner label policy are required"}})
		}
		if !githubregistry.ValidActionsRunnerWorkflowPolicy(policy) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_workflow_policy", Message: "A valid workflow path, ref/event allowlist, and runner label policy are required"}})
		}
		now := time.Now().UTC()
		stored, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.ActionsRunnerWorkflowPolicy, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return githubregistry.ActionsRunnerWorkflowPolicy{}, err
			}
			updated, err := githubregistry.SetRepositoryActionsRunnerWorkflowPolicy(ctx, tx, tenantID, installationID, repositoryID, policy, now)
			if err != nil {
				return githubregistry.ActionsRunnerWorkflowPolicy{}, err
			}
			details, err := json.Marshal(map[string]any{"installation_id": installationID, "repository_id": repositoryID, "workflow_path": updated.WorkflowPath,
				"allowed_refs": updated.AllowedRefs, "allowed_events": updated.AllowedEvents, "required_labels": updated.RequiredLabels, "allowed_labels": updated.AllowedLabels})
			if err != nil {
				return githubregistry.ActionsRunnerWorkflowPolicy{}, errors.New("encode workflow policy audit details")
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "tenant.github_repository.actions_runner.workflow_policy.configure",
				TargetType: "github_repository", TargetID: fmt.Sprint(repositoryID), Details: details, OccurredAt: now}); err != nil {
				return githubregistry.ActionsRunnerWorkflowPolicy{}, err
			}
			return updated, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, githubregistry.ErrInstallationNotActive) || errors.Is(err, githubregistry.ErrRepositoryNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Enabled Tenant GitHub Actions runner repository not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"workflow_policy": stored})
	}
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
			CorrelationID: RequestIDFromContext(c.Request().Context()),
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

func getTenantUsage(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		usage, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaces.QuotaUsage, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return workspaces.QuotaUsage{}, err
			}
			return workspaces.ReadQuotaUsage(ctx, tx, tenantID)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, usage)
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
			CorrelationID:        RequestIDFromContext(c.Request().Context()),
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
