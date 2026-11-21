package workloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
)

// enqueueCancellation shares the desired-state transaction. Pending Runs have
// no attempt; their scheduler observes cancellation before creating one.
func enqueueCancellation(ctx context.Context, tx pgx.Tx, run domain.WorkloadRun, now time.Time) error {
	var command struct {
		SchemaVersion    int                    `json:"schema_version"`
		TenantID         domain.TenantID        `json:"tenant_id"`
		WorkloadRunID    domain.WorkloadRunID   `json:"workload_run_id"`
		AttemptID        string                 `json:"attempt_id"`
		Generation       int64                  `json:"generation"`
		ResourcePlaneID  domain.ResourcePlaneID `json:"resource_plane_id"`
		IdempotencyToken string                 `json:"idempotency_token"`
		CorrelationID    string                 `json:"correlation_id,omitempty"`
	}
	command.SchemaVersion = 1
	command.TenantID = run.TenantID
	command.WorkloadRunID = run.ID
	command.CorrelationID = run.CorrelationID
	err := tx.QueryRow(ctx, `SELECT id,generation,resource_plane_id,idempotency_token FROM workload_run_attempts
		WHERE tenant_id=$1 AND workload_run_id=$2 ORDER BY generation DESC LIMIT 1`, run.TenantID, run.ID).
		Scan(&command.AttemptID, &command.Generation, &command.ResourcePlaneID, &command.IdempotencyToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load Workload cancellation attempt: %w", err)
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return err
	}
	id, err := idgen.New("evt_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(id,aggregate_type,aggregate_id,event_type,payload_json,created_at,published_at,attempt)
		VALUES($1,'workload_attempt',$2,'workload_attempt.cancel',$3,$4,NULL,0)`, id, command.AttemptID, string(payload), now.UTC())
	if err != nil {
		return fmt.Errorf("enqueue Workload cancellation: %w", err)
	}
	return nil
}
