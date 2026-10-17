// Package modelcredentials stores Tenant model-credential references without
// storing or returning provider secret values.
package modelcredentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

var (
	ErrCredentialNotFound = errors.New("Tenant model credential not found")
	ErrCredentialExists   = errors.New("Tenant model credential already exists")
	ErrCredentialConflict = errors.New("Tenant model credential changed or binding is unavailable")
	versionIDPattern      = regexp.MustCompile(`^[A-Fa-f0-9]{32}$`)
	credentialIDPattern   = regexp.MustCompile(`^mcred_[A-Za-z0-9_-]{8,80}$`)
)

const ProviderAnthropic = "anthropic"

type Credential struct {
	ID                   string          `json:"id"`
	TenantID             domain.TenantID `json:"tenant_id"`
	Provider             string          `json:"provider"`
	BindingID            string          `json:"binding_id"`
	SecretVersionID      string          `json:"secret_version_id"`
	Status               string          `json:"status"`
	CredentialGeneration int64           `json:"credential_generation"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// SecretAccess is trusted-process-only metadata. Do not serialize it into a
// public response, queue message, audit record, or runner registration payload.
type SecretAccess struct {
	CredentialID         string
	TenantID             domain.TenantID
	Provider             string
	ReaderRoleARN        string
	SecretARN            string
	SecretVersionID      string
	CredentialGeneration int64
}

func (SecretAccess) String() string { return "Tenant model credential access [REDACTED]" }

func (SecretAccess) GoString() string { return "Tenant model credential access [REDACTED]" }

func (SecretAccess) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED Tenant model credential access]"`), nil
}

type RunnerClaimIdentity struct {
	IntegrationID   string
	WorkflowJobID   int64
	RunAttempt      int64
	ClaimID         string
	ResourcePlaneID string
	LambdaMicroVMID string
}

// ResolveForRunnerClaim returns a Secret reference only for a claim whose
// launch passed authoritative workflow provenance and remains pinned to the
// same Tenant policy, Resource Plane, and Lambda MicroVM.
func ResolveForRunnerClaim(ctx context.Context, tx pgx.Tx, identity RunnerClaimIdentity, now time.Time) (SecretAccess, error) {
	if ctx == nil || tx == nil || identity.IntegrationID == "" || identity.WorkflowJobID <= 0 || identity.RunAttempt <= 0 ||
		identity.ClaimID == "" || identity.ResourcePlaneID == "" || identity.LambdaMicroVMID == "" || now.IsZero() {
		return SecretAccess{}, ErrCredentialNotFound
	}
	var access SecretAccess
	var policyJSON []byte
	var policyHash string
	err := tx.QueryRow(ctx, `SELECT c.id,c.tenant_id,c.provider,b.reader_role_arn,b.secret_arn,c.secret_version_id,c.credential_generation,
		r.actions_runner_workflow_policy,j.provenance_policy_sha256
		FROM github_actions_runner_jobs j
		JOIN tenant_github_integrations i ON i.id=j.integration_id AND i.tenant_id=j.tenant_id AND i.status='active'
		JOIN github_webhook_deliveries d ON d.integration_id=j.integration_id AND d.delivery_id=j.delivery_id
			AND d.processing_status='processed' AND d.credential_generation=i.credential_generation
		JOIN tenant_github_app_installation_bindings ib ON ib.tenant_id=j.tenant_id AND ib.integration_id=j.integration_id AND ib.installation_id=j.installation_id
		JOIN tenant_github_installations x ON x.tenant_id=ib.tenant_id AND x.installation_id=ib.installation_id AND x.status='active' AND x.suspended_at IS NULL
		JOIN tenant_github_repositories r ON r.tenant_id=j.tenant_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.repository_id
		JOIN tenant_model_credentials c ON c.tenant_id=j.tenant_id AND c.provider='anthropic' AND c.status='active'
		JOIN tenant_model_credential_bindings b ON b.tenant_id=c.tenant_id AND b.id=c.binding_id AND b.provider=c.provider AND b.status='approved'
		WHERE j.integration_id=$1 AND j.workflow_job_id=$2 AND j.run_attempt=$3 AND j.claim_id=$4 AND j.resource_plane_id=$5
		AND j.lambda_microvm_id=$6 AND j.state='starting' AND j.lease_expires_at>$7 AND j.provenance_verified_at IS NOT NULL
		AND j.provenance_policy_sha256 IS NOT NULL AND r.actions_runner_enabled=TRUE AND r.actions_runner_workflow_policy IS NOT NULL`,
		identity.IntegrationID, identity.WorkflowJobID, identity.RunAttempt, identity.ClaimID, identity.ResourcePlaneID,
		identity.LambdaMicroVMID, now.UTC()).Scan(&access.CredentialID, &access.TenantID, &access.Provider, &access.ReaderRoleARN,
		&access.SecretARN, &access.SecretVersionID, &access.CredentialGeneration, &policyJSON, &policyHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SecretAccess{}, ErrCredentialNotFound
		}
		return SecretAccess{}, fmt.Errorf("resolve trusted Tenant model credential reference: %w", err)
	}
	hash := sha256.Sum256(policyJSON)
	if hex.EncodeToString(hash[:]) != policyHash {
		return SecretAccess{}, ErrCredentialNotFound
	}
	var workflowPolicy githubregistry.ActionsRunnerWorkflowPolicy
	if json.Unmarshal(policyJSON, &workflowPolicy) != nil || !githubregistry.ValidActionsRunnerWorkflowPolicy(workflowPolicy) || access.Provider != ProviderAnthropic {
		return SecretAccess{}, ErrCredentialNotFound
	}
	return access, nil
}

func List(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) ([]Credential, error) {
	if ctx == nil || tx == nil || tenantID == "" {
		return nil, ErrCredentialNotFound
	}
	rows, err := tx.Query(ctx, `SELECT id,tenant_id,provider,binding_id,secret_version_id,status,credential_generation,created_at,updated_at
		FROM tenant_model_credentials WHERE tenant_id=$1 ORDER BY provider,id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list Tenant model credentials: %w", err)
	}
	defer rows.Close()
	items := make([]Credential, 0)
	for rows.Next() {
		var item Credential
		if err := rows.Scan(&item.ID, &item.TenantID, &item.Provider, &item.BindingID, &item.SecretVersionID, &item.Status,
			&item.CredentialGeneration, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Tenant model credential: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Tenant model credentials: %w", err)
	}
	return items, nil
}

func Create(ctx context.Context, tx pgx.Tx, credential Credential, actor domain.UserID, now time.Time) (Credential, error) {
	if ctx == nil || tx == nil || !validCredential(credential) || actor == "" || now.IsZero() {
		return Credential{}, ErrCredentialNotFound
	}
	var approved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_model_credential_bindings
		WHERE tenant_id=$1 AND id=$2 AND provider=$3 AND status='approved')`, credential.TenantID, credential.BindingID, credential.Provider).Scan(&approved); err != nil {
		return Credential{}, fmt.Errorf("verify Tenant model credential binding: %w", err)
	}
	if !approved {
		return Credential{}, ErrCredentialNotFound
	}
	credential.Status, credential.CredentialGeneration = "active", 1
	credential.CreatedAt, credential.UpdatedAt = now.UTC(), now.UTC()
	err := tx.QueryRow(ctx, `INSERT INTO tenant_model_credentials
		(id,tenant_id,provider,binding_id,secret_version_id,status,credential_generation,created_by,updated_by,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,'active',1,$6,$6,$7,$7)
		RETURNING id,tenant_id,provider,binding_id,secret_version_id,status,credential_generation,created_at,updated_at`,
		credential.ID, credential.TenantID, credential.Provider, credential.BindingID, credential.SecretVersionID, actor, now.UTC()).
		Scan(&credential.ID, &credential.TenantID, &credential.Provider, &credential.BindingID, &credential.SecretVersionID, &credential.Status,
			&credential.CredentialGeneration, &credential.CreatedAt, &credential.UpdatedAt)
	if err != nil {
		var postgresErr *pgconn.PgError
		if errors.As(err, &postgresErr) && postgresErr.Code == "23505" {
			return Credential{}, ErrCredentialExists
		}
		return Credential{}, fmt.Errorf("create Tenant model credential metadata: %w", err)
	}
	return credential, nil
}

func Rotate(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, provider, versionID string, expectedGeneration int64, actor domain.UserID, now time.Time) (Credential, error) {
	if ctx == nil || tx == nil || tenantID == "" || provider != ProviderAnthropic || !versionIDPattern.MatchString(versionID) || expectedGeneration <= 0 || actor == "" || now.IsZero() {
		return Credential{}, ErrCredentialConflict
	}
	return update(ctx, tx, `UPDATE tenant_model_credentials c SET secret_version_id=$4,credential_generation=c.credential_generation+1,
		updated_by=$5,updated_at=$6 FROM tenant_model_credential_bindings b
		WHERE c.tenant_id=$1 AND c.provider=$2 AND c.credential_generation=$3 AND c.status='active'
		AND b.tenant_id=c.tenant_id AND b.id=c.binding_id AND b.provider=c.provider AND b.status='approved'
		RETURNING c.id,c.tenant_id,c.provider,c.binding_id,c.secret_version_id,c.status,c.credential_generation,c.created_at,c.updated_at`,
		tenantID, provider, expectedGeneration, versionID, actor, now.UTC())
}

func Disable(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, provider string, expectedGeneration int64, actor domain.UserID, now time.Time) (Credential, error) {
	if ctx == nil || tx == nil || tenantID == "" || provider != ProviderAnthropic || expectedGeneration <= 0 || actor == "" || now.IsZero() {
		return Credential{}, ErrCredentialConflict
	}
	return update(ctx, tx, `UPDATE tenant_model_credentials c SET status='disabled',credential_generation=c.credential_generation+1,
		updated_by=$4,updated_at=$5
		WHERE c.tenant_id=$1 AND c.provider=$2 AND c.credential_generation=$3 AND c.status='active'
		RETURNING c.id,c.tenant_id,c.provider,c.binding_id,c.secret_version_id,c.status,c.credential_generation,c.created_at,c.updated_at`,
		tenantID, provider, expectedGeneration, actor, now.UTC())
}

func update(ctx context.Context, tx pgx.Tx, query string, args ...any) (Credential, error) {
	var credential Credential
	err := tx.QueryRow(ctx, query, args...).Scan(&credential.ID, &credential.TenantID, &credential.Provider, &credential.BindingID,
		&credential.SecretVersionID, &credential.Status, &credential.CredentialGeneration, &credential.CreatedAt, &credential.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrCredentialConflict
	}
	if err != nil {
		return Credential{}, fmt.Errorf("update Tenant model credential metadata: %w", err)
	}
	return credential, nil
}

func validCredential(credential Credential) bool {
	return credential.TenantID != "" && credential.Provider == ProviderAnthropic && credentialIDPattern.MatchString(credential.ID) &&
		credential.BindingID != "" && versionIDPattern.MatchString(credential.SecretVersionID)
}

func ValidSecretVersionID(value string) bool { return versionIDPattern.MatchString(value) }
