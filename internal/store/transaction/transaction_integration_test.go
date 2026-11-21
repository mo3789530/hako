//go:build integration

package transaction

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWithinCommitsAndRetriesOnlyRetryableErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	policy := Policy{MaxRetries: 2, InitialDelay: time.Microsecond, MaxDelay: time.Millisecond}

	value, err := Within(ctx, pool, policy, func(_ context.Context, tx pgx.Tx) (string, error) {
		if _, execErr := tx.Exec(ctx, `SELECT 1`); execErr != nil {
			return "", execErr
		}
		return "committed", nil
	})
	if err != nil || value != "committed" {
		t.Fatalf("successful transaction = (%q, %v)", value, err)
	}

	attempts := 0
	value, err = Within(ctx, pool, policy, func(context.Context, pgx.Tx) (string, error) {
		attempts++
		if attempts < 3 {
			return "", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
		}
		return "retried", nil
	})
	if err != nil || value != "retried" || attempts != 3 {
		t.Fatalf("retryable callback = (%q, %v), attempts=%d", value, err, attempts)
	}

	attempts = 0
	_, err = Within(ctx, pool, policy, func(context.Context, pgx.Tx) (string, error) {
		attempts++
		return "", errors.New("validation failure")
	})
	if err == nil || attempts != 1 {
		t.Fatalf("non-retryable callback error should execute once, attempts=%d err=%v", attempts, err)
	}
}

func TestWithinRejectsInvalidInputsAndStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if _, err := Within(ctx, nil, Policy{}, func(context.Context, pgx.Tx) (int, error) { return 1, nil }); err == nil {
		t.Fatal("nil pool should fail")
	}
	if _, err := Within[int](ctx, pool, Policy{}, nil); err == nil {
		t.Fatal("nil callback should fail")
	}
	if _, err := Within(ctx, pool, Policy{MaxRetries: -1}, func(context.Context, pgx.Tx) (int, error) { return 1, nil }); err == nil {
		t.Fatal("negative retry policy should fail")
	}
	if _, err := Within(ctx, pool, Policy{MaxRetries: 1, InitialDelay: time.Second, MaxDelay: time.Millisecond}, func(context.Context, pgx.Tx) (int, error) { return 1, nil }); err == nil {
		t.Fatal("max delay below initial delay should fail")
	}

	callCtx, stop := context.WithCancel(ctx)
	called := false
	_, err := Within(callCtx, pool, Policy{MaxRetries: 1, InitialDelay: time.Second, MaxDelay: time.Second}, func(context.Context, pgx.Tx) (int, error) {
		called = true
		stop()
		return 0, &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	})
	if !called || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during retry wait = %v, callback called=%t", err, called)
	}
}
