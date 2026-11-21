package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
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

type ResourcePlaneArtifactCleanupKeyResolver func(context.Context, domain.ResourcePlaneID) ([]byte, error)

type artifactDeletionAcknowledgement struct {
	TenantID   domain.TenantID      `json:"tenant_id"`
	RunID      domain.WorkloadRunID `json:"workload_run_id"`
	StorageRef string               `json:"storage_ref"`
	ExpiresAt  time.Time            `json:"expires_at"`
}

var artifactIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func listResourcePlaneExpiredArtifacts(pool transaction.Beginner, resolveKey ResourcePlaneArtifactCleanupKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		planeID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		limit, err := strconv.Atoi(c.QueryParam("limit"))
		if err != nil || planeID == "" || pool == nil || resolveKey == nil || limit < 1 || limit > workloads.MaxExpiredArtifactBatchSize {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid artifact cleanup request")
		}
		if c.Request().ContentLength != 0 || len(c.Request().TransferEncoding) != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "artifact listing request must not have a body")
		}
		if err := authenticateArtifactCleanupRequest(c, planeID, resolveKey); err != nil {
			return err
		}
		now := time.Now().UTC()
		artifacts, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
			return workloads.ListExpiredArtifactsForResourcePlane(ctx, tx, planeID, now, limit)
		})
		if err != nil {
			return fmt.Errorf("list expired Resource Plane artifacts: %w", err)
		}
		return c.JSON(http.StatusOK, map[string]any{"artifacts": artifacts})
	}
}

func acknowledgeResourcePlaneArtifactDeletion(pool transaction.Beginner, resolveKey ResourcePlaneArtifactCleanupKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		planeID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		artifactID := c.Param("artifact_id")
		if planeID == "" || !artifactIDPattern.MatchString(artifactID) || pool == nil || resolveKey == nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid artifact cleanup request")
		}
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxJSONRequestBytes+1))
		if err != nil || len(body) == 0 || len(body) > maxJSONRequestBytes || c.Request().ContentLength > maxJSONRequestBytes {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid artifact cleanup request")
		}
		if err := authenticateArtifactCleanupRequest(c, planeID, resolveKey, body); err != nil {
			return err
		}
		mediaType, _, mediaTypeErr := mime.ParseMediaType(c.Request().Header.Get("Content-Type"))
		if mediaTypeErr != nil || mediaType != "application/json" {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "artifact cleanup acknowledgement must be JSON")
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var acknowledgement artifactDeletionAcknowledgement
		if decoder.Decode(&acknowledgement) != nil || decoder.Decode(new(any)) != io.EOF || acknowledgement.TenantID == "" ||
			acknowledgement.RunID == "" || acknowledgement.StorageRef == "" || acknowledgement.ExpiresAt.IsZero() {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid artifact cleanup acknowledgement")
		}
		artifact := domain.WorkloadArtifact{ID: artifactID, TenantID: acknowledgement.TenantID,
			RunID: acknowledgement.RunID, StorageRef: acknowledgement.StorageRef, ExpiresAt: &acknowledgement.ExpiresAt}
		removed, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return workloads.DeleteExpiredArtifactForResourcePlane(ctx, tx, planeID, artifact, time.Now().UTC())
		})
		if err != nil {
			return fmt.Errorf("acknowledge deleted Resource Plane artifact: %w", err)
		}
		return c.JSON(http.StatusOK, map[string]bool{"removed": removed})
	}
}

func authenticateArtifactCleanupRequest(c *echo.Context, planeID domain.ResourcePlaneID, resolveKey ResourcePlaneArtifactCleanupKeyResolver, body ...[]byte) error {
	request := c.Request()
	if request.ContentLength > maxJSONRequestBytes || len(request.TransferEncoding) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid artifact cleanup request")
	}
	timestampText := request.Header.Get(healthReportTimestampHeader)
	requestID := request.Header.Get(workloadSpecRequestIDHeader)
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil || !workloadSpecRequestIDPattern.MatchString(requestID) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
	}
	now := time.Now().UTC()
	requestTime := time.Unix(timestamp, 0).UTC()
	if requestTime.Before(now.Add(-5*time.Minute)) || requestTime.After(now.Add(30*time.Second)) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
	}
	keyCtx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	key, err := resolveKey(keyCtx, planeID)
	cancel()
	if err != nil || len(key) < 32 {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
	}
	defer clear(key)
	var payload []byte
	if len(body) > 0 {
		payload = body[0]
	}
	signedPath := request.URL.EscapedPath()
	if request.URL.RawQuery != "" {
		signedPath += "?" + request.URL.RawQuery
	}
	if !validResourcePlaneHealthSignature(key, request.Method, signedPath, timestampText, requestID, payload,
		request.Header.Get(healthReportSignatureHeader)) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
	}
	return nil
}
