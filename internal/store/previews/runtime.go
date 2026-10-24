package previews

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/scheduler"
	"github.com/mo3789530/hako/internal/store/resourcecapacity"
)

var (
	ErrInvalidRuntimeAllocation  = errors.New("invalid Preview runtime allocation")
	ErrRuntimeAllocationConflict = errors.New("Preview runtime allocation changed while provisioning")
	runtimeIdentifierPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
)

type ProvisionJob struct {
	CleanupIdentity
	Generation            int64
	RuntimeWorkspaceID    domain.WorkspaceID
	TargetResourcePlaneID domain.ResourcePlaneID
	HeadSHA               string
	HeadRef               string
	ExpiresAt             time.Time
	ClaimID               string
	LeaseExpiresAt        time.Time
	Attempt               int
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
func ClaimRuntimeAllocations(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, claimID string, now time.Time, lease time.Duration, limit int) ([]ProvisionJob, error) {
	if ctx == nil || tx == nil || !runtimeIdentifierPattern.MatchString(string(resourcePlaneID)) || !validCleanupClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 {
		return nil, ErrInvalidRuntimeAllocation
	}
	return ClaimRuntimeAllocationsOnPlane(ctx, tx, resourcePlaneID, "standard", claimID, now, lease, limit)
}

// ClaimRuntimeAllocationsOnPlane keeps an operator-selected Plane while still
// accounting for configured capacity. Tenant policy/candidate selection is
// intentionally bypassed only for this explicit placement override.
func ClaimRuntimeAllocationsOnPlane(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, runtimeClass, claimID string, now time.Time, lease time.Duration, limit int) ([]ProvisionJob, error) {
	if ctx == nil || tx == nil || !runtimeIdentifierPattern.MatchString(string(resourcePlaneID)) || !validCleanupClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 || runtimeClass == "" {
		return nil, ErrInvalidRuntimeAllocation
	}
	return claimRuntimeAllocations(ctx, tx, &resourcePlaneID, func(ctx context.Context, tx pgx.Tx, _ domain.TenantID, _ domain.WorkspaceID) (domain.ResourcePlaneID, error) {
		available, err := resourcecapacity.Reserve(ctx, tx, resourcePlaneID, runtimeClass, now.UTC())
		if err != nil {
			return "", err
		}
		if !available {
			return "", scheduler.ErrNoEligibleResourcePlane
		}
		return resourcePlaneID, nil
	}, runtimeClass, claimID, now, lease, limit)
}

// ClaimRuntimeAllocationsScheduled selects an eligible Plane independently
// for each first-time generation claim. Selection and target pinning happen in
// the caller's transaction. Existing target Plane pins are always reused.
func ClaimRuntimeAllocationsScheduled(ctx context.Context, tx pgx.Tx, claimID string, now time.Time, lease time.Duration, limit int, runtimeClass string) ([]ProvisionJob, error) {
	if ctx == nil || tx == nil || !validCleanupClaimID(claimID) || now.IsZero() || lease < 5*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 || runtimeClass == "" {
		return nil, ErrInvalidRuntimeAllocation
	}
	return claimRuntimeAllocations(ctx, tx, nil, func(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, _ domain.WorkspaceID) (domain.ResourcePlaneID, error) {
		planeID, err := scheduler.Select(ctx, tx, scheduler.Policy{
			TenantID: tenantID, RuntimeClass: runtimeClass, RequiredCapabilities: []string{"microvm"},
		})
		if err != nil {
			return "", err
		}
		return planeID, nil
	}, runtimeClass, claimID, now, lease, limit)
}

type runtimePlaneSelector func(context.Context, pgx.Tx, domain.TenantID, domain.WorkspaceID) (domain.ResourcePlaneID, error)

func claimRuntimeAllocations(ctx context.Context, tx pgx.Tx, fixedPlane *domain.ResourcePlaneID, selectPlane runtimePlaneSelector, runtimeClass, claimID string, now time.Time, lease time.Duration, limit int) ([]ProvisionJob, error) {
	rows, err := tx.Query(ctx, `SELECT a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,a.generation,
		       a.head_sha,a.head_ref,a.expires_at,a.target_resource_plane_id
		FROM repository_preview_runtime_allocations a
		JOIN repository_previews p USING (tenant_id,installation_id,github_repository_id,pull_request_number)
		WHERE a.desired_state='present' AND p.desired_state='present' AND p.generation=a.generation
		  AND a.expires_at>$1 AND p.expires_at>$1
		  AND (($2::TEXT IS NULL AND (a.target_resource_plane_id IS NOT NULL OR a.provision_attempts=0))
		       OR ($2::TEXT IS NOT NULL AND ((a.target_resource_plane_id IS NULL AND a.provision_attempts=0) OR a.target_resource_plane_id=$2)))
		  AND ((a.state='awaiting_runtime' AND (a.provision_next_attempt_at IS NULL OR a.provision_next_attempt_at <= $1))
		       OR (a.state='provisioning' AND a.provision_lease_expires_at <= $1))
		ORDER BY COALESCE(a.provision_next_attempt_at,a.created_at),a.created_at,a.tenant_id,a.installation_id,a.github_repository_id,a.pull_request_number,a.generation
		LIMIT $3`, now.UTC(), fixedPlane, limit)
	if err != nil {
		return nil, fmt.Errorf("list due Preview runtime allocations: %w", err)
	}
	type candidate struct {
		job   ProvisionJob
		plane domain.ResourcePlaneID
	}
	candidates := make([]candidate, 0, limit)
	for rows.Next() {
		var item candidate
		var targetPlane *string
		if err := rows.Scan(&item.job.TenantID, &item.job.InstallationID, &item.job.RepositoryID, &item.job.PullRequestNumber,
			&item.job.Generation, &item.job.HeadSHA, &item.job.HeadRef, &item.job.ExpiresAt, &targetPlane); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan due Preview runtime allocation: %w", err)
		}
		item.job.ExpiresAt = item.job.ExpiresAt.UTC()
		item.job.RuntimeWorkspaceID = runtimeWorkspaceID(item.job.CleanupIdentity, item.job.Generation)
		if targetPlane != nil {
			item.plane = domain.ResourcePlaneID(*targetPlane)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate due Preview runtime allocations: %w", err)
	}
	rows.Close()
	jobs := make([]ProvisionJob, 0, len(candidates))
	for _, item := range candidates {
		planeID := item.plane
		selectedNewPlane := false
		if planeID == "" && fixedPlane != nil {
			planeID = *fixedPlane
		}
		if planeID == "" {
			if selectPlane == nil {
				return nil, ErrInvalidRuntimeAllocation
			}
			planeID, err = selectPlane(ctx, tx, item.job.TenantID, item.job.RuntimeWorkspaceID)
			if errors.Is(err, scheduler.ErrNoEligibleResourcePlane) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("select Preview Resource Plane: %w", err)
			}
			selectedNewPlane = true
		}
		if !runtimeIdentifierPattern.MatchString(string(planeID)) {
			return nil, ErrInvalidRuntimeAllocation
		}
		leaseExpiresAt := now.UTC().Add(lease)
		tag, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations a
			SET state='provisioning',provision_claim_id=$7,provision_lease_expires_at=$8,
			    provision_attempts=a.provision_attempts+1,provision_next_attempt_at=NULL,provision_last_error='',
			    target_resource_plane_id=COALESCE(a.target_resource_plane_id,$9),updated_at=$1
			FROM repository_previews p
			WHERE a.tenant_id=$2 AND a.installation_id=$3 AND a.github_repository_id=$4 AND a.pull_request_number=$5 AND a.generation=$6
			  AND p.tenant_id=a.tenant_id AND p.installation_id=a.installation_id AND p.github_repository_id=a.github_repository_id
			  AND p.pull_request_number=a.pull_request_number AND p.generation=a.generation AND p.desired_state='present' AND p.expires_at>$1
			  AND a.desired_state='present' AND a.expires_at>$1 AND (a.target_resource_plane_id IS NULL OR a.target_resource_plane_id=$9)
			  AND ((a.state='awaiting_runtime' AND (a.provision_next_attempt_at IS NULL OR a.provision_next_attempt_at <= $1))
			       OR (a.state='provisioning' AND a.provision_lease_expires_at <= $1))`,
			now.UTC(), item.job.TenantID, item.job.InstallationID, item.job.RepositoryID, item.job.PullRequestNumber,
			item.job.Generation, claimID, leaseExpiresAt, planeID)
		if err != nil {
			return nil, fmt.Errorf("claim Preview runtime allocation: %w", err)
		}
		if tag.RowsAffected() != 1 {
			if selectedNewPlane {
				if err := resourcecapacity.Unreserve(ctx, tx, planeID, runtimeClass, now.UTC()); err != nil {
					return nil, fmt.Errorf("undo raced Preview capacity selection: %w", err)
				}
			}
			continue
		}
		if selectedNewPlane {
			if err := resourcecapacity.RecordPreviewReservation(ctx, tx, item.job.RuntimeWorkspaceID, planeID, runtimeClass, now.UTC()); err != nil {
				return nil, err
			}
		}
		item.job.TargetResourcePlaneID, item.job.ClaimID = planeID, claimID
		item.job.LeaseExpiresAt, item.job.Attempt = leaseExpiresAt, 0
		if err := tx.QueryRow(ctx, `SELECT provision_attempts FROM repository_preview_runtime_allocations
			WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4 AND generation=$5`,
			item.job.TenantID, item.job.InstallationID, item.job.RepositoryID, item.job.PullRequestNumber, item.job.Generation).Scan(&item.job.Attempt); err != nil {
			return nil, fmt.Errorf("read Preview runtime claim attempt: %w", err)
		}
		jobs = append(jobs, item.job)
	}
	return jobs, nil
}

// BindRuntimeAllocation records provider IDs only for the exact live claim and
// only while its PR head remains current. If it returns false, the caller must
// compensate any Resource Plane resources it already created.
func BindRuntimeAllocation(ctx context.Context, tx pgx.Tx, job ProvisionJob, binding RuntimeBinding, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validProvisionJob(job) || !validRuntimeBinding(binding) || binding.ResourcePlaneID != job.TargetResourcePlaneID || binding.WorkspaceID != job.RuntimeWorkspaceID || now.IsZero() {
		return false, ErrInvalidRuntimeAllocation
	}
	tag, err := tx.Exec(ctx, `UPDATE repository_preview_runtime_allocations a
		SET state='running',resource_plane_id=$9,workspace_id=$10,microvm_id=$11,gateway_route_id=$12,
		    provision_claim_id=NULL,provision_lease_expires_at=NULL,provision_next_attempt_at=NULL,provision_last_error='',updated_at=$8
		WHERE a.tenant_id=$1 AND a.installation_id=$2 AND a.github_repository_id=$3 AND a.pull_request_number=$4
		  AND a.generation=$5 AND a.desired_state='present' AND a.state='provisioning'
		  AND a.target_resource_plane_id=$9
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
	return validCleanupIdentity(job.CleanupIdentity) && job.Generation > 0 && runtimeIdentifierPattern.MatchString(string(job.TargetResourcePlaneID)) && len(job.HeadSHA) == 40 &&
		job.RuntimeWorkspaceID == runtimeWorkspaceID(job.CleanupIdentity, job.Generation) &&
		!job.ExpiresAt.IsZero() && validCleanupClaimID(job.ClaimID) && !job.LeaseExpiresAt.IsZero() && job.Attempt > 0
}

func runtimeWorkspaceID(identity CleanupIdentity, generation int64) domain.WorkspaceID {
	key := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d", identity.TenantID, identity.InstallationID,
		identity.RepositoryID, identity.PullRequestNumber, generation)
	digest := sha256.Sum256([]byte(key))
	return domain.WorkspaceID("prvws_" + hex.EncodeToString(digest[:16]))
}

// RuntimeWorkspaceID returns the stable, private runtime identity for one PR
// head generation. Resource Plane recovery uses it to rediscover resources by
// Hako tags when a worker stopped before provider IDs were bound in the CP.
func RuntimeWorkspaceID(identity CleanupIdentity, generation int64) (domain.WorkspaceID, error) {
	if !validCleanupIdentity(identity) || generation < 1 {
		return "", ErrInvalidRuntimeAllocation
	}
	return runtimeWorkspaceID(identity, generation), nil
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
