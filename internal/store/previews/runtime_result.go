package previews

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
)

// ApplyRuntimeResult applies a Resource Plane acknowledgement only while the
// exact Preview generation, Plane, claim, attempt, and lease are still active.
// A stale result is an acknowledged no-op and can never overwrite newer PR
// state. The caller supplies its current Control Plane time.
func ApplyRuntimeResult(ctx context.Context, tx pgx.Tx, result previewprotocol.Result, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || now.IsZero() || result.Validate() != nil {
		return false, ErrInvalidRuntimeAllocation
	}
	identity := CleanupIdentity{TenantID: domain.TenantID(result.TenantID), InstallationID: result.InstallationID,
		RepositoryID: result.RepositoryID, PullRequestNumber: result.PullRequestNumber}
	if !validCleanupIdentity(identity) || runtimeWorkspaceID(identity, result.Generation) != domain.WorkspaceID(result.RuntimeWorkspaceID) {
		return false, ErrInvalidRuntimeAllocation
	}
	if result.ErrorCode == "claim_lease_expired" || result.ErrorCode == "preview_expired" {
		return false, nil
	}
	switch result.Action {
	case previewprotocol.ActionProvision:
		return applyProvisionResult(ctx, tx, identity, result, now.UTC())
	case previewprotocol.ActionCleanup:
		return applyCleanupResult(ctx, tx, identity, result, now.UTC())
	default:
		return false, ErrInvalidRuntimeAllocation
	}
}

func applyProvisionResult(ctx context.Context, tx pgx.Tx, identity CleanupIdentity, result previewprotocol.Result, now time.Time) (bool, error) {
	job := ProvisionJob{CleanupIdentity: identity, Generation: result.Generation,
		RuntimeWorkspaceID: domain.WorkspaceID(result.RuntimeWorkspaceID), TargetResourcePlaneID: domain.ResourcePlaneID(result.ResourcePlaneID),
		ClaimID: result.ClaimID, LeaseExpiresAt: result.LeaseExpiresAt.UTC(), Attempt: result.ClaimAttempt}
	err := tx.QueryRow(ctx, `SELECT head_sha,head_ref,expires_at
		FROM repository_preview_runtime_allocations a
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4 AND generation=$5
		  AND desired_state='present' AND state='provisioning' AND target_resource_plane_id=$6
		  AND provision_claim_id=$7 AND provision_attempts=$8 AND provision_lease_expires_at=$9
		  AND provision_lease_expires_at>$10 AND expires_at>$10
		  AND EXISTS (SELECT 1 FROM repository_previews p WHERE p.tenant_id=a.tenant_id AND p.installation_id=a.installation_id
		    AND p.github_repository_id=a.github_repository_id AND p.pull_request_number=a.pull_request_number
		    AND p.generation=a.generation AND p.desired_state='present' AND p.expires_at>$10)`,
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber,
		result.Generation, result.ResourcePlaneID, result.ClaimID, result.ClaimAttempt, result.LeaseExpiresAt.UTC(), now).
		Scan(&job.HeadSHA, &job.HeadRef, &job.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load active Preview provision result claim: %w", err)
	}
	job.ExpiresAt = job.ExpiresAt.UTC()
	if result.Status == "succeeded" {
		binding := RuntimeBinding{ResourcePlaneID: domain.ResourcePlaneID(result.ResourcePlaneID),
			WorkspaceID: domain.WorkspaceID(result.RuntimeWorkspaceID), MicroVMID: result.Binding.MicroVMID, GatewayRouteID: result.Binding.GatewayRouteID}
		return BindRuntimeAllocation(ctx, tx, job, binding, now)
	}
	_, updated, err := RecordRuntimeAllocationFailure(ctx, tx, job, now, result.ErrorCode)
	return updated, err
}

func applyCleanupResult(ctx context.Context, tx pgx.Tx, identity CleanupIdentity, result previewprotocol.Result, now time.Time) (bool, error) {
	job := RuntimeCleanupJob{CleanupIdentity: identity, Generation: result.Generation,
		RuntimeWorkspaceID: domain.WorkspaceID(result.RuntimeWorkspaceID), ClaimID: result.ClaimID,
		LeaseExpiresAt: result.LeaseExpiresAt.UTC(), Attempt: result.ClaimAttempt}
	err := tx.QueryRow(ctx, `SELECT head_sha,head_ref,COALESCE(target_resource_plane_id,resource_plane_id,''),
		       COALESCE(resource_plane_id,''),COALESCE(workspace_id,''),COALESCE(microvm_id,''),COALESCE(gateway_route_id,''),
		       resource_plane_id IS NOT NULL,provision_attempts
		FROM repository_preview_runtime_allocations a
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND pull_request_number=$4 AND generation=$5
		  AND desired_state='absent' AND state='cleanup_pending' AND runtime_cleanup_claim_id=$6
		  AND runtime_cleanup_attempts=$7 AND runtime_cleanup_lease_expires_at=$8 AND runtime_cleanup_lease_expires_at>$9`,
		identity.TenantID, identity.InstallationID, identity.RepositoryID, identity.PullRequestNumber,
		result.Generation, result.ClaimID, result.ClaimAttempt, result.LeaseExpiresAt.UTC(), now).
		Scan(&job.HeadSHA, &job.HeadRef, &job.ResourcePlaneID, &job.BoundResourcePlaneID, &job.WorkspaceID,
			&job.MicroVMID, &job.GatewayRouteID, &job.ProviderBound, &job.ProvisionAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load active Preview cleanup result claim: %w", err)
	}
	if job.ResourcePlaneID != domain.ResourcePlaneID(result.ResourcePlaneID) {
		return false, nil
	}
	if result.Status == "succeeded" {
		return MarkRuntimeAllocationCleaned(ctx, tx, job, now)
	}
	_, updated, err := RecordRuntimeCleanupFailure(ctx, tx, job, now, result.ErrorCode)
	return updated, err
}
