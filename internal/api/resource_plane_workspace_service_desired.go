package api

import (
	"bytes"
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
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const maxWorkspaceServiceDesiredRequestBytes = 2 << 10

var workspaceServiceDesiredRequestIDPattern = regexp.MustCompile(`^[a-f0-9]{48}$`)

type workspaceServiceDesiredStateRequest struct {
	TenantID      domain.TenantID    `json:"tenant_id"`
	WorkspaceID   domain.WorkspaceID `json:"workspace_id"`
	CorrelationID string             `json:"correlation_id,omitempty"`
}

var workspaceServiceCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func resolveResourcePlaneWorkspaceServiceDesiredState(pool transaction.Beginner, resolveKey ResourcePlaneServiceReportKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		planeID := domain.ResourcePlaneID(strings.TrimSpace(c.Param("resource_plane_id")))
		serviceID := domain.ServiceID(strings.TrimSpace(c.Param("service_id")))
		if pool == nil || resolveKey == nil || planeID == "" || serviceID == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		if c.Request().URL.RawQuery != "" || c.Request().URL.ForceQuery || len(c.Request().TransferEncoding) != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid desired-state request")
		}
		if contentType := strings.ToLower(strings.TrimSpace(c.Request().Header.Get("Content-Type"))); contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "desired-state request must be application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxWorkspaceServiceDesiredRequestBytes))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid desired-state request")
		}
		timestampText, requestID := c.Request().Header.Get(healthReportTimestampHeader), c.Request().Header.Get(healthReportIDHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || !workspaceServiceDesiredRequestIDPattern.MatchString(requestID) {
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
			!validResourcePlaneHealthSignature(key, c.Request().Method, c.Request().URL.EscapedPath(), timestampText, requestID, body,
				c.Request().Header.Get(healthReportSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		var lookup workspaceServiceDesiredStateRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&lookup) != nil || decoder.Decode(new(any)) != io.EOF || lookup.TenantID == "" || lookup.WorkspaceID == "" ||
			(lookup.CorrelationID != "" && !workspaceServiceCorrelationIDPattern.MatchString(lookup.CorrelationID)) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid desired-state request")
		}
		command, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkspaceServiceCommand, error) {
			var command domain.WorkspaceServiceCommand
			var config string
			err := tx.QueryRow(ctx, `SELECT s.catalog_version, s.config_json, s.desired_state, s.desired_revision, s.credential_generation
				FROM services s
				JOIN workspaces w ON w.id = s.workspace_id
				JOIN placements p ON p.workspace_id = w.id
				WHERE s.id = $1 AND s.workspace_id = $2 AND w.tenant_id = $3 AND p.resource_plane_id = $4`,
				serviceID, lookup.WorkspaceID, lookup.TenantID, planeID).Scan(
				&command.CatalogVersion, &config, &command.DesiredState, &command.Revision, &command.CredentialGeneration)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.WorkspaceServiceCommand{}, pgx.ErrNoRows
			}
			if err != nil {
				return domain.WorkspaceServiceCommand{}, err
			}
			command.SchemaVersion, command.TenantID, command.WorkspaceID = 1, lookup.TenantID, lookup.WorkspaceID
			command.ResourcePlaneID, command.ServiceID, command.Config = planeID, serviceID, json.RawMessage(config)
			command.CorrelationID = lookup.CorrelationID
			return command, nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "Workspace Service desired state not found")
		}
		if err != nil {
			return fmt.Errorf("resolve current Workspace Service desired state: %w", err)
		}
		template, err := servicecatalog.Decode(bytes.NewReader(command.Config))
		if err != nil || command.CatalogVersion != template.Name+"/"+template.Version {
			return fmt.Errorf("resolve current Workspace Service desired state: invalid stored catalog configuration")
		}
		approved, err := servicecatalog.Resolve(template.Name)
		if err != nil {
			return fmt.Errorf("resolve current Workspace Service desired state: unapproved catalog template")
		}
		canonical, err := servicecatalog.CanonicalJSON(approved)
		if err != nil || !bytes.Equal(canonical, command.Config) {
			return fmt.Errorf("resolve current Workspace Service desired state: stored catalog configuration is not canonical")
		}
		return c.JSON(http.StatusOK, command)
	}
}
