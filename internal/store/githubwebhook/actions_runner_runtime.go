package githubwebhook

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// BindActionsRunnerMicroVM pins the Lambda MicroVM allocated to one live
// runner claim. Re-delivery for the same provider ID is idempotent; a second
// allocation can never replace the first one for this claim.
var actionsRunnerResourcePlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func BindActionsRunnerMicroVM(ctx context.Context, tx pgx.Tx, integrationID string, workflowJobID, runAttempt int64, claimID, resourcePlaneID, microVMID string, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || workflowJobID <= 0 || runAttempt <= 0 ||
		!validActionsRunnerClaimID(claimID) || !actionsRunnerResourcePlaneIDPattern.MatchString(resourcePlaneID) || !validLambdaMicroVMID(microVMID) || now.IsZero() {
		return false, errors.New("transaction, exact live runner claim, Resource Plane, Lambda MicroVM ID, and current time are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_jobs
		SET resource_plane_id=COALESCE(resource_plane_id,$6),lambda_microvm_id=COALESCE(lambda_microvm_id,$7),updated_at=$8
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='starting'
		  AND claim_id=$4 AND lease_expires_at>$5 AND (resource_plane_id IS NULL OR resource_plane_id=$6)
		  AND (lambda_microvm_id IS NULL OR lambda_microvm_id=$7)`,
		integrationID, workflowJobID, runAttempt, claimID, now.UTC(), resourcePlaneID, microVMID, now.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func validLambdaMicroVMID(value string) bool {
	if len(value) < 1 || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}
