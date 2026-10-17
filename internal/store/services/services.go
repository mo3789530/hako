// Package services persists Workspace-local Service instances.
package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var ErrNameConflict = errors.New("service name is already used by a different catalog configuration")
var ErrNotFound = errors.New("service not found")
var ErrDeleted = errors.New("deleted service names cannot be reused")
var ErrConcurrentUpdate = errors.New("service state changed concurrently")

// CreateOrGet creates a desired-running Service, or returns the existing row
// when the same Workspace/name/catalog configuration is retried. Callers must
// authorize Workspace visibility in the same transaction before invoking it.
func CreateOrGet(ctx context.Context, tx pgx.Tx, service domain.Service) (domain.Service, bool, error) {
	if tx == nil {
		return domain.Service{}, false, errors.New("transaction is required")
	}
	if service.ID == "" || service.WorkspaceID == "" || service.Name == "" || service.CatalogVersion == "" || service.ConfigJSON == "" || service.CreatedAt.IsZero() {
		return domain.Service{}, false, errors.New("service identity, catalog configuration, and creation time are required")
	}
	var stored domain.Service
	err := tx.QueryRow(ctx, `INSERT INTO services (id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', 'pending', $6, $6)
		ON CONFLICT (workspace_id, name) DO NOTHING
		RETURNING id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at`,
		service.ID, service.WorkspaceID, service.Name, service.CatalogVersion, service.ConfigJSON, service.CreatedAt).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.CreatedAt, &stored.UpdatedAt)
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Service{}, false, fmt.Errorf("create Workspace Service: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at
		FROM services WHERE workspace_id = $1 AND name = $2`, service.WorkspaceID, service.Name).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.CreatedAt, &stored.UpdatedAt)
	if err != nil {
		return domain.Service{}, false, fmt.Errorf("load existing Workspace Service: %w", err)
	}
	if stored.CatalogVersion != service.CatalogVersion || stored.ConfigJSON != service.ConfigJSON {
		return domain.Service{}, false, ErrNameConflict
	}
	if stored.DesiredState == "deleted" {
		return domain.Service{}, false, ErrDeleted
	}
	return stored, false, nil
}

// RequestState changes the desired lifecycle state while retaining the row as
// an auditable tombstone. It deliberately does not claim that a runtime has
// started, stopped, or physically removed the Service.
func RequestState(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, name, desiredState string, now time.Time) (domain.Service, bool, error) {
	if tx == nil {
		return domain.Service{}, false, errors.New("transaction is required")
	}
	if workspaceID == "" || name == "" || now.IsZero() {
		return domain.Service{}, false, errors.New("Workspace ID, Service name, and action time are required")
	}
	if desiredState != "running" && desiredState != "stopped" && desiredState != "deleted" {
		return domain.Service{}, false, errors.New("desired state must be running, stopped, or deleted")
	}
	var stored domain.Service
	err := tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at
		FROM services WHERE workspace_id = $1 AND name = $2`, workspaceID, name).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Service{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Service{}, false, fmt.Errorf("load Workspace Service: %w", err)
	}
	if stored.DesiredState == "deleted" && desiredState != "deleted" {
		return domain.Service{}, false, ErrDeleted
	}
	if stored.DesiredState == desiredState {
		return stored, false, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		previousState := stored.DesiredState
		err = tx.QueryRow(ctx, `UPDATE services SET desired_state = $3, observed_state = 'pending', updated_at = $4
			WHERE workspace_id = $1 AND name = $2 AND desired_state = $5
			RETURNING id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at`,
			workspaceID, name, desiredState, now, previousState).Scan(
			&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
			&stored.DesiredState, &stored.ObservedState, &stored.CreatedAt, &stored.UpdatedAt)
		if err == nil {
			return stored, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Service{}, false, fmt.Errorf("request Workspace Service state: %w", err)
		}
		err = tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at
			FROM services WHERE workspace_id = $1 AND name = $2`, workspaceID, name).Scan(
			&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
			&stored.DesiredState, &stored.ObservedState, &stored.CreatedAt, &stored.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Service{}, false, ErrNotFound
		}
		if err != nil {
			return domain.Service{}, false, fmt.Errorf("reload Workspace Service after concurrent state change: %w", err)
		}
		if stored.DesiredState == desiredState {
			return stored, false, nil
		}
		if stored.DesiredState == "deleted" {
			return domain.Service{}, false, ErrDeleted
		}
	}
	return domain.Service{}, false, ErrConcurrentUpdate
}

// List returns Services in stable name order. The caller must authorize
// Workspace visibility in the same transaction.
func List(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID) ([]domain.Service, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	if workspaceID == "" {
		return nil, errors.New("Workspace ID is required")
	}
	rows, err := tx.Query(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, created_at, updated_at
		FROM services WHERE workspace_id = $1 ORDER BY name, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list Workspace Services: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Service, 0)
	for rows.Next() {
		var service domain.Service
		if err := rows.Scan(&service.ID, &service.WorkspaceID, &service.Name, &service.CatalogVersion, &service.ConfigJSON,
			&service.DesiredState, &service.ObservedState, &service.CreatedAt, &service.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Workspace Service: %w", err)
		}
		result = append(result, service)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Workspace Services: %w", err)
	}
	return result, nil
}
