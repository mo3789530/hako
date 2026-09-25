// Package resourcecapacity manages transactional Workspace slots per Resource Plane.
package resourcecapacity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var ErrReservationCounterUnderflow = errors.New("Resource Plane reservation counter underflow")

// Reserve atomically claims capacity when the Resource Plane has a configured
// capacity row. An unconfigured plane is treated as unbounded for backward-
// compatible rollout. false means the configured limit is already reached.
func Reserve(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, now time.Time) (bool, error) {
	if tx == nil || resourcePlaneID == "" {
		return false, errors.New("transaction and Resource Plane ID are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var id domain.ResourcePlaneID
	err := tx.QueryRow(ctx, `UPDATE resource_plane_capacities
		SET reserved_workspaces = reserved_workspaces + 1, updated_at = $2
		WHERE resource_plane_id = $1 AND reserved_workspaces < max_workspaces
		RETURNING resource_plane_id`, resourcePlaneID, now).Scan(&id)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("reserve Resource Plane capacity: %w", err)
	}
	var configured bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM resource_plane_capacities WHERE resource_plane_id = $1)`, resourcePlaneID).Scan(&configured); err != nil {
		return false, fmt.Errorf("check Resource Plane capacity configuration: %w", err)
	}
	return !configured, nil
}

// RecordWorkspace stores the selected Plane for every new Workspace, including
// currently-unbounded Planes. This makes later capacity configuration and
// eventual release accounting possible without changing Workspace identity.
func RecordWorkspace(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID, now time.Time) error {
	if tx == nil || workspaceID == "" || resourcePlaneID == "" {
		return errors.New("transaction, Workspace ID, and Resource Plane ID are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO resource_plane_reservations (workspace_id, resource_plane_id, reserved_at) VALUES ($1, $2, $3)`, workspaceID, resourcePlaneID, now); err != nil {
		return fmt.Errorf("record Resource Plane reservation: %w", err)
	}
	return nil
}

// ReleaseWorkspace releases a reservation only when the Workspace is observed
// deleted. Duplicate terminal results are naturally a no-op.
func ReleaseWorkspace(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, now time.Time) error {
	if tx == nil || workspaceID == "" {
		return errors.New("transaction and Workspace ID are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var resourcePlaneID domain.ResourcePlaneID
	err := tx.QueryRow(ctx, `DELETE FROM resource_plane_reservations WHERE workspace_id = $1 RETURNING resource_plane_id`, workspaceID).Scan(&resourcePlaneID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove Resource Plane reservation: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE resource_plane_capacities SET reserved_workspaces = reserved_workspaces - 1, updated_at = $2
		WHERE resource_plane_id = $1 AND reserved_workspaces > 0`, resourcePlaneID, now)
	if err != nil {
		return fmt.Errorf("release Resource Plane capacity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var configured bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM resource_plane_capacities WHERE resource_plane_id = $1)`, resourcePlaneID).Scan(&configured); err != nil {
			return fmt.Errorf("check Resource Plane capacity configuration: %w", err)
		}
		if configured {
			return ErrReservationCounterUnderflow
		}
	}
	return nil
}
