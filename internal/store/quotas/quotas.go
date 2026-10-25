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
	DefaultQueuedWorkloadRuns          int16 = 5
	MaxQueuedWorkloadRuns              int16 = 5
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
	MaxWorkspaceComputeMilliCPU        int64 = 80_000
	MaxWorkspaceComputeMemoryMiB       int64 = 163_840
)

var ErrBelowCurrentUsage = errors.New("Tenant quota cannot be lower than current reserved usage")
var ErrInvalidLimits = errors.New("Tenant quota limits are invalid")
var ErrArtifactQuotaExceeded = errors.New("Tenant artifact storage quota exceeded")
var ErrPreviewQuotaExceeded = errors.New("Tenant Preview quota exceeded")
var ErrWorkloadQueueQuotaExceeded = errors.New("Tenant queued Workload quota exceeded")
var ErrWorkspaceComputeQuotaExceeded = errors.New("Tenant Workspace compute quota exceeded")
var ErrComputeProfileMissing = errors.New("Workspace Runtime Class compute profile is missing")

type Limits struct {
	TenantID                    domain.TenantID `json:"tenant_id"`
	Workspaces                  int16           `json:"workspaces_limit"`
	ConcurrentWorkloadRuns      int16           `json:"workload_runs_limit"`
	QueuedWorkloadRuns          int16           `json:"queued_workload_runs_limit"`
	PreviewRuns                 int16           `json:"preview_runs_limit"`
	PreviewLifetimeSeconds      int64           `json:"preview_lifetime_seconds_limit"`
	ArtifactBytes               int64           `json:"artifact_bytes_limit"`
	WorkspaceServiceMemoryBytes int64           `json:"service_memory_bytes_limit"`
	WorkspaceServiceMilliCPU    int64           `json:"service_milli_cpu_limit"`
	WorkspaceComputeMilliCPU    int64           `json:"workspace_compute_milli_cpu_limit"`
	WorkspaceComputeMemoryMiB   int64           `json:"workspace_compute_memory_mib_limit"`
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
	limits := Limits{TenantID: tenantID, Workspaces: DefaultWorkspaces, ConcurrentWorkloadRuns: DefaultConcurrentWorkloadRuns, QueuedWorkloadRuns: DefaultQueuedWorkloadRuns,
		PreviewRuns: DefaultPreviewRuns, PreviewLifetimeSeconds: DefaultPreviewLifetimeSeconds,
		ArtifactBytes: DefaultArtifactBytes, WorkspaceServiceMemoryBytes: DefaultWorkspaceServiceMemoryBytes,
		WorkspaceServiceMilliCPU: DefaultWorkspaceServiceMilliCPU}
	err := tx.QueryRow(ctx, `SELECT workspace_limit, concurrent_workload_run_limit, queued_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, workspace_compute_milli_cpu_limit, workspace_compute_memory_mib_limit, revision, COALESCE(updated_by, ''), updated_at
		FROM tenant_quota_limits WHERE tenant_id = $1`, tenantID).Scan(
		&limits.Workspaces, &limits.ConcurrentWorkloadRuns, &limits.QueuedWorkloadRuns, &limits.PreviewRuns, &limits.PreviewLifetimeSeconds, &limits.ArtifactBytes,
		&limits.WorkspaceServiceMemoryBytes, &limits.WorkspaceServiceMilliCPU, &limits.WorkspaceComputeMilliCPU, &limits.WorkspaceComputeMemoryMiB,
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
	err := tx.QueryRow(ctx, `INSERT INTO tenant_quota_limits (tenant_id, revision, updated_at)
		VALUES ($1, 1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET revision = tenant_quota_limits.revision + 1
		RETURNING workspace_limit, concurrent_workload_run_limit, queued_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, workspace_compute_milli_cpu_limit, workspace_compute_memory_mib_limit, revision, COALESCE(updated_by, ''), updated_at`,
		tenantID, now.UTC()).Scan(
		&limits.Workspaces, &limits.ConcurrentWorkloadRuns, &limits.QueuedWorkloadRuns, &limits.PreviewRuns, &limits.PreviewLifetimeSeconds, &limits.ArtifactBytes,
		&limits.WorkspaceServiceMemoryBytes, &limits.WorkspaceServiceMilliCPU, &limits.WorkspaceComputeMilliCPU, &limits.WorkspaceComputeMemoryMiB,
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
	current, err := Read(ctx, tx, tenantID)
	if err != nil {
		return Limits{}, err
	}
	queuedWorkloadRuns := current.QueuedWorkloadRuns
	if queuedWorkloadRuns > workloadRuns {
		queuedWorkloadRuns = workloadRuns
	}
	return SetWithQueueAndPreview(ctx, tx, tenantID, actorID, workspaces, workloadRuns, queuedWorkloadRuns,
		previewRuns, previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU, now)
}

// SetWithQueueAndPreview is retained for callers which preserve the current
// optional Workspace compute budgets.
func SetWithQueueAndPreview(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, actorID domain.UserID, workspaces, workloadRuns, queuedWorkloadRuns, previewRuns int16, previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU int64, now time.Time) (Limits, error) {
	current, err := Read(ctx, tx, tenantID)
	if err != nil {
		return Limits{}, err
	}
	return SetWithComputeQueueAndPreview(ctx, tx, tenantID, actorID, workspaces, workloadRuns, queuedWorkloadRuns, previewRuns,
		previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU, current.WorkspaceComputeMilliCPU, current.WorkspaceComputeMemoryMiB, now)
}

// SetWithComputeQueueAndPreview updates Tenant limits after checking active
// reservations, queue backlog, and profile-based Workspace compute demand.
func SetWithComputeQueueAndPreview(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, actorID domain.UserID, workspaces, workloadRuns, queuedWorkloadRuns, previewRuns int16, previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU, workspaceComputeMilliCPU, workspaceComputeMemoryMiB int64, now time.Time) (Limits, error) {
	if tx == nil || tenantID == "" || actorID == "" || now.IsZero() {
		return Limits{}, errors.New("transaction, Tenant, actor, and time are required")
	}
	if workspaces < 1 || workspaces > MaxWorkspaces || workloadRuns < 1 || workloadRuns > MaxConcurrentWorkloadRuns ||
		queuedWorkloadRuns < 1 || queuedWorkloadRuns > MaxQueuedWorkloadRuns || queuedWorkloadRuns > workloadRuns ||
		previewRuns < 1 || previewRuns > MaxPreviewRuns || previewLifetimeSeconds < 300 || previewLifetimeSeconds > MaxPreviewLifetimeSeconds ||
		artifactBytes < 1 || artifactBytes > MaxArtifactBytes || serviceMemoryBytes < 1 || serviceMemoryBytes > MaxWorkspaceServiceMemoryBytes ||
		serviceMilliCPU < 1 || serviceMilliCPU > MaxWorkspaceServiceMilliCPU ||
		workspaceComputeMilliCPU < 0 || workspaceComputeMilliCPU > MaxWorkspaceComputeMilliCPU ||
		workspaceComputeMemoryMiB < 0 || workspaceComputeMemoryMiB > MaxWorkspaceComputeMemoryMiB {
		return Limits{}, ErrInvalidLimits
	}
	var limits Limits
	limits.TenantID = tenantID
	err := tx.QueryRow(ctx, `INSERT INTO tenant_quota_limits
		(tenant_id, workspace_limit, concurrent_workload_run_limit, queued_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		 workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, workspace_compute_milli_cpu_limit, workspace_compute_memory_mib_limit, revision, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1, $12, $13)
		ON CONFLICT (tenant_id) DO UPDATE SET
		workspace_limit = EXCLUDED.workspace_limit,
		concurrent_workload_run_limit = EXCLUDED.concurrent_workload_run_limit,
		queued_workload_run_limit = EXCLUDED.queued_workload_run_limit,
		preview_run_limit = EXCLUDED.preview_run_limit,
		preview_lifetime_seconds = EXCLUDED.preview_lifetime_seconds,
		artifact_bytes_limit = EXCLUDED.artifact_bytes_limit,
		workspace_service_memory_bytes_limit = EXCLUDED.workspace_service_memory_bytes_limit,
		workspace_service_milli_cpu_limit = EXCLUDED.workspace_service_milli_cpu_limit,
		workspace_compute_milli_cpu_limit = EXCLUDED.workspace_compute_milli_cpu_limit,
		workspace_compute_memory_mib_limit = EXCLUDED.workspace_compute_memory_mib_limit,
		revision = tenant_quota_limits.revision + 1,
		updated_by = EXCLUDED.updated_by,
		updated_at = EXCLUDED.updated_at
		RETURNING workspace_limit, concurrent_workload_run_limit, queued_workload_run_limit, preview_run_limit, preview_lifetime_seconds, artifact_bytes_limit,
		workspace_service_memory_bytes_limit, workspace_service_milli_cpu_limit, workspace_compute_milli_cpu_limit, workspace_compute_memory_mib_limit, revision, COALESCE(updated_by, ''), updated_at`,
		tenantID, workspaces, workloadRuns, queuedWorkloadRuns, previewRuns, previewLifetimeSeconds, artifactBytes, serviceMemoryBytes, serviceMilliCPU, workspaceComputeMilliCPU, workspaceComputeMemoryMiB, actorID, now.UTC()).Scan(
		&limits.Workspaces, &limits.ConcurrentWorkloadRuns, &limits.QueuedWorkloadRuns, &limits.PreviewRuns, &limits.PreviewLifetimeSeconds, &limits.ArtifactBytes,
		&limits.WorkspaceServiceMemoryBytes, &limits.WorkspaceServiceMilliCPU, &limits.WorkspaceComputeMilliCPU, &limits.WorkspaceComputeMemoryMiB,
		&limits.Revision, &limits.UpdatedBy, &limits.UpdatedAt)
	if err != nil {
		return Limits{}, fmt.Errorf("update Tenant quota limits: %w", err)
	}
	limits.TenantID = tenantID
	var activeWorkspaces, activeWorkloadRuns, queuedWorkloadRunCount, activePreviewRuns int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces w LEFT JOIN workspace_status s ON s.workspace_id = w.id
		WHERE w.tenant_id = $1 AND COALESCE(s.observed_state, 'pending') <> 'deleted'`, tenantID).Scan(&activeWorkspaces); err != nil {
		return Limits{}, fmt.Errorf("count Tenant Workspace reservations: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1
		AND state NOT IN ('succeeded','failed','cancelled','timed_out')`, tenantID).Scan(&activeWorkloadRuns); err != nil {
		return Limits{}, fmt.Errorf("count Tenant Workload reservations: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1 AND state IN ('pending','queued')`, tenantID).Scan(&queuedWorkloadRunCount); err != nil {
		return Limits{}, fmt.Errorf("count queued Tenant Workload Runs: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM workload_runs WHERE tenant_id=$1 AND kind='preview' AND state NOT IN ('succeeded','failed','cancelled','timed_out')) +
		(SELECT COUNT(*) FROM repository_previews WHERE tenant_id=$1 AND state IN ('awaiting_runtime','running','cleanup_pending'))`, tenantID).Scan(&activePreviewRuns); err != nil {
		return Limits{}, fmt.Errorf("count Tenant Preview reservations: %w", err)
	}
	var previewLifetimeExceeded bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM workload_runs WHERE tenant_id=$1 AND kind='preview'
			AND state NOT IN ('succeeded','failed','cancelled','timed_out') AND timeout_seconds > $2)
		OR EXISTS (SELECT 1 FROM repository_previews WHERE tenant_id=$1 AND desired_state='present'
			AND state IN ('awaiting_runtime','running') AND expires_at > created_at + ($2 * interval '1 second'))`,
		tenantID, previewLifetimeSeconds).Scan(&previewLifetimeExceeded); err != nil {
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
	computeCPU, computeMemory, missingProfiles, err := ReadWorkspaceComputeUsage(ctx, tx, tenantID)
	if err != nil {
		return Limits{}, err
	}
	if (workspaceComputeMilliCPU > 0 || workspaceComputeMemoryMiB > 0) && missingProfiles > 0 {
		return Limits{}, ErrComputeProfileMissing
	}
	if (workspaceComputeMilliCPU > 0 && computeCPU > workspaceComputeMilliCPU) || (workspaceComputeMemoryMiB > 0 && computeMemory > workspaceComputeMemoryMiB) {
		return Limits{}, ErrBelowCurrentUsage
	}
	if activeWorkspaces > int64(workspaces) || activeWorkloadRuns > int64(workloadRuns) || queuedWorkloadRunCount > int64(queuedWorkloadRuns) || activePreviewRuns > int64(previewRuns) ||
		previewLifetimeExceeded || artifactBytesCommitted > artifactBytes || overServiceLimit {
		return Limits{}, ErrBelowCurrentUsage
	}
	return limits, nil
}

// ReadWorkspaceComputeUsage sums configured Runtime Class reservation weights
// for active Workspace allocations. It reports unknown profiles separately so
// callers never treat missing measurements as zero demand.
func ReadWorkspaceComputeUsage(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) (cpuMilliCPU, memoryMiB, missingProfiles int64, err error) {
	if tx == nil || tenantID == "" {
		return 0, 0, 0, errors.New("transaction and Tenant ID are required")
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(r.cpu_millicores), 0), COALESCE(SUM(r.memory_mib), 0),
		COUNT(*) FILTER (WHERE r.cpu_millicores IS NULL OR r.memory_mib IS NULL)
		FROM workspaces w LEFT JOIN workspace_status s ON s.workspace_id = w.id
		LEFT JOIN resource_plane_reservations r ON r.workspace_id = w.id
		WHERE w.tenant_id = $1 AND COALESCE(s.observed_state, 'pending') <> 'deleted'`, tenantID).Scan(&cpuMilliCPU, &memoryMiB, &missingProfiles)
	if err != nil {
		err = fmt.Errorf("read Tenant Workspace compute reservations: %w", err)
	}
	return
}

// EnforceWorkspaceComputeAdmission checks profile-based Tenant compute budgets
// after the new Workspace reservation has been recorded in the same transaction.
func EnforceWorkspaceComputeAdmission(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) error {
	limits, err := Read(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if limits.WorkspaceComputeMilliCPU == 0 && limits.WorkspaceComputeMemoryMiB == 0 {
		return nil
	}
	cpu, memory, missing, err := ReadWorkspaceComputeUsage(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if missing > 0 {
		return ErrComputeProfileMissing
	}
	if (limits.WorkspaceComputeMilliCPU > 0 && cpu > limits.WorkspaceComputeMilliCPU) ||
		(limits.WorkspaceComputeMemoryMiB > 0 && memory > limits.WorkspaceComputeMemoryMiB) {
		return ErrWorkspaceComputeQuotaExceeded
	}
	return nil
}
