package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaces"
)

const maxWorkspaceRepositoryCheckoutReportBytes = 8 << 10

type workspaceRepositoryCheckoutReport struct {
	TenantID        domain.TenantID        `json:"tenant_id"`
	WorkspaceID     domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
	InstallationID  int64                  `json:"installation_id"`
	RepositoryID    int64                  `json:"repository_id"`
	CommitSHA       string                 `json:"commit_sha,omitempty"`
	State           string                 `json:"state"`
	ObservedAt      time.Time              `json:"observed_at"`
}

type workspaceRepositoryCheckoutReportResponse struct {
	Accepted bool `json:"accepted"`
}

func putResourcePlaneWorkspaceRepositoryCheckout(pool transaction.Beginner, resolveKey ResourcePlaneServiceReportKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		planeID := domain.ResourcePlaneID(strings.TrimSpace(c.Param("resource_plane_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		if pool == nil || resolveKey == nil || planeID == "" || workspaceID == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace repository checkout credentials")
		}
		contentType := strings.ToLower(strings.TrimSpace(c.Request().Header.Get("Content-Type")))
		if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "report must be application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxWorkspaceRepositoryCheckoutReportBytes))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace repository checkout report")
		}
		reportID := c.Request().Header.Get(healthReportIDHeader)
		timestampText := c.Request().Header.Get(healthReportTimestampHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || len(reportID) != 48 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace repository checkout credentials")
		}
		now := time.Now().UTC()
		requestTime := time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace repository checkout credentials")
		}
		resolveCtx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(resolveCtx, planeID)
		cancel()
		if err != nil || len(key) < 32 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace repository checkout credentials")
		}
		now = time.Now().UTC()
		requestTime = time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) ||
			!validResourcePlaneHealthSignature(key, c.Request().Method, c.Request().URL.EscapedPath(), timestampText, reportID, body,
				c.Request().Header.Get(healthReportSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace repository checkout credentials")
		}
		var report workspaceRepositoryCheckoutReport
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&report) != nil || decoder.Decode(new(any)) != io.EOF || report.TenantID == "" || report.WorkspaceID != workspaceID ||
			report.ResourcePlaneID != planeID || report.InstallationID < 1 || report.RepositoryID < 1 || report.ObservedAt.IsZero() ||
			(report.State != "ready" && report.State != "failed") || (report.CommitSHA != "" && !validWorkspaceCheckoutCommitSHA(report.CommitSHA)) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace repository checkout report")
		}
		if report.ObservedAt.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || report.ObservedAt.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusBadRequest, "stale Workspace repository checkout report")
		}
		err = workspaces.RecordWorkspaceRepositoryCheckoutResult(c.Request().Context(), pool, transaction.DefaultPolicy(),
			report.TenantID, report.WorkspaceID, report.ResourcePlaneID, report.InstallationID, report.RepositoryID, report.CommitSHA, report.State, now)
		if errors.Is(err, workspaces.ErrWorkspaceRepositoryCheckoutNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "Workspace repository checkout target not found")
		}
		if errors.Is(err, workspaces.ErrWorkspaceRepositoryCommitConflict) || errors.Is(err, workspaces.ErrWorkspaceRepositoryCheckoutConflict) {
			return echo.NewHTTPError(http.StatusConflict, "Workspace repository checkout result conflicts with its immutable pin")
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace repository checkout report")
		}
		return c.JSON(http.StatusOK, workspaceRepositoryCheckoutReportResponse{Accepted: true})
	}
}

func validWorkspaceCheckoutCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
