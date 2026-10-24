// Package quotas provides Tenant-scoped configurable admission limits.
package quotas

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

const (
	DefaultWorkspaces                  int16 = 10
	MaxWorkspaces                      int16 = 10
	DefaultConcurrentWorkloadRuns      int16 = 5
	MaxConcurrentWorkloadRuns          int16 = 5
	DefaultPreviewRuns                 int16 = 2
	MaxPreviewRuns                     int16 = 5
	DefaultPreviewLifetimeSeconds      int64 = 24 * 60 * 60
	MaxPreviewLifetimeSeconds          int64 = 24 * 60 * 60
	DefaultArtifactBytes               int64 = 10 << 30
	MaxArtifactBytes                   int64 = 100 << 30
	MaxArtifactBytesPerWorkloadRun     int64 = 100 << 20
	DefaultWorkspaceServiceMemoryBytes int64 = 2 << 30
	MaxWorkspaceServiceMemoryBytes     int64 = 2 << 30
	DefaultWorkspaceServiceMilliCPU    int64 = 1500
	MaxWorkspaceServiceMilliCPU        int64 = 1500
)

var ErrBelowCurrentUsage = errors.New("Tenant quota cannot be lower than current reserved usage")
var ErrInvalidLimits = errors.New("Tenant quota limits are invalid")
var ErrArtifactQuotaExceeded = errors.New("Tenant artifact storage quota exceeded")
var ErrPreviewQuotaExceeded = errors.New("Tenant Preview quota exceeded")

type Limits struct {
	TenantID                    domain.TenantID `json:"tenant_id"`
	Workspaces                  int16           `json:"workspaces_limit"`
	ConcurrentWorkloadRuns      int16           `json:"workload_runs_limit"`
	PreviewRuns                 int16           `json:"preview_runs_limit"`
	PreviewLifetimeSeconds      int64           `json:"preview_lifetime_seconds_limit"`
	ArtifactBytes               int64           `json:"artifact_bytes_limit"`
	WorkspaceServiceMemoryBytes int64           `json:"service_memory_bytes_limit"`
	WorkspaceServiceMilliCPU    int64           `json:"service_milli_cpu_limit"`
	Revision                    int64           `json:"revision"`
	UpdatedBy                   domain.UserID   `json:"updated_by,omitempty"`
	UpdatedAt                   *time.Time      `json:"updated_at,omitempty"`
}

// Read returns an explicit Tenant override, or the documented platform
// defaults when no Owner/Admin has configured one.
func Read(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) (Limits, error) {
	if tx == nil || tenantID == "" {
		return Limits{}, errors.New("transaction and Tenant ID are required")
	}
	limits := Limits{TenantID: tenantID, Workspaces: DefaultWorkspaces, ConcurrentWorkloadRuns: DefaultConcurrentWorkloadRuns,
		PreviewRuns: DefaultPreviewRuns, PreviewLifetimeSeconds: DefaultPreviewLifetimeSeconds,
		ArtifactBytes: DefaultArtifactBytes, WorkspaceServiceMemoryBytes: DefaultWorkspaceServiceMemoryBytes,
		WorkspaceServiceMilliCPU: DefaultWorkspaceServiceMilliCPU}
	err := tx.QueryRow(ctx, `SELECT workspace_limit, concurrent_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, revision, COALESCE(updated_by, ''), updated_at
		FROM tenant_quota_limits WHERE tenant_id = $1`, tenantID).Scan(
		&limits.Workspaces, &limits.ConcurrentWorkloadRuns, &limits.PreviewRuns, &limits.PreviewLifetimeSeconds, &limits.ArtifactBytes,
		&limits.WorkspaceServiceMemoryBytes, &limits.WorkspaceServiceMilliCPU,
		&limits.Revision, &limits.UpdatedBy, &limits.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return limits, nil
	}
	if err != nil {
		return Limits{}, fmt.Errorf("read Tenant quota limits: %w", err)
	}
	return limits, nil
}

// AdvanceAdmissionGuard creates or updates the Tenant quota row. Every quota
// admission writes this row before reading usage, so concurrent admissions and
// Owner/Admin quota changes serialize through Aurora DSQL OCC.
func AdvanceAdmissionGuard(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, now time.Time) (Limits, error) {
	if tx == nil || tenantID == "" || now.IsZero() {
		return Limits{}, errors.New("transaction, Tenant ID, and time are required")
	}
	var limits Limits
	limits.TenantID = tenantID
	err := tx.QueryRow(ctx, `INSERT INTO tenant_quota_limits
		(tenant_id, workspace_limit, concurrent_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		 workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, revision, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, $9)
		ON CONFLICT (tenant_id) DO UPDATE SET revision = tenant_quota_limits.revision + 1
		RETURNING workspace_limit, concurrent_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, revision, COALESCE(updated_by, ''), updated_at`,
		tenantID, DefaultWorkspaces, DefaultConcurrentWorkloadRuns, DefaultPreviewRuns, DefaultPreviewLifetimeSeconds, DefaultArtifactBytes,
		DefaultWorkspaceServiceMemoryBytes, DefaultWorkspaceServiceMilliCPU, now.UTC()).Scan(
		&limits.Workspaces, &limits.ConcurrentWorkloadRuns, &limits.PreviewRuns, &limits.PreviewLifetimeSeconds, &limits.ArtifactBytes,
		&limits.WorkspaceServiceMemoryBytes, &limits.WorkspaceServiceMilliCPU,
		&limits.Revision, &limits.UpdatedBy, &limits.UpdatedAt)
	if err != nil {
		return Limits{}, fmt.Errorf("advance Tenant quota OCC guard: %w", err)
	}
	limits.TenantID = tenantID
	return limits, nil
}

// Set updates configured limits only when existing reservations fit. The
// caller must authorize Tenant Owner/Admin and audit within this transaction.
func Set(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, actorID domain.UserID, workspaces, workloadRuns int16, artifactBytes, serviceMemoryBytes, serviceMilliCPU int64, now time.Time) (Limits, error) {
	current, err := Read(ctx, tx, tenantID)
	if err != nil {
		return Limits{}, err
	}
	return SetWithPreview(ctx, tx, tenantID, actorID, workspaces, workloadRuns, current.PreviewRuns,
		current.PreviewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU, now)
}

// SetWithPreview updates Tenant limits, including a dedicated Preview admission
// budget and maximum Preview lifetime, only when active reservations fit.
func SetWithPreview(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, actorID domain.UserID, workspaces, workloadRuns, previewRuns int16, previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU int64, now time.Time) (Limits, error) {
	if tx == nil || tenantID == "" || actorID == "" || now.IsZero() {
		return Limits{}, errors.New("transaction, Tenant, actor, and time are required")
	}
	if workspaces < 1 || workspaces > MaxWorkspaces || workloadRuns < 1 || workloadRuns > MaxConcurrentWorkloadRuns ||
		previewRuns < 1 || previewRuns > MaxPreviewRuns || previewLifetimeSeconds < 300 || previewLifetimeSeconds > MaxPreviewLifetimeSeconds ||
		artifactBytes < 1 || artifactBytes > MaxArtifactBytes || serviceMemoryBytes < 1 || serviceMemoryBytes > MaxWorkspaceServiceMemoryBytes ||
		serviceMilliCPU < 1 || serviceMilliCPU > MaxWorkspaceServiceMilliCPU {
		return Limits{}, ErrInvalidLimits
	}
	var limits Limits
	limits.TenantID = tenantID
	err := tx.QueryRow(ctx, `INSERT INTO tenant_quota_limits
		(tenant_id, workspace_limit, concurrent_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		 workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, revision, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, $9, $10)
		ON CONFLICT (tenant_id) DO UPDATE SET
		workspace_limit = EXCLUDED.workspace_limit,
		concurrent_workload_run_limit = EXCLUDED.concurrent_workload_run_limit,
		preview_run_limit = EXCLUDED.preview_run_limit,
		preview_lifetime_seconds = EXCLUDED.preview_lifetime_seconds,
		artifact_bytes_limit = EXCLUDED.artifact_bytes_limit,
		workspace_service_memory_bytes_limit = EXCLUDED.workspace_service_memory_bytes_limit,
		workspace_service_milli_cpu_limit = EXCLUDED.workspace_service_milli_cpu_limit,
		revision = tenant_quota_limits.revision + 1,
		updated_by = EXCLUDED.updated_by,
		updated_at = EXCLUDED.updated_at
		RETURNING workspace_limit, concurrent_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, revision, COALESCE(updated_by, ''), updated_at`,
		tenantID, workspaces, workloadRuns, previewRuns, previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU, actorID, now.UTC()).Scan(
		&limits.Workspaces, &limits.ConcurrentWorkloadRuns, &limits.PreviewRuns, &limits.PreviewLifetimeSeconds, &limits.ArtifactBytes,
		&limits.WorkspaceServiceMemoryBytes, &limits.WorkspaceServiceMilliCPU,
		&limits.Revision, &limits.UpdatedBy, &limits.UpdatedAt)
	if err != nil {
		return Limits{}, fmt.Errorf("update Tenant quota limits: %w", err)
	}
	limits.TenantID = tenantID
	var activeWorkspaces, activeWorkloadRuns, activePreviewRuns int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces w LEFT JOIN workspace_status s ON s.workspace_id = w.id
		WHERE w.tenant_id = $1 AND COALESCE(s.observed_state, 'pending') <> 'deleted'`, tenantID).Scan(&activeWorkspaces); err != nil {
		return Limits{}, fmt.Errorf("count Tenant Workspace reservations: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1
		AND state NOT IN ('succeeded','failed','cancelled','timed_out')`, tenantID).Scan(&activeWorkloadRuns); err != nil {
		return Limits{}, fmt.Errorf("count Tenant Workload reservations: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1 AND kind = 'preview'
		AND state NOT IN ('succeeded','failed','cancelled','timed_out')`, tenantID).Scan(&activePreviewRuns); err != nil {
		return Limits{}, fmt.Errorf("count Tenant Preview reservations: %w", err)
	}
	var previewLifetimeExceeded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workload_runs WHERE tenant_id=$1 AND kind='preview'
		AND state NOT IN ('succeeded','failed','cancelled','timed_out') AND timeout_seconds > $2)`, tenantID, previewLifetimeSeconds).Scan(&previewLifetimeExceeded); err != nil {
		return Limits{}, fmt.Errorf("check active Tenant Preview lifetime: %w", err)
	}
	var artifactBytesCommitted int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COALESCE(SUM(size_bytes), 0) FROM workload_artifacts WHERE tenant_id = $1) +
		(SELECT COALESCE(SUM(reserved_bytes), 0) FROM workload_artifact_quota_reservations WHERE tenant_id = $1)`, tenantID).Scan(&artifactBytesCommitted); err != nil {
		return Limits{}, fmt.Errorf("read Tenant artifact storage usage: %w", err)
	}
	var overServiceLimit bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM workspace_service_quota_reservations r JOIN workspaces w ON w.id = r.workspace_id
		WHERE w.tenant_id = $1 GROUP BY r.workspace_id
		HAVING SUM(r.memory_bytes) > $2 OR SUM(r.milli_cpu) > $3)`, tenantID, serviceMemoryBytes, serviceMilliCPU).Scan(&overServiceLimit); err != nil {
		return Limits{}, fmt.Errorf("check Tenant Workspace Service reservations: %w", err)
	}
	if activeWorkspaces > int64(workspaces) || activeWorkloadRuns > int64(workloadRuns) || activePreviewRuns > int64(previewRuns) ||
		previewLifetimeExceeded || artifactBytesCommitted > artifactBytes || overServiceLimit {
		return Limits{}, ErrBelowCurrentUsage
	}
	return limits, nil
}
