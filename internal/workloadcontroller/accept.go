// Package workloadcontroller handles Resource Plane Workload commands without
// accessing Control Plane DSQL. Acceptance is a checkpoint, not execution.
package workloadcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

var ErrInvalidCommand = errors.New("invalid Workload dispatch command")

var commandCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Command struct {
	SchemaVersion    int    `json:"schema_version"`
	TenantID         string `json:"tenant_id"`
	WorkloadRunID    string `json:"workload_run_id"`
	AttemptID        string `json:"attempt_id"`
	Generation       int64  `json:"generation"`
	ResourcePlaneID  string `json:"resource_plane_id"`
	IdempotencyToken string `json:"idempotency_token"`
	CorrelationID    string `json:"correlation_id,omitempty"`
}

// Decode rejects execution parameters: authoritative source and runtime
// configuration must be resolved separately, never trusted from this envelope.
func Decode(data []byte) (Command, error) {
	var c Command
	if len(data) == 0 || len(data) > 64<<10 {
		return c, ErrInvalidCommand
	}
	// encoding/json accepts duplicate keys and case-insensitive field aliases.
	// This trust boundary requires one exact spelling and one value per field.
	keys := json.NewDecoder(bytes.NewReader(data))
	start, err := keys.Token()
	if err != nil || start != json.Delim('{') {
		return Command{}, ErrInvalidCommand
	}
	allowed := map[string]bool{"schema_version": true, "tenant_id": true, "workload_run_id": true, "attempt_id": true, "generation": true, "resource_plane_id": true, "idempotency_token": true, "correlation_id": true}
	seen := make(map[string]bool)
	for keys.More() {
		token, err := keys.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return Command{}, ErrInvalidCommand
		}
		seen[name] = true
		var value json.RawMessage
		if keys.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return Command{}, ErrInvalidCommand
		}
	}
	if _, err := keys.Token(); err != nil {
		return Command{}, ErrInvalidCommand
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return Command{}, ErrInvalidCommand
	}
	var extra any
	if d.Decode(&extra) != io.EOF || c.SchemaVersion != 1 {
		return Command{}, ErrInvalidCommand
	}
	if _, err := workloadjournal.New(c.identity(), time.Now().UTC()); err != nil {
		return Command{}, ErrInvalidCommand
	}
	if c.CorrelationID != "" && !commandCorrelationIDPattern.MatchString(c.CorrelationID) {
		return Command{}, ErrInvalidCommand
	}
	return c, nil
}

func (c Command) identity() workloadjournal.Command {
	return workloadjournal.Command{TenantID: c.TenantID, WorkloadRunID: c.WorkloadRunID,
		AttemptID: c.AttemptID, Generation: c.Generation, ResourcePlaneID: c.ResourcePlaneID,
		IdempotencyToken: c.IdempotencyToken}
}

type Acceptor struct {
	store           workloadjournal.Store
	resourcePlaneID string
	now             func() time.Time
}

func NewAcceptor(store workloadjournal.Store, resourcePlaneID string, now func() time.Time) (*Acceptor, error) {
	if store == nil || resourcePlaneID == "" {
		return nil, ErrInvalidCommand
	}
	if now == nil {
		now = time.Now
	}
	// Reuse journal identity validation for the configured Resource Plane ID.
	if _, err := workloadjournal.New(workloadjournal.Command{TenantID: "validation", WorkloadRunID: "validation",
		AttemptID: "validation", Generation: 1, ResourcePlaneID: resourcePlaneID, IdempotencyToken: "validation"}, now()); err != nil {
		return nil, ErrInvalidCommand
	}
	return &Acceptor{store: store, resourcePlaneID: resourcePlaneID, now: now}, nil
}

// Accept returns only after a durable journal exists with the exact dispatch
// identity. The caller still needs a recovery/launch path before queue wiring.
// Duplicate deliveries retain cancellation and execution/cleanup checkpoints.
func (a *Acceptor) Accept(ctx context.Context, data []byte) (workloadjournal.Snapshot, error) {
	c, err := Decode(data)
	if err != nil || c.ResourcePlaneID != a.resourcePlaneID {
		return workloadjournal.Snapshot{}, ErrInvalidCommand
	}
	j, err := workloadjournal.New(c.identity(), a.now())
	if err != nil {
		return workloadjournal.Snapshot{}, ErrInvalidCommand
	}
	snapshot, err := a.store.Create(ctx, j)
	if errors.Is(err, workloadjournal.ErrConflict) {
		snapshot, err = a.store.Get(ctx, c.TenantID, c.WorkloadRunID, c.AttemptID)
	}
	if err != nil {
		return workloadjournal.Snapshot{}, err
	}
	saved := snapshot.Journal
	if snapshot.ETag == "" || saved.Validate() != nil || saved.TenantID != c.TenantID ||
		saved.WorkloadRunID != c.WorkloadRunID || saved.AttemptID != c.AttemptID ||
		saved.Generation != c.Generation || saved.ResourcePlaneID != c.ResourcePlaneID ||
		saved.IdempotencyToken != c.IdempotencyToken {
		return workloadjournal.Snapshot{}, workloadjournal.ErrConflict
	}
	return snapshot, nil
}
