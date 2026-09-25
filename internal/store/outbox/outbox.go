// Package outbox contains database operations for claiming and acknowledging
// transactional outbox events. Network publication must happen outside these
// transactions.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrLeaseLost = errors.New("outbox event lease was lost")

type Event struct {
	ID            string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	CreatedAt     time.Time
	Attempt       int
	LeaseToken    string
}

// Claim atomically leases up to limit due events. Expired leases can be
// reclaimed after a crashed dispatcher; deliveries are consequently at least
// once and consumers must deduplicate using Event.ID / Operation ID.
func Claim(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, limit int, leaseDuration time.Duration, now time.Time) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("outbox claim limit must be 1-1000")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("outbox lease duration must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	leaseToken, err := idgen.New("lease_")
	if err != nil {
		return nil, err
	}
	leaseExpiry := now.Add(leaseDuration)
	return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]Event, error) {
		rows, err := tx.Query(ctx, `SELECT id, aggregate_type, aggregate_id, event_type, payload_json, created_at, attempt
			FROM outbox_events
			WHERE published_at IS NULL AND (lease_expires_at IS NULL OR lease_expires_at <= $1)
			ORDER BY created_at, id LIMIT $2`, now, limit)
		if err != nil {
			return nil, fmt.Errorf("select pending outbox events: %w", err)
		}
		candidates := make([]Event, 0, limit)
		for rows.Next() {
			var event Event
			var payload string
			if err := rows.Scan(&event.ID, &event.AggregateType, &event.AggregateID, &event.EventType, &payload, &event.CreatedAt, &event.Attempt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan pending outbox event: %w", err)
			}
			event.Payload = []byte(payload)
			candidates = append(candidates, event)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate pending outbox events: %w", err)
		}
		rows.Close()

		claimed := make([]Event, 0, len(candidates))
		for _, event := range candidates {
			tag, err := tx.Exec(ctx, `UPDATE outbox_events
				SET lease_token = $1, lease_expires_at = $2, attempt = attempt + 1
				WHERE id = $3 AND published_at IS NULL AND (lease_expires_at IS NULL OR lease_expires_at <= $4)`, leaseToken, leaseExpiry, event.ID, now)
			if err != nil {
				return nil, fmt.Errorf("lease outbox event %s: %w", event.ID, err)
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			event.Attempt++
			event.LeaseToken = leaseToken
			claimed = append(claimed, event)
		}
		return claimed, nil
	})
}

// MarkPublished acknowledges an event only if the caller still owns its lease.
func MarkPublished(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, event Event, publishedAt time.Time) error {
	if event.ID == "" || event.LeaseToken == "" {
		return errors.New("outbox event ID and lease token are required")
	}
	if publishedAt.IsZero() {
		publishedAt = time.Now().UTC()
	}
	_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		tag, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = $1, lease_token = NULL, lease_expires_at = NULL WHERE id = $2 AND lease_token = $3 AND published_at IS NULL`, publishedAt, event.ID, event.LeaseToken)
		if err != nil {
			return struct{}{}, fmt.Errorf("mark outbox event published: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return struct{}{}, ErrLeaseLost
		}
		return struct{}{}, nil
	})
	return err
}

// ReleaseAfterFailure relinquishes a lease after moving its expiry forward.
// The existing attempt counter is incremented when an event is claimed.
func ReleaseAfterFailure(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, event Event, retryAt time.Time) error {
	if event.ID == "" || event.LeaseToken == "" {
		return errors.New("outbox event ID and lease token are required")
	}
	if retryAt.IsZero() {
		return errors.New("outbox retry time is required")
	}
	_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		tag, err := tx.Exec(ctx, `UPDATE outbox_events SET lease_token = NULL, lease_expires_at = $1 WHERE id = $2 AND lease_token = $3 AND published_at IS NULL`, retryAt, event.ID, event.LeaseToken)
		if err != nil {
			return struct{}{}, fmt.Errorf("release failed outbox event: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return struct{}{}, ErrLeaseLost
		}
		return struct{}{}, nil
	})
	return err
}
