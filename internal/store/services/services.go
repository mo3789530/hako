// Package services persists Workspace-local Service instances.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/quotas"
)

var ErrNameConflict = errors.New("service name is already used by a different catalog configuration")
var ErrNotFound = errors.New("service not found")
var ErrDeleted = errors.New("deleted service names cannot be reused")
var ErrConcurrentUpdate = errors.New("service state changed concurrently")
var ErrResourceQuotaExceeded = errors.New("Workspace Service resource quota exceeded")
var ErrCredentialRotationUnavailable = errors.New("Workspace Service credential rotation is unavailable")

// The service budget is an admission limit, not a claim about measured guest
// capacity. Keep it below the proposed standard Runtime Class budget until
// Lambda MicroVM profiles expose enforceable, class-specific limits.
const (
	MaxWorkspaceServiceMemoryBytes int64 = quotas.MaxWorkspaceServiceMemoryBytes
	MaxWorkspaceServiceMilliCPU    int64 = quotas.MaxWorkspaceServiceMilliCPU
)

type ResourceUsage struct {
	MemoryBytesUsed  int64 `json:"memory_bytes_used"`
	MemoryBytesLimit int64 `json:"memory_bytes_limit"`
	MilliCPUUsed     int64 `json:"milli_cpu_used"`
	MilliCPULimit    int64 `json:"milli_cpu_limit"`
}

// ReadResourceUsage reports catalog-declared reservations, not measured guest
// usage. Pending stops/deletes remain charged until observed completion.
func ReadResourceUsage(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID) (ResourceUsage, error) {
	if tx == nil || workspaceID == "" {
		return ResourceUsage{}, errors.New("transaction and Workspace ID are required")
	}
	var tenantID domain.TenantID
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&tenantID); err != nil {
		return ResourceUsage{}, fmt.Errorf("load Workspace Tenant for Service usage: %w", err)
	}
	limits, err := quotas.Read(ctx, tx, tenantID)
	if err != nil {
		return ResourceUsage{}, err
	}
	usage := ResourceUsage{MemoryBytesLimit: limits.WorkspaceServiceMemoryBytes, MilliCPULimit: limits.WorkspaceServiceMilliCPU}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(memory_bytes), 0), COALESCE(SUM(milli_cpu), 0)
		FROM workspace_service_quota_reservations WHERE workspace_id = $1`, workspaceID).
		Scan(&usage.MemoryBytesUsed, &usage.MilliCPUUsed); err != nil {
		return ResourceUsage{}, fmt.Errorf("read Workspace Service resource usage: %w", err)
	}
	return usage, nil
}

// CreateOrGet creates a desired-running Service, or returns the existing row
// when the same Workspace/name/catalog configuration is retried. Callers must
// authorize Workspace visibility in the same transaction before invoking it.
func CreateOrGet(ctx context.Context, tx pgx.Tx, service domain.Service) (domain.Service, bool, error) {
	if tx == nil {
		return domain.Service{}, false, errors.New("transaction is required")
	}
	if service.ID == "" || service.WorkspaceID == "" || service.Name == "" || service.CatalogVersion == "" || service.ConfigJSON == "" || service.CreatedAt.IsZero() {
		return domain.Service{}, false, errors.New("service identity, catalog configuration, and creation time are required")
	}
	var stored domain.Service
	err := tx.QueryRow(ctx, `INSERT INTO services (id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, credential_generation, observed_revision, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', 'pending', 1, 1, 0, $6, $6)
		ON CONFLICT (workspace_id, name) DO NOTHING
		RETURNING id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, credential_generation, observed_revision, created_at, updated_at`,
		service.ID, service.WorkspaceID, service.Name, service.CatalogVersion, service.ConfigJSON, service.CreatedAt).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration, &stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
	if err == nil {
		if err := reserveWorkspaceServiceBudget(ctx, tx, stored); err != nil {
			return domain.Service{}, false, err
		}
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Service{}, false, fmt.Errorf("create Workspace Service: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, credential_generation, observed_revision, created_at, updated_at
		FROM services WHERE workspace_id = $1 AND name = $2`, service.WorkspaceID, service.Name).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration, &stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
	if err != nil {
		return domain.Service{}, false, fmt.Errorf("load existing Workspace Service: %w", err)
	}
	if stored.CatalogVersion != service.CatalogVersion || stored.ConfigJSON != service.ConfigJSON {
		return domain.Service{}, false, ErrNameConflict
	}
	if stored.DesiredState == "deleted" {
		return domain.Service{}, false, ErrDeleted
	}
	return stored, false, nil
}

// RequestState changes the desired lifecycle state while retaining the row as
// an auditable tombstone. It deliberately does not claim that a runtime has
// started, stopped, or physically removed the Service.
func RequestState(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, name, desiredState string, now time.Time) (domain.Service, bool, error) {
	if tx == nil {
		return domain.Service{}, false, errors.New("transaction is required")
	}
	if workspaceID == "" || name == "" || now.IsZero() {
		return domain.Service{}, false, errors.New("Workspace ID, Service name, and action time are required")
	}
	if desiredState != "running" && desiredState != "stopped" && desiredState != "deleted" {
		return domain.Service{}, false, errors.New("desired state must be running, stopped, or deleted")
	}
	var stored domain.Service
	err := tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, credential_generation, observed_revision, created_at, updated_at
		FROM services WHERE workspace_id = $1 AND name = $2`, workspaceID, name).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration, &stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Service{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Service{}, false, fmt.Errorf("load Workspace Service: %w", err)
	}
	if stored.DesiredState == "deleted" && desiredState != "deleted" {
		return domain.Service{}, false, ErrDeleted
	}
	if stored.DesiredState == desiredState {
		return stored, false, nil
	}
	if desiredState == "running" {
		if err := reserveWorkspaceServiceBudget(ctx, tx, stored); err != nil {
			return domain.Service{}, false, err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		previousState := stored.DesiredState
		err = tx.QueryRow(ctx, `UPDATE services SET desired_state = $3, observed_state = 'pending', desired_revision = desired_revision + 1, updated_at = $4
			WHERE workspace_id = $1 AND name = $2 AND desired_state = $5
			RETURNING id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, credential_generation, observed_revision, created_at, updated_at`,
			workspaceID, name, desiredState, now, previousState).Scan(
			&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
			&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration, &stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
		if err == nil {
			return stored, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Service{}, false, fmt.Errorf("request Workspace Service state: %w", err)
		}
		err = tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state, desired_revision, credential_generation, observed_revision, created_at, updated_at
			FROM services WHERE workspace_id = $1 AND name = $2`, workspaceID, name).Scan(
			&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
			&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration, &stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Service{}, false, ErrNotFound
		}
		if err != nil {
			return domain.Service{}, false, fmt.Errorf("reload Workspace Service after concurrent state change: %w", err)
		}
		if stored.DesiredState == desiredState {
			return stored, false, nil
		}
		if stored.DesiredState == "deleted" {
			return domain.Service{}, false, ErrDeleted
		}
	}
	return domain.Service{}, false, ErrConcurrentUpdate
}

// RequestCredentialRotation advances a generated credential generation and
// Service revision atomically. The caller must enqueue the returned snapshot
// and append its audit event in the same transaction. Runtime applies the new
// password in place before the Resource Plane promotes it to AWSCURRENT.
func RequestCredentialRotation(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, name string, now time.Time) (domain.Service, error) {
	if tx == nil || workspaceID == "" || name == "" || now.IsZero() {
		return domain.Service{}, errors.New("transaction, Workspace, Service name, and rotation time are required")
	}
	var stored domain.Service
	err := tx.QueryRow(ctx, `SELECT id, workspace_id, name, catalog_version, config_json, desired_state, observed_state,
		desired_revision, credential_generation, observed_revision, created_at, updated_at
		FROM services WHERE workspace_id = $1 AND name = $2`, workspaceID, name).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration,
		&stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Service{}, ErrNotFound
	}
	if err != nil {
		return domain.Service{}, fmt.Errorf("load Workspace Service for credential rotation: %w", err)
	}
	if stored.DesiredState != "running" {
		return domain.Service{}, ErrCredentialRotationUnavailable
	}
	var template struct {
		GeneratedSecrets []string `json:"generated_secrets"`
	}
	if err := json.Unmarshal([]byte(stored.ConfigJSON), &template); err != nil || len(template.GeneratedSecrets) == 0 {
		return domain.Service{}, ErrCredentialRotationUnavailable
	}
	err = tx.QueryRow(ctx, `UPDATE services SET desired_revision = desired_revision + 1,
		credential_generation = credential_generation + 1, observed_state = 'pending', updated_at = $4
		WHERE workspace_id = $1 AND name = $2 AND desired_revision = $3 AND desired_state = 'running'
		RETURNING id, workspace_id, name, catalog_version, config_json, desired_state, observed_state,
			desired_revision, credential_generation, observed_revision, created_at, updated_at`,
		workspaceID, name, stored.DesiredRevision, now.UTC()).Scan(
		&stored.ID, &stored.WorkspaceID, &stored.Name, &stored.CatalogVersion, &stored.ConfigJSON,
		&stored.DesiredState, &stored.ObservedState, &stored.DesiredRevision, &stored.CredentialGeneration,
		&stored.ObservedRevision, &stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Service{}, ErrConcurrentUpdate
	}
	if err != nil {
		return domain.Service{}, fmt.Errorf("request Workspace Service credential rotation: %w", err)
	}
	return stored, nil
}

// reserveWorkspaceServiceBudget increments a per-Workspace OCC guard before
// checking and inserting a durable reservation. Concurrent admissions conflict
// on the guard row and are retried by the transaction policy; no row lock is
// used, keeping the path compatible with Aurora DSQL.
func reserveWorkspaceServiceBudget(ctx context.Context, tx pgx.Tx, candidate domain.Service) error {
	if tx == nil || candidate.WorkspaceID == "" || candidate.ID == "" {
		return errors.New("transaction and Workspace ID are required")
	}
	var resources struct {
		Resources struct {
			MemoryBytes int64 `json:"memory_bytes"`
			MilliCPU    int64 `json:"milli_cpu"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(candidate.ConfigJSON), &resources); err != nil || resources.Resources.MemoryBytes <= 0 || resources.Resources.MilliCPU <= 0 {
		return errors.New("Workspace Service resource request is invalid")
	}
	var tenantID domain.TenantID
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM workspaces WHERE id = $1`, candidate.WorkspaceID).Scan(&tenantID); err != nil {
		return fmt.Errorf("load Workspace Tenant for Service reservation: %w", err)
	}
	limits, err := quotas.AdvanceAdmissionGuard(ctx, tx, tenantID, candidate.UpdatedAt)
	if err != nil {
		return err
	}
	if err := bumpWorkspaceServiceQuotaGuard(ctx, tx, candidate.WorkspaceID, candidate.UpdatedAt); err != nil {
		return err
	}
	var existing domain.ServiceID
	err = tx.QueryRow(ctx, `SELECT service_id FROM workspace_service_quota_reservations WHERE service_id = $1`, candidate.ID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check existing Workspace Service quota reservation: %w", err)
	}
	var memoryBytes, milliCPU int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(memory_bytes), 0), COALESCE(SUM(milli_cpu), 0)
		FROM workspace_service_quota_reservations WHERE workspace_id = $1`, candidate.WorkspaceID).Scan(&memoryBytes, &milliCPU); err != nil {
		return fmt.Errorf("read Workspace Service resource reservations: %w", err)
	}
	if memoryBytes+resources.Resources.MemoryBytes > limits.WorkspaceServiceMemoryBytes || milliCPU+resources.Resources.MilliCPU > limits.WorkspaceServiceMilliCPU {
		return ErrResourceQuotaExceeded
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_service_quota_reservations
		(service_id, workspace_id, memory_bytes, milli_cpu, reserved_at) VALUES ($1, $2, $3, $4, $5)`,
		candidate.ID, candidate.WorkspaceID, resources.Resources.MemoryBytes, resources.Resources.MilliCPU, candidate.UpdatedAt.UTC()); err != nil {
		return fmt.Errorf("persist Workspace Service quota reservation: %w", err)
	}
	return nil
}

// ReleaseWorkspaceServiceBudget removes a reservation only after an accepted
// runtime report confirms the service is stopped or deleted.
func ReleaseWorkspaceServiceBudget(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, serviceID domain.ServiceID, now time.Time) error {
	if tx == nil || workspaceID == "" || serviceID == "" || now.IsZero() {
		return errors.New("transaction, Workspace, Service, and release time are required")
	}
	var tenantID domain.TenantID
	if err := tx.QueryRow(ctx, `SELECT tenant_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&tenantID); err != nil {
		return fmt.Errorf("load Workspace Tenant for Service reservation release: %w", err)
	}
	if _, err := quotas.AdvanceAdmissionGuard(ctx, tx, tenantID, now); err != nil {
		return err
	}
	if err := bumpWorkspaceServiceQuotaGuard(ctx, tx, workspaceID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_service_quota_reservations WHERE service_id = $1 AND workspace_id = $2`, serviceID, workspaceID); err != nil {
		return fmt.Errorf("release Workspace Service quota reservation: %w", err)
	}
	return nil
}

func bumpWorkspaceServiceQuotaGuard(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID, now time.Time) error {
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO workspace_service_quota_guards (workspace_id, revision, updated_at)
		VALUES ($1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET revision = workspace_service_quota_guards.revision + 1,
			updated_at = EXCLUDED.updated_at
		RETURNING revision`, workspaceID, now.UTC()).Scan(&revision)
	if err != nil {
		return fmt.Errorf("advance Workspace Service quota OCC guard: %w", err)
	}
	return nil
}

// List returns Services in stable name order. The caller must authorize
// Workspace visibility in the same transaction.
func List(ctx context.Context, tx pgx.Tx, workspaceID domain.WorkspaceID) ([]domain.Service, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	if workspaceID == "" {
		return nil, errors.New("Workspace ID is required")
	}
	rows, err := tx.Query(ctx, `SELECT s.id, s.workspace_id, s.name, s.catalog_version, s.config_json, s.desired_state,
		CASE WHEN s.desired_state = 'running' AND g.service_id IS NULL THEN 'pending' ELSE s.observed_state END,
		s.desired_revision, s.credential_generation, s.observed_revision, COALESCE(g.allocation_generation, 0), s.created_at, s.updated_at
		FROM services s
		LEFT JOIN placements p ON p.workspace_id = s.workspace_id
		LEFT JOIN resource_bindings rb ON rb.workspace_id = s.workspace_id AND rb.resource_plane_id = p.resource_plane_id AND rb.kind = 'lambda_microvm'
		LEFT JOIN workspace_service_runtime_generations g ON g.service_id = s.id AND g.workspace_id = s.workspace_id
			AND g.resource_plane_id = p.resource_plane_id AND g.allocation_generation = rb.allocation_generation
			AND g.microvm_id = rb.external_id AND g.desired_revision = s.desired_revision AND g.retired = FALSE
		WHERE s.workspace_id = $1 ORDER BY s.name, s.id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list Workspace Services: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Service, 0)
	for rows.Next() {
		var service domain.Service
		if err := rows.Scan(&service.ID, &service.WorkspaceID, &service.Name, &service.CatalogVersion, &service.ConfigJSON,
			&service.DesiredState, &service.ObservedState, &service.DesiredRevision, &service.CredentialGeneration, &service.ObservedRevision,
			&service.ObservedRuntimeGeneration, &service.CreatedAt, &service.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Workspace Service: %w", err)
		}
		result = append(result, service)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Workspace Services: %w", err)
	}
	return result, nil
}
