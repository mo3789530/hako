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

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type createRepositoryPreviewSessionRequest struct {
	Permissions []workspacesession.Permission `json:"permissions"`
}

type repositoryPreviewSessionResponse struct {
	SessionID   domain.SessionID              `json:"session_id"`
	WorkspaceID domain.WorkspaceID            `json:"workspace_id"`
	Ticket      string                        `json:"ticket"`
	ExpiresAt   time.Time                     `json:"expires_at"`
	Permissions []workspacesession.Permission `json:"permissions"`
}

var errRepositoryPreviewNotRunning = errors.New("Repository Preview is not running")
var errRepositoryPreviewExpiring = errors.New("Repository Preview expires before a Gateway session could be used")

// createRepositoryPreviewSession issues a regular, short-lived Gateway ticket
// for the current admitted PR generation. It does not expose provider IDs.
func createRepositoryPreviewSession(pool transaction.Beginner, issuer workspacesession.Issuer) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil || issuer == nil {
			return databaseUnavailable(c)
		}
		var request createRepositoryPreviewSessionRequest
		decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, maxJSONRequestBytes))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Preview session request is invalid"}})
		}
		if len(request.Permissions) == 0 {
			request.Permissions = []workspacesession.Permission{workspacesession.PermissionVSCode}
		}
		if workspacesession.ValidatePermissions(request.Permissions) != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Preview session permissions are invalid"}})
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installationID, installErr := strconv.ParseInt(c.Param("installation_id"), 10, 64)
		repositoryID, repoErr := strconv.ParseInt(c.Param("repository_id"), 10, 64)
		pullRequestNumber, prErr := strconv.ParseInt(c.Param("pull_request_number"), 10, 64)
		if !validTenantID(string(tenantID)) || installErr != nil || installationID <= 0 || repoErr != nil || repositoryID <= 0 || prErr != nil || pullRequestNumber <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Preview identity is invalid"}})
		}
		now := time.Now().UTC()
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (repositoryPreviewSessionResponse, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			var workspaceID domain.WorkspaceID
			var planeID domain.ResourcePlaneID
			var generation int64
			var previewExpiry time.Time
			err := tx.QueryRow(ctx, `SELECT a.workspace_id,a.resource_plane_id,a.generation,a.expires_at
				FROM repository_previews p
				JOIN repository_preview_runtime_allocations a USING (tenant_id,installation_id,github_repository_id,pull_request_number)
				WHERE p.tenant_id=$1 AND p.installation_id=$2 AND p.github_repository_id=$3 AND p.pull_request_number=$4
				  AND p.desired_state='present' AND p.generation=a.generation AND p.expires_at>$5
				  AND a.desired_state='present' AND a.state='running' AND a.expires_at>$5
				  AND a.workspace_id IS NOT NULL AND a.resource_plane_id IS NOT NULL AND a.microvm_id IS NOT NULL AND a.gateway_route_id IS NOT NULL
				ORDER BY a.generation DESC LIMIT 1`, tenantID, installationID, repositoryID, pullRequestNumber, now).
				Scan(&workspaceID, &planeID, &generation, &previewExpiry)
			if errors.Is(err, pgx.ErrNoRows) {
				return repositoryPreviewSessionResponse{}, errRepositoryPreviewNotRunning
			}
			if err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			if !previewExpiry.After(time.Now().UTC().Add(time.Second)) {
				return repositoryPreviewSessionResponse{}, errRepositoryPreviewExpiring
			}
			requestedClaims := workspacesession.Claims{UserID: user.ID, TenantID: tenantID,
				WorkspaceID: workspaceID, ResourcePlane: planeID, Permissions: request.Permissions}
			var ticket string
			var claims workspacesession.Claims
			var ticketErr error
			if bounded, ok := issuer.(interface {
				IssueBefore(workspacesession.Claims, time.Time) (string, workspacesession.Claims, error)
			}); ok {
				ticket, claims, ticketErr = bounded.IssueBefore(requestedClaims, previewExpiry)
			} else {
				ticket, claims, ticketErr = issuer.Issue(requestedClaims)
			}
			if ticketErr != nil {
				return repositoryPreviewSessionResponse{}, ticketErr
			}
			permissionsJSON, err := json.Marshal(claims.Permissions)
			if err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			expiresAt := time.Unix(claims.ExpiresAt, 0).UTC()
			if previewExpiry.Before(expiresAt) {
				return repositoryPreviewSessionResponse{}, errRepositoryPreviewExpiring
			}
			if _, err := tx.Exec(ctx, `DELETE FROM repository_preview_sessions WHERE tenant_id=$1 AND user_id=$2 AND expires_at<=$3`, tenantID, user.ID, now); err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO repository_preview_sessions
				(id,user_id,tenant_id,installation_id,github_repository_id,pull_request_number,generation,workspace_id,resource_plane_id,permissions_json,expires_at,created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, claims.TicketID, user.ID, tenantID,
				installationID, repositoryID, pullRequestNumber, generation, workspaceID, planeID, string(permissionsJSON), expiresAt, now); err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			details, err := json.Marshal(map[string]any{"installation_id": installationID, "repository_id": repositoryID,
				"pull_request_number": pullRequestNumber, "generation": generation, "permissions": claims.Permissions, "expires_at": expiresAt})
			if err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "github.preview.session.create",
				TargetType: "repository_preview_session", TargetID: claims.TicketID, Details: details, OccurredAt: now}); err != nil {
				return repositoryPreviewSessionResponse{}, err
			}
			return repositoryPreviewSessionResponse{SessionID: domain.SessionID(claims.TicketID), WorkspaceID: workspaceID, Ticket: ticket,
				ExpiresAt: expiresAt, Permissions: claims.Permissions}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, errRepositoryPreviewNotRunning) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "preview_not_running", Message: "Repository Preview is not running"}})
		}
		if errors.Is(err, errRepositoryPreviewExpiring) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "preview_expiring", Message: "Repository Preview expires too soon to open a Gateway session"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusCreated, result)
	}
}

func revokeRepositoryPreviewSession(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		sessionID := strings.TrimSpace(c.Param("session_id"))
		installationID, installErr := strconv.ParseInt(c.Param("installation_id"), 10, 64)
		repositoryID, repoErr := strconv.ParseInt(c.Param("repository_id"), 10, 64)
		pullRequestNumber, prErr := strconv.ParseInt(c.Param("pull_request_number"), 10, 64)
		if !validTenantID(string(tenantID)) || !validPreviewSessionID(sessionID) || installErr != nil || installationID <= 0 ||
			repoErr != nil || repositoryID <= 0 || prErr != nil || pullRequestNumber <= 0 {
			return workspaceNotFound(c)
		}
		now := time.Now().UTC()
		_, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			if _, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID); err != nil {
				return struct{}{}, err
			}
			var deletedID string
			err := tx.QueryRow(ctx, `DELETE FROM repository_preview_sessions
				WHERE id=$1 AND user_id=$2 AND tenant_id=$3 AND installation_id=$4 AND github_repository_id=$5 AND pull_request_number=$6 RETURNING id`,
				sessionID, user.ID, tenantID, installationID, repositoryID, pullRequestNumber).Scan(&deletedID)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, errSessionNotFound
			}
			if err != nil {
				return struct{}{}, err
			}
			details, err := json.Marshal(map[string]string{"session_id": deletedID})
			if err != nil {
				return struct{}{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "github.preview.session.revoke",
				TargetType: "repository_preview_session", TargetID: deletedID, Details: details, OccurredAt: now}); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, errSessionNotFound) {
			return workspaceNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.NoContent(http.StatusNoContent)
	}
}
