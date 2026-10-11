package githubchecks

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

type DBStore struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

func (s DBStore) Load(ctx context.Context, tenantID domain.TenantID, runID domain.WorkloadRunID) (DesiredCheck, error) {
	if s.Pool == nil || ctx == nil {
		return DesiredCheck{}, errors.New("context and GitHub Check database are required")
	}
	return transaction.Within(ctx, s.Pool, s.Policy, func(ctx context.Context, tx pgx.Tx) (DesiredCheck, error) {
		check, err := workloads.GetGitHubCheck(ctx, tx, tenantID, runID)
		if err != nil {
			return DesiredCheck{}, err
		}
		run, err := workloads.Get(ctx, tx, tenantID, runID)
		if err != nil {
			return DesiredCheck{}, err
		}
		return DesiredCheck{Check: check, Run: run}, nil
	})
}

func (s DBStore) SaveID(ctx context.Context, tenantID domain.TenantID, runID domain.WorkloadRunID, commitSHA string, checkRunID int64, now time.Time) error {
	if s.Pool == nil || ctx == nil {
		return errors.New("context and GitHub Check database are required")
	}
	_, err := transaction.Within(ctx, s.Pool, s.Policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, workloads.SaveGitHubCheckRunID(ctx, tx, tenantID, runID, commitSHA, checkRunID, now)
	})
	return err
}
