package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func TestOutboxMutationValidation(t *testing.T) {
	ctx := t.Context()
	validPool := poolThatMustNotBeUsed{}
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"zero claim limit", func() error {
			_, err := Claim(ctx, validPool, transaction.Policy{}, 0, time.Second, time.Now())
			return err
		}},
		{"oversized claim limit", func() error {
			_, err := Claim(ctx, validPool, transaction.Policy{}, 1001, time.Second, time.Now())
			return err
		}},
		{"non-positive lease", func() error { _, err := Claim(ctx, validPool, transaction.Policy{}, 1, 0, time.Now()); return err }},
		{"publish requires lease", func() error { return MarkPublished(ctx, validPool, transaction.Policy{}, Event{ID: "evt"}, time.Now()) }},
		{"release requires lease", func() error {
			return ReleaseAfterFailure(ctx, validPool, transaction.Policy{}, Event{ID: "evt"}, time.Now())
		}},
		{"release requires retry time", func() error {
			return ReleaseAfterFailure(ctx, validPool, transaction.Policy{}, Event{ID: "evt", LeaseToken: "lease"}, time.Time{})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil || errors.Is(err, ErrLeaseLost) {
				t.Fatalf("expected input validation error, got %v", err)
			}
		})
	}
}

// pgx.Tx is deliberately not required by these invalid-input paths. Using a
// nil Beginner proves validation happens before any database operation.
type poolThatMustNotBeUsed struct{}

func (poolThatMustNotBeUsed) Begin(context.Context) (pgx.Tx, error) {
	panic("validation must run before Begin")
}
