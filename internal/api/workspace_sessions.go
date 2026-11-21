package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/workspacesession"
)

var errWorkspaceNotRunning = errors.New("Workspace is not running")

type createWorkspaceSessionRequest struct {
	Permissions []workspacesession.Permission `json:"permissions"`
}

type createWorkspaceSessionResponse struct {
	SessionID   domain.SessionID              `json:"session_id"`
	Ticket      string                        `json:"ticket"`
	ExpiresAt   time.Time                     `json:"expires_at"`
	Permissions []workspacesession.Permission `json:"permissions"`
}

func createWorkspaceSession(pool transaction.Beginner, issuer workspacesession.Issuer) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil || issuer == nil {
			return databaseUnavailable(c)
		}
		var request createWorkspaceSessionRequest
		body := http.MaxBytesReader(c.Response(), c.Request().Body, maxJSONRequestBytes)
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Workspace session permissions are invalid"}})
		}
		if len(request.Permissions) == 0 {
			request.Permissions = []workspacesession.Permission{workspacesession.PermissionShell}
		}
		if err := workspacesession.ValidatePermissions(request.Permissions); err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Workspace session permissions are invalid"}})
		}
		for _, permission := range request.Permissions {
			if permission == workspacesession.PermissionPreviewLogs {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Workspace session permissions are invalid"}})
			}
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		now := time.Now().UTC()
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (createWorkspaceSessionResponse, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE tenant_id = $1 AND user_id = $2 AND expires_at <= $3`, tenantID, user.ID, now); err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			view, err := workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role)
			if err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			if view.Status.DesiredState != domain.DesiredWorkspaceRunning || view.Status.ObservedState != domain.ObservedWorkspaceRunning {
				return createWorkspaceSessionResponse{}, errWorkspaceNotRunning
			}
			var resourcePlaneID domain.ResourcePlaneID
			if err := tx.QueryRow(ctx, `SELECT resource_plane_id FROM placements WHERE workspace_id = $1`, workspaceID).Scan(&resourcePlaneID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return createWorkspaceSessionResponse{}, workspaces.ErrWorkspaceAccessDenied
				}
				return createWorkspaceSessionResponse{}, err
			}
			ticket, claims, err := issuer.Issue(workspacesession.Claims{UserID: user.ID, TenantID: tenantID, WorkspaceID: workspaceID,
				ResourcePlane: resourcePlaneID, Permissions: request.Permissions})
			if err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			permissionsJSON, err := json.Marshal(claims.Permissions)
			if err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			expiresAt := time.Unix(claims.ExpiresAt, 0).UTC()
			if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, claims.TicketID, user.ID, tenantID, workspaceID, resourcePlaneID, string(permissionsJSON), expiresAt, now); err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			details, err := json.Marshal(map[string]any{"permissions": claims.Permissions, "expires_at": expiresAt})
			if err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "workspace.session.create",
				TargetType: "workspace_session", TargetID: claims.TicketID, Details: details, OccurredAt: now}); err != nil {
				return createWorkspaceSessionResponse{}, err
			}
			return createWorkspaceSessionResponse{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
				ExpiresAt: expiresAt, Permissions: claims.Permissions}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return workspaceNotFound(c)
		}
		if errors.Is(err, errWorkspaceNotRunning) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "workspace_not_running", Message: "Workspace must be running before a session can be opened"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusCreated, result)
	}
}

func revokeWorkspaceSession(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		sessionID := strings.TrimSpace(c.Param("session_id"))
		now := time.Now().UTC()
		_, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return struct{}{}, err
			}
			if _, err := workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role); err != nil {
				return struct{}{}, err
			}
			var revokedID string
			err = tx.QueryRow(ctx, `DELETE FROM sessions WHERE id = $1 AND tenant_id = $2 AND workspace_id = $3 RETURNING id`,
				sessionID, tenantID, workspaceID).Scan(&revokedID)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, errSessionNotFound
			}
			if err != nil {
				return struct{}{}, err
			}
			details, err := json.Marshal(map[string]string{"session_id": revokedID})
			if err != nil {
				return struct{}{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "workspace.session.revoke",
				TargetType: "workspace_session", TargetID: revokedID, Details: details, OccurredAt: now}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) || errors.Is(err, errSessionNotFound) {
			return workspaceNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.NoContent(http.StatusNoContent)
	}
}

var errSessionNotFound = errors.New("Workspace session not found")
