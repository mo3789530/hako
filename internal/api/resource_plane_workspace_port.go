package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/store/services"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/workspacesession"
)

const maxWorkspacePortAuthorizationBody = 8 << 10

var workspacePortRequestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

type workspacePortAuthorizationRequest struct {
	Claims workspacesession.Claims `json:"claims"`
	Port   uint16                  `json:"port"`
}

// authorizeResourcePlaneWorkspacePort is the trusted-state half of Gateway
// port forwarding. It only authorizes a TCP port declared by a healthy,
// current-generation Service on a running Workspace.
func authorizeResourcePlaneWorkspacePort(pool transaction.Beginner, resolveKey ResourcePlaneWorkspaceSessionKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		planeID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		if !activeSessionLookupPlaneIDPattern.MatchString(string(planeID)) || pool == nil || resolveKey == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		contentType, _, contentTypeErr := mime.ParseMediaType(c.Request().Header.Get("Content-Type"))
		if contentTypeErr != nil || contentType != "application/json" {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "port authorization requires application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxWorkspacePortAuthorizationBody))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid port authorization request")
		}
		var input workspacePortAuthorizationRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Port == 0 || !validWorkspacePortClaims(input.Claims, planeID) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		timestampText := c.Request().Header.Get(activeLookupTimestampHeader)
		requestID := c.Request().Header.Get(activeLookupRequestIDHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || !workspacePortRequestIDPattern.MatchString(requestID) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		now := time.Now().UTC()
		requestTime := time.Unix(timestamp, 0).UTC()
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
			!workspacesession.VerifyActiveLookupSignature(key, http.MethodPost, c.Request().URL.EscapedPath(), timestampText, requestID, body,
				c.Request().Header.Get(activeLookupSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		allowed, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			var userID domain.UserID
			var tenantID domain.TenantID
			var workspaceID domain.WorkspaceID
			var storedPlaneID domain.ResourcePlaneID
			var permissionsJSON string
			var expiresAt time.Time
			err := tx.QueryRow(ctx, `SELECT user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at
				FROM sessions WHERE id = $1`, input.Claims.TicketID).Scan(&userID, &tenantID, &workspaceID, &storedPlaneID, &permissionsJSON, &expiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			var storedPermissions []workspacesession.Permission
			if json.Unmarshal([]byte(permissionsJSON), &storedPermissions) != nil ||
				userID != input.Claims.UserID || tenantID != input.Claims.TenantID || workspaceID != input.Claims.WorkspaceID || storedPlaneID != planeID ||
				!equalSessionPermissions(storedPermissions, input.Claims.Permissions) || expiresAt.Unix() != input.Claims.ExpiresAt || !expiresAt.After(now) {
				return false, nil
			}
			var desiredState, observedState string
			err = tx.QueryRow(ctx, `SELECT s.desired_state, s.observed_state FROM workspace_status s
				JOIN workspaces w ON w.id = s.workspace_id
				WHERE w.id = $1 AND w.tenant_id = $2`, workspaceID, tenantID).Scan(&desiredState, &observedState)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if desiredState != string(domain.DesiredWorkspaceRunning) || observedState != string(domain.ObservedWorkspaceRunning) {
				return false, nil
			}
			var placementMatches bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM placements WHERE workspace_id = $1 AND resource_plane_id = $2)`,
				workspaceID, planeID).Scan(&placementMatches); err != nil {
				return false, err
			}
			if !placementMatches {
				return false, nil
			}
			items, err := services.List(ctx, tx, workspaceID)
			if err != nil {
				return false, err
			}
			for _, service := range items {
				if service.DesiredState != "running" || service.ObservedState != "healthy" || service.DesiredRevision < 1 ||
					service.ObservedRevision != service.DesiredRevision || service.ObservedRuntimeGeneration < 1 {
					continue
				}
				template, err := servicecatalog.Decode(strings.NewReader(service.ConfigJSON))
				if err != nil {
					continue
				}
				for _, declaredPort := range template.Ports {
					if strings.EqualFold(declaredPort.Protocol, "tcp") && declaredPort.Number == input.Port {
						return true, nil
					}
				}
			}
			return false, nil
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "Workspace port authorization is unavailable")
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, map[string]bool{"allowed": allowed})
	}
}

func validWorkspacePortClaims(claims workspacesession.Claims, planeID domain.ResourcePlaneID) bool {
	if claims.ResourcePlane != planeID || claims.Version != 1 || claims.Audience != workspacesession.AudienceGateway || claims.UserID == "" ||
		claims.TenantID == "" || claims.WorkspaceID == "" || !activeSessionLookupTicketIDPattern.MatchString(claims.TicketID) ||
		claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(workspacesession.TicketMaxTTL/time.Second) ||
		workspacesession.ValidatePermissions(claims.Permissions) != nil {
		return false
	}
	for _, permission := range claims.Permissions {
		if permission == workspacesession.PermissionPortForward {
			return true
		}
	}
	return false
}
