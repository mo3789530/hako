package previews

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrInvalidRuntimeAllocation  = errors.New("invalid Preview runtime allocation")
	ErrRuntimeAllocationConflict = errors.New("Preview runtime allocation changed while provisioning")
	runtimeIdentifierPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
)

type ProvisionJob struct {
	CleanupIdentity
	Generation     int64
	HeadSHA        string
	HeadRef        string
	ExpiresAt      time.Time
	ClaimID        string
	LeaseExpiresAt time.Time
	Attempt        int
}

// RuntimeBinding is produced by the Resource Plane after it has created the
// isolated Lambda MicroVM and expiring Gateway route for one PR head.
type RuntimeBinding struct {
	ResourcePlaneID domain.ResourcePlaneID
	WorkspaceID     domain.WorkspaceID
	MicroVMID       string
	GatewayRouteID  string
}

// ClaimRuntimeAllocations leases a bounded batch of current, unexpired PR-head
// generations. Expired leases are recoverable after a worker restart. A later
// BindRuntimeAllocation call rechecks the current generation before recording
// any provider identity.
func ClaimRuntimeAllocations(ctx context.Context, tx pgx.Tx, claimID string, now time.Time, lease time.Duration, limit int) ([]ProvisionJob, error) {
	if ctx == nil || tx == nil || !validCleanupClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 {
		return nil, ErrInvalidRuntimeAllocation
	}
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,a.generation
		FROM repository_preview_runtime_allocations a
		JOIN repository_previews p USING (tenant_id,installation_id,github_repository_id,pull_request_number)
		WHERE a.desired_state='present' AND p.desired_state='present' AND p.generation=a.generation
		  AND a.expires_at>$1 AND p.expires_at>$1
		  AND ((a.state='awaiting_runtime' AND (a.provision_next_attempt_at IS NULL OR a.provision_next_attempt_at <= $1))
		       OR (a.state='provisioning' AND a.provision_lease_expires_at <= $1))
		ORDER BY COALESCE(a.provision_next_attempt_at,a.created_at),a.created_at,a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,a.generation
		LIMIT $2
	)
	UPDATE repository_preview_runtime_allocations a
	SET state='provisioning',provision_claim_id=$3,provision_lease_expires_at=$4,
	    provision_attempts=a.provision_attempts+1,provision_next_attempt_at=NULL,provision_last_error='',updated_at=$1
	FROM due
	WHERE a.tenant_id=due.tenant_id AND a.installation_id=due.installation_id
	  AND a.github_repository_id=due.github_repository_id AND a.pull_request_number=due.pull_request_number AND a.generation=due.generation
	  AND a.desired_state='present' AND a.expires_at>$1
	  AND ((a.state='awaiting_runtime' AND (a.provision_next_attempt_at IS NULL OR a.provision_next_attempt_at <= $1))
	       OR (a.state='provisioning' AND a.provision_lease_expires_at <= $1))
	RETURNING a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,a.generation,
	          a.head_sha,a.head_ref,a.expires_at,a.provision_claim_id,a.provision_lease_expires_at,a.provision_attempts`,
		now.UTC(), limit, claimID, now.UTC().Add(lease))
	if err != nil {
		return nil, fmt.Errorf("claim Preview runtime allocations: %w", err)
	}
	defer rows.Close()
	jobs := make([]ProvisionJob, 0, limit)
	for rows.Next() {
		var job ProvisionJob
		if err := rows.Scan(&job.TenantID, &job.InstallationID, &job.RepositoryID, &job.PullRequestNumber,
			&job.Generation, &job.HeadSHA, &job.HeadRef, &job.ExpiresAt, &job.ClaimID, &job.LeaseExpiresAt, &job.Attempt); err != nil {
			return nil, fmt.Errorf("scan claimed Preview runtime allocation: %w", err)
		}
		job.ExpiresAt, job.LeaseExpiresAt = job.ExpiresAt.UTC(), job.LeaseExpiresAt.UTC()
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed Preview runtime allocations: %w", err)
	}
	return jobs, nil
}

// BindRuntimeAllocation records provider IDs only for the exact live claim and
// only while its PR head remains current. If it returns false, the caller must
// compensate any Resource Plane resources it already created.
func BindRuntimeAllocation(ctx context.Context, tx pgx.Tx, job ProvisionJob, binding RuntimeBinding, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validProvisionJob(job) || !validRuntimeBinding(binding) || now.IsZero() {
		return false, ErrInvalidRuntimeAllocation
	}
	tag, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations a
		SET state='running',resource_plane_id=$9,workspace_id=$10,microvm_id=$11,gateway_route_id=$12,
		    provision_claim_id=NULL,provision_lease_expires_at=NULL,provision_next_attempt_at=NULL,provision_last_error='',updated_at=$8
		WHERE a.tenant_id=$1 AND a.installation_id=$2 AND a.github_repository_id=$3 AND a.pull_request_number=$4
		  AND a.generation=$5 AND a.desired_state='present' AND a.state='provisioning'
		  AND a.provision_claim_id=$6 AND a.provision_attempts=$7 AND a.provision_lease_expires_at=$13
		  AND a.provision_lease_expires_at>$8 AND a.expires_at>$8
		  AND EXISTS (SELECT 1 FROM repository_previews p WHERE p.tenant_id=a.tenant_id
		    AND p.installation_id=a.installation_id AND p.github_repository_id=a.github_repository_id
		    AND p.pull_request_number=a.pull_request_number AND p.generation=a.generation
		    AND p.desired_state='present' AND p.expires_at>$8)`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.Generation,
		job.ClaimID, job.Attempt, now.UTC(), binding.ResourcePlaneID, binding.WorkspaceID,
		binding.MicroVMID, binding.GatewayRouteID, job.LeaseExpiresAt.UTC())
	if err != nil {
		return false, fmt.Errorf("bind Preview runtime allocation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	return true, nil
}

// RecordRuntimeAllocationFailure records a fixed failure class and retry time
// only for the current unexpired provisioning lease. Raw provider errors are
// deliberately not accepted by this store API.
func RecordRuntimeAllocationFailure(ctx context.Context, tx pgx.Tx, job ProvisionJob, now time.Time, errorCode string) (time.Time, bool, error) {
	if ctx == nil || tx == nil || !validProvisionJob(job) || now.IsZero() || !validRuntimeProvisionError(errorCode) {
		return time.Time{}, false, ErrInvalidRuntimeAllocation
	}
	retryAt := now.UTC().Add(runtimeProvisionRetryDelay(job.Attempt))
	tag, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations
		SET state='awaiting_runtime',provision_claim_id=NULL,provision_lease_expires_at=NULL,
		    provision_next_attempt_at=$9,provision_last_error=$10,updated_at=$8
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4
		  AND generation=$5 AND desired_state='present' AND state='provisioning' AND provision_claim_id=$6
		  AND provision_attempts=$7 AND provision_lease_expires_at=$11 AND provision_lease_expires_at>$8 AND expires_at>$8
		  AND EXISTS (SELECT 1 FROM repository_previews p WHERE p.tenant_id=repository_preview_runtime_allocations.tenant_id
		    AND p.installation_id=repository_preview_runtime_allocations.installation_id
		    AND p.github_repository_id=repository_preview_runtime_allocations.github_repository_id
		    AND p.pull_request_number=repository_preview_runtime_allocations.pull_request_number
		    AND p.generation=repository_preview_runtime_allocations.generation AND p.desired_state='present' AND p.expires_at>$8)`,
		job.TenantID, job.InstallationID, job.RepositoryID, job.PullRequestNumber, job.Generation,
		job.ClaimID, job.Attempt, now.UTC(), retryAt, errorCode, job.LeaseExpiresAt.UTC())
	if err != nil {
		return time.Time{}, false, fmt.Errorf("record Preview runtime failure: %w", err)
	}
	return retryAt, tag.RowsAffected() == 1, nil
}

func validProvisionJob(job ProvisionJob) bool {
	return validCleanupIdentity(job.CleanupIdentity) && job.Generation > 0 && len(job.HeadSHA) == 40 &&
		!job.ExpiresAt.IsZero() && validCleanupClaimID(job.ClaimID) && !job.LeaseExpiresAt.IsZero() && job.Attempt > 0
}

func validRuntimeBinding(binding RuntimeBinding) bool {
	return runtimeIdentifierPattern.MatchString(string(binding.ResourcePlaneID)) &&
		runtimeIdentifierPattern.MatchString(string(binding.WorkspaceID)) &&
		runtimeIdentifierPattern.MatchString(binding.MicroVMID) && runtimeIdentifierPattern.MatchString(binding.GatewayRouteID)
}

func validRuntimeProvisionError(code string) bool {
	switch code {
	case "runtime_profile_missing", "microvm_launch_failed", "gateway_route_publish_failed", "provider_unavailable":
		return true
	default:
		return false
	}
}

func runtimeProvisionRetryDelay(attempt int) time.Duration {
	delay := cleanupRetryBase
	for i := 1; i < attempt && delay < cleanupRetryMax; i++ {
		delay *= 2
	}
	if delay > cleanupRetryMax {
		return cleanupRetryMax
	}
	return delay
}
