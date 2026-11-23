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
	workspacestore "github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresWorkspaceCheckoutResolverRequiresCurrentTenantPin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	sha := strings.Repeat("a", 40)
	seeds := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_ws_broker','Workspace broker',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_ws_broker','ws-broker-user','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp-ws-broker','aws','ap-northeast-1','["microvm"]')`, nil},
		{`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('ws_broker_credential','tenant_ws_broker',811,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/key-AbCdEf','oauth','webhook',$1,'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`, []any{now}},
		{`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('ws_broker_registration',811,'tenant_owned','tenant_ws_broker','workspace-broker','client',$1)`, []any{now}},
		{`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('ws_broker_integration','tenant_ws_broker','ws_broker_registration',811,'ws_broker_credential','active',4,6,'ws-broker-key','hash','usr_ws_broker',$1,$1)`, []any{now}},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_ws_broker',8111,'acme','active','usr_ws_broker',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8111,'tenant_ws_broker',$1)`, []any{now}},
		{`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at)
		 VALUES ('tenant_ws_broker','ws_broker_integration',8111,$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at)
		 VALUES ('tenant_ws_broker',8111,8112,'acme','workspace','main',$1)`, []any{now}},
		{`INSERT INTO workspaces(id,tenant_id,owner_id,name,runtime_class,image,created_at,updated_at)
		 VALUES ('ws_broker','tenant_ws_broker','usr_ws_broker','workspace','standard','hako/test',$1,$1)`, []any{now}},
		{`INSERT INTO workspace_status(workspace_id,desired_state,observed_state,updated_at) VALUES ('ws_broker','running','running',$1)`, []any{now}},
		{`INSERT INTO placements(workspace_id,resource_plane_id,placed_at) VALUES ('ws_broker','rp-ws-broker',$1)`, []any{now}},
		{`INSERT INTO workspace_github_checkouts(tenant_id,workspace_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,commit_sha,checkout_state,created_at,updated_at)
		 VALUES ('tenant_ws_broker','ws_broker',8111,8112,'acme','workspace','main',NULL,'pending',$1,$1)`, []any{now}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed Workspace checkout Broker: %v", err)
		}
	}
	identity := WorkspaceCheckoutIdentity{TenantID: "tenant_ws_broker", WorkspaceID: "ws_broker", ResourcePlaneID: "rp-ws-broker"}
	resolve := NewPostgresWorkspaceCheckoutResolver(pool, transaction.DefaultPolicy(), func() time.Time { return now })
	caller := "spiffe://hako/resource-plane/rp-ws-broker"
	resolved, err := resolve(ctx, caller, identity)
	if err != nil || resolved.Identity != identity || resolved.CommitSHA != "" || resolved.Install != 8111 || resolved.RepositoryID != 8112 ||
		resolved.Repository.Owner != "acme" || resolved.Repository.Name != "workspace" || resolved.Access.IntegrationID != "ws_broker_integration" ||
		resolved.Access.ConfigRevision != 4 || resolved.Access.CredentialGeneration != 6 || resolved.Access.SecretVersionID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("resolve Workspace Tenant pin = %+v err=%v", resolved, err)
	}
	if err := workspacestore.PinWorkspaceRepositoryCommit(ctx, pool, transaction.DefaultPolicy(), identity.TenantID, identity.WorkspaceID, sha, now); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolve(ctx, caller, identity)
	if err != nil || resolved.CommitSHA != sha {
		t.Fatalf("resolve initially pinned commit = %+v err=%v", resolved, err)
	}
	if _, err := resolve(ctx, caller, WorkspaceCheckoutIdentity{TenantID: "tenant_other", WorkspaceID: identity.WorkspaceID, ResourcePlaneID: identity.ResourcePlaneID}); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatalf("foreign Tenant checkout identity was accepted: %v", err)
	}
	if _, err := resolve(ctx, "spiffe://hako/resource-plane/rp-other", identity); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatalf("foreign Resource Plane accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_github_checkouts SET checkout_state='ready' WHERE workspace_id='ws_broker'`); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolve(ctx, caller, identity)
	if err != nil || resolved.CommitSHA != sha {
		t.Fatalf("ready checkout could not be retried at pinned commit: %+v err=%v", resolved, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_github_checkouts SET checkout_state='pending'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET status='revoked' WHERE installation_id=8111`); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(ctx, caller, identity); !errors.Is(err, ErrCheckoutDenied) {
		t.Fatalf("suspended Installation was accepted: %v", err)
	}
}
