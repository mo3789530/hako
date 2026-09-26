package workloads

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

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
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, workload_run_id, kind, storage_ref, size_bytes, sha256, redacted, expires_at, created_at
		FROM workload_artifacts WHERE tenant_id = $1 AND workload_run_id = $2 ORDER BY created_at, id`, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list Workload artifacts: %w", err)
	}
	defer rows.Close()
	artifacts := make([]domain.WorkloadArtifact, 0)
	for rows.Next() {
		var artifact domain.WorkloadArtifact
		if err := rows.Scan(&artifact.ID, &artifact.TenantID, &artifact.RunID, &artifact.Kind,
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
