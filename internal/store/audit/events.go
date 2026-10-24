// Package audit appends actor-oriented audit records in the caller's transaction.
package audit

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

type Event struct {
	TenantID   domain.TenantID
	ActorID    domain.UserID
	ActorType  string
	ActorName  string
	Action     string
	TargetType string
	TargetID   string
	Details    json.RawMessage
	OccurredAt time.Time
}

// Append writes one immutable audit record. Callers should invoke this inside
// the same transaction as the user-visible mutation so a failed audit write
// rolls back the mutation instead of silently losing evidence.
func Append(ctx context.Context, tx pgx.Tx, event Event) error {
	if event.ActorType == "" {
		event.ActorType = "user"
	}
	if tx == nil || event.TenantID == "" || event.ActorType != "user" || event.ActorID == "" || event.ActorName != "" || event.Action == "" || event.TargetType == "" || event.TargetID == "" {
		return errors.New("transaction, tenant, actor, action, and target are required")
	}
	return append(ctx, tx, event)
}

// AppendSystem records an explicit non-user actor in the same transaction as
// its system action. It never attributes automation to a Tenant member.
func AppendSystem(ctx context.Context, tx pgx.Tx, event Event) error {
	if tx == nil || event.TenantID == "" || event.ActorID != "" || event.ActorType != "system" || event.ActorName == "" ||
		event.Action == "" || event.TargetType == "" || event.TargetID == "" {
		return errors.New("transaction, tenant, named system actor, action, and target are required")
	}
	return append(ctx, tx, event)
}

func append(ctx context.Context, tx pgx.Tx, event Event) error {
	if len(event.Details) == 0 {
		event.Details = json.RawMessage(`{}`)
	}
	if !json.Valid(event.Details) {
		return errors.New("audit details must be valid JSON")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	id, err := idgen.New("aud_")
	if err != nil {
		return fmt.Errorf("generate audit event ID: %w", err)
	}
	var actorID *string
	if event.ActorID != "" {
		value := string(event.ActorID)
		actorID = &value
	}
	var actorName *string
	if event.ActorName != "" {
		actorName = &event.ActorName
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, actor_type, actor_name, action, target_type, target_id, details_json, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, event.TenantID, actorID, event.ActorType, actorName, event.Action, event.TargetType, event.TargetID, string(event.Details), event.OccurredAt); err != nil {
		return fmt.Errorf("append audit event %s: %w", event.Action, err)
	}
	return nil
}
