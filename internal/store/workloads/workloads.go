// Package workloads stores short-lived Job, Agent, and Preview run state.
// Workload runs have their own lifecycle and event stream; Workspace
// Operations remain tied to long-lived Workspaces.
package workloads

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/store/quotas"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const (
	DefaultTimeout             = 30 * time.Minute
	MaxTimeout                 = 24 * time.Hour
	MaxExpireBatch             = 100
	MaxConcurrentRunsPerTenant = int(quotas.MaxConcurrentWorkloadRuns)
)

var (
	ErrNotFound             = errors.New("Workload Run not found")
	ErrIdempotencyConflict  = errors.New("Workload Run idempotency key conflicts with another request")
	ErrTransitionConflict   = errors.New("Workload Run revision or state does not match")
	ErrRepositoryInactive   = errors.New("Workload Run Repository is not registered and active for Tenant")
	ErrQuotaExceeded        = errors.New("Tenant concurrent Workload Run quota exceeded")
	ErrPreviewQuotaExceeded = quotas.ErrPreviewQuotaExceeded
	commitSHAPattern        = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	correlationIDPattern    = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	errorCodePattern        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

const runColumns = `id, tenant_id, kind, COALESCE(requested_by, ''), COALESCE(github_installation_id, 0),
COALESCE(correlation_id, ''), COALESCE(github_repository_id, 0), COALESCE(github_integration_id, ''), COALESCE(github_config_revision, 0), COALESCE(github_credential_generation, 0), COALESCE(commit_sha, ''), ref, trigger_event, trigger_branch, runtime_class, trust_level,
desired_state, state, COALESCE(resource_plane_id, ''), idempotency_key, request_hash, timeout_seconds,
timeout_at, started_at, completed_at, COALESCE(error_code, ''), revision, created_at, updated_at`

// CreateOrGet inserts an immutable run request or returns the existing run for
// the same Tenant/idempotency key. Append creation events in the same caller
// transaction. The caller must authorize Tenant/repository ownership first.
func CreateOrGet(ctx context.Context, tx pgx.Tx, run domain.WorkloadRun) (domain.WorkloadRun, bool, error) {
	if tx == nil {
		return domain.WorkloadRun{}, false, errors.New("transaction is required")
	}
	now := run.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	run.CreatedAt, run.UpdatedAt = now.UTC(), now.UTC()
	if run.TimeoutSeconds == 0 {
		if run.TimeoutAt.IsZero() {
			run.TimeoutSeconds = int64(DefaultTimeout / time.Second)
		} else {
			duration := run.TimeoutAt.Sub(run.CreatedAt)
			if duration%time.Second != 0 {
				return domain.WorkloadRun{}, false, errors.New("Workload Run timeout must use whole seconds")
			}
			run.TimeoutSeconds = int64(duration / time.Second)
		}
	}
	if run.TimeoutSeconds < 1 || run.TimeoutSeconds > int64(MaxTimeout/time.Second) {
		return domain.WorkloadRun{}, false, fmt.Errorf("Workload Run timeout must be between 1 second and %s", MaxTimeout)
	}
	deadline := run.CreatedAt.Add(time.Duration(run.TimeoutSeconds) * time.Second)
	if !run.TimeoutAt.IsZero() && !run.TimeoutAt.Equal(deadline) {
		return domain.WorkloadRun{}, false, errors.New("Workload Run timeout_at does not match timeout_seconds")
	}
	run.TimeoutAt = deadline
	if err := validateRun(run); err != nil {
		return domain.WorkloadRun{}, false, err
	}
	if run.DesiredState != "" && run.DesiredState != domain.WorkloadDesiredRunning || run.State != "" && run.State != domain.WorkloadPending {
		return domain.WorkloadRun{}, false, errors.New("new Workload Run must start pending with desired state running")
	}
	run.DesiredState = domain.WorkloadDesiredRunning
	run.State = domain.WorkloadPending
	run.Revision = 1
	legacyRequest := struct {
		TenantID           domain.TenantID
		Kind               domain.WorkloadKind
		RequestedBy        domain.UserID
		GitHubInstallation int64
		GitHubRepository   int64
		CommitSHA          string
		Ref                string
		RuntimeClass       string
		TrustLevel         domain.WorkloadTrustLevel
		ResourcePlaneID    domain.ResourcePlaneID
		TimeoutSeconds     int64
	}{run.TenantID, run.Kind, run.RequestedBy, run.GitHubInstallation, run.GitHubRepository, run.CommitSHA, run.Ref, run.RuntimeClass, run.TrustLevel, run.ResourcePlaneID, run.TimeoutSeconds}
	requestHash, err := idempotency.Fingerprint(legacyRequest)
	if run.TriggerEvent != "" && err == nil {
		requestHash, err = idempotency.Fingerprint(struct {
			TenantID           domain.TenantID
			Kind               domain.WorkloadKind
			RequestedBy        domain.UserID
			GitHubInstallation int64
			GitHubRepository   int64
			CommitSHA          string
			Ref                string
			RuntimeClass       string
			TrustLevel         domain.WorkloadTrustLevel
			ResourcePlaneID    domain.ResourcePlaneID
			TimeoutSeconds     int64
			TriggerEvent       string
			TriggerBranch      string
		}{run.TenantID, run.Kind, run.RequestedBy, run.GitHubInstallation, run.GitHubRepository, run.CommitSHA, run.Ref,
			run.RuntimeClass, run.TrustLevel, run.ResourcePlaneID, run.TimeoutSeconds, run.TriggerEvent, run.TriggerBranch})
	}
	if err != nil {
		return domain.WorkloadRun{}, false, fmt.Errorf("fingerprint Workload Run request: %w", err)
	}
	run.RequestHash = requestHash
	if existing, err := GetByIdempotencyKey(ctx, tx, run.TenantID, run.IdempotencyKey); err == nil {
		if existing.RequestHash != requestHash {
			return domain.WorkloadRun{}, false, ErrIdempotencyConflict
		}
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return domain.WorkloadRun{}, false, err
	}
	if run.GitHubRepository > 0 {
		var available bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM tenant_github_repositories r
			JOIN github_app_installation_bindings b ON b.tenant_id = r.tenant_id AND b.installation_id = r.installation_id
			JOIN tenant_github_installations i ON i.tenant_id = b.tenant_id AND i.installation_id = b.installation_id
			WHERE r.tenant_id = $1 AND r.installation_id = $2 AND r.github_repository_id = $3
			AND i.status = 'active' AND i.suspended_at IS NULL
		)`, run.TenantID, run.GitHubInstallation, run.GitHubRepository).Scan(&available); err != nil {
			return domain.WorkloadRun{}, false, fmt.Errorf("authorize Workload Run Repository: %w", err)
		}
		if !available {
			return domain.WorkloadRun{}, false, ErrRepositoryInactive
		}
		if err := resolveRunGitHubIntegration(ctx, tx, &run); err != nil {
			return domain.WorkloadRun{}, false, err
		}
	}
	var requestedBy, correlationID, commitSHA, resourcePlaneID any
	if run.RequestedBy != "" {
		requestedBy = run.RequestedBy
	}
	if run.CorrelationID != "" {
		correlationID = run.CorrelationID
	}
	if run.CommitSHA != "" {
		commitSHA = run.CommitSHA
	}
	if run.ResourcePlaneID != "" {
		resourcePlaneID = run.ResourcePlaneID
	}
	var integrationID, configRevision, credentialGeneration any
	if run.GitHubIntegrationID != "" {
		integrationID, configRevision, credentialGeneration = run.GitHubIntegrationID, run.GitHubConfigRevision, run.GitHubCredentialGeneration
	}
	var installationID, repositoryID any
	if run.GitHubInstallation != 0 {
		installationID, repositoryID = run.GitHubInstallation, run.GitHubRepository
	}
	tag, err := tx.Exec(ctx, `INSERT INTO workload_runs
		(id, tenant_id, kind, requested_by, github_installation_id, correlation_id, github_repository_id, github_integration_id, github_config_revision, github_credential_generation, commit_sha, ref, trigger_event, trigger_branch,
		 runtime_class, trust_level, desired_state, state, resource_plane_id, idempotency_key, request_hash,
		 timeout_seconds, timeout_at, revision, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, 'running', 'pending', $17, $18, $19, $20, $21, 1, $22, $22)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		run.ID, run.TenantID, run.Kind, requestedBy, installationID, correlationID, repositoryID, integrationID, configRevision, credentialGeneration, commitSHA, run.Ref,
		run.TriggerEvent, run.TriggerBranch, run.RuntimeClass, run.TrustLevel, resourcePlaneID, run.IdempotencyKey, requestHash, run.TimeoutSeconds, run.TimeoutAt, run.CreatedAt)
	if err != nil {
		return domain.WorkloadRun{}, false, fmt.Errorf("create Workload Run: %w", err)
	}
	created := tag.RowsAffected() == 1
	stored, err := GetByIdempotencyKey(ctx, tx, run.TenantID, run.IdempotencyKey)
	if err != nil {
		return domain.WorkloadRun{}, false, err
	}
	if stored.RequestHash != requestHash {
		return domain.WorkloadRun{}, false, ErrIdempotencyConflict
	}
	if created {
		if err := reserveConcurrentRunSlot(ctx, tx, stored); err != nil {
			return domain.WorkloadRun{}, false, err
		}
		if err := reserveArtifactQuota(ctx, tx, stored); err != nil {
			return domain.WorkloadRun{}, false, err
		}
		if err := appendEvent(ctx, tx, stored, "workload.pending", run.CreatedAt); err != nil {
			return domain.WorkloadRun{}, false, err
		}
		if err := syncGitHubCheck(ctx, tx, stored, run.CreatedAt); err != nil {
			return domain.WorkloadRun{}, false, err
		}
	}
	return stored, created, nil
}

// reserveArtifactQuota keeps enough Tenant capacity for the bounded redacted
// log artifact before execution can upload it. The reservation is converted
// into actual artifact bytes when the result is persisted, or released when a
// terminal result has no artifact.
func reserveArtifactQuota(ctx context.Context, tx pgx.Tx, run domain.WorkloadRun) error {
	limits, err := quotas.Read(ctx, tx, run.TenantID)
	if err != nil {
		return err
	}
	if run.Kind == domain.WorkloadPreview && run.TimeoutSeconds > limits.PreviewLifetimeSeconds {
		return ErrPreviewQuotaExceeded
	}
	// Repair reservations left behind by older versions or interrupted
	// terminal processing before counting Tenant usage. Admission already
	// advanced the Tenant quota OCC guard, so cleanup is serialized with quota
	// updates and other Workload admissions.
	if _, err := tx.Exec(ctx, `DELETE FROM workload_artifact_quota_reservations q USING workload_runs r
		WHERE q.tenant_id = r.tenant_id AND q.workload_run_id = r.id
		AND r.tenant_id = $1 AND r.state IN ('succeeded','failed','cancelled','timed_out')`, run.TenantID); err != nil {
		return fmt.Errorf("remove stale terminal Workload artifact reservations: %w", err)
	}
	var committedBytes int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COALESCE(SUM(size_bytes), 0) FROM workload_artifacts WHERE tenant_id = $1) +
		(SELECT COALESCE(SUM(reserved_bytes), 0) FROM workload_artifact_quota_reservations WHERE tenant_id = $1)`, run.TenantID).Scan(&committedBytes); err != nil {
		return fmt.Errorf("read Tenant artifact quota before Workload admission: %w", err)
	}
	if quotas.MaxArtifactBytesPerWorkloadRun > limits.ArtifactBytes-committedBytes {
		return quotas.ErrArtifactQuotaExceeded
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_artifact_quota_reservations
		(tenant_id, workload_run_id, reserved_bytes, created_at) VALUES ($1,$2,$3,$4)`,
		run.TenantID, run.ID, quotas.MaxArtifactBytesPerWorkloadRun, run.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("reserve Tenant artifact quota for Workload Run: %w", err)
	}
	return nil
}

// reserveConcurrentRunSlot atomically reserves one of the configured Tenant
// slots. It advances the shared Tenant quota OCC guard before counting active
// runs, and the unique slot index fences concurrent claims.
func reserveConcurrentRunSlot(ctx context.Context, tx pgx.Tx, run domain.WorkloadRun) error {
	if tx == nil || run.TenantID == "" || run.ID == "" {
		return errors.New("Tenant and Workload Run are required for quota reservation")
	}
	limits, err := quotas.AdvanceAdmissionGuard(ctx, tx, run.TenantID, run.CreatedAt)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workload_quota_slots q USING workload_runs r
		WHERE q.tenant_id = r.tenant_id AND q.workload_run_id = r.id
		AND r.tenant_id = $1 AND r.state IN ('succeeded','failed','cancelled','timed_out')`, run.TenantID); err != nil {
		return fmt.Errorf("remove stale terminal Workload quota reservations: %w", err)
	}
	var active int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1
		AND state NOT IN ('succeeded','failed','cancelled','timed_out')`, run.TenantID).Scan(&active); err != nil {
		return fmt.Errorf("count active Tenant Workload Runs: %w", err)
	}
	var queued int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1 AND state IN ('pending','queued')`, run.TenantID).Scan(&queued); err != nil {
		return fmt.Errorf("count queued Tenant Workload Runs: %w", err)
	}
	if queued > int64(limits.QueuedWorkloadRuns) {
		return quotas.ErrWorkloadQueueQuotaExceeded
	}
	if active > int64(limits.ConcurrentWorkloadRuns) {
		return ErrQuotaExceeded
	}
	if run.Kind == domain.WorkloadPreview {
		var activePreviews int64
		if err := tx.QueryRow(ctx, `SELECT
			(SELECT COUNT(*) FROM workload_runs WHERE tenant_id = $1 AND kind = 'preview' AND state NOT IN ('succeeded','failed','cancelled','timed_out')) +
			(SELECT COUNT(*) FROM repository_previews WHERE tenant_id=$1 AND state IN ('awaiting_runtime','running','cleanup_pending'))`, run.TenantID).Scan(&activePreviews); err != nil {
			return fmt.Errorf("count active Tenant Preview Runs: %w", err)
		}
		if activePreviews > int64(limits.PreviewRuns) {
			return ErrPreviewQuotaExceeded
		}
	}
	for slot := int32(1); slot <= int32(limits.ConcurrentWorkloadRuns); slot++ {
		var reserved int32
		err := tx.QueryRow(ctx, `INSERT INTO workload_quota_slots (tenant_id, slot, workload_run_id, created_at)
			VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id,slot) DO NOTHING RETURNING slot`,
			run.TenantID, slot, run.ID, run.CreatedAt).Scan(&reserved)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("reserve Tenant Workload quota slot: %w", err)
		}
	}
	return ErrQuotaExceeded
}

func validateRun(run domain.WorkloadRun) error {
	if run.ID == "" || run.TenantID == "" || run.RuntimeClass == "" || run.IdempotencyKey == "" {
		return errors.New("Workload Run ID, Tenant, runtime class, and idempotency key are required")
	}
	if err := idempotency.ValidateKey(run.IdempotencyKey); err != nil {
		return err
	}
	if run.CorrelationID != "" && !correlationIDPattern.MatchString(run.CorrelationID) {
		return errors.New("Workload Run correlation ID is invalid")
	}
	if run.Kind != domain.WorkloadJob && run.Kind != domain.WorkloadAgent && run.Kind != domain.WorkloadPreview {
		return errors.New("Workload Run kind must be job, agent, or preview")
	}
	if run.TrustLevel != domain.WorkloadUntrusted && run.TrustLevel != domain.WorkloadTrusted && run.TrustLevel != domain.WorkloadPrivileged {
		return errors.New("Workload Run trust level must be untrusted, trusted, or privileged")
	}
	if len(run.RuntimeClass) > 100 || len(run.Ref) > 1024 || strings.ContainsAny(run.Ref, "\r\n\x00") {
		return errors.New("Workload Run runtime class or ref exceeds its limit")
	}
	if (run.TriggerEvent == "") != (run.TriggerBranch == "") ||
		(run.TriggerEvent != "" && run.TriggerEvent != "push" && run.TriggerEvent != "pull_request" && run.TriggerEvent != "issue_comment") ||
		!validTriggerBranch(run.TriggerBranch) {
		return errors.New("Workload Run trigger event or branch is invalid")
	}
	if (run.GitHubInstallation == 0) != (run.GitHubRepository == 0) || run.GitHubInstallation < 0 || run.GitHubRepository < 0 {
		return errors.New("GitHub Installation and Repository IDs must both be positive or both be absent")
	}
	if (run.GitHubIntegrationID == "") != (run.GitHubConfigRevision == 0) || (run.GitHubIntegrationID == "") != (run.GitHubCredentialGeneration == 0) || run.GitHubConfigRevision < 0 || run.GitHubCredentialGeneration < 0 {
		return errors.New("GitHub integration identity requires a positive revision and credential generation")
	}
	if run.GitHubRepository > 0 && !commitSHAPattern.MatchString(run.CommitSHA) {
		return errors.New("repository Workload Run requires a lowercase 40- or 64-character commit SHA")
	}
	if run.CommitSHA != "" && !commitSHAPattern.MatchString(run.CommitSHA) {
		return errors.New("Workload Run commit SHA is invalid")
	}
	if run.TimeoutSeconds < 1 || run.TimeoutSeconds > int64(MaxTimeout/time.Second) ||
		!run.TimeoutAt.Equal(run.CreatedAt.Add(time.Duration(run.TimeoutSeconds)*time.Second)) {
		return fmt.Errorf("Workload Run timeout must be between 1 second and %s", MaxTimeout)
	}
	return nil
}

func validTriggerBranch(branch string) bool {
	if branch == "" {
		return true
	}
	if len(branch) > 255 || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "@{") {
		return false
	}
	for _, char := range branch {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}

// resolveRunGitHubIntegration pins an active Tenant-owned App to a new Run
// based on the persisted Installation binding. A Tenant with an active owned
// integration never silently falls back to shared credentials when that
// Installation is not explicitly bound.
func resolveRunGitHubIntegration(ctx context.Context, tx pgx.Tx, run *domain.WorkloadRun) error {
	if run == nil || tx == nil || run.GitHubInstallation <= 0 || run.TenantID == "" {
		return ErrRepositoryInactive
	}
	err := tx.QueryRow(ctx, `SELECT i.id, i.config_revision, i.credential_generation
		FROM tenant_github_app_installation_bindings b
		JOIN tenant_github_integrations i ON i.tenant_id = b.tenant_id AND i.id = b.integration_id
		WHERE b.tenant_id = $1 AND b.installation_id = $2 AND i.status = 'active'`, run.TenantID, run.GitHubInstallation).
		Scan(&run.GitHubIntegrationID, &run.GitHubConfigRevision, &run.GitHubCredentialGeneration)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("resolve Workload Run GitHub App identity: %w", err)
	}
	var activeOwned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_github_integrations WHERE tenant_id = $1 AND status = 'active')`, run.TenantID).Scan(&activeOwned); err != nil {
		return fmt.Errorf("check active Tenant GitHub App state: %w", err)
	}
	if activeOwned {
		return ErrRepositoryInactive
	}
	return nil
}

func Get(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, error) {
	if tx == nil || tenantID == "" || runID == "" {
		return domain.WorkloadRun{}, errors.New("transaction, Tenant ID, and Workload Run ID are required")
	}
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM workload_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WorkloadRun{}, ErrNotFound
	}
	if err != nil {
		return domain.WorkloadRun{}, fmt.Errorf("get Workload Run: %w", err)
	}
	if err := loadPipelineResults(ctx, tx, &run); err != nil {
		return domain.WorkloadRun{}, err
	}
	return run, nil
}

func loadPipelineResults(ctx context.Context, tx pgx.Tx, run *domain.WorkloadRun) error {
	if tx == nil || run == nil || run.TenantID == "" || run.ID == "" {
		return errors.New("transaction and Workload Run are required to load pipeline results")
	}
	rows, err := tx.Query(ctx, `SELECT pipeline_name, status, exit_code, COALESCE(error_code,'')
		FROM workload_pipeline_results WHERE tenant_id=$1 AND workload_run_id=$2
		AND generation=(SELECT MAX(generation) FROM workload_pipeline_results WHERE tenant_id=$1 AND workload_run_id=$2)
		ORDER BY pipeline_name`, run.TenantID, run.ID)
	if err != nil {
		return fmt.Errorf("list Workload pipeline results: %w", err)
	}
	defer rows.Close()
	run.PipelineResults = make([]domain.WorkloadPipelineResult, 0)
	for rows.Next() {
		var result domain.WorkloadPipelineResult
		if err := rows.Scan(&result.Name, &result.Status, &result.ExitCode, &result.ErrorCode); err != nil {
			return fmt.Errorf("scan Workload pipeline result: %w", err)
		}
		run.PipelineResults = append(run.PipelineResults, result)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate Workload pipeline results: %w", err)
	}
	return nil
}

// List returns Tenant-scoped Workload Runs. A non-empty requestedBy restricts
// the result to one requester; callers must authorize Tenant membership and
// decide whether to pass that filter.
func List(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, requestedBy domain.UserID, limit, offset int) ([]domain.WorkloadRun, int64, error) {
	if tx == nil || tenantID == "" || limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		return nil, 0, errors.New("transaction, Tenant ID, and valid Workload Run pagination are required")
	}
	var total int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs
		WHERE tenant_id = $1 AND ($2 = '' OR requested_by = $2)`, tenantID, requestedBy).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count Workload Runs: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM workload_runs
		WHERE tenant_id = $1 AND ($2 = '' OR requested_by = $2)
		ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4`, tenantID, requestedBy, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list Workload Runs: %w", err)
	}
	defer rows.Close()
	runs := make([]domain.WorkloadRun, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan Workload Run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate Workload Runs: %w", err)
	}
	return runs, total, nil
}

func GetByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, key string) (domain.WorkloadRun, error) {
	if tx == nil || tenantID == "" || key == "" {
		return domain.WorkloadRun{}, errors.New("transaction, Tenant ID, and idempotency key are required")
	}
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM workload_runs WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WorkloadRun{}, ErrNotFound
	}
	if err != nil {
		return domain.WorkloadRun{}, fmt.Errorf("get Workload Run by idempotency key: %w", err)
	}
	if err := loadPipelineResults(ctx, tx, &run); err != nil {
		return domain.WorkloadRun{}, err
	}
	return run, nil
}

type TransitionInput struct {
	TenantID         domain.TenantID
	RunID            domain.WorkloadRunID
	ExpectedRevision int64
	NextState        domain.WorkloadRunState
	ResourcePlaneID  domain.ResourcePlaneID
	ErrorCode        string
	OccurredAt       time.Time
}

func Transition(ctx context.Context, tx pgx.Tx, input TransitionInput) (domain.WorkloadRun, error) {
	if tx == nil || input.TenantID == "" || input.RunID == "" || input.ExpectedRevision <= 0 || input.OccurredAt.IsZero() {
		return domain.WorkloadRun{}, errors.New("transaction, Tenant, Workload Run, positive revision, and timestamp are required")
	}
	if !validTransition(input.NextState) {
		return domain.WorkloadRun{}, errors.New("invalid Workload Run target state")
	}
	if input.ErrorCode != "" && !errorCodePattern.MatchString(input.ErrorCode) {
		return domain.WorkloadRun{}, errors.New("Workload Run error code must be a short identifier")
	}
	current, err := Get(ctx, tx, input.TenantID, input.RunID)
	if err != nil {
		return domain.WorkloadRun{}, err
	}
	if current.Revision != input.ExpectedRevision || !transitionAllowed(current.State, input.NextState) {
		return domain.WorkloadRun{}, ErrTransitionConflict
	}
	if input.ResourcePlaneID != "" && current.ResourcePlaneID != "" && input.ResourcePlaneID != current.ResourcePlaneID {
		return domain.WorkloadRun{}, errors.New("Workload Run placement cannot be changed after assignment")
	}
	startedAt, completedAt := current.StartedAt, current.CompletedAt
	if input.NextState == domain.WorkloadRunning && startedAt == nil {
		at := input.OccurredAt.UTC()
		startedAt = &at
	}
	if terminal(input.NextState) {
		at := input.OccurredAt.UTC()
		completedAt = &at
	}
	desired := current.DesiredState
	if input.NextState == domain.WorkloadCancelled || input.NextState == domain.WorkloadTimedOut {
		desired = domain.WorkloadDesiredCancelled
	}
	if input.ResourcePlaneID != "" {
		current.ResourcePlaneID = input.ResourcePlaneID
	}
	current, err = scanRun(tx.QueryRow(ctx, `UPDATE workload_runs SET desired_state = $1, state = $2,
		resource_plane_id = NULLIF($3, ''), started_at = $4, completed_at = $5, error_code = NULLIF($6, ''),
		revision = revision + 1, updated_at = $7
		WHERE tenant_id = $8 AND id = $9 AND revision = $10 AND state = $11
		RETURNING `+runColumns,
		desired, input.NextState, current.ResourcePlaneID, startedAt, completedAt, input.ErrorCode,
		input.OccurredAt.UTC(), input.TenantID, input.RunID, input.ExpectedRevision, current.State))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WorkloadRun{}, ErrTransitionConflict
	}
	if err != nil {
		return domain.WorkloadRun{}, fmt.Errorf("transition Workload Run: %w", err)
	}
	if err := appendEvent(ctx, tx, current, "workload."+string(input.NextState), input.OccurredAt); err != nil {
		return domain.WorkloadRun{}, err
	}
	if err := syncGitHubCheck(ctx, tx, current, input.OccurredAt); err != nil {
		return domain.WorkloadRun{}, err
	}
	if terminal(input.NextState) {
		if _, err := tx.Exec(ctx, `DELETE FROM workload_quota_slots WHERE tenant_id = $1 AND workload_run_id = $2`, input.TenantID, input.RunID); err != nil {
			return domain.WorkloadRun{}, fmt.Errorf("release terminal Workload quota reservation: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workload_artifact_quota_reservations WHERE tenant_id = $1 AND workload_run_id = $2`, input.TenantID, input.RunID); err != nil {
			return domain.WorkloadRun{}, fmt.Errorf("release terminal artifact quota reservation: %w", err)
		}
	}
	return current, nil
}

func RequestCancel(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID, occurredAt time.Time) (domain.WorkloadRun, error) {
	current, err := Get(ctx, tx, tenantID, runID)
	if err != nil {
		return domain.WorkloadRun{}, err
	}
	if terminal(current.State) {
		return current, nil
	}
	if current.DesiredState == domain.WorkloadDesiredCancelled {
		return current, nil
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	current, err = scanRun(tx.QueryRow(ctx, `UPDATE workload_runs SET desired_state = 'cancelled', revision = revision + 1, updated_at = $1
		WHERE tenant_id = $2 AND id = $3 AND revision = $4 AND state = $5
		RETURNING `+runColumns, occurredAt.UTC(), tenantID, runID, current.Revision, current.State))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WorkloadRun{}, ErrTransitionConflict
	}
	if err != nil {
		return domain.WorkloadRun{}, fmt.Errorf("request Workload Run cancellation: %w", err)
	}
	if err := appendEvent(ctx, tx, current, "workload.cancel_requested", occurredAt); err != nil {
		return domain.WorkloadRun{}, err
	}
	if err := enqueueCancellation(ctx, tx, current, occurredAt); err != nil {
		return domain.WorkloadRun{}, err
	}
	return current, nil
}

func ListEvents(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID) ([]domain.WorkloadRunEvent, error) {
	if tx == nil || tenantID == "" || runID == "" {
		return nil, errors.New("transaction, Tenant ID, and Workload Run ID are required")
	}
	rows, err := tx.Query(ctx, `SELECT workload_run_id, sequence, event_type, created_at FROM workload_run_events
		WHERE tenant_id = $1 AND workload_run_id = $2 ORDER BY sequence`, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list Workload Run events: %w", err)
	}
	defer rows.Close()
	events := make([]domain.WorkloadRunEvent, 0)
	for rows.Next() {
		var event domain.WorkloadRunEvent
		if err := rows.Scan(&event.RunID, &event.Sequence, &event.Type, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan Workload Run event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Workload Run events: %w", err)
	}
	return events, nil
}

type ExpirePool interface {
	transaction.Beginner
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type ExpireStats struct{ Claimed, TimedOut, Skipped int }

func ExpireDue(ctx context.Context, pool ExpirePool, policy transaction.Policy, now time.Time, limit int) (ExpireStats, error) {
	var stats ExpireStats
	if pool == nil || now.IsZero() || limit < 1 || limit > MaxExpireBatch {
		return stats, errors.New("database, current time, and batch limit between 1 and 100 are required")
	}
	rows, err := pool.Query(ctx, `SELECT tenant_id, id, revision FROM workload_runs
		WHERE completed_at IS NULL AND timeout_at <= $1 ORDER BY timeout_at, id LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return stats, fmt.Errorf("select timed-out Workload Runs: %w", err)
	}
	type candidate struct {
		tenantID domain.TenantID
		runID    domain.WorkloadRunID
		revision int64
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.tenantID, &item.runID, &item.revision); err != nil {
			rows.Close()
			return stats, fmt.Errorf("scan timed-out Workload Run: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("read timed-out Workload Runs: %w", err)
	}
	rows.Close()
	for _, item := range candidates {
		stats.Claimed++
		_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			return Transition(ctx, tx, TransitionInput{TenantID: item.tenantID, RunID: item.runID,
				ExpectedRevision: item.revision, NextState: domain.WorkloadTimedOut, OccurredAt: now.UTC()})
		})
		if errors.Is(err, ErrTransitionConflict) {
			stats.Skipped++
			continue
		}
		if err != nil {
			return stats, fmt.Errorf("expire Workload Run %s: %w", item.runID, err)
		}
		stats.TimedOut++
	}
	return stats, nil
}

func validTransition(state domain.WorkloadRunState) bool {
	switch state {
	case domain.WorkloadQueued, domain.WorkloadProvisioning, domain.WorkloadRunning,
		domain.WorkloadSucceeded, domain.WorkloadFailed, domain.WorkloadCancelled, domain.WorkloadTimedOut:
		return true
	default:
		return false
	}
}

func transitionAllowed(from, to domain.WorkloadRunState) bool {
	switch from {
	case domain.WorkloadPending:
		return to == domain.WorkloadQueued || to == domain.WorkloadProvisioning || to == domain.WorkloadFailed || to == domain.WorkloadCancelled || to == domain.WorkloadTimedOut
	case domain.WorkloadQueued:
		return to == domain.WorkloadProvisioning || to == domain.WorkloadFailed || to == domain.WorkloadCancelled || to == domain.WorkloadTimedOut
	case domain.WorkloadProvisioning:
		return to == domain.WorkloadRunning || to == domain.WorkloadFailed || to == domain.WorkloadCancelled || to == domain.WorkloadTimedOut
	case domain.WorkloadRunning:
		return to == domain.WorkloadSucceeded || to == domain.WorkloadFailed || to == domain.WorkloadCancelled || to == domain.WorkloadTimedOut
	default:
		return false
	}
}

func terminal(state domain.WorkloadRunState) bool {
	return state == domain.WorkloadSucceeded || state == domain.WorkloadFailed || state == domain.WorkloadCancelled || state == domain.WorkloadTimedOut
}

func appendEvent(ctx context.Context, tx pgx.Tx, run domain.WorkloadRun, eventType string, occurredAt time.Time) error {
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM workload_run_events WHERE workload_run_id = $1`, run.ID).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate Workload Run event sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_run_events (tenant_id, workload_run_id, sequence, event_type, created_at)
		VALUES ($1, $2, $3, $4, $5)`, run.TenantID, run.ID, sequence, eventType, occurredAt.UTC()); err != nil {
		return fmt.Errorf("append Workload Run event: %w", err)
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner) (domain.WorkloadRun, error) {
	var run domain.WorkloadRun
	var requestedBy, commitSHA, resourcePlaneID string
	err := row.Scan(&run.ID, &run.TenantID, &run.Kind, &requestedBy, &run.GitHubInstallation, &run.CorrelationID,
		&run.GitHubRepository, &run.GitHubIntegrationID, &run.GitHubConfigRevision, &run.GitHubCredentialGeneration, &commitSHA, &run.Ref, &run.TriggerEvent, &run.TriggerBranch, &run.RuntimeClass, &run.TrustLevel, &run.DesiredState,
		&run.State, &resourcePlaneID, &run.IdempotencyKey, &run.RequestHash, &run.TimeoutSeconds, &run.TimeoutAt, &run.StartedAt,
		&run.CompletedAt, &run.ErrorCode, &run.Revision, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return domain.WorkloadRun{}, err
	}
	run.RequestedBy, run.CommitSHA, run.ResourcePlaneID = domain.UserID(requestedBy), commitSHA, domain.ResourcePlaneID(resourcePlaneID)
	return run, nil
}
