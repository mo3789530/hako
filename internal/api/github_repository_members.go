package api

import (
	"context"
	"encoding/json"
	"errors"
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
)

type repositoryMember struct {
	UserID domain.UserID        `json:"user_id"`
	Role   authz.RepositoryRole `json:"role"`
}

func repositoryMembers(pool transaction.Beginner, method string) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenant := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		installation, installationErr := strconv.ParseInt(c.Param("installation_id"), 10, 64)
		repository, repositoryErr := strconv.ParseInt(c.Param("repository_id"), 10, 64)
		if installationErr != nil || repositoryErr != nil || installation <= 0 || repository <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_repository", Message: "Valid Installation and Repository IDs are required"}})
		}
		key := authz.RepositoryKey{TenantID: tenant, InstallationID: installation, RepositoryID: repository}
		target := domain.UserID(strings.TrimSpace(c.Param("user_id")))
		var input struct {
			Role authz.RepositoryRole `json:"role"`
		}
		if method == http.MethodPut {
			if err := decodeJSONRequest(c, &input); err != nil {
				if isRequestTooLarge(err) {
					return requestTooLarge(c)
				}
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "A valid grant is required"}})
			}
			if !input.Role.Valid() {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_role", Message: "Role must be viewer or operator"}})
			}
		}

		now := time.Now().UTC()
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]repositoryMember, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenant, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return nil, err
			}
			boundAt, err := authz.ActiveRepositoryBinding(ctx, tx, key)
			if err != nil {
				return nil, err
			}
			if method == http.MethodGet {
				rows, err := tx.Query(ctx, `SELECT g.user_id, g.role FROM tenant_repository_members g
					JOIN tenant_members m ON m.tenant_id=g.tenant_id AND m.user_id=g.user_id AND m.joined_at=g.membership_joined_at
					WHERE g.tenant_id=$1 AND g.installation_id=$2 AND g.github_repository_id=$3 AND g.installation_bound_at=$4
					ORDER BY g.user_id`, tenant, installation, repository, boundAt)
				if err != nil {
					return nil, err
				}
				return pgx.CollectRows(rows, pgx.RowToStructByPos[repositoryMember])
			}
			if err := changeRepositoryMember(ctx, tx, key, boundAt, user.ID, target, input.Role, now); err != nil {
				return nil, err
			}
			return nil, nil
		})
		switch {
		case errors.Is(err, authz.ErrTenantAccessDenied):
			return tenantNotFound(c)
		case errors.Is(err, authz.ErrRepositoryAccessDenied):
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Repository or member not found"}})
		case err != nil:
			return databaseUnavailable(c)
		case method == http.MethodGet:
			return c.JSON(http.StatusOK, map[string]any{"items": result})
		case method == http.MethodPut:
			return c.JSON(http.StatusOK, repositoryMember{UserID: target, Role: input.Role})
		default:
			return c.NoContent(http.StatusNoContent)
		}
	}
}

func changeRepositoryMember(ctx context.Context, tx pgx.Tx, key authz.RepositoryKey, boundAt time.Time, actor, target domain.UserID, role authz.RepositoryRole, now time.Time) error {
	var previous authz.RepositoryRole
	err := tx.QueryRow(ctx, `SELECT role FROM tenant_repository_members
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND user_id=$4`,
		key.TenantID, key.InstallationID, key.RepositoryID, target).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if role == "" {
		tag, err := tx.Exec(ctx, `DELETE FROM tenant_repository_members
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND user_id=$4`,
			key.TenantID, key.InstallationID, key.RepositoryID, target)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
	} else {
		member, err := authz.RequireTenantMembership(ctx, tx, target, key.TenantID)
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return authz.ErrRepositoryAccessDenied
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_repository_members
			(tenant_id, installation_id, github_repository_id, user_id, role, membership_joined_at, installation_bound_at, granted_by, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, installation_id, github_repository_id, user_id) DO UPDATE SET
			role=excluded.role, membership_joined_at=excluded.membership_joined_at,
			installation_bound_at=excluded.installation_bound_at, granted_by=excluded.granted_by, updated_at=excluded.updated_at`,
			key.TenantID, key.InstallationID, key.RepositoryID, target, role, member.JoinedAt, boundAt, actor, now); err != nil {
			return err
		}
	}
	details, err := json.Marshal(map[string]any{"installation_id": key.InstallationID, "repository_id": key.RepositoryID,
		"user_id": target, "previous_role": previous, "role": role})
	if err != nil {
		return err
	}
	return audit.Append(ctx, tx, audit.Event{TenantID: key.TenantID, ActorID: actor, Action: "tenant.github_repository.membership.change",
		TargetType: "github_repository", TargetID: strconv.FormatInt(key.RepositoryID, 10), Details: details, OccurredAt: now})
}
