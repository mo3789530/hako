package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	ErrInvalidReport      = errors.New("invalid Workspace Service report")
	ErrReportReplay       = errors.New("Workspace Service report ID was reused with different content")
	ErrStaleReport        = errors.New("stale Workspace Service report")
	serviceReportIDRegexp = regexp.MustCompile(`^[a-f0-9]{48}$`)
	serviceReportHashRE   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type ObservationInput struct {
	ReportID             string
	PayloadSHA256        string
	TenantID             domain.TenantID
	WorkspaceID          domain.WorkspaceID
	ResourcePlaneID      domain.ResourcePlaneID
	MicroVMID            string
	AllocationGeneration int64
	ServiceID            domain.ServiceID
	Revision             int64
	ObservedRevision     int64
	DesiredState         string
	ObservedState        string
	Healthy              bool
	ErrorCode            string
	ObservedAt           time.Time
	ReceivedAt           time.Time
}

// ApplyObservation records the report ID and updates Service state only when
// it still matches the active Workspace Placement, Lambda MicroVM binding,
// desired revision and desired state. Stale results are idempotently stored
// and acknowledged without overwriting newer desired state.
func ApplyObservation(ctx context.Context, tx pgx.Tx, input ObservationInput) (bool, error) {
	if tx == nil || !serviceReportIDRegexp.MatchString(input.ReportID) || !serviceReportHashRE.MatchString(input.PayloadSHA256) ||
		input.TenantID == "" || input.WorkspaceID == "" || input.ResourcePlaneID == "" || input.MicroVMID == "" || input.ServiceID == "" || input.AllocationGeneration < 1 ||
		input.Revision < 1 || input.ObservedRevision < 0 || input.ObservedRevision > input.Revision ||
		input.ObservedAt.IsZero() || input.ReceivedAt.IsZero() || !validObservationState(input) {
		return false, ErrInvalidReport
	}
	var desiredRevision, observedRevision int64
	var desiredState, observedState string
	var bindingMatches bool
	err := tx.QueryRow(ctx, `SELECT s.desired_revision, s.observed_revision, s.desired_state, s.observed_state,
		EXISTS (SELECT 1 FROM resource_bindings rb WHERE rb.workspace_id = w.id AND rb.resource_plane_id = p.resource_plane_id
			AND rb.kind = 'lambda_microvm' AND rb.external_id = $5 AND rb.allocation_generation = $6)
		FROM services s
		JOIN workspaces w ON w.id = s.workspace_id
		JOIN placements p ON p.workspace_id = w.id
		WHERE s.id = $1 AND s.workspace_id = $2 AND w.tenant_id = $3 AND p.resource_plane_id = $4`,
		input.ServiceID, input.WorkspaceID, input.TenantID, input.ResourcePlaneID, input.MicroVMID, input.AllocationGeneration).Scan(
		&desiredRevision, &observedRevision, &desiredState, &observedState, &bindingMatches)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("resolve Workspace Service report binding: %w", err)
	}
	claim, err := tx.Exec(ctx, `INSERT INTO workspace_service_report_events
		(resource_plane_id, report_id, service_id, revision, payload_sha256, observed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (resource_plane_id, report_id) DO NOTHING`,
		input.ResourcePlaneID, input.ReportID, input.ServiceID, input.Revision, input.PayloadSHA256, input.ObservedAt.UTC(), input.ReceivedAt.UTC())
	if err != nil {
		return false, fmt.Errorf("claim Workspace Service report ID: %w", err)
	}
	if claim.RowsAffected() == 0 {
		var priorHash string
		var priorAccepted bool
		if err := tx.QueryRow(ctx, `SELECT payload_sha256, accepted FROM workspace_service_report_events WHERE resource_plane_id = $1 AND report_id = $2`,
			input.ResourcePlaneID, input.ReportID).Scan(&priorHash, &priorAccepted); err != nil {
			return false, fmt.Errorf("resolve Workspace Service report replay: %w", err)
		}
		if priorHash != input.PayloadSHA256 {
			return false, ErrReportReplay
		}
		return priorAccepted, nil
	}
	accepted := bindingMatches && input.Revision == desiredRevision && input.DesiredState == desiredState && input.ObservedRevision >= observedRevision
	if accepted && input.ObservedRevision == observedRevision && observedState != "pending" &&
		(input.ObservedState != observedState || input.Healthy != (observedState == "healthy")) {
		accepted = false
	}
	if accepted && input.ObservedRevision == observedRevision && input.ObservedState == "failed" && observedState != "pending" && observedState != "failed" {
		accepted = false
	}
	if accepted {
		if input.ObservedState == "deleted" {
			var previousGenerations int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_service_runtime_generations
				WHERE service_id = $1 AND resource_plane_id = $2 AND retired = FALSE AND allocation_generation <> $3`,
				input.ServiceID, input.ResourcePlaneID, input.AllocationGeneration).Scan(&previousGenerations); err != nil {
				return false, fmt.Errorf("check prior Workspace Service runtime generations: %w", err)
			}
			if previousGenerations != 0 {
				accepted = false
			}
		}
	}
	if accepted {
		generationTag, err := tx.Exec(ctx, `INSERT INTO workspace_service_runtime_generations
			(service_id, workspace_id, resource_plane_id, allocation_generation, microvm_id, desired_revision, observed_state, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (service_id, resource_plane_id, allocation_generation) DO UPDATE SET
			microvm_id = EXCLUDED.microvm_id, desired_revision = EXCLUDED.desired_revision,
			observed_state = EXCLUDED.observed_state, updated_at = EXCLUDED.updated_at
			WHERE workspace_service_runtime_generations.microvm_id = EXCLUDED.microvm_id
			AND workspace_service_runtime_generations.retired = FALSE
			AND workspace_service_runtime_generations.desired_revision <= EXCLUDED.desired_revision`,
			input.ServiceID, input.WorkspaceID, input.ResourcePlaneID, input.AllocationGeneration, input.MicroVMID,
			input.Revision, input.ObservedState, input.ReceivedAt.UTC())
		if err != nil {
			return false, fmt.Errorf("record Workspace Service runtime generation: %w", err)
		}
		accepted = generationTag.RowsAffected() == 1
	}
	if accepted {
		tag, err := tx.Exec(ctx, `UPDATE services SET observed_state = $2, observed_revision = $3, updated_at = $4
			WHERE id = $1 AND desired_revision = $5 AND desired_state = $6 AND observed_revision <= $3`,
			input.ServiceID, input.ObservedState, input.ObservedRevision, input.ReceivedAt.UTC(), input.Revision, input.DesiredState)
		if err != nil {
			return false, fmt.Errorf("apply Workspace Service observation: %w", err)
		}
		accepted = tag.RowsAffected() == 1
	}
	if accepted && (input.ObservedState == "stopped" || input.ObservedState == "deleted") {
		if err := ReleaseWorkspaceServiceBudget(ctx, tx, input.WorkspaceID, input.ServiceID, input.ReceivedAt.UTC()); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_service_report_events SET accepted = $3 WHERE resource_plane_id = $1 AND report_id = $2`,
		input.ResourcePlaneID, input.ReportID, accepted); err != nil {
		return false, fmt.Errorf("persist Workspace Service report acceptance: %w", err)
	}
	return accepted, nil
}

func validObservationState(input ObservationInput) bool {
	switch input.ObservedState {
	case "healthy":
		return input.DesiredState == "running" && input.Healthy && input.ObservedRevision == input.Revision && input.ErrorCode == ""
	case "unhealthy":
		return input.DesiredState == "running" && !input.Healthy && input.ObservedRevision == input.Revision && input.ErrorCode == ""
	case "stopped":
		return input.DesiredState == "stopped" && !input.Healthy && input.ObservedRevision == input.Revision && input.ErrorCode == ""
	case "deleted":
		return input.DesiredState == "deleted" && !input.Healthy && input.ObservedRevision == input.Revision && input.ErrorCode == ""
	case "failed":
		return !input.Healthy && input.ObservedRevision < input.Revision && input.ErrorCode != ""
	default:
		return false
	}
}
