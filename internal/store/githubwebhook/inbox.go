// Package githubwebhook stores authenticated GitHub webhook deliveries in the
// Control Plane inbox. Normalized events are stored alongside the raw
// authenticated delivery so downstream processing never reparses GitHub JSON.
package githubwebhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrDeliveryIDConflict = githubwebhook.ErrDeliveryIDConflict

type Inbox struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
}

// Record persists an authenticated delivery and returns true only when this
// call inserted it. Retries with the same ID and payload are acknowledged as
// duplicates; reusing an ID for different bytes is rejected.
func (i Inbox) Record(ctx context.Context, delivery githubwebhook.VerifiedDelivery, receivedAt time.Time) (bool, error) {
	if i.Pool == nil {
		return false, errors.New("GitHub webhook inbox database is required")
	}
	if delivery.DeliveryID == "" || delivery.Event == "" || len(delivery.Payload) == 0 {
		return false, errors.New("verified GitHub webhook delivery is incomplete")
	}
	normalized, err := githubwebhook.Normalize(delivery)
	if err != nil {
		return false, fmt.Errorf("normalize GitHub webhook delivery: %w", err)
	}
	normalizedJSON, err := json.Marshal(normalized)
	if err != nil {
		return false, fmt.Errorf("encode normalized GitHub event: %w", err)
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	digest := sha256.Sum256(delivery.Payload)
	payloadHash := hex.EncodeToString(digest[:])
	return transaction.Within(ctx, i.Pool, i.Policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		tag, err := tx.Exec(ctx, `INSERT INTO github_webhook_deliveries
			(delivery_id, event_type, action, payload_json, payload_sha256, received_at, processing_status, event_schema_version, normalized_event_json)
			VALUES ($1, $2, $3, $4, $5, $6, 'received', $7, $8)
			ON CONFLICT (delivery_id) DO NOTHING`, delivery.DeliveryID, delivery.Event, delivery.Action, string(delivery.Payload), payloadHash, receivedAt, githubwebhook.RepositoryEventSchemaVersion, string(normalizedJSON))
		if err != nil {
			return false, fmt.Errorf("insert GitHub webhook delivery: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return true, nil
		}
		var existingHash string
		var schemaVersion int
		if err := tx.QueryRow(ctx, `SELECT payload_sha256, event_schema_version FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&existingHash, &schemaVersion); err != nil {
			return false, fmt.Errorf("read existing GitHub webhook delivery: %w", err)
		}
		if existingHash != payloadHash {
			return false, ErrDeliveryIDConflict
		}
		if schemaVersion == 0 {
			if _, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries
				SET event_schema_version = $2, normalized_event_json = $3
				WHERE delivery_id = $1 AND payload_sha256 = $4 AND event_schema_version = 0`,
				delivery.DeliveryID, githubwebhook.RepositoryEventSchemaVersion, string(normalizedJSON), payloadHash); err != nil {
				return false, fmt.Errorf("backfill normalized GitHub event: %w", err)
			}
		}
		return false, nil
	})
}
