package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/services"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaces"
)

type serviceCatalogResponse struct {
	SchemaVersion int                       `json:"schema_version"`
	Items         []servicecatalog.Template `json:"items"`
}

type serviceCreateRequest struct {
	CatalogName string `json:"catalog_name"`
	Name        string `json:"name,omitempty"`
}

type serviceActionRequest struct {
	Action string `json:"action"`
}

func listServiceCatalog(c *echo.Context) error {
	return c.JSON(http.StatusOK, serviceCatalogResponse{SchemaVersion: servicecatalog.SchemaVersion, Items: servicecatalog.List()})
}

func listWorkspaceServices(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		items, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.Service, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return nil, err
			}
			if _, err := workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role); err != nil {
				return nil, err
			}
			return services.List(ctx, tx, workspaceID)
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return workspaceNotFound(c)
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, map[string]any{"items": items})
	}
}

func addWorkspaceService(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request serviceCreateRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "catalog_name and an optional valid service name are required"}})
		}
		template, err := servicecatalog.Resolve(request.CatalogName)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "unknown_catalog_template", Message: "Service catalog template is not available"}})
		}
		name := strings.TrimSpace(request.Name)
		if name == "" {
			name = template.Name
		}
		if !servicecatalog.ValidateInstanceName(name) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_service_name", Message: "Service name must start with a lowercase letter and contain only lowercase letters, numbers, or hyphens (max 32 characters)"}})
		}
		config, err := servicecatalog.CanonicalJSON(template)
		if err != nil {
			return databaseUnavailable(c)
		}
		serviceID, err := idgen.New("svc_")
		if err != nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		now := time.Now().UTC()
		candidate := domain.Service{ID: domain.ServiceID(serviceID), WorkspaceID: workspaceID, Name: name,
			CatalogVersion: template.Name + "/" + template.Version, ConfigJSON: string(config), CreatedAt: now, UpdatedAt: now}
		stored, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.Service, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return domain.Service{}, err
			}
			if _, err := workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role); err != nil {
				return domain.Service{}, err
			}
			stored, created, err := services.CreateOrGet(ctx, tx, candidate)
			if err != nil || !created {
				return stored, err
			}
			details, err := json.Marshal(map[string]string{"catalog_name": template.Name, "catalog_version": template.Version, "service_name": name})
			if err != nil {
				return domain.Service{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "workspace.service.add", TargetType: "service", TargetID: string(stored.ID), Details: details, OccurredAt: now}); err != nil {
				return domain.Service{}, err
			}
			return stored, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return workspaceNotFound(c)
		}
		if errors.Is(err, services.ErrNameConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_name_conflict", Message: "Service name is already used by a different catalog configuration"}})
		}
		if errors.Is(err, services.ErrDeleted) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_deleted", Message: "Deleted Service names cannot be reused"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, stored)
	}
}

func requestWorkspaceServiceAction(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		var request serviceActionRequest
		if err := decodeJSONRequest(c, &request); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return invalidServiceAction(c)
		}
		var desiredState string
		switch request.Action {
		case "start":
			desiredState = "running"
		case "stop":
			desiredState = "stopped"
		case "remove":
			desiredState = "deleted"
		default:
			return invalidServiceAction(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		serviceName := strings.TrimSpace(c.Param("service_name"))
		if !servicecatalog.ValidateInstanceName(serviceName) {
			return invalidServiceAction(c)
		}
		now := time.Now().UTC()
		stored, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.Service, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return domain.Service{}, err
			}
			if _, err := workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role); err != nil {
				return domain.Service{}, err
			}
			service, changed, err := services.RequestState(ctx, tx, workspaceID, serviceName, desiredState, now)
			if err != nil || !changed {
				return service, err
			}
			details, err := json.Marshal(map[string]string{"service_name": serviceName, "desired_state": desiredState})
			if err != nil {
				return domain.Service{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: "workspace.service." + request.Action,
				TargetType: "service", TargetID: string(service.ID), Details: details, OccurredAt: now}); err != nil {
				return domain.Service{}, err
			}
			return service, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) {
			return tenantNotFound(c)
		}
		if errors.Is(err, workspaces.ErrWorkspaceAccessDenied) {
			return workspaceNotFound(c)
		}
		if errors.Is(err, services.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Workspace Service not found"}})
		}
		if errors.Is(err, services.ErrDeleted) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_deleted", Message: "Deleted Services cannot be restarted or reused"}})
		}
		if errors.Is(err, services.ErrConcurrentUpdate) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_state_conflict", Message: "Service state changed concurrently; retry the action"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, stored)
	}
}

func invalidServiceAction(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_service_action", Message: "action must be start, stop, or remove"}})
}
