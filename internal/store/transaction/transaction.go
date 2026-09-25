// Package transaction runs database-only callbacks with bounded serialization
// conflict retries.
package transaction

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/awslabs/aurora-dsql-connectors/go/pgx/occretry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Beginner is satisfied by pgxpool.Pool and keeps the transaction runner
// usable with both Aurora DSQL and ordinary PostgreSQL.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Policy bounds OCC/serialization retries for a transaction.
// MaxRetries is the number of attempts after the initial attempt.
type Policy struct {
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

func DefaultPolicy() Policy {
	return Policy{
		MaxRetries:   5,
		InitialDelay: 50 * time.Millisecond,
		MaxDelay:     time.Second,
	}
}

// Within starts a fresh transaction for each attempt. The callback may run
// multiple times, so it must contain only database work and must not publish
// messages, call AWS, write files, or mutate caller-owned state.
func Within[T any](ctx context.Context, pool Beginner, policy Policy, fn func(context.Context, pgx.Tx) (T, error)) (T, error) {
	var zero T
	if pool == nil {
		return zero, errors.New("transaction pool is required")
	}
	if fn == nil {
		return zero, errors.New("transaction callback is required")
	}
	policy, err := normalize(policy)
	if err != nil {
		return zero, err
	}

	delay := policy.InitialDelay
	var lastErr error
	for attempt := 0; attempt <= policy.MaxRetries; attempt++ {
		result, err := runOnce(ctx, pool, fn)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryable(err) || attempt == policy.MaxRetries {
			break
		}
		if err := wait(ctx, delay); err != nil {
			return zero, err
		}
		if delay > policy.MaxDelay/2 {
			delay = policy.MaxDelay
		} else {
			delay *= 2
		}
	}
	return zero, fmt.Errorf("transaction failed after %d retries: %w", policy.MaxRetries, lastErr)
}

func runOnce[T any](ctx context.Context, pool Beginner, fn func(context.Context, pgx.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	result, err := fn(ctx, tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

func normalize(policy Policy) (Policy, error) {
	if policy.MaxRetries == 0 && policy.InitialDelay == 0 && policy.MaxDelay == 0 {
		return DefaultPolicy(), nil
	}
	if policy.MaxRetries < 0 || policy.InitialDelay < 0 || policy.MaxDelay < 0 {
		return Policy{}, errors.New("transaction retry policy values must be non-negative")
	}
	if policy.InitialDelay == 0 {
		policy.InitialDelay = DefaultPolicy().InitialDelay
	}
	if policy.MaxDelay == 0 {
		policy.MaxDelay = DefaultPolicy().MaxDelay
	}
	if policy.MaxDelay < policy.InitialDelay {
		return Policy{}, errors.New("transaction retry max delay must be at least the initial delay")
	}
	return policy, nil
}

func retryable(err error) bool {
	if occretry.IsOCCError(err) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func wait(ctx context.Context, ceiling time.Duration) error {
	delay := ceiling
	if ceiling > 1 {
		delay = time.Duration(rand.Int63n(int64(ceiling)))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
