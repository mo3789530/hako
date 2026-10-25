package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
	"github.com/mo3789530/hako/internal/workspacesession"
)

var (
	workloadArtifactAccessID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	workloadArtifactTicketID = regexp.MustCompile(`^[a-f0-9]{48}$`)
)

type workloadArtifactTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Kind      string    `json:"kind"`
}

type workloadArtifactAccessGrant struct {
	Claims workloadartifactaccess.Claims `json:"claims"`
}

type workloadArtifactAccessResult struct {
	StorageRef string `json:"storage_ref"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	Kind       string `json:"kind"`
}

func createWorkloadArtifactAccessTicket(pool transaction.Beginner, resolveKey ResourcePlaneWorkspaceSessionKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil || resolveKey == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		runID := domain.WorkloadRunID(strings.TrimSpace(c.Param("run_id")))
		artifactID := strings.TrimSpace(c.Param("artifact_id"))
		if !workloadArtifactAccessID.MatchString(string(tenantID)) || !workloadArtifactAccessID.MatchString(string(runID)) ||
			!workloadArtifactAccessID.MatchString(artifactID) || c.Request().Body != nil && c.Request().ContentLength != 0 {
			return invalidWorkloadArtifactAccess(c)
		}
		stored, artifact, err := lookupVisibleWorkloadArtifact(c.Request().Context(), pool, user.ID, tenantID, runID, artifactID)
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, workloads.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Workload artifact not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		if !isS3WorkloadArtifact(artifact.StorageRef) || artifact.SizeBytes > workloadartifactaccess.MaxArtifactSize {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "artifact_unavailable", Message: "Workload artifact is not available for download"}})
		}
		keyCtx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(keyCtx, stored.ResourcePlaneID)
		cancel()
		if err != nil || len(key) < 32 || len(key) > 4096 {
			clear(key)
			return databaseUnavailable(c)
		}
		defer clear(key)
		now := time.Now().UTC()
		ticket, claims, err := workloadartifactaccess.Issue(key, workloadartifactaccess.Claims{
			UserID: string(user.ID), TenantID: string(tenantID), WorkloadRunID: string(runID),
			ArtifactID: artifact.ID, ResourcePlane: string(stored.ResourcePlaneID),
		}, now, nil)
		if err != nil {
			return databaseUnavailable(c)
		}
		if err := recordWorkloadArtifactAccess(c.Request().Context(), pool, user.ID, claims, now); err != nil {
			if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, workloads.ErrNotFound) {
				return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Workload artifact not found"}})
			}
			return databaseUnavailable(c)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusCreated, workloadArtifactTicketResponse{Ticket: ticket, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
			SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256, Kind: artifact.Kind})
	}
}

func lookupVisibleWorkloadArtifact(ctx context.Context, pool transaction.Beginner, userID domain.UserID, tenantID domain.TenantID,
	runID domain.WorkloadRunID, artifactID string) (domain.WorkloadRun, domain.WorkloadArtifact, error) {
	type result struct {
		run      domain.WorkloadRun
		artifact domain.WorkloadArtifact
	}
	resolved, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (result, error) {
		membership, err := authz.RequireTenantMembership(ctx, tx, userID, tenantID)
		if err != nil {
			return result{}, err
		}
		run, err := workloads.Get(ctx, tx, tenantID, runID)
		if err != nil {
			return result{}, err
		}
		if membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin && run.RequestedBy != userID {
			return result{}, workloads.ErrNotFound
		}
		artifacts, err := workloads.ListArtifacts(ctx, tx, tenantID, runID)
		if err != nil {
			return result{}, err
		}
		for _, artifact := range artifacts {
			if artifact.ID == artifactID {
				return result{run: run, artifact: artifact}, nil
			}
		}
		return result{}, workloads.ErrNotFound
	})
	return resolved.run, resolved.artifact, err
}

func recordWorkloadArtifactAccess(ctx context.Context, pool transaction.Beginner, userID domain.UserID,
	claims workloadartifactaccess.Claims, now time.Time) error {
	_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		membership, err := authz.RequireTenantMembership(ctx, tx, userID, domain.TenantID(claims.TenantID))
		if err != nil {
			return struct{}{}, err
		}
		run, err := workloads.Get(ctx, tx, domain.TenantID(claims.TenantID), domain.WorkloadRunID(claims.WorkloadRunID))
		if err != nil {
			return struct{}{}, err
		}
		if membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin && run.RequestedBy != userID {
			return struct{}{}, workloads.ErrNotFound
		}
		artifacts, err := workloads.ListArtifacts(ctx, tx, run.TenantID, run.ID)
		if err != nil {
			return struct{}{}, err
		}
		found := false
		for _, artifact := range artifacts {
			if artifact.ID == claims.ArtifactID && isS3WorkloadArtifact(artifact.StorageRef) {
				found = true
				break
			}
		}
		if !found || run.ResourcePlaneID != domain.ResourcePlaneID(claims.ResourcePlane) {
			return struct{}{}, workloads.ErrNotFound
		}
		details, err := json.Marshal(map[string]string{"artifact_id": claims.ArtifactID, "kind": "download_ticket"})
		if err != nil {
			return struct{}{}, err
		}
		if err := audit.Append(ctx, tx, audit.Event{TenantID: run.TenantID, ActorID: userID,
			Action: "workload_artifact.download_ticket", TargetType: "workload_artifact", TargetID: claims.ArtifactID,
			Details: details, OccurredAt: now}); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

func isS3WorkloadArtifact(reference string) bool {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.Path == "" {
		return false
	}
	for _, component := range strings.Split(parsed.Path, "/") {
		if component == ".." || component == "." {
			return false
		}
	}
	return true
}

func invalidWorkloadArtifactAccess(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Tenant, Run, and artifact identity are required"}})
}

// resolveResourcePlaneWorkloadArtifact returns storage metadata only to the
// Resource Plane whose HMAC identity matches the ticket's immutable Plane.
func resolveResourcePlaneWorkloadArtifact(pool transaction.Beginner, resolveKey ResourcePlaneWorkspaceSessionKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		planeID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		if !workloadArtifactAccessID.MatchString(string(planeID)) || pool == nil || resolveKey == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		mediaType, _, mediaErr := mime.ParseMediaType(c.Request().Header.Get("Content-Type"))
		if mediaErr != nil || mediaType != "application/json" {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "artifact access requires application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 8<<10))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid artifact access request")
		}
		var request workloadArtifactAccessGrant
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Claims.ResourcePlane != string(planeID) ||
			request.Claims.ArtifactID != c.Param("artifact_id") ||
			!workloadArtifactAccessID.MatchString(request.Claims.UserID) || !workloadArtifactAccessID.MatchString(request.Claims.TenantID) ||
			!workloadArtifactAccessID.MatchString(request.Claims.WorkloadRunID) || !workloadArtifactAccessID.MatchString(request.Claims.ArtifactID) ||
			request.Claims.Version != 1 || request.Claims.Audience != workloadartifactaccess.Audience ||
			!workloadArtifactTicketID.MatchString(request.Claims.TicketID) || request.Claims.IssuedAt <= 0 || request.Claims.ExpiresAt <= request.Claims.IssuedAt ||
			request.Claims.ExpiresAt-request.Claims.IssuedAt > int64(workloadartifactaccess.TicketMaxTTL/time.Second) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		timestampText := c.Request().Header.Get(activeLookupTimestampHeader)
		requestID := c.Request().Header.Get(activeLookupRequestIDHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || !activeSessionLookupRequestIDPattern.MatchString(requestID) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		now := time.Now().UTC()
		requestTime := time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-5*time.Minute)) || requestTime.After(now.Add(30*time.Second)) ||
			!now.Before(time.Unix(request.Claims.ExpiresAt, 0)) || time.Unix(request.Claims.IssuedAt, 0).After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		keyCtx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(keyCtx, planeID)
		cancel()
		if err != nil || len(key) < 32 || len(key) > 4096 {
			clear(key)
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		defer clear(key)
		now = time.Now().UTC()
		requestTime = time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-5*time.Minute)) || requestTime.After(now.Add(30*time.Second)) ||
			!now.Before(time.Unix(request.Claims.ExpiresAt, 0)) ||
			!workspacesession.VerifyActiveLookupSignature(key, http.MethodPost, c.Request().URL.EscapedPath(), timestampText,
				requestID, body, c.Request().Header.Get(activeLookupSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		run, artifact, err := lookupVisibleWorkloadArtifact(c.Request().Context(), pool, domain.UserID(request.Claims.UserID),
			domain.TenantID(request.Claims.TenantID), domain.WorkloadRunID(request.Claims.WorkloadRunID), request.Claims.ArtifactID)
		if err != nil || run.ResourcePlaneID != planeID || !isS3WorkloadArtifact(artifact.StorageRef) {
			return echo.NewHTTPError(http.StatusNotFound, "Workload artifact not found")
		}
		return c.JSON(http.StatusOK, workloadArtifactAccessResult{StorageRef: artifact.StorageRef, SHA256: artifact.SHA256,
			SizeBytes: artifact.SizeBytes, Kind: artifact.Kind})
	}
}
