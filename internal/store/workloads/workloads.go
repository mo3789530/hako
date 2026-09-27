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
	"github.com/mo3789530/hako/internal/store/transaction"
)

const (
	DefaultTimeout = 30 * time.Minute
	MaxTimeout     = 24 * time.Hour
	MaxExpireBatch = 100
)

var (
	ErrNotFound            = errors.New("Workload Run not found")
	ErrIdempotencyConflict = errors.New("Workload Run idempotency key conflicts with another request")
	ErrTransitionConflict  = errors.New("Workload Run revision or state does not match")
	ErrRepositoryInactive  = errors.New("Workload Run Repository is not registered and active for Tenant")
	commitSHAPattern       = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	errorCodePattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

const runColumns = `id, tenant_id, kind, COALESCE(requested_by, ''), COALESCE(github_installation_id, 0),
COALESCE(github_repository_id, 0), COALESCE(commit_sha, ''), ref, runtime_class, trust_level,
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
	requestHash, err := idempotency.Fingerprint(struct {
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
	}{run.TenantID, run.Kind, run.RequestedBy, run.GitHubInstallation, run.GitHubRepository, run.CommitSHA, run.Ref, run.RuntimeClass, run.TrustLevel, run.ResourcePlaneID, run.TimeoutSeconds})
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
	}
	var requestedBy, commitSHA, resourcePlaneID any
	if run.RequestedBy != "" {
		requestedBy = run.RequestedBy
	}
	if run.CommitSHA != "" {
		commitSHA = run.CommitSHA
	}
	if run.ResourcePlaneID != "" {
		resourcePlaneID = run.ResourcePlaneID
	}
	var installationID, repositoryID any
	if run.GitHubInstallation != 0 {
		installationID, repositoryID = run.GitHubInstallation, run.GitHubRepository
	}
	tag, err := tx.Exec(ctx, `INSERT INTO workload_runs
		(id, tenant_id, kind, requested_by, github_installation_id, github_repository_id, commit_sha, ref,
		 runtime_class, trust_level, desired_state, state, resource_plane_id, idempotency_key, request_hash,
		 timeout_seconds, timeout_at, revision, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'running', 'pending', $11, $12, $13, $14, $15, 1, $16, $16)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		run.ID, run.TenantID, run.Kind, requestedBy, installationID, repositoryID, commitSHA, run.Ref,
		run.RuntimeClass, run.TrustLevel, resourcePlaneID, run.IdempotencyKey, requestHash, run.TimeoutSeconds, run.TimeoutAt, run.CreatedAt)
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
		if err := appendEvent(ctx, tx, stored, "workload.pending", run.CreatedAt); err != nil {
			return domain.WorkloadRun{}, false, err
		}
	}
	return stored, created, nil
}

func validateRun(run domain.WorkloadRun) error {
	if run.ID == "" || run.TenantID == "" || run.RuntimeClass == "" || run.IdempotencyKey == "" {
		return errors.New("Workload Run ID, Tenant, runtime class, and idempotency key are required")
	}
	if err := idempotency.ValidateKey(run.IdempotencyKey); err != nil {
		return err
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
	if (run.GitHubInstallation == 0) != (run.GitHubRepository == 0) || run.GitHubInstallation < 0 || run.GitHubRepository < 0 {
		return errors.New("GitHub Installation and Repository IDs must both be positive or both be absent")
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
	return run, nil
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
	err := row.Scan(&run.ID, &run.TenantID, &run.Kind, &requestedBy, &run.GitHubInstallation,
		&run.GitHubRepository, &commitSHA, &run.Ref, &run.RuntimeClass, &run.TrustLevel, &run.DesiredState,
		&run.State, &resourcePlaneID, &run.IdempotencyKey, &run.RequestHash, &run.TimeoutSeconds, &run.TimeoutAt, &run.StartedAt,
		&run.CompletedAt, &run.ErrorCode, &run.Revision, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return domain.WorkloadRun{}, err
	}
	run.RequestedBy, run.CommitSHA, run.ResourcePlaneID = domain.UserID(requestedBy), commitSHA, domain.ResourcePlaneID(resourcePlaneID)
	return run, nil
}
