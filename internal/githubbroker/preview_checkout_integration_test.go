//go:build integration

package githubbroker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresPreviewCheckoutResolverRequiresCurrentClaimAndPinnedTenantApp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	head := strings.Repeat("a", 40)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_broker','Preview broker',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_broker','preview-broker','',$1)`, []any{now}},
		{`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('preview_broker_credential','tenant_preview_broker',811,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/key-AbCdEf','oauth','webhook',$1,'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`, []any{now}},
		{`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('preview_broker_registration',811,'tenant_owned','tenant_preview_broker','preview-broker','client',$1)`, []any{now}},
		{`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('preview_broker_integration','tenant_preview_broker','preview_broker_registration',811,'preview_broker_credential','active',4,6,'preview-broker-key','hash','usr_preview_broker',$1,$1)`, []any{now}},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_preview_broker',8111,'preview-owner','active','usr_preview_broker',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8111,'tenant_preview_broker',$1)`, []any{now}},
		{`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at)
		 VALUES ('tenant_preview_broker','preview_broker_integration',8111,$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at)
		 VALUES ('tenant_preview_broker',8111,8112,'preview-owner','preview-repo','main',$1)`, []any{now}},
		{`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,generation,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_broker',8111,8112,21,$2,'feature/preview','present','awaiting_runtime','preview-broker-delivery',$1,3,$3,$1,$1)`, []any{now, head, now.Add(time.Hour)}},
		{`INSERT INTO repository_preview_runtime_allocations(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,target_resource_plane_id,provision_attempts,provision_claim_id,provision_lease_expires_at,github_integration_id,github_config_revision,github_credential_generation,created_at,updated_at)
		 VALUES ('tenant_preview_broker',8111,8112,21,3,$2,'feature/preview','present','provisioning',$3,'rp-test',2,'claim_preview_123',$3,'preview_broker_integration',4,6,$1,$1)`, []any{now, head, now.Add(time.Hour)}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	identity := PreviewCheckoutIdentity{TenantID: "tenant_preview_broker", InstallationID: 8111, RepositoryID: 8112,
		PullRequest: 21, Generation: 3, ClaimID: "claim_preview_123", ClaimAttempt: 2, ResourcePlaneID: "rp-test", HeadSHA: head}
	resolve := NewPostgresPreviewCheckoutResolver(pool, transaction.DefaultPolicy(), func() time.Time { return now })
	resolved, err := resolve(ctx, "spiffe://hako/resource-plane/rp-test", identity)
	if err != nil || resolved.Identity != identity || resolved.Access.IntegrationID != "preview_broker_integration" ||
		resolved.Access.ConfigRevision != 4 || resolved.Access.CredentialGeneration != 6 || resolved.Access.SecretVersionID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("resolve pinned Preview App for current claim = %+v err=%v", resolved, err)
	}
	for name, mutate := range map[string]func(*PreviewCheckoutIdentity){
		"foreign plane":    func(value *PreviewCheckoutIdentity) { value.ResourcePlaneID = "rp-other" },
		"stale generation": func(value *PreviewCheckoutIdentity) { value.Generation-- },
		"stale claim":      func(value *PreviewCheckoutIdentity) { value.ClaimID = "claim_preview_old" },
		"stale attempt":    func(value *PreviewCheckoutIdentity) { value.ClaimAttempt++ },
		"wrong head":       func(value *PreviewCheckoutIdentity) { value.HeadSHA = strings.Repeat("b", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := identity
			mutate(&candidate)
			_, err := resolve(ctx, "spiffe://hako/resource-plane/rp-test", candidate)
			if !errors.Is(err, ErrCheckoutDenied) {
				t.Fatalf("stale/foreign Preview checkout identity was accepted: %v", err)
			}
		})
	}
	if _, err := resolve(ctx, "spiffe://hako/resource-plane/rp-other", identity); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatal("caller Plane differing from persisted allocation was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation=7 WHERE id='preview_broker_integration'`); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(ctx, "spiffe://hako/resource-plane/rp-test", identity); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatal("rotated Tenant App credential was used for a pinned Preview generation")
	}
}
