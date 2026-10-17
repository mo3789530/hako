package githubwebhook

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

// ActionsRunnerJobClaimer applies an admitted runner job claim in a bounded
// OCC-retrying database-only transaction. SQS publication is deliberately
// performed by the caller after this transaction commits.
type ActionsRunnerJobClaimer struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

func (c ActionsRunnerJobClaimer) ClaimNext(ctx context.Context, claimID string, now time.Time, lease time.Duration) (ActionsRunnerJob, bool, error) {
	if ctx == nil || c.Pool == nil {
		return ActionsRunnerJob{}, false, errors.New("runner claim store and context are required")
	}
	type claimResult struct {
		job   ActionsRunnerJob
		found bool
	}
	result, err := transaction.Within(ctx, c.Pool, c.Policy, func(ctx context.Context, tx pgx.Tx) (claimResult, error) {
		job, found, err := ClaimNextActionsRunnerJob(ctx, tx, claimID, now, lease)
		if err != nil {
			return claimResult{}, err
		}
		return claimResult{job: job, found: found}, nil
	})
	return result.job, result.found, err
}
