// Package workspaceactivity stores durable Gateway connection leases used by
// the Workspace idle-suspension reconciler. It stores connection metadata only;
// terminal and application bytes remain on the Data Plane.
package workspaceactivity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

const (
	MaxLeaseTTL                 = 2 * time.Minute
	MaxActiveLeasesPerWorkspace = 10
)

var (
	ErrInvalidLease     = errors.New("invalid Workspace connection lease")
	ErrStaleLease       = errors.New("Workspace connection lease does not match the active runtime")
	ErrConnectionLimit  = errors.New("Workspace active connection limit reached")
	ErrSessionConnected = errors.New("Workspace Session already has an active connection")
	leaseIDPattern      = regexp.MustCompile(`^[a-f0-9]{48}$`)
)

type Lease struct {
	ID                   string
	SessionID            string
	TenantID             domain.TenantID
	WorkspaceID          domain.WorkspaceID
	ResourcePlaneID      domain.ResourcePlaneID
	MicroVMID            string
	AllocationGeneration int64
	ExpiresAt            time.Time
	Now                  time.Time
}

type IdleCandidate struct {
	TenantID             domain.TenantID
	WorkspaceID          domain.WorkspaceID
	ResourcePlaneID      domain.ResourcePlaneID
	MicroVMID            string
	AllocationGeneration int64
	IdleSince            time.Time
}

// Renew inserts or extends a lease only while the exact Lambda MicroVM
// allocation is still the active, running Workspace binding. A lease ID is
// immutable across every identity dimension and cannot be revived after
// release.
func Renew(ctx context.Context, tx pgx.Tx, lease Lease) error {
	if tx == nil || ctx == nil || !validLease(lease) {
		return ErrInvalidLease
	}
	var lockedWorkspace domain.WorkspaceID
	err := tx.QueryRow(ctx, `SELECT s.workspace_id FROM workspaces w
		JOIN placements p ON p.workspace_id = w.id
		JOIN workspace_status s ON s.workspace_id = w.id
		JOIN resource_bindings rb ON rb.workspace_id = w.id
		WHERE w.id = $1 AND w.tenant_id = $2 AND p.resource_plane_id = $3
			AND s.desired_state = 'running' AND s.observed_state = 'running'
			AND rb.kind = 'lambda_microvm' AND rb.resource_plane_id = $3
			AND rb.external_id = $4 AND rb.allocation_generation = $5
		FOR UPDATE OF s`, lease.WorkspaceID, lease.TenantID, lease.ResourcePlaneID, lease.MicroVMID, lease.AllocationGeneration).Scan(&lockedWorkspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleLease
	}
	if err != nil {
		return fmt.Errorf("resolve active Workspace connection binding: %w", err)
	}
	if lockedWorkspace != lease.WorkspaceID {
		return ErrStaleLease
	}
	// The FOR UPDATE read of the current Workspace status row above gives every
	// Gateway replica the same OCC conflict fence. Count active leases for this exact
	// runtime allocation, excluding this lease so an existing stream can renew
	// while the Workspace is at capacity.
	var activeLeases int
	err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_connection_leases
		WHERE tenant_id = $1 AND workspace_id = $2 AND resource_plane_id = $3
			AND microvm_id = $4 AND allocation_generation = $5 AND lease_id <> $6
			AND released_at IS NULL AND expires_at > $7`, lease.TenantID, lease.WorkspaceID,
		lease.ResourcePlaneID, lease.MicroVMID, lease.AllocationGeneration, lease.ID, lease.Now.UTC()).Scan(&activeLeases)
	if err != nil {
		return fmt.Errorf("count active Workspace connection leases: %w", err)
	}
	if activeLeases >= MaxActiveLeasesPerWorkspace {
		return ErrConnectionLimit
	}
	var claimedSessionID string
	err = tx.QueryRow(ctx, `UPDATE sessions SET active_connection_lease_id = $6,
		active_connection_expires_at = LEAST($7, expires_at)
		WHERE id = $1 AND tenant_id = $2 AND workspace_id = $3 AND resource_plane_id = $4
			AND expires_at > $5
			AND (active_connection_lease_id IS NULL OR active_connection_lease_id = $6 OR active_connection_expires_at <= $5)
		RETURNING id`, lease.SessionID, lease.TenantID, lease.WorkspaceID, lease.ResourcePlaneID,
		lease.Now.UTC(), lease.ID, lease.ExpiresAt.UTC()).Scan(&claimedSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSessionConnected
	}
	if err != nil {
		return fmt.Errorf("claim Workspace Session connection: %w", err)
	}
	if claimedSessionID != lease.SessionID {
		return ErrSessionConnected
	}
	var insertedID string
	err = tx.QueryRow(ctx, `INSERT INTO workspace_connection_leases
		(lease_id, tenant_id, workspace_id, resource_plane_id, microvm_id, allocation_generation, expires_at, created_at, renewed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (lease_id) DO UPDATE SET
			expires_at = GREATEST(workspace_connection_leases.expires_at, EXCLUDED.expires_at),
			renewed_at = GREATEST(workspace_connection_leases.renewed_at, EXCLUDED.renewed_at)
		WHERE workspace_connection_leases.tenant_id = EXCLUDED.tenant_id
			AND workspace_connection_leases.workspace_id = EXCLUDED.workspace_id
			AND workspace_connection_leases.resource_plane_id = EXCLUDED.resource_plane_id
			AND workspace_connection_leases.microvm_id = EXCLUDED.microvm_id
			AND workspace_connection_leases.allocation_generation = EXCLUDED.allocation_generation
			AND workspace_connection_leases.released_at IS NULL
		RETURNING lease_id`, lease.ID, lease.TenantID, lease.WorkspaceID, lease.ResourcePlaneID, lease.MicroVMID,
		lease.AllocationGeneration, lease.ExpiresAt.UTC(), lease.Now.UTC()).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleLease
	}
	if err != nil {
		return fmt.Errorf("write Workspace connection lease: %w", err)
	}
	if insertedID != lease.ID {
		return ErrStaleLease
	}
	return nil
}

// Release is idempotent for an exact lease identity. Replays cannot release a
// different Workspace, runtime generation, or MicroVM lease.
func Release(ctx context.Context, tx pgx.Tx, lease Lease) (bool, error) {
	if tx == nil || ctx == nil || !validLeaseIdentity(lease) {
		return false, ErrInvalidLease
	}
	result, err := tx.Exec(ctx, `UPDATE workspace_connection_leases SET released_at = COALESCE(released_at, $7)
		WHERE lease_id = $1 AND tenant_id = $2 AND workspace_id = $3 AND resource_plane_id = $4
			AND microvm_id = $5 AND allocation_generation = $6`, lease.ID, lease.TenantID, lease.WorkspaceID,
		lease.ResourcePlaneID, lease.MicroVMID, lease.AllocationGeneration, lease.Now.UTC())
	if err != nil {
		return false, fmt.Errorf("release Workspace connection lease: %w", err)
	}
	if result.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET active_connection_lease_id = NULL, active_connection_expires_at = NULL
		WHERE id = $1 AND tenant_id = $2 AND workspace_id = $3 AND resource_plane_id = $4
			AND active_connection_lease_id = $5`, lease.SessionID, lease.TenantID, lease.WorkspaceID,
		lease.ResourcePlaneID, lease.ID); err != nil {
		return false, fmt.Errorf("release Workspace Session connection claim: %w", err)
	}
	return true, nil
}

func HasActiveLeases(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, planeID domain.ResourcePlaneID,
	microVMID string, generation int64, now time.Time) (bool, error) {
	if tx == nil || ctx == nil || workspaceID == "" || planeID == "" || microVMID == "" || generation < 1 || now.IsZero() {
		return false, ErrInvalidLease
	}
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_connection_leases
		WHERE workspace_id = $1 AND resource_plane_id = $2 AND microvm_id = $3 AND allocation_generation = $4
			AND released_at IS NULL AND expires_at > $5)`, workspaceID, planeID, microVMID, generation, now.UTC()).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active Workspace connection leases: %w", err)
	}
	return active, nil
}

// ListIdleCandidates returns running Workspaces whose last connection lease
// ended before idleBefore. Workspaces without any recorded lease are omitted,
// preventing first-use or newly-created Workspaces from being suspended by an
// idle sweep. Callers must recheck policy, state, leases, and allocation inside
// the transaction that creates the suspend Operation.
func ListIdleCandidates(ctx context.Context, tx pgx.Tx, idleBefore time.Time, limit int) ([]IdleCandidate, error) {
	if tx == nil || ctx == nil || idleBefore.IsZero() || limit < 1 || limit > 1000 {
		return nil, ErrInvalidLease
	}
	rows, err := tx.Query(ctx, `SELECT w.tenant_id, w.id, p.resource_plane_id, rb.external_id, rb.allocation_generation,
		MAX(COALESCE(l.released_at, l.expires_at)) AS idle_since
		FROM workspace_connection_leases l
		JOIN workspaces w ON w.id = l.workspace_id AND w.tenant_id = l.tenant_id
		JOIN placements p ON p.workspace_id = w.id AND p.resource_plane_id = l.resource_plane_id
		JOIN workspace_status s ON s.workspace_id = w.id
		JOIN resource_bindings rb ON rb.workspace_id = w.id AND rb.resource_plane_id = p.resource_plane_id
		WHERE rb.kind = 'lambda_microvm' AND rb.external_id = l.microvm_id
			AND rb.allocation_generation = l.allocation_generation
			AND s.desired_state = 'running' AND s.observed_state = 'running'
		GROUP BY w.tenant_id, w.id, p.resource_plane_id, rb.external_id, rb.allocation_generation
		HAVING BOOL_AND(l.released_at IS NOT NULL OR l.expires_at <= $1)
			AND MAX(COALESCE(l.released_at, l.expires_at)) <= $1
		ORDER BY idle_since, w.id LIMIT $2`, idleBefore.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list idle Workspace candidates: %w", err)
	}
	defer rows.Close()
	result := make([]IdleCandidate, 0)
	for rows.Next() {
		var candidate IdleCandidate
		if err := rows.Scan(&candidate.TenantID, &candidate.WorkspaceID, &candidate.ResourcePlaneID,
			&candidate.MicroVMID, &candidate.AllocationGeneration, &candidate.IdleSince); err != nil {
			return nil, fmt.Errorf("scan idle Workspace candidate: %w", err)
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read idle Workspace candidates: %w", err)
	}
	return result, nil
}

func validLease(lease Lease) bool {
	return validLeaseIdentity(lease) && !lease.ExpiresAt.IsZero() && lease.ExpiresAt.After(lease.Now) &&
		!lease.ExpiresAt.After(lease.Now.Add(MaxLeaseTTL))
}

func validLeaseIdentity(lease Lease) bool {
	return leaseIDPattern.MatchString(lease.ID) && leaseIDPattern.MatchString(lease.SessionID) && lease.TenantID != "" && lease.WorkspaceID != "" && lease.ResourcePlaneID != "" &&
		lease.MicroVMID != "" && lease.AllocationGeneration > 0 && !lease.Now.IsZero()
}
