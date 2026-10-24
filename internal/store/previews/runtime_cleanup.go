package previews

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/store/resourcecapacity"
)

type RuntimeCleanupJob struct {
	CleanupIdentity
	Generation           int64
	HeadSHA              string
	HeadRef              string
	ExpiresAt            time.Time
	RuntimeWorkspaceID   domain.WorkspaceID
	ResourcePlaneID      domain.ResourcePlaneID
	BoundResourcePlaneID domain.ResourcePlaneID
	WorkspaceID          domain.WorkspaceID
	MicroVMID            string
	GatewayRouteID       string
	ProviderBound        bool
	ProvisionAttempts    int
	ClaimID              string
	LeaseExpiresAt       time.Time
	Attempt              int
}

// ClaimRuntimeCleanupAndEnqueue atomically leases retired generations and
// records one cleanup command for every generation that may own provider
// resources. Generations with no provision attempt have nothing to discover;
// they are confirmed clean in the same transaction without a Resource Plane
// round trip.
func ClaimRuntimeCleanupAndEnqueue(ctx context.Context, tx pgx.Tx, claimID string, eventIDs []string, now time.Time, lease time.Duration, limit int) (claimed, enqueued, locallyCleaned int, err error) {
	if ctx == nil || tx == nil || len(eventIDs) != limit || limit < 1 || limit > 100 {
		return 0, 0, 0, ErrInvalidRuntimeAllocation
	}
	jobs, err := ClaimRuntimeCleanupDue(ctx, tx, claimID, now, lease, limit)
	if err != nil {
		return 0, 0, 0, err
	}
	claimed = len(jobs)
	for index, job := range jobs {
		if !job.ProviderBound && job.ProvisionAttempts == 0 {
			cleaned, err := MarkRuntimeAllocationCleaned(ctx, tx, job, now)
			if err != nil {
				return 0, 0, 0, err
			}
			if !cleaned {
				return 0, 0, 0, ErrInvalidRuntimeAllocation
			}
			locallyCleaned++
			continue
		}
		if eventIDs[index] == "" || job.ResourcePlaneID == "" {
			return 0, 0, 0, ErrInvalidRuntimeAllocation
		}
		command := previewprotocol.Command{
			SchemaVersion: 1, Action: previewprotocol.ActionCleanup,
			TenantID: string(job.TenantID), InstallationID: job.InstallationID, RepositoryID: job.RepositoryID,
			PullRequestNumber: job.PullRequestNumber, Generation: job.Generation,
			RuntimeWorkspaceID: string(job.RuntimeWorkspaceID), ResourcePlaneID: string(job.ResourcePlaneID),
			ClaimID: job.ClaimID, ClaimAttempt: job.Attempt, LeaseExpiresAt: job.LeaseExpiresAt, ExpiresAt: job.ExpiresAt,
		}
		payload, err := previewprotocol.Encode(command)
		if err != nil {
			return 0, 0, 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
			(id,aggregate_type,aggregate_id,event_type,payload_json,created_at,published_at,attempt)
			VALUES ($1,$2,$3,$4,$5,$6,NULL,0)`, eventIDs[index], previewprotocol.AggregateType,
			string(job.RuntimeWorkspaceID), previewprotocol.EventType(previewprotocol.ActionCleanup), string(payload), now.UTC()); err != nil {
			return 0, 0, 0, fmt.Errorf("enqueue Preview runtime cleanup command: %w", err)
		}
		enqueued++
	}
	return claimed, enqueued, locallyCleaned, nil
}

// ClaimRuntimeCleanupDue leases retired generations with either a durable
// provider binding or an incomplete provision attempt that requires tag-based
// provider recovery. A live provision lease is allowed to finish or compensate
// before cleanup starts; an expired lease can be recovered after a crash.
func ClaimRuntimeCleanupDue(ctx context.Context, tx pgx.Tx, claimID string, now time.Time, lease time.Duration, limit int) ([]RuntimeCleanupJob, error) {
	if ctx == nil || tx == nil || !validCleanupClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 {
		return nil, ErrInvalidRuntimeAllocation
	}
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT tenant_id,installation_id,github_repository_id,pull_request_number,generation
		FROM repository_preview_runtime_allocations
		WHERE desired_state='absent' AND state='cleanup_pending'
		  AND ((resource_plane_id IS NOT NULL AND workspace_id IS NOT NULL AND microvm_id IS NOT NULL AND gateway_route_id IS NOT NULL)
		       OR (resource_plane_id IS NULL AND workspace_id IS NULL AND microvm_id IS NULL AND gateway_route_id IS NULL
		           AND (provision_attempts=0 OR target_resource_plane_id IS NOT NULL)))
		  AND (provision_claim_id IS NULL OR provision_lease_expires_at <= $1)
		  AND ((runtime_cleanup_claim_id IS NULL AND (runtime_cleanup_next_attempt_at IS NULL OR runtime_cleanup_next_attempt_at <= $1))
		       OR (runtime_cleanup_claim_id IS NOT NULL AND runtime_cleanup_lease_expires_at <= $1))
		ORDER BY COALESCE(runtime_cleanup_next_attempt_at,updated_at),updated_at,tenant_id,installation_id,github_repository_id,pull_request_number,generation
		LIMIT $2
	)
	UPDATE repository_preview_runtime_allocations a
	SET runtime_cleanup_claim_id=$3,runtime_cleanup_lease_expires_at=$4,
	    runtime_cleanup_attempts=a.runtime_cleanup_attempts+1,runtime_cleanup_next_attempt_at=NULL,runtime_cleanup_last_error='',updated_at=$1
	FROM due
	WHERE a.tenant_id=due.tenant_id AND a.installation_id=due.installation_id
	  AND a.github_repository_id=due.github_repository_id AND a.pull_request_number=due.pull_request_number AND a.generation=due.generation
	  AND a.desired_state='absent' AND a.state='cleanup_pending'
	  AND ((a.resource_plane_id IS NOT NULL AND a.workspace_id IS NOT NULL AND a.microvm_id IS NOT NULL AND a.gateway_route_id IS NOT NULL)
	       OR (a.resource_plane_id IS NULL AND a.workspace_id IS NULL AND a.microvm_id IS NULL AND a.gateway_route_id IS NULL
	           AND (a.provision_attempts=0 OR a.target_resource_plane_id IS NOT NULL)))
	  AND (a.provision_claim_id IS NULL OR a.provision_lease_expires_at <= $1)
	  AND ((a.runtime_cleanup_claim_id IS NULL AND (a.runtime_cleanup_next_attempt_at IS NULL OR a.runtime_cleanup_next_attempt_at <= $1))
	       OR (a.runtime_cleanup_claim_id IS NOT NULL AND a.runtime_cleanup_lease_expires_at <= $1))
	RETURNING a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,a.generation,a.head_sha,a.head_ref,
	          COALESCE(a.target_resource_plane_id,a.resource_plane_id,''),COALESCE(a.resource_plane_id,''),
	          COALESCE(a.workspace_id,''),COALESCE(a.microvm_id,''),COALESCE(a.gateway_route_id,''),
	          a.resource_plane_id IS NOT NULL,a.provision_attempts,a.runtime_cleanup_claim_id,
	          a.runtime_cleanup_lease_expires_at,a.runtime_cleanup_attempts,a.expires_at`, now.UTC(), limit, claimID, now.UTC().Add(lease))
	if err != nil {
		return nil, fmt.Errorf("claim retired Preview runtime generations: %w", err)
	}
	defer rows.Close()
	jobs := make([]RuntimeCleanupJob, 0, limit)
	for rows.Next() {
		var job RuntimeCleanupJob
		if err := rows.Scan(&job.TenantID, &job.InstallationID, &job.RepositoryID, &job.PullRequestNumber,
			&job.Generation, &job.HeadSHA, &job.HeadRef, &job.ResourcePlaneID, &job.BoundResourcePlaneID, &job.WorkspaceID, &job.MicroVMID,
			&job.GatewayRouteID, &job.ProviderBound, &job.ProvisionAttempts, &job.ClaimID, &job.LeaseExpiresAt, &job.Attempt, &job.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan claimed Preview runtime cleanup: %w", err)
		}
		job.LeaseExpiresAt = job.LeaseExpiresAt.UTC()
		job.ExpiresAt = job.ExpiresAt.UTC()
		job.RuntimeWorkspaceID = runtimeWorkspaceID(job.CleanupIdentity, job.Generation)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed Preview runtime cleanup: %w", err)
	}
	return jobs, nil
}

// RecordRuntimeCleanupFailure schedules a fixed-category retry for the exact
// generation and active cleanup lease. Provider errors are never persisted.
func RecordRuntimeCleanupFailure(ctx context.Context, tx pgx.Tx, job RuntimeCleanupJob, now time.Time, errorCode string) (time.Time, bool, error) {
	if ctx == nil || tx == nil || !validRuntimeCleanupJob(job) || now.IsZero() || !validCleanupErrorCode(errorCode) {
		return time.Time{}, false, ErrInvalidRuntimeAllocation
	}
	retryAt := now.UTC().Add(cleanupRetryDelay(job.Attempt))
	tag, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations
		SET runtime_cleanup_claim_id=NULL,runtime_cleanup_lease_expires_at=NULL,
		    runtime_cleanup_next_attempt_at=$10,runtime_cleanup_last_error=$11,updated_at=$9
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4 AND generation=$5
		  AND desired_state='absent' AND state='cleanup_pending' AND runtime_cleanup_claim_id=$6
		  AND runtime_cleanup_attempts=$7 AND runtime_cleanup_lease_expires_at=$8 AND runtime_cleanup_lease_expires_at>$9`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.Generation,
		job.ClaimID, job.Attempt, job.LeaseExpiresAt.UTC(), now.UTC(), retryAt, errorCode)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("record Preview runtime cleanup failure: %w", err)
	}
	return retryAt, tag.RowsAffected() == 1, nil
}

// MarkRuntimeAllocationCleaned acknowledges physical deletion for the exact
// retired generation. It releases the PR quota only after every generation is
// confirmed absent and cleaned.
func MarkRuntimeAllocationCleaned(ctx context.Context, tx pgx.Tx, job RuntimeCleanupJob, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validRuntimeCleanupJob(job) || now.IsZero() {
		return false, ErrInvalidRuntimeAllocation
	}
	tag, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations
		SET state='cleaned',provision_claim_id=NULL,provision_lease_expires_at=NULL,provision_next_attempt_at=NULL,
		    runtime_cleanup_claim_id=NULL,runtime_cleanup_lease_expires_at=NULL,
		    runtime_cleanup_next_attempt_at=NULL,runtime_cleanup_last_error='',updated_at=$9
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4 AND generation=$5
		  AND desired_state='absent' AND state='cleanup_pending' AND runtime_cleanup_claim_id=$6
		  AND runtime_cleanup_attempts=$7 AND runtime_cleanup_lease_expires_at=$8 AND runtime_cleanup_lease_expires_at>$9`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.Generation,
		job.ClaimID, job.Attempt, job.LeaseExpiresAt.UTC(), now.UTC())
	if err != nil {
		return false, fmt.Errorf("confirm Preview runtime generation cleanup: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := resourcecapacity.ReleasePreviewReservation(ctx, tx, job.RuntimeWorkspaceID, now.UTC()); err != nil {
			return false, err
		}
	}
	if tag.RowsAffected() != 1 {
		var alreadyCleaned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repository_preview_runtime_allocations
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
			  AND generation=$5 AND desired_state='absent' AND state='cleaned')`,
			job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.Generation).Scan(&alreadyCleaned); err != nil {
			return false, fmt.Errorf("verify Preview runtime generation cleanup: %w", err)
		}
		return alreadyCleaned, nil
	}
	_, err = tx.Exec(ctx, `UPDATE repository_previews p
		SET state='cleaned',cleanup_claim_id=NULL,cleanup_lease_expires_at=NULL,cleanup_next_attempt_at=NULL,cleanup_last_error='',updated_at=$5
		WHERE p.tenant_id=$1 AND p.installation_id=$2 AND p.github_repository_id=$3 AND p.pull_request_number=$4
		  AND p.desired_state='absent' AND p.state='cleanup_pending'
		  AND NOT EXISTS (SELECT 1 FROM repository_preview_runtime_allocations a
		    WHERE a.tenant_id=p.tenant_id AND a.installation_id=p.installation_id AND a.github_repository_id=p.github_repository_id
		      AND a.pull_request_number=p.pull_request_number AND a.state<>'cleaned')`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, now.UTC())
	if err != nil {
		return false, fmt.Errorf("release cleaned Preview lifecycle reservation: %w", err)
	}
	return true, nil
}

func validRuntimeCleanupJob(job RuntimeCleanupJob) bool {
	if !validCleanupIdentity(job.CleanupIdentity) || job.Generation <= 0 || job.RuntimeWorkspaceID != runtimeWorkspaceID(job.CleanupIdentity, job.Generation) ||
		!validCleanupClaimID(job.ClaimID) || job.LeaseExpiresAt.IsZero() || job.Attempt <= 0 {
		return false
	}
	if job.ProviderBound {
		return job.ResourcePlaneID == job.BoundResourcePlaneID && validRuntimeBinding(RuntimeBinding{ResourcePlaneID: job.BoundResourcePlaneID,
			WorkspaceID: job.WorkspaceID, MicroVMID: job.MicroVMID, GatewayRouteID: job.GatewayRouteID})
	}
	return (job.ProvisionAttempts == 0 || runtimeIdentifierPattern.MatchString(string(job.ResourcePlaneID))) &&
		job.BoundResourcePlaneID == "" && job.WorkspaceID == "" && job.MicroVMID == "" && job.GatewayRouteID == ""
}
