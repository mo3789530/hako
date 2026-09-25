package transaction

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/awslabs/aurora-dsql-connectors/go/pgx/occretry"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNormalizeRetryPolicy(t *testing.T) {
	if got, err := normalize(Policy{}); err != nil || got != DefaultPolicy() {
		t.Fatalf("zero policy = %+v, %v", got, err)
	}
	for _, policy := range []Policy{
		{MaxRetries: -1},
		{InitialDelay: -time.Millisecond},
		{MaxDelay: -time.Millisecond},
		{InitialDelay: time.Second, MaxDelay: time.Millisecond},
	} {
		if _, err := normalize(policy); err == nil {
			t.Errorf("invalid policy %+v should be rejected", policy)
		}
	}
	got, err := normalize(Policy{MaxRetries: 2, InitialDelay: time.Millisecond})
	if err != nil || got.MaxDelay != DefaultPolicy().MaxDelay || got.MaxRetries != 2 {
		t.Fatalf("partially specified policy = %+v, %v", got, err)
	}
}

func TestRetryableAndWaitCancellation(t *testing.T) {
	if !retryable(&pgconn.PgError{Code: occretry.ErrorCodeMutation}) || !retryable(&pgconn.PgError{Code: "40001"}) ||
		!retryable(&pgconn.PgError{Code: "40P01"}) || retryable(&pgconn.PgError{Code: "23505"}) || retryable(errors.New("ordinary error")) {
		t.Fatal("retry classification mismatch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := wait(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on cancelled context = %v", err)
	}
}
