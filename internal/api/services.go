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

func getWorkspaceServiceUsage(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		workspaceID := domain.WorkspaceID(strings.TrimSpace(c.Param("workspace_id")))
		usage, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (services.ResourceUsage, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return services.ResourceUsage{}, err
			}
			if _, err := workspaces.GetVisible(ctx, tx, tenantID, workspaceID, user.ID, membership.Role); err != nil {
				return services.ResourceUsage{}, err
			}
			return services.ReadResourceUsage(ctx, tx, workspaceID)
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
		return c.JSON(http.StatusOK, usage)
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
			resourcePlaneID, err := workspaceServiceResourcePlane(ctx, tx, workspaceID)
			if err != nil {
				return domain.Service{}, err
			}
			if err := enqueueWorkspaceService(ctx, tx, tenantID, resourcePlaneID, stored, RequestIDFromContext(c.Request().Context()), now); err != nil {
				return domain.Service{}, err
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
		if errors.Is(err, services.ErrResourceQuotaExceeded) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_resource_quota_exceeded", Message: "Workspace Service memory or CPU budget would be exceeded"}})
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
		rotateCredentials := false
		switch request.Action {
		case "start":
			desiredState = "running"
		case "stop":
			desiredState = "stopped"
		case "remove":
			desiredState = "deleted"
		case "rotate-credentials":
			desiredState = "running"
			rotateCredentials = true
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
			var service domain.Service
			if rotateCredentials {
				service, err = services.RequestCredentialRotation(ctx, tx, workspaceID, serviceName, now)
				if err != nil {
					return domain.Service{}, err
				}
			} else {
				var changed bool
				service, changed, err = services.RequestState(ctx, tx, workspaceID, serviceName, desiredState, now)
				if err != nil || !changed {
					return service, err
				}
			}
			resourcePlaneID, err := workspaceServiceResourcePlane(ctx, tx, workspaceID)
			if err != nil {
				return domain.Service{}, err
			}
			if err := enqueueWorkspaceService(ctx, tx, tenantID, resourcePlaneID, service, RequestIDFromContext(c.Request().Context()), now); err != nil {
				return domain.Service{}, err
			}
			auditAction := "workspace.service." + request.Action
			details, err := json.Marshal(map[string]string{"service_name": serviceName, "desired_state": desiredState})
			if err != nil {
				return domain.Service{}, err
			}
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID, Action: auditAction,
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
		if errors.Is(err, services.ErrResourceQuotaExceeded) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_resource_quota_exceeded", Message: "Workspace Service memory or CPU budget would be exceeded"}})
		}
		if errors.Is(err, services.ErrCredentialRotationUnavailable) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "service_credential_rotation_unavailable", Message: "Only a running Service with generated credentials can rotate credentials"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusAccepted, stored)
	}
}

func workspaceServiceResourcePlane(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID) (domain.ResourcePlaneID, error) {
	var resourcePlaneID domain.ResourcePlaneID
	if err := tx.QueryRow(ctx, `SELECT resource_plane_id FROM placements WHERE workspace_id = $1`, workspaceID).Scan(&resourcePlaneID); err != nil {
		return "", errors.New("Workspace placement is required for Service runtime reconciliation")
	}
	if resourcePlaneID == "" {
		return "", errors.New("Workspace placement has no Resource Plane")
	}
	return resourcePlaneID, nil
}

func enqueueWorkspaceService(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, resourcePlaneID domain.ResourcePlaneID,
	service domain.Service, correlationID string, createdAt time.Time) error {
	eventID, err := idgen.New("evt_")
	if err != nil {
		return err
	}
	config := json.RawMessage(service.ConfigJSON)
	if !json.Valid(config) {
		return errors.New("Workspace Service catalog config is invalid JSON")
	}
	command, err := json.Marshal(domain.WorkspaceServiceCommand{
		SchemaVersion: 1, CorrelationID: correlationID, TenantID: tenantID, WorkspaceID: service.WorkspaceID,
		ResourcePlaneID: resourcePlaneID, ServiceID: service.ID, Revision: service.DesiredRevision,
		CredentialGeneration: service.CredentialGeneration, DesiredState: service.DesiredState,
		CatalogVersion: service.CatalogVersion, Config: config,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
		(id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt)
		VALUES ($1, 'workspace_service', $2, 'workspace_service.reconcile_requested', $3, $4, NULL, 0)`,
		eventID, service.ID, string(command), createdAt); err != nil {
		return errors.New("enqueue Workspace Service runtime reconciliation")
	}
	return nil
}

func invalidServiceAction(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_service_action", Message: "action must be start, stop, remove, or rotate-credentials"}})
}
