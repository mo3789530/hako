package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

const workloadSpecRequestIDHeader = "X-Hako-Request-ID"

var workloadSpecRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// ResourcePlaneWorkloadSpecKeyResolver returns the HMAC key for one exact
// Resource Plane. Production implementations should use a separate secret
// reference from health reporting; key bytes must never be logged or stored in DSQL.
type ResourcePlaneWorkloadSpecKeyResolver func(context.Context, domain.ResourcePlaneID) ([]byte, error)

type resourcePlaneWorkloadSpecResponse struct {
	SchemaVersion        int                       `json:"schema_version"`
	TenantID             domain.TenantID           `json:"tenant_id"`
	WorkloadRunID        domain.WorkloadRunID      `json:"workload_run_id"`
	AttemptID            string                    `json:"attempt_id"`
	Generation           int64                     `json:"generation"`
	ResourcePlaneID      domain.ResourcePlaneID    `json:"resource_plane_id"`
	GitHubInstallationID int64                     `json:"github_installation_id"`
	GitHubRepositoryID   int64                     `json:"github_repository_id"`
	GitHubOwner          string                    `json:"github_owner"`
	GitHubRepository     string                    `json:"github_repository"`
	GitHubIntegrationID  string                    `json:"github_integration_id,omitempty"`
	GitHubConfigRevision int64                     `json:"github_config_revision,omitempty"`
	GitHubCredentialGen  int64                     `json:"github_credential_generation,omitempty"`
	CommitSHA            string                    `json:"commit_sha"`
	Ref                  string                    `json:"ref"`
	RuntimeClass         string                    `json:"runtime_class"`
	TrustLevel           domain.WorkloadTrustLevel `json:"trust_level"`
	RequestedAt          time.Time                 `json:"requested_at"`
	Deadline             time.Time                 `json:"deadline"`
}

func getResourcePlaneWorkloadSpecification(pool transaction.Beginner, resolveKey ResourcePlaneWorkloadSpecKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		planeID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		tenantID := domain.TenantID(c.Param("tenant_id"))
		runID := domain.WorkloadRunID(c.Param("workload_run_id"))
		attemptID := c.Param("attempt_id")
		generation, generationErr := strconv.ParseInt(c.Param("generation"), 10, 64)
		if planeID == "" || tenantID == "" || runID == "" || attemptID == "" || generationErr != nil ||
			generation < 1 || pool == nil || resolveKey == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		if c.Request().ContentLength != 0 || len(c.Request().TransferEncoding) != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "Workload specification request must not have a body")
		}
		timestampText := c.Request().Header.Get(healthReportTimestampHeader)
		requestID := c.Request().Header.Get(workloadSpecRequestIDHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || !workloadSpecRequestIDPattern.MatchString(requestID) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		requestTime := time.Unix(timestamp, 0).UTC()
		now := time.Now().UTC()
		if requestTime.Before(now.Add(-5*time.Minute)) || requestTime.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		keyCtx, cancelKey := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(keyCtx, planeID)
		cancelKey()
		if err != nil || len(key) < 32 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		now = time.Now().UTC()
		requestTime = time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-5*time.Minute)) || requestTime.After(now.Add(30*time.Second)) ||
			!validResourcePlaneHealthSignature(key, http.MethodGet, c.Request().URL.EscapedPath(), timestampText, requestID, nil,
				c.Request().Header.Get(healthReportSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		metadata, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workloads.ExecutionMetadata, error) {
			return workloads.ResolveExecutionMetadata(ctx, tx, planeID, tenantID, runID, attemptID, generation, now)
		})
		if errors.Is(err, workloads.ErrExecutionSpecNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "Workload execution specification not found")
		}
		if err != nil {
			return fmt.Errorf("resolve Resource Plane Workload specification: %w", err)
		}
		return c.JSON(http.StatusOK, resourcePlaneWorkloadSpecResponse{
			SchemaVersion: 1, TenantID: metadata.TenantID, WorkloadRunID: metadata.WorkloadRunID,
			AttemptID: metadata.AttemptID, Generation: metadata.Generation, ResourcePlaneID: metadata.ResourcePlaneID,
			GitHubInstallationID: metadata.GitHubInstallationID, GitHubRepositoryID: metadata.GitHubRepositoryID,
			GitHubOwner: metadata.GitHubOwner, GitHubRepository: metadata.GitHubRepository,
			GitHubIntegrationID: metadata.GitHubIntegrationID, GitHubConfigRevision: metadata.GitHubConfigRevision,
			GitHubCredentialGen: metadata.GitHubCredentialGen, CommitSHA: metadata.CommitSHA,
			Ref: metadata.Ref, RuntimeClass: metadata.RuntimeClass, TrustLevel: metadata.TrustLevel,
			RequestedAt: metadata.RequestedAt.UTC(), Deadline: metadata.Deadline.UTC(),
		})
	}
}
