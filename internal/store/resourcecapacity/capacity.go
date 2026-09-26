// Package resourcecapacity manages transactional Workspace slots per Resource Plane.
package resourcecapacity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var ErrReservationCounterUnderflow = errors.New("Resource Plane reservation counter underflow")

// Reserve atomically claims Workspace, CPU, and memory capacity when configured
// for the Resource Plane. An unconfigured plane is unbounded. false means a
// configured limit is already reached.
func Reserve(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, runtimeClass string, now time.Time) (bool, error) {
	if tx == nil || resourcePlaneID == "" {
		return false, errors.New("transaction and Resource Plane ID are required")
	}
	runtimeClass = strings.ToLower(strings.TrimSpace(runtimeClass))
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var maxCPU, maxMemory sql.NullInt64
	if err := tx.QueryRow(ctx, `SELECT max_cpu_millicores, max_memory_mib FROM resource_plane_capacities WHERE resource_plane_id = $1`, resourcePlaneID).Scan(&maxCPU, &maxMemory); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return false, fmt.Errorf("read Resource Plane compute capacity: %w", err)
	}
	cpu, memory, profileFound, err := runtimeClassDemand(ctx, tx, runtimeClass)
	if err != nil {
		return false, err
	}
	if (maxCPU.Valid || maxMemory.Valid) && !profileFound {
		return false, fmt.Errorf("runtime class %q has no configured CPU/RAM demand", runtimeClass)
	}
	var id domain.ResourcePlaneID
	err = tx.QueryRow(ctx, `UPDATE resource_plane_capacities
		SET reserved_workspaces = reserved_workspaces + 1,
			reserved_cpu_millicores = reserved_cpu_millicores + $3,
			reserved_memory_mib = reserved_memory_mib + $4,
			updated_at = $2
		WHERE resource_plane_id = $1
		  AND reserved_workspaces < max_workspaces
		  AND (max_cpu_millicores IS NULL OR reserved_cpu_millicores + $3 <= max_cpu_millicores)
		  AND (max_memory_mib IS NULL OR reserved_memory_mib + $4 <= max_memory_mib)
		RETURNING resource_plane_id`, resourcePlaneID, now, cpu, memory).Scan(&id)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("reserve Resource Plane capacity: %w", err)
	}
	return false, nil
}

// RecordWorkspace stores the selected Plane for every new Workspace, including
// currently-unbounded Planes. This makes later capacity configuration and
// eventual release accounting possible without changing Workspace identity.
func RecordWorkspace(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID, runtimeClass string, now time.Time) error {
	if tx == nil || workspaceID == "" || resourcePlaneID == "" {
		return errors.New("transaction, Workspace ID, and Resource Plane ID are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	runtimeClass = strings.ToLower(strings.TrimSpace(runtimeClass))
	cpu, memory, found, err := runtimeClassDemand(ctx, tx, runtimeClass)
	if err != nil {
		return err
	}
	var cpuValue, memoryValue any
	if found {
		cpuValue, memoryValue = cpu, memory
	}
	if _, err := tx.Exec(ctx, `INSERT INTO resource_plane_reservations (workspace_id, resource_plane_id, reserved_at, runtime_class, cpu_millicores, memory_mib) VALUES ($1, $2, $3, $4, $5, $6)`, workspaceID, resourcePlaneID, now, nullIfEmpty(runtimeClass), cpuValue, memoryValue); err != nil {
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
	var cpu, memory sql.NullInt64
	err := tx.QueryRow(ctx, `DELETE FROM resource_plane_reservations WHERE workspace_id = $1 RETURNING resource_plane_id, cpu_millicores, memory_mib`, workspaceID).Scan(&resourcePlaneID, &cpu, &memory)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove Resource Plane reservation: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE resource_plane_capacities
		SET reserved_workspaces = reserved_workspaces - 1,
			reserved_cpu_millicores = reserved_cpu_millicores - $3,
			reserved_memory_mib = reserved_memory_mib - $4,
			updated_at = $2
		WHERE resource_plane_id = $1 AND reserved_workspaces > 0
		  AND reserved_cpu_millicores >= $3 AND reserved_memory_mib >= $4`, resourcePlaneID, now, nullInt64Value(cpu), nullInt64Value(memory))
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

func runtimeClassDemand(ctx context.Context, tx pgx.Tx, runtimeClass string) (int64, int64, bool, error) {
	if runtimeClass == "" {
		return 0, 0, false, nil
	}
	var cpu, memory int64
	err := tx.QueryRow(ctx, `SELECT cpu_millicores, memory_mib FROM runtime_class_resources WHERE runtime_class = $1`, runtimeClass).Scan(&cpu, &memory)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read Runtime Class %q resource profile: %w", runtimeClass, err)
	}
	return cpu, memory, true, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullInt64Value(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}
