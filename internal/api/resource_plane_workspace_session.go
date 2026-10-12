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
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/workspacesession"
)

const maxWorkspaceSessionLookupBody = 8 << 10

const (
	activeLookupTimestampHeader = "X-Hako-Timestamp"
	activeLookupRequestIDHeader = "X-Hako-Request-ID"
	activeLookupSignatureHeader = "X-Hako-Signature"
)

var (
	activeSessionLookupRequestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	activeSessionLookupTicketIDPattern  = regexp.MustCompile(`^[0-9a-f]{48}$`)
	activeSessionLookupPlaneIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

type ResourcePlaneWorkspaceSessionKeyResolver func(context.Context, domain.ResourcePlaneID) ([]byte, error)

type activeWorkspaceSessionResponse struct {
	Active bool `json:"active"`
}

// getActiveWorkspaceSession exposes only the active bit to an authenticated
// Resource Plane. The ticket claims are matched against the persisted row so
// a valid Plane credential cannot mistake another user's session as active.
func getActiveWorkspaceSession(pool transaction.Beginner, resolveKey ResourcePlaneWorkspaceSessionKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		planeID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		if !activeSessionLookupPlaneIDPattern.MatchString(string(planeID)) || pool == nil || resolveKey == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid Resource Plane credentials")
		}
		contentType, _, contentTypeErr := mime.ParseMediaType(c.Request().Header.Get("Content-Type"))
		if contentTypeErr != nil || contentType != "application/json" {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "session lookup requires application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxWorkspaceSessionLookupBody))
		if err != nil || len(body) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid session lookup request")
		}
		var claims workspacesession.Claims
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF || claims.ResourcePlane != planeID ||
			claims.Version != 1 || claims.Audience != workspacesession.AudienceGateway || claims.UserID == "" || claims.TenantID == "" ||
			claims.WorkspaceID == "" || !activeSessionLookupTicketIDPattern.MatchString(claims.TicketID) ||
			claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(workspacesession.TicketMaxTTL/time.Second) ||
			workspacesession.ValidatePermissions(claims.Permissions) != nil {
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
		active, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			var userID domain.UserID
			var tenantID domain.TenantID
			var workspaceID domain.WorkspaceID
			var resourcePlaneID domain.ResourcePlaneID
			var permissionsJSON string
			var expiresAt time.Time
			err := tx.QueryRow(ctx, `SELECT user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at
				FROM sessions WHERE id = $1`, claims.TicketID).Scan(&userID, &tenantID, &workspaceID, &resourcePlaneID, &permissionsJSON, &expiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			var storedPermissions []workspacesession.Permission
			if json.Unmarshal([]byte(permissionsJSON), &storedPermissions) != nil {
				return false, nil
			}
			return userID == claims.UserID && tenantID == claims.TenantID && workspaceID == claims.WorkspaceID &&
				resourcePlaneID == claims.ResourcePlane && equalSessionPermissions(storedPermissions, claims.Permissions) &&
				expiresAt.Unix() == claims.ExpiresAt && expiresAt.After(now), nil
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "Workspace session lookup is unavailable")
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, activeWorkspaceSessionResponse{Active: active})
	}
}

func equalSessionPermissions(left, right []workspacesession.Permission) bool {
	left = append([]workspacesession.Permission(nil), left...)
	right = append([]workspacesession.Permission(nil), right...)
	sort.Slice(left, func(i, j int) bool { return left[i] < left[j] })
	sort.Slice(right, func(i, j int) bool { return right[i] < right[j] })
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
