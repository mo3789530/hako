package operations

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type unusedBeginner struct{}

func (unusedBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, nil
}

func TestFailStaleValidatesInputsBeforeOpeningTransaction(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	valid := unusedBeginner{}
	tests := []struct {
		name   string
		pool   transaction.Beginner
		cutoff time.Time
		now    time.Time
		limit  int
	}{
		{name: "nil pool", cutoff: now.Add(-time.Minute), now: now, limit: 1},
		{name: "missing cutoff", pool: valid, now: now, limit: 1},
		{name: "cutoff after now", pool: valid, cutoff: now.Add(time.Second), now: now, limit: 1},
		{name: "zero now", pool: valid, cutoff: now.Add(-time.Minute), limit: 1},
		{name: "zero limit", pool: valid, cutoff: now.Add(-time.Minute), now: now},
		{name: "too large limit", pool: valid, cutoff: now.Add(-time.Minute), now: now, limit: MaxStaleOperationBatch + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := FailStale(context.Background(), test.pool, transaction.Policy{}, test.cutoff, test.now, test.limit); err == nil {
				t.Fatal("invalid inputs should be rejected")
			}
		})
	}
}
