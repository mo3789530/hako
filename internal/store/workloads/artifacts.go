package workloads

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/quotas"
)

const MaxExpiredArtifactBatchSize = 1000

var artifactSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var errLogsMustBeRedacted = errors.New("Workload logs must be redacted before storage")

// AddArtifact persists only an opaque storage reference and integrity metadata.
// Log artifacts must be redacted before they can be registered.
func AddArtifact(ctx context.Context, tx pgx.Tx, artifact domain.WorkloadArtifact) error {
	if tx == nil || artifact.ID == "" || artifact.TenantID == "" || artifact.RunID == "" {
		return errors.New("transaction, artifact ID, Tenant ID, and Workload Run ID are required")
	}
	if err := validateArtifact(artifact); err != nil {
		return err
	}
	limits, err := quotas.AdvanceAdmissionGuard(ctx, tx, artifact.TenantID, artifact.CreatedAt)
	if err != nil {
		return err
	}
	var usedBytes int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM workload_artifacts WHERE tenant_id = $1`, artifact.TenantID).Scan(&usedBytes); err != nil {
		return fmt.Errorf("read Tenant artifact quota usage: %w", err)
	}
	var reservedBytes, ownReservation, ownArtifactBytes int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COALESCE(SUM(reserved_bytes), 0) FROM workload_artifact_quota_reservations WHERE tenant_id = $1),
		COALESCE((SELECT reserved_bytes FROM workload_artifact_quota_reservations WHERE tenant_id = $1 AND workload_run_id = $2), 0),
		(SELECT COALESCE(SUM(size_bytes), 0) FROM workload_artifacts WHERE tenant_id = $1 AND workload_run_id = $2)`, artifact.TenantID, artifact.RunID).
		Scan(&reservedBytes, &ownReservation, &ownArtifactBytes); err != nil {
		return fmt.Errorf("read Tenant artifact quota reservations: %w", err)
	}
	otherReservations := reservedBytes - ownReservation
	if ownReservation > 0 && artifact.SizeBytes > ownReservation-ownArtifactBytes ||
		artifact.SizeBytes > limits.ArtifactBytes-usedBytes-otherReservations {
		return quotas.ErrArtifactQuotaExceeded
	}
	var expiresAt any
	if artifact.ExpiresAt != nil {
		if !artifact.ExpiresAt.After(artifact.CreatedAt) {
			return errors.New("artifact expiry must be after its creation time")
		}
		expiresAt = artifact.ExpiresAt.UTC()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_artifacts
		(id, tenant_id, workload_run_id, kind, storage_ref, size_bytes, sha256, redacted, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, artifact.ID, artifact.TenantID,
		artifact.RunID, artifact.Kind, artifact.StorageRef, artifact.SizeBytes, artifact.SHA256,
		artifact.Redacted, expiresAt, artifact.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("record Workload artifact: %w", err)
	}
	return nil
}

func validateArtifact(artifact domain.WorkloadArtifact) error {
	if artifact.ID == "" || artifact.TenantID == "" || artifact.RunID == "" {
		return errors.New("artifact ID, Tenant ID, and Workload Run ID are required")
	}
	if !oneOf(artifact.Kind, "logs", "test_report", "coverage", "sbom", "binary", "container_image", "other") {
		return errors.New("unsupported Workload artifact kind")
	}
	if artifact.SizeBytes < 0 || !artifactSHA256Pattern.MatchString(artifact.SHA256) {
		return errors.New("artifact size or SHA-256 is invalid")
	}
	if artifact.Kind == "logs" && !artifact.Redacted {
		return errLogsMustBeRedacted
	}
	if err := validateStorageRef(artifact.StorageRef); err != nil {
		return err
	}
	if artifact.CreatedAt.IsZero() {
		return errors.New("artifact creation time is required")
	}
	if artifact.ExpiresAt != nil && !artifact.ExpiresAt.After(artifact.CreatedAt) {
		return errors.New("artifact expiry must be after its creation time")
	}
	return nil
}

func ListArtifacts(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, runID domain.WorkloadRunID) ([]domain.WorkloadArtifact, error) {
	if tx == nil || tenantID == "" || runID == "" {
		return nil, errors.New("transaction, Tenant ID, and Workload Run ID are required")
	}
	rows, err := tx.Query(ctx, `SELECT a.id, a.tenant_id, a.workload_run_id, COALESCE(r.correlation_id, ''),
		a.kind, a.storage_ref, a.size_bytes, a.sha256, a.redacted, a.expires_at, a.created_at
		FROM workload_artifacts a JOIN workload_runs r ON r.tenant_id = a.tenant_id AND r.id = a.workload_run_id
		WHERE a.tenant_id = $1 AND a.workload_run_id = $2 ORDER BY a.created_at, a.id`, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list Workload artifacts: %w", err)
	}
	defer rows.Close()
	return scanArtifacts(rows)
}

func scanArtifacts(rows pgx.Rows) ([]domain.WorkloadArtifact, error) {
	artifacts := make([]domain.WorkloadArtifact, 0)
	for rows.Next() {
		var artifact domain.WorkloadArtifact
		if err := rows.Scan(&artifact.ID, &artifact.TenantID, &artifact.RunID, &artifact.CorrelationID, &artifact.Kind,
			&artifact.StorageRef, &artifact.SizeBytes, &artifact.SHA256, &artifact.Redacted,
			&artifact.ExpiresAt, &artifact.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan Workload artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Workload artifacts: %w", err)
	}
	return artifacts, nil
}

// ListExpiredArtifacts returns a bounded batch of expired metadata rows for a
// cleanup worker. It deliberately does not remove rows: callers must first
// delete the backing object and only then call DeleteExpiredArtifact so quota
// usage is not released while bytes may still be retained in object storage.
func ListExpiredArtifacts(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]domain.WorkloadArtifact, error) {
	if tx == nil || now.IsZero() {
		return nil, errors.New("transaction and cleanup time are required")
	}
	if limit < 1 || limit > MaxExpiredArtifactBatchSize {
		return nil, fmt.Errorf("expired artifact batch size must be between 1 and %d", MaxExpiredArtifactBatchSize)
	}
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, workload_run_id, kind, storage_ref, size_bytes, sha256, redacted, expires_at, created_at
		FROM workload_artifacts WHERE expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at, id LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired Workload artifacts: %w", err)
	}
	defer rows.Close()
	artifacts := make([]domain.WorkloadArtifact, 0)
	for rows.Next() {
		var artifact domain.WorkloadArtifact
		if err := rows.Scan(&artifact.ID, &artifact.TenantID, &artifact.RunID, &artifact.Kind,
			&artifact.StorageRef, &artifact.SizeBytes, &artifact.SHA256, &artifact.Redacted,
			&artifact.ExpiresAt, &artifact.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan expired Workload artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired Workload artifacts: %w", err)
	}
	return artifacts, nil
}

// ListExpiredArtifactsForResourcePlane exposes cleanup candidates only for Runs
// pinned to the authenticated Resource Plane. It is the internal RP API's
// read path; Resource Plane processes must never query DSQL directly.
func ListExpiredArtifactsForResourcePlane(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, now time.Time, limit int) ([]domain.WorkloadArtifact, error) {
	if tx == nil || resourcePlaneID == "" || now.IsZero() {
		return nil, errors.New("transaction, Resource Plane ID, and cleanup time are required")
	}
	if limit < 1 || limit > MaxExpiredArtifactBatchSize {
		return nil, fmt.Errorf("expired artifact batch size must be between 1 and %d", MaxExpiredArtifactBatchSize)
	}
	rows, err := tx.Query(ctx, `SELECT a.id, a.tenant_id, a.workload_run_id, COALESCE(r.correlation_id, ''),
		a.kind, a.storage_ref, a.size_bytes, a.sha256, a.redacted, a.expires_at, a.created_at
		FROM workload_artifacts a JOIN workload_runs r ON r.tenant_id = a.tenant_id AND r.id = a.workload_run_id
		WHERE r.resource_plane_id = $1 AND a.expires_at IS NOT NULL AND a.expires_at <= $2
		ORDER BY a.expires_at, a.id LIMIT $3`, resourcePlaneID, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list Resource Plane expired Workload artifacts: %w", err)
	}
	defer rows.Close()
	return scanArtifacts(rows)
}

// DeleteExpiredArtifact removes metadata only if the exact object reference is
// still expired. A worker may safely retry object deletion after a crash: S3
// DeleteObject is idempotent, and this conditional delete releases quota once.
func DeleteExpiredArtifact(ctx context.Context, tx pgx.Tx, artifact domain.WorkloadArtifact, now time.Time) (bool, error) {
	if tx == nil || artifact.ID == "" || artifact.TenantID == "" || artifact.StorageRef == "" || now.IsZero() {
		return false, errors.New("transaction, artifact identity, storage reference, and cleanup time are required")
	}
	command, err := tx.Exec(ctx, `DELETE FROM workload_artifacts
		WHERE id = $1 AND tenant_id = $2 AND storage_ref = $3 AND expires_at IS NOT NULL AND expires_at <= $4`,
		artifact.ID, artifact.TenantID, artifact.StorageRef, now.UTC())
	if err != nil {
		return false, fmt.Errorf("delete expired Workload artifact metadata: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

// DeleteExpiredArtifactForResourcePlane is the plane-scoped acknowledgement
// path. The exact tenant/run/reference/expiry tuple is rechecked in the DELETE.
func DeleteExpiredArtifactForResourcePlane(ctx context.Context, tx pgx.Tx, resourcePlaneID domain.ResourcePlaneID, artifact domain.WorkloadArtifact, now time.Time) (bool, error) {
	if tx == nil || resourcePlaneID == "" || artifact.ID == "" || artifact.TenantID == "" || artifact.RunID == "" || artifact.StorageRef == "" || artifact.ExpiresAt == nil || now.IsZero() {
		return false, errors.New("transaction, Resource Plane ID, artifact identity, storage reference, and cleanup time are required")
	}
	command, err := tx.Exec(ctx, `DELETE FROM workload_artifacts
		WHERE id = $1 AND tenant_id = $2 AND workload_run_id = $3 AND storage_ref = $4
		AND expires_at = $5 AND expires_at <= $6
		AND EXISTS (SELECT 1 FROM workload_runs r WHERE r.tenant_id = workload_artifacts.tenant_id AND r.id = workload_artifacts.workload_run_id AND r.resource_plane_id = $7)`,
		artifact.ID, artifact.TenantID, artifact.RunID, artifact.StorageRef, artifact.ExpiresAt.UTC(), now.UTC(), resourcePlaneID)
	if err != nil {
		return false, fmt.Errorf("delete Resource Plane expired artifact metadata: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

func validateStorageRef(value string) error {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || len(value) > 2048 || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("artifact storage reference must be an opaque URI without credentials, query, or fragment")
	}
	if parsed.Scheme != "s3" && parsed.Scheme != "ecr" && parsed.Scheme != "https" {
		return errors.New("artifact storage reference must use s3, ecr, or unsigned https URI")
	}
	return nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
