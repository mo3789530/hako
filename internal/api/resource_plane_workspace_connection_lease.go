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
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
)

const maxWorkspaceConnectionLeaseRequestBytes = 2 << 10

var workspaceConnectionLeaseIDPattern = workspaceServiceDesiredRequestIDPattern

type workspaceConnectionLeaseRequest struct {
	SessionID            string                 `json:"session_id"`
	TenantID             domain.TenantID        `json:"tenant_id"`
	WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID      domain.ResourcePlaneID `json:"resource_plane_id"`
	MicroVMID            string                 `json:"microvm_id"`
	AllocationGeneration int64                  `json:"allocation_generation"`
	ExpiresAt            time.Time              `json:"expires_at"`
}

func workspaceConnectionLeaseHandler(pool transaction.Beginner, resolveKey ResourcePlaneServiceReportKeyResolver, release bool) echo.HandlerFunc {
	return func(c *echo.Context) error {
		planeID := domain.ResourcePlaneID(strings.TrimSpace(c.Param("resource_plane_id")))
		leaseID := strings.TrimSpace(c.Param("lease_id"))
		if pool == nil || resolveKey == nil || planeID == "" || !workspaceConnectionLeaseIDPattern.MatchString(leaseID) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		if (release && c.Request().Method != http.MethodPost) || (!release && c.Request().Method != http.MethodPut) ||
			c.Request().URL.RawQuery != "" || c.Request().URL.ForceQuery || len(c.Request().TransferEncoding) != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace connection lease request")
		}
		contentType := strings.ToLower(strings.TrimSpace(c.Request().Header.Get("Content-Type")))
		if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "Workspace connection lease must be application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxWorkspaceConnectionLeaseRequestBytes))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace connection lease request")
		}
		timestampText := c.Request().Header.Get(healthReportTimestampHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || c.Request().Header.Get(healthReportIDHeader) != leaseID {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		now := time.Now().UTC()
		requestTime := time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		keyCtx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(keyCtx, planeID)
		cancel()
		if err != nil || len(key) < 32 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		now = time.Now().UTC()
		requestTime = time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) ||
			!validResourcePlaneHealthSignature(key, c.Request().Method, c.Request().URL.EscapedPath(), timestampText, leaseID, body,
				c.Request().Header.Get(healthReportSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		var request workspaceConnectionLeaseRequest
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.ResourcePlaneID != planeID ||
			!workspaceConnectionLeaseIDPattern.MatchString(request.SessionID) || request.TenantID == "" || request.WorkspaceID == "" ||
			request.MicroVMID == "" || request.AllocationGeneration < 1 || request.ExpiresAt.IsZero() {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace connection lease request")
		}
		lease := workspaceactivity.Lease{ID: leaseID, SessionID: request.SessionID, TenantID: request.TenantID, WorkspaceID: request.WorkspaceID,
			ResourcePlaneID: planeID, MicroVMID: request.MicroVMID, AllocationGeneration: request.AllocationGeneration,
			ExpiresAt: request.ExpiresAt, Now: now}
		var denial workspaceactivity.AdmissionDenial
		denial, err = transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workspaceactivity.AdmissionDenial, error) {
			if release {
				_, err := workspaceactivity.Release(ctx, tx, lease)
				return workspaceactivity.AdmissionAllowed, err
			}
			return workspaceactivity.Admit(ctx, tx, lease)
		})
		if err == nil {
			switch denial {
			case workspaceactivity.AdmissionRateLimited:
				return echo.NewHTTPError(http.StatusTooManyRequests, "Workspace connection start rate limit reached")
			case workspaceactivity.AdmissionAtCapacity:
				return echo.NewHTTPError(http.StatusTooManyRequests, "Workspace connection limit reached")
			case workspaceactivity.AdmissionSessionConnected:
				return echo.NewHTTPError(http.StatusConflict, "Workspace Session already has an active connection")
			}
		}
		if errors.Is(err, workspaceactivity.ErrInvalidLease) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace connection lease")
		}
		if errors.Is(err, workspaceactivity.ErrStaleLease) {
			return echo.NewHTTPError(http.StatusConflict, "Workspace connection lease is stale")
		}
		if errors.Is(err, workspaceactivity.ErrConnectionLimit) || errors.Is(err, workspaceactivity.ErrConnectionRateLimit) {
			return echo.NewHTTPError(http.StatusTooManyRequests, "Workspace connection limit reached")
		}
		if errors.Is(err, workspaceactivity.ErrSessionConnected) {
			return echo.NewHTTPError(http.StatusConflict, "Workspace Session already has an active connection")
		}
		if err != nil {
			return fmt.Errorf("persist Workspace connection lease: %w", err)
		}
		return c.NoContent(http.StatusNoContent)
	}
}
