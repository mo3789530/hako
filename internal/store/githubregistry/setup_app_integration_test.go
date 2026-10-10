//go:build integration

package githubregistry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantSetupPinsAppAndRejectsChangedConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_setup_app','Setup App',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_setup_app','setup-app-sub','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_setup_app','usr_setup_app','owner',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('setup_binding','tenant_setup_app',911,'approved','private','oauth','webhook',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('setup_registration',911,'tenant_owned','tenant_setup_app','setup-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('setup_integration','tenant_setup_app','setup_registration',911,'setup_binding','active',3,6,'setup-key','hash','usr_setup_app',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	stateHash := HashSetupState("owned-setup-state")
	pin, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (SetupAppPin, error) {
		pin, err := ResolveSetupAppPin(ctx, tx, "tenant_setup_app", "setup_integration")
		if err != nil {
			return SetupAppPin{}, err
		}
		if err := CreateTenantAppSetupState(ctx, tx, pin, "usr_setup_app", stateHash, strings.Repeat("v", 43), now, now.Add(10*time.Minute)); err != nil {
			return SetupAppPin{}, err
		}
		return pin, SetSetupInstallation(ctx, tx, stateHash, 9201, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveCredential := func(candidate SetupAppPin) (SetupCredential, error) {
		return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (SetupCredential, error) {
			return ResolveSetupCredential(ctx, tx, candidate)
		})
	}
	if _, err := resolveCredential(pin); !errors.Is(err, ErrAppBindingDenied) {
		t.Fatalf("unprovisioned OAuth access accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_credential_bindings SET oauth_reader_role_arn='arn:aws:iam::123456789012:role/hako/oauth-reader',oauth_secret_ref='arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/oauth-AbCdEf',oauth_secret_version_id='0123456789abcdef0123456789abcdef' WHERE id='setup_binding'`); err != nil {
		t.Fatal(err)
	}
	credential, err := resolveCredential(pin)
	if err != nil || credential.Pin != pin || credential.Slug != "setup-app" || credential.ClientID != "client" {
		t.Fatalf("OAuth credential resolution: %v %v", credential, err)
	}
	for _, mutate := range []func(*SetupAppPin){
		func(p *SetupAppPin) { p.TenantID = "tenant_other" },
		func(p *SetupAppPin) { p.GitHubAppID++ },
		func(p *SetupAppPin) { p.ConfigRevision++ },
		func(p *SetupAppPin) { p.CredentialGeneration++ },
	} {
		candidate := pin
		mutate(&candidate)
		if _, err := resolveCredential(candidate); !errors.Is(err, ErrAppBindingDenied) {
			t.Fatalf("mismatched OAuth pin accepted: %v", err)
		}
	}
	for _, test := range []struct{ name, mutate, restore string }{
		{"revision", `UPDATE tenant_github_integrations SET config_revision=4 WHERE id='setup_integration'`, `UPDATE tenant_github_integrations SET config_revision=3 WHERE id='setup_integration'`},
		{"generation", `UPDATE tenant_github_integrations SET credential_generation=7 WHERE id='setup_integration'`, `UPDATE tenant_github_integrations SET credential_generation=6 WHERE id='setup_integration'`},
		{"disabled", `UPDATE tenant_github_integrations SET status='disabled' WHERE id='setup_integration'`, `UPDATE tenant_github_integrations SET status='active' WHERE id='setup_integration'`},
		{"binding revoked", `UPDATE github_credential_bindings SET status='disabled' WHERE id='setup_binding'`, `UPDATE github_credential_bindings SET status='approved' WHERE id='setup_binding'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, test.mutate); err != nil {
				t.Fatal(err)
			}
			_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (SetupState, error) {
				return ValidateSetupState(ctx, tx, stateHash, now)
			})
			if !errors.Is(err, ErrInvalidSetupState) {
				t.Fatalf("changed setup state accepted: %v", err)
			}
			if _, err := pool.Exec(ctx, test.restore); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (SetupAppPin, error) {
		return ResolveSetupAppPin(ctx, tx, "tenant_other", "setup_integration")
	}); !errors.Is(err, ErrAppBindingDenied) {
		t.Fatalf("cross-Tenant pin resolved: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, stateHash, 9201, "acme", now, 912)
	}); !errors.Is(err, ErrSetupInstallationMismatch) {
		t.Fatalf("wrong-App callback accepted: %v", err)
	}
	installation, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, stateHash, 9201, "acme", now, pin.GitHubAppID)
	})
	if err != nil || installation.Status != "active" {
		t.Fatalf("complete owned setup: %+v %v", installation, err)
	}
	var integration string
	if err := pool.QueryRow(ctx, `SELECT integration_id FROM tenant_github_app_installation_bindings WHERE installation_id=9201`).Scan(&integration); err != nil || integration != pin.IntegrationID {
		t.Fatalf("binding: %s %v", integration, err)
	}
	sharedHash := HashSetupState("shared-setup-cannot-replace-owned")
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		if err := CreateSetupState(ctx, tx, pin.TenantID, "usr_setup_app", sharedHash, strings.Repeat("v", 43), now, now.Add(10*time.Minute)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, SetSetupInstallation(ctx, tx, sharedHash, 9201, now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (Installation, error) {
		return CompleteSetupState(ctx, tx, sharedHash, 9201, "acme", now)
	}); !errors.Is(err, ErrInstallationBound) {
		t.Fatalf("shared callback replaced owned binding: %v", err)
	}
}
