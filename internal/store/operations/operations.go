// Package operations persists Control Plane operations.
package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
)

var ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
var ErrOperationNotFound = errors.New("operation not found")
var ErrTransitionConflict = errors.New("operation status does not match expected status")

const insertIdempotentOperation = `
INSERT INTO operations (
	id, tenant_id, workspace_id, resource_plane_id, type, status,
	idempotency_key, request_hash, attempt, error_code, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (tenant_id, idempotency_key)
DO UPDATE SET idempotency_key = operations.idempotency_key
RETURNING id, tenant_id, workspace_id, resource_plane_id, type, status,
	idempotency_key, request_hash, attempt, COALESCE(error_code, ''), created_at, updated_at`

const selectOperationByIdempotencyKey = `
SELECT id, tenant_id, workspace_id, resource_plane_id, type, status,
	idempotency_key, request_hash, attempt, COALESCE(error_code, ''), created_at, updated_at
FROM operations
WHERE tenant_id = $1 AND idempotency_key = $2`

const selectOperationByID = `
SELECT id, tenant_id, workspace_id, resource_plane_id, type, status,
	idempotency_key, request_hash, attempt, COALESCE(error_code, ''), created_at, updated_at
FROM operations
WHERE id = $1`

func RequestHash(tenantID domain.TenantID, operationType domain.OperationType, normalizedRequest any) (string, error) {
	return idempotency.Fingerprint(struct {
		TenantID domain.TenantID
		Type     domain.OperationType
		Request  any
	}{
		TenantID: tenantID,
		Type:     operationType,
		Request:  normalizedRequest,
	})
}

func VerifyRequest(operation domain.Operation, operationType domain.OperationType, normalizedRequest any) error {
	requestHash, err := RequestHash(operation.TenantID, operationType, normalizedRequest)
	if err != nil {
		return fmt.Errorf("fingerprint operation request: %w", err)
	}
	if operation.Type != operationType || operation.RequestHash != requestHash {
		return ErrIdempotencyConflict
	}
	return nil
}

func GetByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, key string) (domain.Operation, bool, error) {
	if tx == nil {
		return domain.Operation{}, false, errors.New("transaction is required")
	}
	if err := idempotency.ValidateKey(key); err != nil {
		return domain.Operation{}, false, err
	}
	operation, err := scanOperation(tx.QueryRow(ctx, selectOperationByIdempotencyKey, tenantID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Operation{}, false, nil
	}
	if err != nil {
		return domain.Operation{}, false, fmt.Errorf("get operation by idempotency key: %w", err)
	}
	return operation, true, nil
}

// Get loads an operation using the caller's transaction.
func Get(ctx context.Context, tx pgx.Tx, id domain.OperationID) (domain.Operation, error) {
	if tx == nil {
		return domain.Operation{}, errors.New("transaction is required")
	}
	if id == "" {
		return domain.Operation{}, errors.New("operation id is required")
	}
	operation, err := scanOperation(tx.QueryRow(ctx, selectOperationByID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Operation{}, ErrOperationNotFound
	}
	if err != nil {
		return domain.Operation{}, fmt.Errorf("get operation: %w", err)
	}
	return operation, nil
}

// InsertOrGet stores an operation or returns the existing operation for the
// same tenant/key. normalizedRequest must have defaults applied and exclude
// transport metadata and generated values. Call it inside transaction.Within
// so OCC conflicts retry the entire database-only transaction.
func InsertOrGet(ctx context.Context, tx pgx.Tx, operation domain.Operation, normalizedRequest any) (domain.Operation, error) {
	if tx == nil {
		return domain.Operation{}, errors.New("transaction is required")
	}
	if err := idempotency.ValidateKey(operation.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	if operation.ID == "" || operation.TenantID == "" || operation.WorkspaceID == "" || operation.ResourcePlaneID == "" {
		return domain.Operation{}, errors.New("operation, tenant, workspace, and resource plane ids are required")
	}
	if operation.Type == "" || operation.Status == "" {
		return domain.Operation{}, errors.New("operation type and status are required")
	}
	if operation.Attempt < 0 {
		return domain.Operation{}, errors.New("operation attempt must be non-negative")
	}
	if operation.CreatedAt.IsZero() {
		operation.CreatedAt = time.Now().UTC()
	}
	if operation.UpdatedAt.IsZero() {
		operation.UpdatedAt = operation.CreatedAt
	}
	requestHash, err := RequestHash(operation.TenantID, operation.Type, normalizedRequest)
	if err != nil {
		return domain.Operation{}, fmt.Errorf("fingerprint operation request: %w", err)
	}

	stored, err := scanOperation(tx.QueryRow(ctx, insertIdempotentOperation,
		operation.ID,
		operation.TenantID,
		operation.WorkspaceID,
		operation.ResourcePlaneID,
		operation.Type,
		operation.Status,
		operation.IdempotencyKey,
		requestHash,
		operation.Attempt,
		operation.ErrorCode,
		operation.CreatedAt,
		operation.UpdatedAt,
	))
	if err != nil {
		return domain.Operation{}, fmt.Errorf("insert or get operation: %w", err)
	}
	if stored.RequestHash != requestHash {
		return domain.Operation{}, ErrIdempotencyConflict
	}
	return stored, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperation(row rowScanner) (domain.Operation, error) {
	var operation domain.Operation
	err := row.Scan(
		&operation.ID,
		&operation.TenantID,
		&operation.WorkspaceID,
		&operation.ResourcePlaneID,
		&operation.Type,
		&operation.Status,
		&operation.IdempotencyKey,
		&operation.RequestHash,
		&operation.Attempt,
		&operation.ErrorCode,
		&operation.CreatedAt,
		&operation.UpdatedAt,
	)
	if err != nil {
		return domain.Operation{}, err
	}
	return operation, nil
}
