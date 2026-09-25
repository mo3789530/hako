//go:build integration

package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestClaimLeaseRetryAndPublishAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt) VALUES ($1, 'operation', 'op_1', 'operation.requested', $2, $3, NULL, 0)`, "evt_outbox_test", `{}`, now); err != nil {
		t.Fatalf("insert test Outbox event: %v", err)
	}
	policy := transaction.DefaultPolicy()
	claimed, err := Claim(ctx, pool, policy, 10, time.Minute, now)
	if err != nil || len(claimed) != 1 || claimed[0].Attempt != 1 {
		t.Fatalf("claim Outbox event: claimed=%+v error=%v", claimed, err)
	}
	duplicate, err := Claim(ctx, pool, policy, 10, time.Minute, now.Add(30*time.Second))
	if err != nil || len(duplicate) != 0 {
		t.Fatalf("active lease must prevent concurrent claim: claimed=%+v error=%v", duplicate, err)
	}
	retryAt := now.Add(2 * time.Minute)
	if err := ReleaseAfterFailure(ctx, pool, policy, claimed[0], retryAt); err != nil {
		t.Fatalf("release failed event: %v", err)
	}
	tooSoon, err := Claim(ctx, pool, policy, 10, time.Minute, now.Add(90*time.Second))
	if err != nil || len(tooSoon) != 0 {
		t.Fatalf("retry delay must be respected: claimed=%+v error=%v", tooSoon, err)
	}
	retried, err := Claim(ctx, pool, policy, 10, time.Minute, retryAt)
	if err != nil || len(retried) != 1 || retried[0].Attempt != 2 {
		t.Fatalf("event should be reclaimable after backoff: claimed=%+v error=%v", retried, err)
	}
	if err := MarkPublished(ctx, pool, policy, claimed[0], retryAt.Add(time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale lease acknowledgement should fail, got %v", err)
	}
	if err := MarkPublished(ctx, pool, policy, retried[0], retryAt.Add(time.Second)); err != nil {
		t.Fatalf("acknowledge retried event: %v", err)
	}
	remaining, err := Claim(ctx, pool, policy, 10, time.Minute, retryAt.Add(2*time.Minute))
	if err != nil || len(remaining) != 0 {
		t.Fatalf("published event must not be claimed again: claimed=%+v error=%v", remaining, err)
	}
}
