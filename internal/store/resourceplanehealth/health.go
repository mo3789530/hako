// Package resourceplanehealth manages operator-reported Resource Plane health.
package resourceplanehealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
)

const FreshnessTTL = 5 * time.Minute
const MaxReasonLength = 500

var (
	ErrNotFound = errors.New("Resource Plane not found")
	ErrInvalid  = errors.New("invalid Resource Plane health report")
)

type Status string

const (
	Healthy   Status = "healthy"
	Degraded  Status = "degraded"
	Unhealthy Status = "unhealthy"
	Unknown   Status = "unknown"
)

type ReportSource string

const (
	OperatorReport  ReportSource = "operator"
	AutomatedReport ReportSource = "automated"
)

type Health struct {
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
	Status          Status                 `json:"status"`
	EffectiveStatus Status                 `json:"effective_status"`
	Reason          string                 `json:"reason,omitempty"`
	ReportSource    ReportSource           `json:"report_source,omitempty"`
	ReportedAt      *time.Time             `json:"reported_at,omitempty"`
}

func Validate(status Status, reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if status != Healthy && status != Degraded && status != Unhealthy {
		return "", fmt.Errorf("%w: status must be healthy, degraded, or unhealthy", ErrInvalid)
	}
	if len(reason) > MaxReasonLength {
		return "", fmt.Errorf("%w: reason must be at most %d bytes", ErrInvalid, MaxReasonLength)
	}
	for _, char := range reason {
		if unicode.IsControl(char) {
			return "", fmt.Errorf("%w: reason must not contain control characters", ErrInvalid)
		}
	}
	return reason, nil
}

// Get returns the latest report with the same freshness interpretation used
// for new placement. An unreported Plane retains the legacy healthy default.
func Get(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, now time.Time) (Health, error) {
	if tx == nil || resourcePlaneID == "" {
		return Health{}, errors.New("transaction and Resource Plane ID are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var health Health
	var status, reason, source sql.NullString
	var updatedAt sql.NullTime
	err := tx.QueryRow(ctx, `SELECT rp.id, rph.status, rph.reason, rph.report_source, rph.updated_at
		FROM resource_planes rp LEFT JOIN resource_plane_health rph ON rph.resource_plane_id = rp.id
		WHERE rp.id = $1`, resourcePlaneID).Scan(&health.ResourcePlaneID, &status, &reason, &source, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Health{}, ErrNotFound
	}
	if err != nil {
		return Health{}, fmt.Errorf("get Resource Plane health: %w", err)
	}
	if !updatedAt.Valid {
		health.Status = Unknown
		health.EffectiveStatus = Healthy
		return health, nil
	}
	health.Status = Status(status.String)
	health.Reason = reason.String
	health.ReportSource = ReportSource(source.String)
	if health.ReportSource == "" {
		health.ReportSource = OperatorReport
	}
	reportedAt := updatedAt.Time
	health.ReportedAt = &reportedAt
	health.EffectiveStatus = health.Status
	if health.Status == Healthy && updatedAt.Time.Before(now.Add(-FreshnessTTL)) {
		health.EffectiveStatus = Degraded
	}
	return health, nil
}

// Put writes the report and its platform audit entry atomically. The supplied
// reason is deliberately omitted from audit details to avoid copying arbitrary
// operator text into a second durable record.
func Put(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, status Status, reason string, actorID domain.UserID, now time.Time) (Health, error) {
	if tx == nil || resourcePlaneID == "" || actorID == "" {
		return Health{}, errors.New("transaction, Resource Plane ID, and actor are required")
	}
	cleanReason, err := Validate(status, reason)
	if err != nil {
		return Health{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := saveReport(ctx, tx, resourcePlaneID, status, cleanReason, OperatorReport, now); err != nil {
		return Health{}, err
	}
	reportEventID, err := idgen.New("rph_")
	if err != nil {
		return Health{}, fmt.Errorf("generate health report event ID: %w", err)
	}
	if err := insertReportEvent(ctx, tx, reportEventID, resourcePlaneID, status, cleanReason, OperatorReport, now); err != nil {
		return Health{}, err
	}
	auditEventID, err := idgen.New("paud_")
	if err != nil {
		return Health{}, fmt.Errorf("generate platform audit ID: %w", err)
	}
	details, _ := json.Marshal(map[string]string{"status": string(status)})
	if _, err := tx.Exec(ctx, `INSERT INTO platform_audit_events
		(id, actor_user_id, action, target_type, target_id, details_json, occurred_at)
		VALUES ($1, $2, 'resource_plane.health.update', 'resource_plane', $3, $4, $5)`, auditEventID, actorID, resourcePlaneID, string(details), now); err != nil {
		return Health{}, fmt.Errorf("append Resource Plane health audit event: %w", err)
	}
	return Get(ctx, tx, resourcePlaneID, now)
}

// PutAutomated stores a non-human health report. Callers must authenticate and
// authorize the reporting Resource Plane before invoking this persistence API.
func PutAutomated(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, status Status, reason string, now time.Time) (Health, error) {
	if tx == nil || resourcePlaneID == "" {
		return Health{}, errors.New("transaction and Resource Plane ID are required")
	}
	cleanReason, err := Validate(status, reason)
	if err != nil {
		return Health{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := saveReport(ctx, tx, resourcePlaneID, status, cleanReason, AutomatedReport, now); err != nil {
		return Health{}, err
	}
	eventID, err := idgen.New("rph_")
	if err != nil {
		return Health{}, fmt.Errorf("generate automated health report ID: %w", err)
	}
	if err := insertReportEvent(ctx, tx, eventID, resourcePlaneID, status, cleanReason, AutomatedReport, now); err != nil {
		return Health{}, err
	}
	return Get(ctx, tx, resourcePlaneID, now)
}

func saveReport(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, status Status, reason string, source ReportSource, now time.Time) error {
	tag, err := tx.Exec(ctx, `INSERT INTO resource_plane_health (resource_plane_id, status, reason, report_source, updated_at)
		SELECT id, $2, $3, $4, $5 FROM resource_planes WHERE id = $1
		ON CONFLICT (resource_plane_id) DO UPDATE SET status = EXCLUDED.status, reason = EXCLUDED.reason, report_source = EXCLUDED.report_source, updated_at = EXCLUDED.updated_at`, resourcePlaneID, status, reason, source, now)
	if err != nil {
		return fmt.Errorf("save Resource Plane health: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func insertReportEvent(ctx context.Context, tx pgx.Tx, id string, resourcePlaneID domain.ResourcePlaneID, status Status, reason string, source ReportSource, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO resource_plane_health_events
		(id, resource_plane_id, status, report_source, reason, reported_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, resourcePlaneID, status, source, reason, now); err != nil {
		return fmt.Errorf("append Resource Plane health report event: %w", err)
	}
	return nil
}
