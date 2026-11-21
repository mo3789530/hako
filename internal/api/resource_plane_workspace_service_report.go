package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/mo3789530/hako/internal/store/services"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const maxWorkspaceServiceReportBytes = 8 << 10

type ResourcePlaneServiceReportKeyResolver func(context.Context, domain.ResourcePlaneID) ([]byte, error)

type workspaceServiceRuntimeReport struct {
	TenantID             domain.TenantID        `json:"tenant_id"`
	WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID      domain.ResourcePlaneID `json:"resource_plane_id"`
	MicroVMID            string                 `json:"microvm_id"`
	AllocationGeneration int64                  `json:"allocation_generation"`
	ServiceID            domain.ServiceID       `json:"service_id"`
	Revision             int64                  `json:"revision"`
	ObservedRevision     int64                  `json:"observed_revision"`
	DesiredState         string                 `json:"desired_state"`
	ObservedState        string                 `json:"observed_state"`
	Healthy              bool                   `json:"healthy"`
	ErrorCode            string                 `json:"error_code,omitempty"`
	ObservedAt           time.Time              `json:"observed_at"`
}

type workspaceServiceReportResponse struct {
	Accepted bool  `json:"accepted"`
	Revision int64 `json:"revision"`
}

func putResourcePlaneWorkspaceServiceReport(pool transaction.Beginner, resolveKey ResourcePlaneServiceReportKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		resourcePlaneID := domain.ResourcePlaneID(strings.TrimSpace(c.Param("resource_plane_id")))
		serviceID := domain.ServiceID(strings.TrimSpace(c.Param("service_id")))
		if pool == nil || resolveKey == nil || resourcePlaneID == "" || serviceID == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace Service report credentials")
		}
		contentType := strings.ToLower(strings.TrimSpace(c.Request().Header.Get("Content-Type")))
		if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "report must be application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxWorkspaceServiceReportBytes))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace Service report body")
		}
		reportID := c.Request().Header.Get(healthReportIDHeader)
		timestampText := c.Request().Header.Get(healthReportTimestampHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || len(reportID) != 48 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace Service report credentials")
		}
		now := time.Now().UTC()
		requestTime := time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace Service report credentials")
		}
		resolveCtx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(resolveCtx, resourcePlaneID)
		cancel()
		if err != nil || len(key) < 32 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace Service report credentials")
		}
		now = time.Now().UTC()
		requestTime = time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) ||
			!validResourcePlaneHealthSignature(key, c.Request().Method, c.Request().URL.EscapedPath(), timestampText, reportID, body,
				c.Request().Header.Get(healthReportSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Workspace Service report credentials")
		}
		var report workspaceServiceRuntimeReport
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&report) != nil || decoder.Decode(new(any)) != io.EOF || report.ServiceID != serviceID ||
			report.ResourcePlaneID != resourcePlaneID || report.ObservedAt.IsZero() {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace Service report")
		}
		payloadDigest := sha256.Sum256(body)
		accepted, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return services.ApplyObservation(ctx, tx, services.ObservationInput{
				ReportID: reportID, PayloadSHA256: hex.EncodeToString(payloadDigest[:]), TenantID: report.TenantID,
				WorkspaceID: report.WorkspaceID, ResourcePlaneID: report.ResourcePlaneID, MicroVMID: report.MicroVMID,
				AllocationGeneration: report.AllocationGeneration,
				ServiceID:            report.ServiceID, Revision: report.Revision, ObservedRevision: report.ObservedRevision,
				DesiredState: report.DesiredState, ObservedState: report.ObservedState, Healthy: report.Healthy,
				ErrorCode: report.ErrorCode, ObservedAt: report.ObservedAt, ReceivedAt: now,
			})
		})
		if errors.Is(err, services.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "Workspace Service report target not found")
		}
		if errors.Is(err, services.ErrInvalidReport) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid Workspace Service report")
		}
		if errors.Is(err, services.ErrReportReplay) || errors.Is(err, services.ErrStaleReport) {
			return echo.NewHTTPError(http.StatusConflict, "Workspace Service report rejected")
		}
		if err != nil {
			return fmt.Errorf("apply Workspace Service runtime report: %w", err)
		}
		return c.JSON(http.StatusOK, workspaceServiceReportResponse{Accepted: accepted, Revision: report.Revision})
	}
}
