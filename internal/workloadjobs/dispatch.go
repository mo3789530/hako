package workloadjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var (
	ErrScheduleTenantMismatch   = errors.New("Workload schedule Tenant and Run do not match")
	ErrScheduleUnsupportedRun   = errors.New("Workload Run is not eligible for dispatch")
	ErrSchedulePlacementMissing = errors.New("Workload Run has no assigned Resource Plane")
)

// PlacementSelector chooses a Resource Plane for a newly scheduled Run. The
// selected value is written to the Run and attempt in the same transaction as
// its dispatch Outbox event, so a retry cannot move an attempt.
type PlacementSelector interface {
	Select(context.Context, domain.WorkloadRun) (domain.ResourcePlaneID, error)
}

type PlacementSelectorFunc func(context.Context, domain.WorkloadRun) (domain.ResourcePlaneID, error)

func (f PlacementSelectorFunc) Select(ctx context.Context, run domain.WorkloadRun) (domain.ResourcePlaneID, error) {
	return f(ctx, run)
}

type DispatchHandler struct {
	Pool      transaction.Beginner
	Policy    transaction.Policy
	Placement PlacementSelector
	Now       func() time.Time
}

type DispatchCommand struct {
	SchemaVersion    int                    `json:"schema_version"`
	TenantID         domain.TenantID        `json:"tenant_id"`
	WorkloadRunID    domain.WorkloadRunID   `json:"workload_run_id"`
	AttemptID        string                 `json:"attempt_id"`
	Generation       int64                  `json:"generation"`
	ResourcePlane    domain.ResourcePlaneID `json:"resource_plane_id"`
	IdempotencyToken string                 `json:"idempotency_token"`
	TimeoutAt        time.Time              `json:"timeout_at"`
}

// Handle durably hands a scheduler delivery to the Resource Plane. It never
// runs repository code and ignores execution fields because the envelope only
// contains the Tenant and Run identifiers.
func (h *DispatchHandler) Handle(ctx context.Context, request ScheduleRequest) error {
	if h == nil || h.Pool == nil || h.Placement == nil || ctx == nil {
		return errors.New("context, database, and Workload placement selector are required")
	}
	if request.SchemaVersion != 1 || request.TenantID == "" || request.WorkloadRunID == "" {
		return errors.New("Workload schedule envelope is unsupported or incomplete")
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	occurredAt := now().UTC()
	attemptID, err := idgen.New("attempt_")
	if err != nil {
		return fmt.Errorf("generate Workload attempt ID: %w", err)
	}
	eventID, err := idgen.New("evt_")
	if err != nil {
		return fmt.Errorf("generate Workload dispatch event ID: %w", err)
	}
	idempotencyToken, err := idgen.New("token_")
	if err != nil {
		return fmt.Errorf("generate Workload attempt idempotency token: %w", err)
	}
	_, err = transaction.Within(ctx, h.Pool, h.Policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		run, err := workloads.Get(ctx, tx, request.TenantID, request.WorkloadRunID)
		if errors.Is(err, workloads.ErrNotFound) {
			return struct{}{}, ErrScheduleTenantMismatch
		}
		if err != nil {
			return struct{}{}, err
		}
		if isTerminalWorkloadState(run.State) {
			return struct{}{}, nil
		}
		// The concurrent winner has committed the handoff. A duplicate is
		// acknowledged without creating another attempt or dispatch event.
		if run.State == domain.WorkloadProvisioning || run.State == domain.WorkloadRunning {
			return struct{}{}, nil
		}
		if run.DesiredState == domain.WorkloadDesiredCancelled {
			_, err := workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID, RunID: run.ID,
				ExpectedRevision: run.Revision, NextState: domain.WorkloadCancelled, ErrorCode: "cancelled_before_dispatch", OccurredAt: occurredAt})
			return struct{}{}, err
		}
		if run.Kind != domain.WorkloadJob || run.CommitSHA == "" || run.GitHubRepository <= 0 || run.GitHubInstallation <= 0 {
			_, err := workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID, RunID: run.ID,
				ExpectedRevision: run.Revision, NextState: domain.WorkloadFailed, ErrorCode: "invalid_workload_input", OccurredAt: occurredAt})
			return struct{}{}, err
		}
		if !occurredAt.Before(run.TimeoutAt) {
			_, err := workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID, RunID: run.ID,
				ExpectedRevision: run.Revision, NextState: domain.WorkloadTimedOut, ErrorCode: "job_timeout", OccurredAt: occurredAt})
			return struct{}{}, err
		}
		planeID := run.ResourcePlaneID
		if planeID == "" {
			planeID, err = h.Placement.Select(ctx, run)
			if err != nil {
				return struct{}{}, fmt.Errorf("select Workload Resource Plane: %w", err)
			}
		}
		if planeID == "" {
			return struct{}{}, ErrSchedulePlacementMissing
		}
		var generation, attemptNumber int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(generation), 0) + 1, COALESCE(MAX(attempt_number), 0) + 1
			FROM workload_run_attempts WHERE tenant_id = $1 AND workload_run_id = $2`, run.TenantID, run.ID).Scan(&generation, &attemptNumber); err != nil {
			return struct{}{}, fmt.Errorf("allocate Workload attempt generation: %w", err)
		}
		run, err = workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID, RunID: run.ID,
			ExpectedRevision: run.Revision, NextState: domain.WorkloadProvisioning, ResourcePlaneID: planeID, OccurredAt: occurredAt})
		if err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workload_run_attempts
			(id, tenant_id, workload_run_id, generation, attempt_number, resource_plane_id, idempotency_token, state, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'dispatch_pending', $8, $8)`,
			attemptID, run.TenantID, run.ID, generation, attemptNumber, planeID, idempotencyToken, occurredAt); err != nil {
			return struct{}{}, fmt.Errorf("persist Workload attempt: %w", err)
		}
		command := DispatchCommand{SchemaVersion: 1, TenantID: run.TenantID, WorkloadRunID: run.ID,
			AttemptID: attemptID, Generation: generation, ResourcePlane: planeID, IdempotencyToken: idempotencyToken, TimeoutAt: run.TimeoutAt}
		payload, err := json.Marshal(command)
		if err != nil {
			return struct{}{}, fmt.Errorf("encode Workload dispatch command: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
			(id, aggregate_type, aggregate_id, event_type, payload_json, created_at, published_at, attempt)
			VALUES ($1, 'workload_attempt', $2, 'workload_attempt.dispatch', $3, $4, NULL, 0)`,
			eventID, attemptID, string(payload), occurredAt); err != nil {
			return struct{}{}, fmt.Errorf("enqueue Workload dispatch: %w", err)
		}
		return struct{}{}, nil
	})
	return err
}

func isTerminalWorkloadState(state domain.WorkloadRunState) bool {
	switch state {
	case domain.WorkloadSucceeded, domain.WorkloadFailed, domain.WorkloadCancelled, domain.WorkloadTimedOut:
		return true
	default:
		return false
	}
}
