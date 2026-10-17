//go:build integration

package modelcredentials

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantModelCredentialCreateRotateDisableAndTenantFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_model_a','Model A',now()),('tenant_model_b','Model B',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_model_a','model-a','',now())`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_model_a','usr_model_a','owner',now())`,
		`INSERT INTO tenant_model_credential_bindings(id,tenant_id,provider,secret_arn,reader_role_arn,status,created_at,updated_at)
		 VALUES ('binding_model_a','tenant_model_a','anthropic','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/model/a-AbCdEf',
		 'arn:aws:iam::123456789012:role/hako/model-reader-a','approved',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	credential := Credential{ID: "mcred_modeltest1", TenantID: domain.TenantID("tenant_model_a"), Provider: ProviderAnthropic,
		BindingID: "binding_model_a", SecretVersionID: "11111111111111111111111111111111"}
	created, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Credential, error) {
		return Create(ctx, tx, credential, domain.UserID("usr_model_a"), time.Now().UTC())
	})
	if err != nil || created.Status != "active" || created.CredentialGeneration != 1 {
		t.Fatalf("Create() = %+v, %v", created, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Credential, error) {
		crossTenant := credential
		crossTenant.ID, crossTenant.TenantID, crossTenant.BindingID = "mcred_modeltest2", "tenant_model_b", "binding_model_a"
		return Create(ctx, tx, crossTenant, domain.UserID("usr_model_a"), time.Now().UTC())
	}); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("cross-Tenant binding Create() error = %v", err)
	}
	rotated, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Credential, error) {
		return Rotate(ctx, tx, "tenant_model_a", ProviderAnthropic, "22222222222222222222222222222222", 1,
			"usr_model_a", time.Now().UTC())
	})
	if err != nil || rotated.CredentialGeneration != 2 || rotated.SecretVersionID != "22222222222222222222222222222222" {
		t.Fatalf("Rotate() = %+v, %v", rotated, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Credential, error) {
		return Rotate(ctx, tx, "tenant_model_a", ProviderAnthropic, "33333333333333333333333333333333", 1,
			"usr_model_a", time.Now().UTC())
	}); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("stale Rotate() error = %v", err)
	}
	disabled, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Credential, error) {
		return Disable(ctx, tx, "tenant_model_a", ProviderAnthropic, 2, "usr_model_a", time.Now().UTC())
	})
	if err != nil || disabled.Status != "disabled" || disabled.CredentialGeneration != 3 {
		t.Fatalf("Disable() = %+v, %v", disabled, err)
	}
	items, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]Credential, error) {
		return List(ctx, tx, "tenant_model_a")
	})
	if err != nil || len(items) != 1 || items[0].Status != "disabled" || items[0].TenantID != "tenant_model_a" {
		t.Fatalf("List() = %+v, %v", items, err)
	}
}
