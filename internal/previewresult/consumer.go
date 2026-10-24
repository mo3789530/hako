// Package previewresult applies Resource Plane Preview acknowledgements to the
// Control Plane's generation-fenced lifecycle records.
package previewresult

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrConfiguration = errors.New("Preview result consumer is not configured")

type Consumer struct {
	Pool transaction.Beginner
	Now  func() time.Time
}

// Handle validates SQS routing attributes and applies one result. False means
// the message was valid but its claim was already stale; this is an ackable
// no-op because a newer generation or attempt owns the lifecycle now.
func (c Consumer) Handle(ctx context.Context, payload []byte, claimID, action, resourcePlaneID string) (bool, error) {
	if ctx == nil || ctx.Err() != nil || c.Pool == nil {
		return false, ErrConfiguration
	}
	result, err := previewprotocol.DecodeResult(payload)
	if err != nil {
		return false, err
	}
	if result.ClaimID != claimID || result.Action != action || result.ResourcePlaneID != resourcePlaneID {
		return false, errors.New("Preview result body does not match SQS routing attributes")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	observedAt := now().UTC()
	if result.CompletedAt.After(observedAt.Add(5 * time.Minute)) {
		return false, errors.New("Preview result completion time is too far in the future")
	}
	applied, err := transaction.Within(ctx, c.Pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return previews.ApplyRuntimeResult(ctx, tx, result, observedAt)
	})
	if err != nil {
		return false, fmt.Errorf("apply Preview Resource Plane result: %w", err)
	}
	return applied, nil
}
